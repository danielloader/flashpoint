package runner

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/danielloader/flashpoint/internal/config"
	"github.com/danielloader/flashpoint/internal/event"
	"github.com/danielloader/flashpoint/internal/proc"
)

// readyTimeout bounds the wait for a new server to answer its health path.
const readyTimeout = 60 * time.Second

// trigger is one reason to build: saved files, or a request.
type trigger struct {
	files   []string
	reasons []string // "signal", "cli", "key"
	force   bool     // restart even if the binary did not change
	waiters []chan<- Result
}

// Result is how a build cycle ended, for `flashpoint reload --wait`.
type Result struct {
	OK      bool     `json:"ok"`
	State   string   `json:"state"` // queued, ready, no_change, build_failed, not_ready, stopping
	BuildMs int64    `json:"buildMs,omitempty"`
	Detail  string   `json:"detail,omitempty"`
	Errors  []string `json:"errors,omitempty"` // compiler output, or the server's last lines
	// Logs are the log files in use, by stream.
	Logs map[string]string `json:"logs,omitempty"`

	took time.Duration
}

func marker(n int, r Result) string {
	switch r.State {
	case "ready":
		return fmt.Sprintf("--- build #%d ok in %s", n, event.Seconds(r.took))
	case "no_change":
		return fmt.Sprintf("--- build #%d ok in %s, no change", n, event.Seconds(r.took))
	case "build_failed":
		return fmt.Sprintf("--- build #%d failed in %s", n, event.Seconds(r.took))
	}
	return fmt.Sprintf("--- build #%d %s: %s", n, strings.ReplaceAll(r.State, "_", " "), r.Detail)
}

// queue holds at most one pending trigger; pushes while a build runs merge
// into it, so a burst during a build is one more build, not many.
type queue struct {
	mu sync.Mutex
	t  *trigger
	c  chan struct{}
}

func newQueue() *queue { return &queue{c: make(chan struct{}, 1)} }

func (q *queue) push(t trigger) {
	q.mu.Lock()
	if q.t == nil {
		q.t = &trigger{}
	}
	q.t.files = append(q.t.files, t.files...)
	q.t.reasons = append(q.t.reasons, t.reasons...)
	q.t.waiters = append(q.t.waiters, t.waiters...)
	q.t.force = q.t.force || t.force
	q.mu.Unlock()
	select {
	case q.c <- struct{}{}:
	default:
	}
}

func (q *queue) take() (trigger, bool) {
	q.mu.Lock()
	defer q.mu.Unlock()
	if q.t == nil {
		return trigger{}, false
	}
	t := *q.t
	q.t = nil
	return t, true
}

// apiServer builds the Go server and restarts it. All of its methods run on
// the loop goroutine except where noted.
type apiServer struct {
	plan    *config.Plan
	log     *logger
	bin     string
	env     []string
	started bool

	p      *proc.Proc
	sum    [sha256.Size]byte
	off    *offline
	exited chan *proc.Proc
	tail   *tail
	probe  *http.Client
	builds int
	logs   map[string]string
}

func (a *apiServer) loop(ctx context.Context, q *queue, relist func() error) {
	for {
		select {
		case <-ctx.Done():
			if t, ok := q.take(); ok {
				reply(t.waiters, Result{State: "stopping", Detail: "flashpoint is stopping"})
			}
			a.off.stop()
			a.stopChild()
			return
		case <-q.c:
			t, ok := q.take()
			if !ok {
				continue
			}
			a.builds++
			why := "start"
			switch {
			case len(t.reasons) > 0:
				why = strings.Join(uniq(t.reasons), ", ")
			case len(t.files) > 0:
				why = summarise(t.files)
			}
			a.log.line(event.API, event.Marker, fmt.Sprintf("--- build #%d started (%s)", a.builds, why))
			res := a.cycle(ctx, t)
			if ctx.Err() != nil {
				res = Result{State: "stopping", Detail: "flashpoint is stopping"}
			}
			a.log.line(event.API, event.Marker, marker(a.builds, res))
			res.Logs = a.logs
			reply(t.waiters, res)
			if ctx.Err() == nil {
				if err := relist(); err != nil {
					a.log.errorf(event.API, "go list: %v", err)
				}
			}
		case p := <-a.exited:
			if p != a.p || p.Stopping() {
				continue
			}
			code := p.ExitCode()
			a.log.errorf(event.API, "exited with code %d; waiting for a change (r rebuilds)", code)
			a.log.update(func(s *event.Status) {
				s.API, s.Serving, s.APIDetail = event.APICrashed, false, fmt.Sprintf("exited with code %d", code)
			})
			a.goOffline(fmt.Sprintf("the API server exited with code %d.\n\nIts last output:\n\n%s", code, a.tail.String()))
		}
	}
}

func (a *apiServer) cycle(ctx context.Context, t trigger) Result {
	switch {
	case len(t.files) > 0 && len(t.reasons) > 0:
		a.log.infof(event.API, "%s changed, and reload requested (%s); building", summarise(t.files), strings.Join(uniq(t.reasons), ", "))
	case len(t.files) > 0:
		a.log.infof(event.API, "%s changed; building", summarise(t.files))
	case len(t.reasons) > 0:
		a.log.infof(event.API, "reload requested (%s); building", strings.Join(uniq(t.reasons), ", "))
	case a.started:
		a.log.infof(event.API, "building")
	}
	a.log.update(func(s *event.Status) { s.API = event.APIBuilding })

	t0 := time.Now()
	out, err := a.build(ctx)
	if ctx.Err() != nil {
		return Result{}
	}
	tb := time.Since(t0)
	if err != nil {
		summary := a.buildFailed(out, tb)
		return Result{State: "build_failed", BuildMs: tb.Milliseconds(), Detail: summary, Errors: lines(string(out)), took: tb}
	}
	sum, err := hashFile(a.bin)
	if err != nil {
		a.log.errorf(event.API, "%v", err)
		return Result{State: "not_ready", Detail: err.Error()}
	}
	if sum == a.sum && a.p.Running() && !t.force {
		msg := fmt.Sprintf("no change (%s)", event.Seconds(tb))
		a.log.infof(event.API, "%s", msg)
		a.log.update(func(s *event.Status) { s.API, s.APIDetail, s.LastBuild = event.APIReady, "", tb })
		return Result{OK: true, State: "no_change", BuildMs: tb.Milliseconds(), Detail: msg, took: tb}
	}
	a.sum = sum

	t1 := time.Now()
	a.stopChild()
	a.off.stop()
	a.off = nil
	a.waitPortFree()
	if err := a.startChild(); err != nil {
		a.log.errorf(event.API, "start: %v", err)
		a.log.update(func(s *event.Status) { s.API, s.Serving, s.APIDetail = event.APIOffline, false, err.Error() })
		a.goOffline("flashpoint could not start the API server: " + err.Error())
		return Result{State: "not_ready", Detail: err.Error()}
	}
	if !a.waitReady(ctx) {
		detail := "the server did not answer " + a.plan.Health
		if !a.p.Running() {
			detail = fmt.Sprintf("the server exited with code %d", a.p.ExitCode())
		}
		return Result{State: "not_ready", BuildMs: tb.Milliseconds(), Detail: detail, Errors: lines(a.tail.String())}
	}
	total := time.Since(t0)
	msg := fmt.Sprintf("ready in %s (build %s, restart %s)", event.Seconds(total), event.Seconds(tb), event.Seconds(time.Since(t1)))
	a.log.infof(event.API, "%s", msg)
	a.log.update(func(s *event.Status) {
		s.API, s.APIDetail, s.Serving, s.LastBuild, s.LastReady = event.APIReady, "", true, tb, total
	})
	a.started = true
	return Result{OK: true, State: "ready", BuildMs: tb.Milliseconds(), Detail: msg, took: total}
}

func lines(s string) []string {
	var out []string
	for _, l := range strings.Split(strings.TrimRight(s, "\n"), "\n") {
		if l != "" {
			out = append(out, l)
		}
	}
	return out
}

func reply(waiters []chan<- Result, r Result) {
	for _, w := range waiters {
		w <- r
	}
}

func uniq(s []string) []string {
	var out []string
	seen := map[string]bool{}
	for _, v := range s {
		if !seen[v] {
			seen[v] = true
			out = append(out, v)
		}
	}
	return out
}

func (a *apiServer) build(ctx context.Context) ([]byte, error) {
	// Fast dev flags: no VCS stamp and no DWARF; neither fragments the build
	// cache that go test shares. No -trimpath, so stack traces stay paths.
	args := append([]string{"build", "-buildvcs=false", "-ldflags=-w"}, a.plan.BuildFlags...)
	args = append(args, "-o", a.bin, a.plan.Main)
	cmd := exec.CommandContext(ctx, "go", args...)
	cmd.Dir = a.plan.Root
	var buf bytes.Buffer
	cmd.Stdout, cmd.Stderr = &buf, &buf
	err := cmd.Run()
	return buf.Bytes(), err
}

var goErr = regexp.MustCompile(`^\S+\.go:\d+(:\d+)?: `)

func (a *apiServer) buildFailed(out []byte, tb time.Duration) string {
	state := "nothing is serving until it builds"
	if a.p.Running() {
		state = "still serving the last good build"
	}
	a.log.errorf(event.API, "build failed in %s; %s", event.Seconds(tb), state)
	var first uint64
	var summary string
	var errs int
	sc := bufio.NewScanner(bytes.NewReader(out))
	for sc.Scan() {
		l := sc.Text()
		n := a.log.line(event.API, event.Diagnostic, l)
		if first == 0 {
			first = n
		}
		if goErr.MatchString(l) {
			errs++
			if summary == "" {
				summary = l
			}
		}
	}
	if summary == "" {
		summary = firstLine(out)
	}
	if errs > 1 {
		summary += fmt.Sprintf(" (+%d more)", errs-1)
	}
	running := a.p.Running()
	a.log.update(func(s *event.Status) {
		s.API, s.APIDetail, s.ErrorSeq, s.LastBuild, s.Serving = event.APIBuildFailed, summary, first, tb, running
	})
	if !running {
		a.goOffline("the API build failed:\n\n" + string(out))
	}
	return summary
}

func firstLine(b []byte) string {
	for _, l := range strings.Split(string(b), "\n") {
		if l = strings.TrimSpace(l); l != "" && !strings.HasPrefix(l, "#") {
			return l
		}
	}
	return "go build failed"
}

// portFreeTimeout bounds the wait for the old server's port to be released.
const portFreeTimeout = 2 * time.Second

// waitPortFree waits until the port can be bound again. The old process has
// exited by now, but a child it left behind could still hold the socket;
// after the bound the new server is started anyway and reports the clash.
func (a *apiServer) waitPortFree() {
	addr := a.addr()
	deadline := time.Now().Add(portFreeTimeout)
	for {
		ln, err := net.Listen("tcp", addr)
		if err == nil {
			ln.Close()
			return
		}
		if time.Now().After(deadline) {
			a.log.errorf(event.API, "port %d still in use %s after the old server stopped", a.plan.APIPort, portFreeTimeout)
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func (a *apiServer) addr() string {
	return net.JoinHostPort(a.plan.APIHost, strconv.Itoa(a.plan.APIPort))
}

func (a *apiServer) startChild() error {
	spec := proc.Spec{
		Path:       a.bin,
		Args:       a.plan.Args,
		Dir:        a.plan.Root,
		Env:        a.env,
		StopSignal: a.plan.StopSignal,
		Grace:      a.plan.StopTimeout,
		Line: func(s string) {
			a.tail.add(s)
			a.log.line(event.API, event.Output, s)
		},
	}
	a.tail.reset()
	p, err := proc.Start(spec)
	if err != nil {
		return err
	}
	a.p = p
	go func() {
		<-p.Done()
		a.exited <- p
	}()
	return nil
}

func (a *apiServer) stopChild() {
	if a.p.Running() {
		a.p.Stop()
	}
}

// waitReady polls the health path until the server answers.
func (a *apiServer) waitReady(ctx context.Context) bool {
	host := a.plan.APIHost
	if host == "" || host == "0.0.0.0" || host == "::" {
		host = "127.0.0.1"
	}
	url := "http://" + net.JoinHostPort(host, strconv.Itoa(a.plan.APIPort)) + a.plan.Health
	c := a.probe
	// None is kept open to the server between restarts.
	defer c.CloseIdleConnections()
	poll := backoff{d: 20 * time.Millisecond, max: 250 * time.Millisecond}
	deadline := time.Now().Add(readyTimeout)
	for p := a.p; time.Now().Before(deadline); {
		if !p.Running() || ctx.Err() != nil {
			return false
		}
		req, _ := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
		if r, err := c.Do(req); err == nil {
			io.Copy(io.Discard, r.Body)
			r.Body.Close()
			if r.StatusCode < 500 {
				return true
			}
		}
		select {
		case <-ctx.Done():
			return false
		case <-p.Done():
			return false
		case <-time.After(poll.next()):
		}
	}
	a.log.errorf(event.API, "not answering %s after %s", url, readyTimeout)
	a.log.update(func(s *event.Status) {
		s.API, s.APIDetail = event.APIOffline, "not answering "+a.plan.Health
	})
	return false
}

// goOffline answers on the API port while no server runs, until the next
// start, which closes it first.
func (a *apiServer) goOffline(text string) {
	a.off.stop()
	body := "flashpoint: " + text
	a.off = serveOffline(a.addr(), body)
}

// summarise names the changed files for a log line.
func summarise(files []string) string {
	switch len(files) {
	case 0:
		return "nothing"
	case 1:
		return files[0]
	case 2:
		return files[0] + " and " + files[1]
	}
	return fmt.Sprintf("%s and %d more", files[0], len(files)-1)
}

func hashFile(name string) ([sha256.Size]byte, error) {
	var sum [sha256.Size]byte
	f, err := os.Open(name)
	if err != nil {
		return sum, err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return sum, err
	}
	copy(sum[:], h.Sum(nil))
	return sum, nil
}

// tail keeps the server's last lines for the offline page after a crash.
type tail struct {
	mu    sync.Mutex
	lines []string
}

const tailLines = 30

func (t *tail) add(s string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.lines = append(t.lines, s)
	if len(t.lines) > tailLines {
		t.lines = t.lines[len(t.lines)-tailLines:]
	}
}

func (t *tail) reset() {
	t.mu.Lock()
	t.lines = nil
	t.mu.Unlock()
}

func (t *tail) String() string {
	t.mu.Lock()
	defer t.mu.Unlock()
	return strings.Join(t.lines, "\n")
}

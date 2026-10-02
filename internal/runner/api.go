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
	"runtime"
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

// trigger is one reason to build: saved files, or a manual rebuild.
type trigger struct {
	files []string
	force bool // restart even if the binary did not change
}

// queue holds at most one pending trigger; pushes while a build runs merge
// into it, so a burst during a build is one more build, not many.
type queue struct {
	mu sync.Mutex
	t  *trigger
	c  chan struct{}
}

func newQueue() *queue { return &queue{c: make(chan struct{}, 1)} }

func (q *queue) push(files []string, force bool) {
	q.mu.Lock()
	if q.t == nil {
		q.t = &trigger{}
	}
	q.t.files = append(q.t.files, files...)
	q.t.force = q.t.force || force
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

// apiServer builds the Go server and swaps it in. All of its methods run on
// the loop goroutine except where noted.
type apiServer struct {
	plan    *config.Plan
	log     *logger
	bin     string
	env     []string
	ln      *os.File // the held socket; nil when the server binds its own
	reload  string   // touched after each swap, for the Vite plugin
	started bool

	p       *proc.Proc
	sum     [sha256.Size]byte
	off     *offline
	exited  chan *proc.Proc
	tail    *tail
	offText string
}

func (a *apiServer) loop(ctx context.Context, q *queue, relist func() error) {
	for {
		select {
		case <-ctx.Done():
			a.off.stop()
			a.stopChild()
			return
		case <-q.c:
			t, ok := q.take()
			if !ok {
				continue
			}
			a.cycle(ctx, t)
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

func (a *apiServer) cycle(ctx context.Context, t trigger) {
	if len(t.files) > 0 {
		a.log.infof(event.API, "%s changed; building", summarise(t.files))
	} else if a.started {
		a.log.infof(event.API, "building")
	}
	a.log.update(func(s *event.Status) { s.API = event.APIBuilding })
	// Requests wait in the accept queue during the build rather than get a
	// stale error from the offline page.
	a.off.stop()
	a.off = nil

	t0 := time.Now()
	out, err := a.build(ctx)
	if ctx.Err() != nil {
		return
	}
	tb := time.Since(t0)
	if err != nil {
		a.buildFailed(out, tb)
		return
	}
	sum, err := hashFile(a.bin)
	if err != nil {
		a.log.errorf(event.API, "%v", err)
		return
	}
	if sum == a.sum && a.p.Running() && !t.force {
		a.log.infof(event.API, "no change (%s)", event.Seconds(tb))
		a.log.update(func(s *event.Status) { s.API, s.APIDetail, s.LastBuild = event.APIReady, "", tb })
		return
	}
	a.sum = sum

	var tp time.Duration
	if a.ln != nil && runtime.GOOS == "darwin" {
		tp = a.preflight(ctx)
	}
	t1 := time.Now()
	a.stopChild()
	if err := a.startChild(); err != nil {
		a.log.errorf(event.API, "start: %v", err)
		a.log.update(func(s *event.Status) { s.API, s.Serving, s.APIDetail = event.APIOffline, false, err.Error() })
		a.goOffline("flashpoint could not start the API server: " + err.Error())
		return
	}
	if !a.waitReady(ctx) {
		return
	}
	total := time.Since(t0)
	detail := fmt.Sprintf("build %s", event.Seconds(tb))
	if tp > 0 {
		detail += fmt.Sprintf(", preflight %s", event.Seconds(tp))
	}
	what := "swap"
	if a.ln == nil {
		what = "restart"
	}
	detail += fmt.Sprintf(", %s %s", what, event.Seconds(time.Since(t1)))
	a.log.infof(event.API, "ready in %s (%s)", event.Seconds(total), detail)
	a.log.update(func(s *event.Status) {
		s.API, s.APIDetail, s.Serving, s.LastBuild, s.LastReady = event.APIReady, "", true, tb, total
	})
	if a.started {
		os.WriteFile(a.reload, []byte(strconv.FormatInt(time.Now().UnixNano(), 10)), 0o644)
	}
	a.started = true
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

func (a *apiServer) buildFailed(out []byte, tb time.Duration) {
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
}

func firstLine(b []byte) string {
	for _, l := range strings.Split(string(b), "\n") {
		if l = strings.TrimSpace(l); l != "" && !strings.HasPrefix(l, "#") {
			return l
		}
	}
	return "go build failed"
}

// preflight execs the new binary once while the old one still serves: macOS
// assesses a never-run executable on its first exec, which took 0.35–1s in
// measurements, and that should not be downtime. The listen package's init
// exits at once under FLASHPOINT_PREFLIGHT=1.
func (a *apiServer) preflight(ctx context.Context) time.Duration {
	t := time.Now()
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, a.bin)
	cmd.Dir = a.plan.Root
	cmd.Env = append(os.Environ(), "FLASHPOINT_PREFLIGHT=1")
	cmd.Run()
	return time.Since(t)
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
	if a.ln != nil {
		spec.ExtraFiles = []*os.File{a.ln}
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

// waitReady polls the health path until the server answers. Under handoff
// a request made before the server accepts waits in the queue rather than
// failing, so this returns as soon as it is up.
func (a *apiServer) waitReady(ctx context.Context) bool {
	host := a.plan.APIHost
	if host == "" || host == "0.0.0.0" || host == "::" {
		host = "127.0.0.1"
	}
	url := "http://" + net.JoinHostPort(host, strconv.Itoa(a.plan.APIPort)) + a.plan.Health
	c := http.Client{Timeout: 2 * time.Second}
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
		case <-time.After(20 * time.Millisecond):
		}
	}
	a.log.errorf(event.API, "not answering %s after %s", url, readyTimeout)
	a.log.update(func(s *event.Status) {
		s.API, s.APIDetail = event.APIOffline, "not answering "+a.plan.Health
	})
	return false
}

func (a *apiServer) goOffline(text string) {
	if a.ln == nil {
		return
	}
	a.off.stop()
	a.offText = "flashpoint: " + text
	body := a.offText
	a.off = serveOffline(a.ln, func() string { return body })
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

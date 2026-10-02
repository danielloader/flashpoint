package runner

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/danielloader/flashpoint/internal/event"
	"github.com/danielloader/flashpoint/internal/logfile"
)

// StateDirName is the per-project directory for the pidfile and the control
// socket. It ignores itself in git.
const StateDirName = ".flashpoint"

// ErrNotRunning means no flashpoint is running for the project.
var ErrNotRunning = errors.New("no flashpoint is running for this project")

type reqKind int

const (
	reqReload reqKind = iota
	reqRestartWeb
)

type request struct {
	kind   reqKind
	reason string      // "signal" or "cli"
	done   chan Result // nil: fire and forget
}

func waiters(c chan Result) []chan<- Result {
	if c == nil {
		return nil
	}
	return []chan<- Result{c}
}

// control is how something outside the TUI asks for a rebuild: SIGUSR1
// (SIGUSR2 restarts the web server), or `flashpoint reload` over a unix
// socket, which can wait for the result.
type control struct {
	pidfile, sock, addrFile string
	ln                      net.Listener
	sigs                    chan os.Signal
	requests                chan request
}

func openControl(root string) (*control, error) {
	dir := filepath.Join(root, StateDirName)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}
	gi := filepath.Join(dir, ".gitignore")
	if _, err := os.Stat(gi); err != nil {
		os.WriteFile(gi, []byte("*\n"), 0o644)
	}
	c := &control{
		pidfile:  filepath.Join(dir, "pid"),
		addrFile: filepath.Join(dir, "ctl.addr"),
		requests: make(chan request),
		sigs:     make(chan os.Signal, 4),
	}
	if pid := readPid(c.pidfile); pid > 0 && pid != os.Getpid() && alive(pid) {
		return nil, fmt.Errorf("flashpoint is already running for this project (pid %d)", pid)
	}
	if err := os.WriteFile(c.pidfile, []byte(strconv.Itoa(os.Getpid())+"\n"), 0o644); err != nil {
		return nil, err
	}
	c.sock = socketPath(dir)
	os.Remove(c.sock)
	ln, err := net.Listen("unix", c.sock)
	if err != nil {
		os.Remove(c.pidfile)
		return nil, fmt.Errorf("control socket: %w", err)
	}
	os.Chmod(c.sock, 0o600)
	c.ln = ln
	if link := filepath.Join(dir, "ctl"); link != c.sock {
		// connect(2) follows a symlink, and the link's own path is short
		// when given relatively, so `curl --unix-socket .flashpoint/ctl`
		// still works.
		os.Remove(link)
		os.Symlink(c.sock, link)
	}
	os.WriteFile(c.addrFile, []byte(c.sock+"\n"), 0o644)
	signal.Notify(c.sigs, syscall.SIGUSR1, syscall.SIGUSR2)
	return c, nil
}

// socketPath is .flashpoint/ctl, unless that is longer than a unix socket
// path may be (104 bytes on macOS); then a hashed name in the temp dir.
// ctl.addr records which.
func socketPath(dir string) string {
	p := filepath.Join(dir, "ctl")
	if len(p) < 100 {
		return p
	}
	sum := sha256.Sum256([]byte(dir))
	return filepath.Join(os.TempDir(), "flashpoint-"+hex.EncodeToString(sum[:])[:12]+".sock")
}

func (c *control) serve(ctx context.Context, status func() event.Status, logs *logfile.Sink) {
	send := func(r request) bool {
		select {
		case c.requests <- r:
			return true
		case <-ctx.Done():
			return false
		}
	}
	go func() {
		for {
			select {
			case <-ctx.Done():
				return
			case s := <-c.sigs:
				if s == syscall.SIGUSR2 {
					send(request{kind: reqRestartWeb, reason: "signal"})
				} else {
					send(request{kind: reqReload, reason: "signal"})
				}
			}
		}
	}()
	srv := &http.Server{Handler: c.handler(ctx, send, status, logs), ReadHeaderTimeout: 5 * time.Second}
	go srv.Serve(c.ln)
}

// handler is the control API, plain HTTP on the unix socket:
//
//	POST /reload[?wait=1]   rebuild; with wait, 200 when ready, 422 on failure
//	POST /restart-web       restart the dev server
//	GET  /status            both processes' states, URLs and the last build
func (c *control) handler(ctx context.Context, send func(request) bool, status func() event.Status, logs *logfile.Sink) http.Handler {
	mux := http.NewServeMux()
	queue := func(w http.ResponseWriter, r *http.Request, kind reqKind) {
		req := request{kind: kind, reason: "cli"}
		wait := r.URL.Query().Get("wait")
		if wait != "" && wait != "0" && wait != "false" {
			req.done = make(chan Result, 1)
		}
		res := Result{OK: true, State: "queued"}
		if !send(req) {
			res = stopping
		} else if req.done != nil {
			select {
			case res = <-req.done:
			case <-ctx.Done():
				res = stopping
			case <-r.Context().Done():
				return
			}
		}
		code := http.StatusOK
		switch {
		case res.State == "build_failed":
			code = http.StatusUnprocessableEntity
		case res.State == "stopping":
			code = http.StatusServiceUnavailable
		case !res.OK:
			code = http.StatusBadGateway
		}
		writeJSON(w, code, res)
	}
	mux.HandleFunc("POST /reload", func(w http.ResponseWriter, r *http.Request) { queue(w, r, reqReload) })
	mux.HandleFunc("POST /restart-web", func(w http.ResponseWriter, r *http.Request) { queue(w, r, reqRestartWeb) })
	mux.HandleFunc("GET /status", func(w http.ResponseWriter, r *http.Request) {
		d := statusJSON(status())
		d.Logs = logs.Paths()
		writeJSON(w, http.StatusOK, d)
	})
	mux.HandleFunc("GET /logs/{stream}", func(w http.ResponseWriter, r *http.Request) {
		n, _ := strconv.Atoi(r.URL.Query().Get("n"))
		since := r.URL.Query().Get("since_build")
		lines, ok := logs.Tail(r.PathValue("stream"), n, since != "" && since != "0" && since != "false")
		if !ok {
			http.Error(w, "unknown stream; use api, web, flashpoint or all", http.StatusNotFound)
			return
		}
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		for _, l := range lines {
			fmt.Fprintln(w, l)
		}
	})
	return mux
}

var stopping = Result{State: "stopping", Detail: "flashpoint is stopping"}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	enc.Encode(v)
}

type statusDoc struct {
	API struct {
		State    string `json:"state"`
		Detail   string `json:"detail,omitempty"`
		Serving  bool   `json:"serving"`
		URL      string `json:"url"`
		Handoff  bool   `json:"handoff"`
		BuildMs  int64  `json:"buildMs"`
		ReadyMs  int64  `json:"readyMs"`
		Watching bool   `json:"watching"`
	} `json:"api"`
	ProbeDials int64             `json:"probeDials"`
	Logs       map[string]string `json:"logs,omitempty"`
	Web        *struct {
		State  string `json:"state"`
		Detail string `json:"detail,omitempty"`
		URL    string `json:"url"`
	} `json:"web,omitempty"`
}

func statusJSON(s event.Status) statusDoc {
	var d statusDoc
	d.API.State, d.API.Detail, d.API.Serving, d.API.URL = s.API.String(), s.APIDetail, s.Serving, s.APIURL
	d.API.Handoff, d.API.Watching = s.Handoff, s.Watching
	d.API.BuildMs, d.API.ReadyMs = s.LastBuild.Milliseconds(), s.LastReady.Milliseconds()
	d.ProbeDials = s.ProbeDials
	if s.Web != event.WebNone {
		d.Web = &struct {
			State  string `json:"state"`
			Detail string `json:"detail,omitempty"`
			URL    string `json:"url"`
		}{s.Web.String(), s.WebDetail, s.WebURL}
	}
	return d
}

func (c *control) close() {
	signal.Stop(c.sigs)
	c.ln.Close()
	os.Remove(c.sock)
	os.Remove(c.addrFile)
	if link := filepath.Join(filepath.Dir(c.pidfile), "ctl"); link != c.sock {
		os.Remove(link)
	}
	if readPid(c.pidfile) == os.Getpid() {
		os.Remove(c.pidfile)
	}
}

func readPid(path string) int {
	b, err := os.ReadFile(path)
	if err != nil {
		return 0
	}
	n, _ := strconv.Atoi(strings.TrimSpace(string(b)))
	return n
}

func alive(pid int) bool {
	err := syscall.Kill(pid, 0)
	return err == nil || errors.Is(err, syscall.EPERM)
}

// Client returns an HTTP client that talks to the control socket of the
// flashpoint running for root, or ErrNotRunning.
func Client(root string) (*http.Client, error) {
	addr, err := os.ReadFile(filepath.Join(root, StateDirName, "ctl.addr"))
	if err != nil {
		return nil, ErrNotRunning
	}
	sock := strings.TrimSpace(string(addr))
	if c, err := net.DialTimeout("unix", sock, 2*time.Second); err != nil {
		return nil, ErrNotRunning
	} else {
		c.Close()
	}
	return &http.Client{Transport: &http.Transport{
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			var d net.Dialer
			return d.DialContext(ctx, "unix", sock)
		},
	}}, nil
}

// Request POSTs to path ("/reload" or "/restart-web") on root's flashpoint.
// With wait it returns once the rebuild has finished.
func Request(root, path string, wait bool, timeout time.Duration) (Result, error) {
	c, err := Client(root)
	if err != nil {
		return Result{}, err
	}
	c.Timeout = timeout
	url := "http://flashpoint" + path
	if wait {
		url += "?wait=1"
	}
	resp, err := c.Post(url, "", nil)
	if err != nil {
		var ne net.Error
		if errors.As(err, &ne) && ne.Timeout() {
			return Result{}, ErrTimeout
		}
		return Result{}, err
	}
	defer resp.Body.Close()
	var res Result
	if err := json.NewDecoder(resp.Body).Decode(&res); err != nil {
		return Result{}, fmt.Errorf("control socket: %s: %w", resp.Status, err)
	}
	return res, nil
}

// ErrTimeout means the rebuild did not finish within the timeout.
var ErrTimeout = errors.New("timed out waiting for the rebuild")

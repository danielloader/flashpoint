//go:build unix

package main

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"
)

var flashpointBin, stubWebBin string

// buildFlags builds flashpoint itself under -race when the tests run so.
var buildFlags []string

// maxOutage bounds the restart gap: the time from the old server stopping to
// the new one answering. It includes macOS's first-run scan of the new
// binary (0.3–1s) on a machine without Developer Tools enabled.
var maxOutage = 3 * time.Second

func TestMain(m *testing.M) {
	dir, err := os.MkdirTemp("", "flashpoint-e2e")
	if err != nil {
		panic(err)
	}
	flashpointBin = filepath.Join(dir, "flashpoint")
	// Build output goes here, not the user's cache directory.
	os.Setenv("FLASHPOINT_CACHE_DIR", filepath.Join(dir, "cache"))
	stubWebBin = filepath.Join(dir, "stubweb")
	for bin, pkg := range map[string]string{flashpointBin: ".", stubWebBin: "./testdata/stubweb"} {
		args := append(append([]string{"build"}, buildFlags...), "-o", bin, pkg)
		if out, err := exec.Command("go", args...).CombinedOutput(); err != nil {
			panic(string(out))
		}
	}
	code := m.Run()
	os.RemoveAll(dir)
	os.Exit(code)
}

// project copies examples/basic's server into a temporary module that builds
// against this checkout, with the stub in place of Vite.
func project(t *testing.T, apiPort, webPort int) string {
	t.Helper()
	repo, _ := filepath.Abs("../..")
	root, _ := filepath.EvalSymlinks(t.TempDir())
	main, err := os.ReadFile(filepath.Join(repo, "examples", "basic", "main.go"))
	if err != nil {
		t.Fatal(err)
	}
	files := map[string]string{
		"main.go": string(main),
		"go.mod":  "module example.com/e2e\n\ngo 1.26.0\n",
		// The app's own configuration picks its ports (PORT, the stub's
		// argument); flashpoint is only told them.
		"flashpoint.toml": "[api]\nport = " + strconv.Itoa(apiPort) + "\nenv = { PORT = \"" + strconv.Itoa(apiPort) + "\" }\n\n[web]\nport = " + strconv.Itoa(webPort) +
			"\ncommand = \"exec " + stubWebBin + " " + strconv.Itoa(webPort) + "\"\n\n[watch]\ndebounce = \"50ms\"\n",
	}
	for name, body := range files {
		if err := os.WriteFile(filepath.Join(root, name), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return root
}

type session struct {
	cmd   *exec.Cmd
	mu    sync.Mutex
	lines []string
	ready chan string
	done  chan struct{}
}

func start(t *testing.T, root string, args ...string) *session {
	t.Helper()
	cmd := exec.Command(flashpointBin, append([]string{"--no-tui"}, args...)...)
	cmd.Dir = root
	cmd.Env = os.Environ()
	out, _ := cmd.StdoutPipe()
	cmd.Stderr = cmd.Stdout
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	r := &session{cmd: cmd, ready: make(chan string, 16), done: make(chan struct{})}
	go func() {
		sc := bufio.NewScanner(out)
		for sc.Scan() {
			l := sc.Text()
			r.mu.Lock()
			r.lines = append(r.lines, l)
			r.mu.Unlock()
			if strings.HasPrefix(l, "api ▸ ready in") || strings.HasPrefix(l, "api ▸ build failed") || strings.HasPrefix(l, "api ▸ no change") {
				r.ready <- l
			}
		}
		cmd.Wait()
		close(r.done)
	}()
	t.Cleanup(func() {
		cmd.Process.Signal(syscall.SIGTERM)
		select {
		case <-r.done:
		case <-time.After(15 * time.Second):
			cmd.Process.Kill()
		}
		if t.Failed() {
			t.Logf("flashpoint output:\n%s", r.output())
		}
	})
	return r
}

func (r *session) output() string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return strings.Join(r.lines, "\n")
}

func (r *session) wait(t *testing.T, prefix string) {
	t.Helper()
	select {
	case l := <-r.ready:
		if !strings.HasPrefix(l, prefix) {
			t.Fatalf("got %q, want %q", l, prefix)
		}
	case <-r.done:
		t.Fatalf("flashpoint exited waiting for %q", prefix)
	case <-time.After(60 * time.Second):
		t.Fatalf("timed out waiting for %q", prefix)
	}
}

// testDials counts every connection the tests open to the API.
var testDials atomic.Int64

// client is shared, with enough idle connections for the probes, so they
// reuse keep-alive connections instead of churning through ephemeral ports.
var client = &http.Client{
	Timeout: 5 * time.Second,
	Transport: &http.Transport{
		MaxIdleConnsPerHost: 8,
		DialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
			testDials.Add(1)
			var d net.Dialer
			return d.DialContext(ctx, network, addr)
		},
	},
}

// ctlClient talks to the control socket by its relative path, as curl would:
// the absolute temp path is too long for a unix socket.
func ctlClient(t *testing.T, root string) *http.Client {
	t.Helper()
	t.Chdir(root)
	return &http.Client{Timeout: 5 * time.Second, Transport: &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
		var d net.Dialer
		return d.DialContext(ctx, "unix", ".flashpoint/ctl")
	}}}
}

type statusDoc struct {
	API struct {
		State   string `json:"state"`
		Serving bool   `json:"serving"`
	} `json:"api"`
	ProbeDials int64             `json:"probeDials"`
	Logs       map[string]string `json:"logs"`
}

func status(t *testing.T, c *http.Client) statusDoc {
	t.Helper()
	resp, err := c.Get("http://flashpoint/status")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var d statusDoc
	if err := json.NewDecoder(resp.Body).Decode(&d); err != nil {
		t.Fatal(err)
	}
	return d
}

func get(url string) (string, error) {
	res, err := client.Get(url)
	if err != nil {
		return "", err
	}
	defer res.Body.Close()
	b, _ := io.ReadAll(res.Body)
	if res.StatusCode != http.StatusOK {
		return string(b), &statusError{res.StatusCode}
	}
	return string(b), nil
}

type statusError struct{ code int }

func (e *statusError) Error() string { return "status " + strconv.Itoa(e.code) }

func edit(t *testing.T, root, old, new string) {
	t.Helper()
	p := filepath.Join(root, "main.go")
	b, _ := os.ReadFile(p)
	if !strings.Contains(string(b), old) {
		t.Fatalf("main.go has no %q", old)
	}
	if err := os.WriteFile(p, []byte(strings.Replace(string(b), old, new, 1)), 0o644); err != nil {
		t.Fatal(err)
	}
}

func portOpen(port int) bool {
	for _, host := range []string{"127.0.0.1", "::1"} {
		c, err := net.DialTimeout("tcp", net.JoinHostPort(host, strconv.Itoa(port)), 100*time.Millisecond)
		if err == nil {
			c.Close()
			return true
		}
	}
	return false
}

// spans groups failure times into outages: failures less than gap apart
// belong to the same one. It returns the longest outage.
func longestOutage(times []time.Time, gap time.Duration) time.Duration {
	var longest time.Duration
	for i := 0; i < len(times); {
		j := i
		for j+1 < len(times) && times[j+1].Sub(times[j]) < gap {
			j++
		}
		longest = max(longest, times[j].Sub(times[i]))
		i = j + 1
	}
	return longest
}

func TestRestartGapIsBounded(t *testing.T) {
	const apiPort, webPort = 8511, 5511
	root := project(t, apiPort, webPort)
	r := start(t, root)
	r.wait(t, "api ▸ ready in")
	url := "http://127.0.0.1:" + strconv.Itoa(apiPort) + "/api/hello"
	ctl := ctlClient(t, root)
	probeBefore := status(t, ctl).ProbeDials
	dialsBefore := testDials.Load()

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	var ok atomic.Int64
	var sawNew atomic.Bool
	var mu sync.Mutex
	var failures []time.Time
	var wg sync.WaitGroup
	for range 4 {
		wg.Go(func() {
			for ctx.Err() == nil {
				body, err := get(url)
				if err != nil {
					mu.Lock()
					failures = append(failures, time.Now())
					mu.Unlock()
				} else {
					ok.Add(1)
					if strings.Contains(body, "Hello from flashpoint") {
						sawNew.Store(true)
					}
				}
				time.Sleep(20 * time.Millisecond)
			}
		})
	}
	// A failed wait below must not leave the probes running.
	t.Cleanup(func() {
		cancel()
		wg.Wait()
	})
	nfail := func() int {
		mu.Lock()
		defer mu.Unlock()
		return len(failures)
	}

	time.Sleep(200 * time.Millisecond)
	// A broken build must leave the running server answering throughout.
	edit(t, root, `const message =`, `const message`)
	r.wait(t, "api ▸ build failed")
	time.Sleep(200 * time.Millisecond)
	if n := nfail(); n > 0 {
		t.Fatalf("%d requests failed while a broken build ran; the old server should have kept serving", n)
	}
	// Fixed, with a new message: one restart.
	edit(t, root, `const message "Hello from Go"`, `const message = "Hello from flashpoint"`)
	r.wait(t, "api ▸ ready in")
	time.Sleep(200 * time.Millisecond)
	cancel()
	wg.Wait()

	mu.Lock()
	outage := longestOutage(failures, 200*time.Millisecond)
	n := len(failures)
	mu.Unlock()
	t.Logf("restart: %d failed requests, longest outage %v, %d ok", n, outage, ok.Load())
	if outage > maxOutage {
		t.Fatalf("the API was down for %v during the restart", outage)
	}
	if !sawNew.Load() {
		t.Fatal("never saw the new build's response")
	}
	// Each probe reconnects once after the restart, and each failed request
	// was one refused dial; anything beyond that is churn.
	const probes, restarts = 4, 1
	td := testDials.Load() - dialsBefore
	pd := status(t, ctl).ProbeDials - probeBefore
	bound := int64(probes*(restarts+1) + n + 2)
	t.Logf("dials: test probes %d (bound %d), flashpoint probes %d (bound %d)", td, bound, pd, 10*restarts)
	if td > bound {
		t.Errorf("test probes opened %d connections for %d restart(s)", td, restarts)
	}
	if pd > 10*restarts {
		t.Errorf("flashpoint opened %d probe connections for %d restart(s)", pd, restarts)
	}
}

func TestIdenticalBinaryIsNotRestarted(t *testing.T) {
	const apiPort, webPort = 8512, 5512
	root := project(t, apiPort, webPort)
	r := start(t, root)
	r.wait(t, "api ▸ ready in")
	before, _ := get("http://127.0.0.1:" + strconv.Itoa(apiPort) + "/api/hello")
	// The same bytes again, as a formatter or a branch switch writes them.
	// (A comment edit in the main package would change the binary's build ID;
	// in any other package it does not.)
	p := filepath.Join(root, "main.go")
	b, _ := os.ReadFile(p)
	os.WriteFile(p, b, 0o644)
	r.wait(t, "api ▸ no change")
	after, _ := get("http://127.0.0.1:" + strconv.Itoa(apiPort) + "/api/hello")
	if before == "" || before != after {
		t.Fatalf("restarted: %q then %q", before, after)
	}
}

func TestStopLeavesNothingBehind(t *testing.T) {
	for _, sig := range []syscall.Signal{syscall.SIGTERM, syscall.SIGKILL} {
		t.Run(sig.String(), func(t *testing.T) {
			const apiPort, webPort = 8513, 5513
			root := project(t, apiPort, webPort)
			r := start(t, root)
			r.wait(t, "api ▸ ready in")
			deadline := time.Now().Add(10 * time.Second)
			for !portOpen(webPort) && time.Now().Before(deadline) {
				time.Sleep(50 * time.Millisecond)
			}
			r.cmd.Process.Signal(sig)
			<-r.done
			if sig == syscall.SIGTERM {
				if code := r.cmd.ProcessState.ExitCode(); code != 0 {
					t.Fatalf("exit code %d after SIGTERM", code)
				}
			}
			// After SIGKILL only the shims' pipe watch can clean up.
			deadline = time.Now().Add(5 * time.Second)
			for (portOpen(apiPort) || portOpen(webPort)) && time.Now().Before(deadline) {
				time.Sleep(50 * time.Millisecond)
			}
			if portOpen(apiPort) || portOpen(webPort) {
				t.Fatalf("a port is still held after %v", sig)
			}
		})
	}
}

func TestPortInUseFailsWithoutKilling(t *testing.T) {
	const apiPort, webPort = 8515, 5515
	ln, err := net.Listen("tcp", "127.0.0.1:"+strconv.Itoa(apiPort))
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	root := project(t, apiPort, webPort)
	cmd := exec.Command(flashpointBin, "--no-tui")
	cmd.Dir = root
	out, _ := cmd.CombinedOutput()
	if code := cmd.ProcessState.ExitCode(); code != exitPort {
		t.Fatalf("exit code %d, want %d\n%s", code, exitPort, out)
	}
	if !strings.Contains(string(out), "already in use") {
		t.Fatalf("output %s", out)
	}
	if c, err := net.Dial("tcp", ln.Addr().String()); err != nil {
		t.Fatal("the holder was disturbed")
	} else {
		c.Close()
	}
}

func TestOfflinePageExplainsAFailedBuild(t *testing.T) {
	const apiPort, webPort = 8516, 5516
	root := project(t, apiPort, webPort)
	edit(t, root, `const message =`, `const message`)
	r := start(t, root)
	r.wait(t, "api ▸ build failed")
	body, err := get("http://127.0.0.1:" + strconv.Itoa(apiPort) + "/api/hello")
	var se *statusError
	if !errors.As(err, &se) || se.code != http.StatusServiceUnavailable {
		t.Fatalf("err %v, want a 503", err)
	}
	if !strings.Contains(body, "build failed") || !strings.Contains(body, "main.go:") {
		t.Fatalf("body %q", body)
	}
	edit(t, root, `const message`, `const message =`)
	r.wait(t, "api ▸ ready in")
	if _, err := get("http://127.0.0.1:" + strconv.Itoa(apiPort) + "/api/hello"); err != nil {
		t.Fatal(err)
	}
}

func reloadCmd(t *testing.T, root string, args ...string) (int, string, string) {
	t.Helper()
	cmd := exec.Command(flashpointBin, append([]string{"reload"}, args...)...)
	cmd.Dir = root
	var stdout, stderr strings.Builder
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	cmd.Run()
	return cmd.ProcessState.ExitCode(), stdout.String(), stderr.String()
}

func TestSignalReloadWithWatchingOff(t *testing.T) {
	const apiPort, webPort = 8517, 5517
	root := project(t, apiPort, webPort)
	r := start(t, root, "--watch=false")
	r.wait(t, "api ▸ ready in")
	url := "http://127.0.0.1:" + strconv.Itoa(apiPort) + "/api/hello"

	edit(t, root, `"Hello from Go"`, `"Hello by signal"`)
	time.Sleep(600 * time.Millisecond)
	if strings.Contains(r.output(), "changed") {
		t.Fatal("rebuilt on a file change with watching off")
	}
	pid, err := os.ReadFile(filepath.Join(root, ".flashpoint", "pid"))
	if err != nil || strings.TrimSpace(string(pid)) != strconv.Itoa(r.cmd.Process.Pid) {
		t.Fatalf("pidfile %q, %v", pid, err)
	}
	r.cmd.Process.Signal(syscall.SIGUSR1)
	r.wait(t, "api ▸ ready in")
	if !strings.Contains(r.output(), "reload requested (signal)") {
		t.Fatal("no reload reason in the log")
	}
	if body, _ := get(url); !strings.Contains(body, "Hello by signal") {
		t.Fatalf("body %q", body)
	}
	r.cmd.Process.Signal(syscall.SIGTERM)
	<-r.done
	if _, err := os.Stat(filepath.Join(root, ".flashpoint", "pid")); !os.IsNotExist(err) {
		t.Fatal("pidfile left behind")
	}
}

func TestReloadWait(t *testing.T) {
	const apiPort, webPort = 8518, 5518
	root := project(t, apiPort, webPort)
	if code, _, _ := reloadCmd(t, root, "--wait"); code != exitNotRunning {
		t.Fatalf("exit %d with nothing running, want %d", code, exitNotRunning)
	}
	r := start(t, root, "--watch=false")
	r.wait(t, "api ▸ ready in")

	edit(t, root, `"Hello from Go"`, `"Hello by cli"`)
	code, stdout, stderr := reloadCmd(t, root, "--wait")
	if code != 0 || !strings.HasPrefix(stdout, "ready in") {
		t.Fatalf("exit %d, stdout %q, stderr %q", code, stdout, stderr)
	}
	if body, _ := get("http://127.0.0.1:" + strconv.Itoa(apiPort) + "/api/hello"); !strings.Contains(body, "Hello by cli") {
		t.Fatalf("reload --wait returned before the new build served: %q", body)
	}
	if !strings.Contains(r.output(), "reload requested (cli)") {
		t.Fatal("no reload reason in the log")
	}

	edit(t, root, `const message =`, `const message`)
	code, _, stderr = reloadCmd(t, root, "--wait")
	if code != exitError || !strings.Contains(stderr, "main.go:") || !strings.Contains(stderr, "build failed") {
		t.Fatalf("exit %d, stderr %q", code, stderr)
	}

	// The same socket speaks plain HTTP.
	d := status(t, ctlClient(t, root))
	if d.API.State != "build failed" || !d.API.Serving {
		t.Fatalf("status %+v", d)
	}
}

func TestIdleOpensNoConnections(t *testing.T) {
	const apiPort, webPort = 8519, 5519
	root := project(t, apiPort, webPort)
	r := start(t, root)
	r.wait(t, "api ▸ ready in")
	ctl := ctlClient(t, root)
	deadline := time.Now().Add(10 * time.Second)
	for !portOpen(webPort) && time.Now().Before(deadline) {
		time.Sleep(50 * time.Millisecond)
	}
	time.Sleep(time.Second)
	before := status(t, ctl).ProbeDials
	time.Sleep(10 * time.Second)
	after := status(t, ctl).ProbeDials
	t.Logf("flashpoint probe dials while idle for 10s: %d (total since start %d)", after-before, after)
	if after != before {
		t.Fatalf("flashpoint opened %d connections while idle", after-before)
	}
}

func TestLogDir(t *testing.T) {
	const apiPort, webPort = 8510, 5510
	root := project(t, apiPort, webPort)
	r := start(t, root, "--log-dir", filepath.Join(root, ".flashpoint", "logs"), "--watch=false")
	r.wait(t, "api ▸ ready in")
	edit(t, root, `const message =`, `const message`)
	if code, _, _ := reloadCmd(t, root, "--wait"); code != exitError {
		t.Fatalf("reload exit %d", code)
	}
	logs := filepath.Join(root, ".flashpoint", "logs")
	api, _ := os.ReadFile(filepath.Join(logs, "api.log"))
	for _, want := range []string{"=== flashpoint session started", "--- build #1 started (start)", "--- build #1 ok in", "listening on", "--- build #2 started (cli)", "--- build #2 failed in", "syntax error"} {
		if !strings.Contains(string(api), want) {
			t.Errorf("api.log lacks %q:\n%s", want, api)
		}
	}
	web, _ := os.ReadFile(filepath.Join(logs, "web.log"))
	if !strings.Contains(string(web), "stub web ready") || strings.Contains(string(web), "listening on") {
		t.Errorf("web.log:\n%s", web)
	}
	cmd := exec.Command(flashpointBin, "logs", "api", "--since-build")
	cmd.Dir = root
	out, err := cmd.Output()
	if err != nil || !strings.HasPrefix(string(out), "--- build #2 started") || strings.Contains(string(out), "listening on") {
		t.Fatalf("logs --since-build: %q, %v", out, err)
	}
	if d := status(t, ctlClient(t, root)); d.Logs["api"] != filepath.Join(logs, "api.log") {
		t.Fatalf("status logs %v", d.Logs)
	}
}

// writeConfig replaces the project's flashpoint.toml.
func writeConfig(t *testing.T, root, body string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(root, "flashpoint.toml"), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestDeclaredAPIPort(t *testing.T) {
	const apiPort, webPort = 8514, 5514
	root := project(t, apiPort, webPort)
	// No api.port in the file: the server listens where its own env says,
	// and flashpoint is told with --api-port.
	writeConfig(t, root, "[api]\nenv = { PORT = \"8514\" }\n\n[web]\nport = 5514\ncommand = \"exec "+stubWebBin+" 5514\"\n")
	r := start(t, root, "--api-port", "8514")
	r.wait(t, "api ▸ ready in")
	if body, err := get("http://127.0.0.1:8514/api/hello"); err != nil || !strings.Contains(body, "Hello") {
		t.Fatalf("body %q, %v", body, err)
	}
	if !strings.Contains(r.output(), "api http://localhost:8514 · web http://localhost:5514") {
		t.Fatal("startup line lacks the declared ports")
	}
}

func TestNotListeningHint(t *testing.T) {
	const apiPort, webPort = 8514, 5514
	root := project(t, apiPort, webPort)
	// The server listens on 8512, but flashpoint was told 8514.
	writeConfig(t, root, "[api]\nport = 8514\nready_timeout = \"1s\"\nenv = { PORT = \"8512\" }\n\n[web]\nport = 5514\ncommand = \"exec "+stubWebBin+" 5514\"\n")
	r := start(t, root)
	deadline := time.Now().Add(20 * time.Second)
	for !strings.Contains(r.output(), "API not listening on :8514 (check --api-port)") {
		if time.Now().After(deadline) {
			t.Fatal("no not-listening hint")
		}
		time.Sleep(100 * time.Millisecond)
	}
	if d := status(t, ctlClient(t, root)); d.API.State != "offline" {
		t.Fatalf("status %+v", d)
	}
}

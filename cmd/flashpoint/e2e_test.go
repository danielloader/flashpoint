//go:build unix

package main

import (
	"bufio"
	"context"
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

func TestMain(m *testing.M) {
	dir, err := os.MkdirTemp("", "flashpoint-e2e")
	if err != nil {
		panic(err)
	}
	flashpointBin = filepath.Join(dir, "flashpoint")
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
func project(t *testing.T, apiPort, webPort int, useListen bool) string {
	t.Helper()
	repo, _ := filepath.Abs("../..")
	root, _ := filepath.EvalSymlinks(t.TempDir())
	src, err := os.ReadFile(filepath.Join(repo, "examples", "basic", "main.go"))
	if err != nil {
		t.Fatal(err)
	}
	main := string(src)
	if !useListen {
		main = strings.Replace(main, "\t\"github.com/danielloader/flashpoint/listen\"\n", "\t\"net\"\n", 1)
		main = strings.Replace(main, "listen.Listen(\":\" + port)", "net.Listen(\"tcp\", \":\"+port)", 1)
	}
	files := map[string]string{
		"main.go": main,
		"go.mod":  "module example.com/e2e\n\ngo 1.26.0\n\nrequire github.com/danielloader/flashpoint v0.0.0\n\nreplace github.com/danielloader/flashpoint => " + repo + "\n",
		"flashpoint.toml": "[api]\nport = " + strconv.Itoa(apiPort) + "\nhealth = \"/healthz\"\n\n[web]\nport = " + strconv.Itoa(webPort) +
			"\ncommand = \"exec " + stubWebBin + " {port}\"\n\n[watch]\ndebounce = \"50ms\"\n",
	}
	if !useListen {
		files["go.mod"] = "module example.com/e2e\n\ngo 1.26.0\n"
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

func start(t *testing.T, root string) *session {
	t.Helper()
	cmd := exec.Command(flashpointBin, "--no-tui")
	cmd.Dir = root
	cmd.Env = append(os.Environ(), "FLASHPOINT_API_PORT=", "FLASHPOINT_WEB_PORT=")
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

func get(url string) (string, error) {
	c := http.Client{Timeout: 5 * time.Second}
	res, err := c.Get(url)
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

func TestSwapRefusesNoConnections(t *testing.T) {
	const apiPort, webPort = 8511, 5511
	root := project(t, apiPort, webPort, true)
	r := start(t, root)
	r.wait(t, "api ▸ ready in")
	url := "http://127.0.0.1:" + strconv.Itoa(apiPort) + "/api/hello"

	ctx, cancel := context.WithCancel(context.Background())
	var ok, failed atomic.Int64
	var sawNew atomic.Bool
	var firstErr atomic.Value
	var wg sync.WaitGroup
	for range 4 {
		wg.Go(func() {
			for ctx.Err() == nil {
				body, err := get(url)
				if err != nil {
					failed.Add(1)
					firstErr.CompareAndSwap(nil, err.Error())
				} else {
					ok.Add(1)
					if strings.Contains(body, "Hello from flashpoint") {
						sawNew.Store(true)
					}
				}
				time.Sleep(2 * time.Millisecond)
			}
		})
	}

	time.Sleep(200 * time.Millisecond)
	edit(t, root, `"Hello from Go"`, `"Hello from flashpoint"`)
	r.wait(t, "api ▸ ready in")
	// A broken build must leave the last good one serving.
	edit(t, root, `const message =`, `const message`)
	r.wait(t, "api ▸ build failed")
	time.Sleep(200 * time.Millisecond)
	// Fixed back to the bytes already serving: nothing to restart.
	edit(t, root, `const message`, `const message =`)
	r.wait(t, "api ▸ no change")
	time.Sleep(200 * time.Millisecond)
	cancel()
	wg.Wait()

	if n := failed.Load(); n > 0 {
		t.Fatalf("%d of %d requests failed during the swaps; first: %v", n, n+ok.Load(), firstErr.Load())
	}
	if !sawNew.Load() {
		t.Fatal("never saw the new build's response")
	}
	if ok.Load() < 50 {
		t.Fatalf("only %d requests ran", ok.Load())
	}
	if !portOpen(webPort) {
		t.Fatal("the web stub is not serving")
	}
}

func TestIdenticalBinaryIsNotRestarted(t *testing.T) {
	const apiPort, webPort = 8512, 5512
	root := project(t, apiPort, webPort, true)
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
			root := project(t, apiPort, webPort, true)
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

func TestServerWithoutListenStillReloads(t *testing.T) {
	const apiPort, webPort = 8514, 5514
	root := project(t, apiPort, webPort, false)
	r := start(t, root)
	r.wait(t, "api ▸ ready in")
	edit(t, root, `"Hello from Go"`, `"Hello again"`)
	r.wait(t, "api ▸ ready in")
	body, err := get("http://127.0.0.1:" + strconv.Itoa(apiPort) + "/api/hello")
	if err != nil || !strings.Contains(body, "Hello again") {
		t.Fatalf("body %q, err %v", body, err)
	}
	if !strings.Contains(r.output(), "binds its own port") {
		t.Fatal("no hint about the listen package")
	}
}

func TestPortInUseFailsWithoutKilling(t *testing.T) {
	const apiPort, webPort = 8515, 5515
	ln, err := net.Listen("tcp", "127.0.0.1:"+strconv.Itoa(apiPort))
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	root := project(t, apiPort, webPort, true)
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
	root := project(t, apiPort, webPort, true)
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

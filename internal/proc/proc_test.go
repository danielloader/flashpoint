//go:build unix

package proc

import (
	"os"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"
)

func TestMain(m *testing.M) {
	if len(os.Args) > 1 && os.Args[1] == ShimArg {
		os.Exit(RunShim(os.Args[2:]))
	}
	Self = func() (string, error) { return os.Args[0], nil }
	os.Exit(m.Run())
}

type lines struct {
	mu sync.Mutex
	l  []string
	ch chan string
}

func (l *lines) add(s string) {
	l.mu.Lock()
	l.l = append(l.l, s)
	l.mu.Unlock()
	l.ch <- s
}

func (l *lines) next(t *testing.T) string {
	t.Helper()
	select {
	case s := <-l.ch:
		return s
	case <-time.After(5 * time.Second):
		t.Fatal("no output")
		return ""
	}
}

func alive(pid int) bool { return syscall.Kill(pid, 0) == nil }

func TestStopKillsTheWholeGroup(t *testing.T) {
	out := &lines{ch: make(chan string, 16)}
	// The background sleep is a grandchild, as node is under npm: it must go
	// with the group, not be orphaned.
	p, err := Start(Spec{
		Path:  "sh",
		Args:  []string{"-c", `sleep 60 & echo $!; wait`},
		Env:   os.Environ(),
		Grace: 2 * time.Second,
		Line:  out.add,
	})
	if err != nil {
		t.Fatal(err)
	}
	grandchild, _ := strconv.Atoi(strings.TrimSpace(out.next(t)))
	if !alive(grandchild) {
		t.Fatal("grandchild not running")
	}
	p.Stop()
	if p.Running() {
		t.Fatal("still running after Stop")
	}
	deadline := time.Now().Add(2 * time.Second)
	for alive(grandchild) && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if alive(grandchild) {
		syscall.Kill(grandchild, syscall.SIGKILL)
		t.Fatal("grandchild outlived the stop")
	}
}

func TestExitCodePassesThrough(t *testing.T) {
	p, err := Start(Spec{Path: "sh", Args: []string{"-c", "exit 7"}, Env: os.Environ()})
	if err != nil {
		t.Fatal(err)
	}
	<-p.Done()
	if got := p.ExitCode(); got != 7 {
		t.Fatalf("exit code %d, want 7", got)
	}
}

func TestStopSignalReachesChild(t *testing.T) {
	out := &lines{ch: make(chan string, 16)}
	p, err := Start(Spec{
		Path:       "sh",
		Args:       []string{"-c", `trap 'echo got-int; exit 0' INT; echo ready; while :; do sleep 0.05; done`},
		Env:        os.Environ(),
		StopSignal: syscall.SIGINT,
		Grace:      5 * time.Second,
		Line:       out.add,
	})
	if err != nil {
		t.Fatal(err)
	}
	if s := out.next(t); s != "ready" {
		t.Fatalf("got %q", s)
	}
	start := time.Now()
	p.Stop()
	if time.Since(start) > 3*time.Second {
		t.Fatal("waited for the grace period: SIGINT did not arrive")
	}
	out.mu.Lock()
	defer out.mu.Unlock()
	if !strings.Contains(strings.Join(out.l, "\n"), "got-int") {
		t.Fatalf("output %q", out.l)
	}
	if p.ExitCode() != 0 {
		t.Fatalf("exit %d", p.ExitCode())
	}
}

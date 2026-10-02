// Package proc starts the processes flashpoint supervises.
//
// Each child runs under a small shim, flashpoint itself re-executed with
// ShimArg. The shim puts the child in a process group of its own and holds a
// pipe from flashpoint as stdin. When flashpoint stops it, or dies without
// stopping it (even by SIGKILL), the pipe ends and the shim signals the whole
// group: npm, the shell it starts and node all go, and nothing is left holding
// a port. A child never needs to cooperate for that.
package proc

import (
	"bufio"
	"errors"
	"io"
	"os"
	"os/exec"
	"strconv"
	"sync"
	"sync/atomic"
	"syscall"
	"time"
)

// ShimArg is the hidden first argument that makes flashpoint act as the shim.
const ShimArg = "__flashpoint-child"

// Self is the executable that implements ShimArg; tests point it elsewhere.
var Self = os.Executable

// Spec says how to run a child.
type Spec struct {
	Path       string
	Args       []string
	Dir        string
	Env        []string
	StopSignal syscall.Signal
	Grace      time.Duration
	// Line receives each line the child writes to stdout or stderr.
	Line func(string)
}

// Proc is a running child.
type Proc struct {
	cmd      *exec.Cmd
	stdin    io.Closer
	done     chan struct{}
	err      error
	stopping atomic.Bool
	stopOnce sync.Once
}

// Start runs s under the shim.
func Start(s Spec) (*Proc, error) {
	self, err := Self()
	if err != nil {
		return nil, err
	}
	if s.StopSignal == 0 {
		s.StopSignal = syscall.SIGTERM
	}
	if s.Grace == 0 {
		s.Grace = 10 * time.Second
	}
	args := append([]string{ShimArg, "-signal", strconv.Itoa(int(s.StopSignal)), "-grace", s.Grace.String(), "--", s.Path}, s.Args...)
	cmd := exec.Command(self, args...)
	cmd.Dir = s.Dir
	cmd.Env = s.Env
	pr, pw, err := os.Pipe()
	if err != nil {
		return nil, err
	}
	cmd.Stdout, cmd.Stderr = pw, pw
	stdin, err := cmd.StdinPipe()
	if err != nil {
		pr.Close()
		pw.Close()
		return nil, err
	}
	if err := cmd.Start(); err != nil {
		pr.Close()
		pw.Close()
		return nil, err
	}
	pw.Close()
	p := &Proc{cmd: cmd, stdin: stdin, done: make(chan struct{})}
	drained := make(chan struct{})
	go func() {
		defer close(drained)
		sc := bufio.NewScanner(pr)
		sc.Buffer(make([]byte, 64*1024), 1024*1024)
		for sc.Scan() {
			if s.Line != nil {
				s.Line(sc.Text())
			}
		}
	}()
	go func() {
		p.err = cmd.Wait()
		// The shim kills its group as it exits, so EOF follows promptly; the
		// bound is for a grandchild that escaped the group with the pipe.
		select {
		case <-drained:
		case <-time.After(time.Second):
		}
		pr.Close()
		close(p.done)
	}()
	return p, nil
}

// Done is closed once the child and its shim have exited.
func (p *Proc) Done() <-chan struct{} { return p.done }

// Err is the shim's exit error, which carries the child's exit status. Valid
// after Done.
func (p *Proc) Err() error { return p.err }

// ExitCode is the child's exit code, or -1 while it runs.
func (p *Proc) ExitCode() int {
	if p.Running() {
		return -1
	}
	var ee *exec.ExitError
	if errors.As(p.err, &ee) {
		return ee.ExitCode()
	}
	if p.err != nil {
		return -1
	}
	return 0
}

// Pid is the shim's pid.
func (p *Proc) Pid() int { return p.cmd.Process.Pid }

// Running reports whether the child has not yet exited.
func (p *Proc) Running() bool {
	if p == nil {
		return false
	}
	select {
	case <-p.done:
		return false
	default:
		return true
	}
}

// Stopping reports whether Stop was called, so an exit is not a crash.
func (p *Proc) Stopping() bool { return p != nil && p.stopping.Load() }

// Stop asks the shim to stop the child's group with its stop signal, then
// SIGKILL after the grace period, and waits for it.
func (p *Proc) Stop() {
	if p == nil {
		return
	}
	p.stopOnce.Do(func() {
		p.stopping.Store(true)
		p.stdin.Close()
	})
	<-p.done
}

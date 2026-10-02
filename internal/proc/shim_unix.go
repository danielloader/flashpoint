//go:build unix

package proc

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"strconv"
	"syscall"
	"time"
)

// RunShim is the shim's main: it runs the command after "--" in its own
// process group and returns the exit code to exit with.
func RunShim(args []string) int {
	fs := flag.NewFlagSet(ShimArg, flag.ContinueOnError)
	sig := fs.Int("signal", int(syscall.SIGTERM), "stop signal")
	grace := fs.Duration("grace", 10*time.Second, "time between the stop signal and SIGKILL")
	if err := fs.Parse(args); err != nil || fs.NArg() == 0 {
		fmt.Fprintln(os.Stderr, "flashpoint: the shim is internal; run flashpoint instead")
		return 2
	}
	// The terminal's Ctrl-C reaches the shim in flashpoint's group; flashpoint
	// decides when to stop. Handled, not ignored: an ignored signal would stay
	// ignored in the child after exec.
	hold := make(chan os.Signal, 4)
	signal.Notify(hold, syscall.SIGINT, syscall.SIGHUP, syscall.SIGQUIT)
	term := make(chan os.Signal, 1)
	signal.Notify(term, syscall.SIGTERM)

	cmd := exec.Command(fs.Arg(0), fs.Args()[1:]...)
	cmd.Stdout, cmd.Stderr = os.Stdout, os.Stderr
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if n, _ := strconv.Atoi(os.Getenv("LISTEN_FDS")); n > 0 {
		for i := range n {
			cmd.ExtraFiles = append(cmd.ExtraFiles, os.NewFile(uintptr(3+i), "listener"))
		}
	}
	if err := cmd.Start(); err != nil {
		fmt.Fprintf(os.Stderr, "flashpoint: %v\n", err)
		return 127
	}
	for _, f := range cmd.ExtraFiles {
		f.Close()
	}
	pgid := cmd.Process.Pid
	exited := make(chan error, 1)
	go func() { exited <- cmd.Wait() }()
	eof := make(chan struct{})
	go func() {
		io.Copy(io.Discard, os.Stdin)
		close(eof)
	}()

	var err error
	select {
	case err = <-exited:
	case <-eof:
		err = stopGroup(pgid, syscall.Signal(*sig), *grace, exited)
	case <-term:
		err = stopGroup(pgid, syscall.Signal(*sig), *grace, exited)
	}
	// Whatever the leader left behind in its group goes with it.
	syscall.Kill(-pgid, syscall.SIGKILL)
	return exitCode(err)
}

func stopGroup(pgid int, sig syscall.Signal, grace time.Duration, exited <-chan error) error {
	syscall.Kill(-pgid, sig)
	select {
	case err := <-exited:
		return err
	case <-time.After(grace):
		fmt.Fprintf(os.Stderr, "flashpoint: still running %s after %v; killing\n", grace, sig)
		syscall.Kill(-pgid, syscall.SIGKILL)
		return <-exited
	}
}

func exitCode(err error) int {
	var ee *exec.ExitError
	if !errors.As(err, &ee) {
		if err != nil {
			return 1
		}
		return 0
	}
	if ws, ok := ee.Sys().(syscall.WaitStatus); ok && ws.Signaled() {
		return 128 + int(ws.Signal())
	}
	return ee.ExitCode()
}

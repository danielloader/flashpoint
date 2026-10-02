package runner

import (
	"os/exec"
	"syscall"
	"time"
)

// preflight gets macOS's first-exec assessment of a new binary (0.3–1s,
// measured) done while the old server still serves. The binary is exec'd
// under PT_TRACE_ME, so it stops at its first instruction and is killed
// there: none of the server's code runs.
func preflight(bin, dir string) time.Duration {
	t := time.Now()
	cmd := exec.Command(bin)
	cmd.Dir = dir
	cmd.SysProcAttr = &syscall.SysProcAttr{Ptrace: true}
	if err := cmd.Start(); err != nil {
		return 0
	}
	pid := cmd.Process.Pid
	// Wait for the stop at exec, then kill it. A traced, stopped process
	// does not act on SIGKILL until it is detached.
	var ws syscall.WaitStatus
	syscall.Wait4(pid, &ws, syscall.WUNTRACED, nil)
	syscall.Kill(pid, syscall.SIGKILL)
	if ws.Stopped() {
		syscall.PtraceDetach(pid)
	}
	for !ws.Exited() && !ws.Signaled() {
		if _, err := syscall.Wait4(pid, &ws, 0, nil); err != nil {
			break
		}
	}
	cmd.Process.Release()
	return time.Since(t)
}

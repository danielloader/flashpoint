// Package listen lets a Go server pick up the socket flashpoint holds for it,
// so a rebuild never refuses a connection: requests that arrive while the old
// process stops and the new one starts wait in the kernel's accept queue.
//
// Outside flashpoint, Listen is net.Listen("tcp", addr), so the import is safe
// to leave in for production builds.
//
//	ln, err := listen.Listen(":" + os.Getenv("PORT"))
//	if err != nil { log.Fatal(err) }
//	http.Serve(ln, mux)
package listen

import (
	"net"
	"os"
	"strconv"
)

// fd 3 is the first inherited file, as in systemd socket activation.
const firstFD = 3

// flashpoint runs each new build once with FLASHPOINT_PREFLIGHT=1 while the
// old one is still serving. That first exec of a new binary is slow on macOS
// (the system scans it), and should not count as downtime. Exiting from init
// keeps the preflight from doing anything the server's main would.
func init() {
	if os.Getenv("FLASHPOINT_PREFLIGHT") == "1" {
		os.Exit(0)
	}
}

// Listen returns the listener flashpoint passed in, or binds addr itself when
// the process was not started by flashpoint (or by systemd socket activation).
func Listen(addr string) (net.Listener, error) {
	if !inherited() {
		return net.Listen("tcp", addr)
	}
	// Not for any process this server starts in turn.
	os.Unsetenv("LISTEN_FDS")
	os.Unsetenv("LISTEN_PID")
	os.Unsetenv("LISTEN_FDNAMES")
	f := os.NewFile(firstFD, "flashpoint-listener")
	defer f.Close()
	return net.FileListener(f)
}

// Inherited reports whether Listen will use an inherited socket.
func Inherited() bool { return inherited() }

func inherited() bool {
	if n, _ := strconv.Atoi(os.Getenv("LISTEN_FDS")); n < 1 {
		return false
	}
	// systemd sets LISTEN_PID; flashpoint cannot know the pid before exec.
	if pid := os.Getenv("LISTEN_PID"); pid != "" && pid != strconv.Itoa(os.Getpid()) {
		return false
	}
	return true
}

package runner

import (
	"fmt"
	"net"
	"os/exec"
	"strconv"
	"strings"
	"time"
)

// PortError is a port something else already holds. flashpoint never kills
// the holder: it might be another checkout's dev stack.
type PortError struct {
	Name   string
	Port   int
	Holder string // "node (pid 4242)", when lsof can tell
	Err    error
}

func (e *PortError) Error() string {
	msg := fmt.Sprintf("%s port %d is already in use", e.Name, e.Port)
	if e.Holder != "" {
		msg += " by " + e.Holder
	}
	flag := "--api-port"
	if e.Name == "web" {
		flag = "--web-port"
	}
	return msg + fmt.Sprintf("\n  stop it, or pick another port with %s (0 picks a free one)", flag)
}

// checkFree fails if anything answers on port or holds it. A bind alone is
// not proof: with SO_REUSEADDR, BSD lets a wildcard bind share a port that a
// process holds on 127.0.0.1 or ::1, which is exactly how Vite binds.
func checkFree(name string, port int) error {
	for _, host := range []string{"127.0.0.1", "::1"} {
		c, err := net.DialTimeout("tcp", net.JoinHostPort(host, strconv.Itoa(port)), 200*time.Millisecond)
		if err == nil {
			c.Close()
			return &PortError{Name: name, Port: port, Holder: holder(port)}
		}
	}
	ln, err := net.Listen("tcp", ":"+strconv.Itoa(port))
	if err != nil {
		return &PortError{Name: name, Port: port, Holder: holder(port), Err: err}
	}
	return ln.Close()
}

// holder names the process listening on port, if lsof is installed.
func holder(port int) string {
	out, err := exec.Command("lsof", "-nP", "-iTCP:"+strconv.Itoa(port), "-sTCP:LISTEN", "-Fpc").Output()
	if err != nil {
		return ""
	}
	var pid, cmd string
	for _, l := range strings.Split(string(out), "\n") {
		switch {
		case strings.HasPrefix(l, "p") && pid == "":
			pid = l[1:]
		case strings.HasPrefix(l, "c") && cmd == "":
			cmd = l[1:]
		}
	}
	if pid == "" {
		return ""
	}
	return fmt.Sprintf("%s (pid %s)", cmd, pid)
}

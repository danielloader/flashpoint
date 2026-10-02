package config

import (
	"fmt"
	"net"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// Default ports, used when no flag, environment variable or config sets one.
const (
	DefaultAPIPort = 8080
	DefaultWebPort = 5173
)

// Overrides are the command-line flags; the zero value overrides nothing.
type Overrides struct {
	Main    string
	APIPort *int
	WebPort *int
	WebDir  string
	NoWeb   bool
	Args    []string
}

// Plan is everything flashpoint needs to run, resolved from flags, the
// environment, flashpoint.toml and detection, in that order of precedence.
type Plan struct {
	Root       string
	ConfigFile string // empty when there is no flashpoint.toml

	Main        string
	BuildFlags  []string
	Args        []string
	Env         []string
	APIHost     string
	APIPort     int
	Health      string
	PortEnv     string
	StopSignal  syscall.Signal
	StopTimeout time.Duration

	Web *WebPlan // nil when there is no web app or it is disabled

	Include  []string
	Exclude  []string
	Debounce time.Duration
}

// WebPlan is how to run the dev server.
type WebPlan struct {
	Dir  string
	Argv []string
	// PortOnArgv is false when flashpoint could not put the port on the
	// command line; the dev server must read FLASHPOINT_WEB_PORT itself.
	PortOnArgv     bool
	PackageManager string
	Port           int
	Env            []string
}

// APIURL is the address the dev server's proxy should use. 127.0.0.1, not
// localhost: Node may resolve localhost to ::1 first.
func (p *Plan) APIURL() string { return fmt.Sprintf("http://127.0.0.1:%d", p.APIPort) }

// APIDisplayURL is the address shown to a person.
func (p *Plan) APIDisplayURL() string { return fmt.Sprintf("http://localhost:%d", p.APIPort) }

// WebURL is the dev server's address, or "" without one.
func (p *Plan) WebURL() string {
	if p.Web == nil {
		return ""
	}
	return fmt.Sprintf("http://localhost:%d", p.Web.Port)
}

// Resolve builds the Plan for the project at root.
func Resolve(root string, f File, o Overrides, getenv func(string) string) (*Plan, error) {
	p := &Plan{
		Root:        root,
		Main:        first(o.Main, f.API.Main),
		BuildFlags:  f.API.BuildFlags,
		Args:        f.API.Args,
		APIHost:     f.API.Host,
		Health:      first(f.API.Health, "/"),
		PortEnv:     first(f.API.PortEnv, "PORT"),
		Include:     f.Watch.Include,
		Exclude:     f.Watch.Exclude,
		Debounce:    or(f.Watch.Debounce.Duration, 150*time.Millisecond),
		StopTimeout: or(f.API.StopTimeout.Duration, 10*time.Second),
	}
	if _, err := os.Stat(filepath.Join(root, FileName)); err == nil {
		p.ConfigFile = filepath.Join(root, FileName)
	}
	if len(o.Args) > 0 {
		p.Args = o.Args
	}
	if !strings.HasPrefix(p.Health, "/") {
		return nil, fmt.Errorf("api.health %q must start with /", p.Health)
	}
	sig, err := ParseSignal(first(f.API.StopSignal, "SIGINT"))
	if err != nil {
		return nil, err
	}
	p.StopSignal = sig
	if p.APIPort, err = port("API", o.APIPort, getenv("FLASHPOINT_API_PORT"), f.API.Port, DefaultAPIPort); err != nil {
		return nil, err
	}
	p.Env = envList(f.API.Env)
	if p.Main == "" {
		if p.Main, err = DetectMain(root); err != nil {
			return nil, err
		}
	}

	enabled := f.Web.Enabled == nil || *f.Web.Enabled
	if o.NoWeb || !enabled {
		return p, nil
	}
	dir := first(o.WebDir, f.Web.Dir)
	switch {
	case dir != "":
		if !filepath.IsAbs(dir) {
			dir = filepath.Join(root, dir)
		}
		if _, err := os.Stat(dir); err != nil {
			return nil, fmt.Errorf("web dir: %w", err)
		}
	case f.Web.Command != "":
		dir = root
	default:
		dir = DetectWebDir(root)
	}
	if dir == "" {
		return p, nil
	}
	w := &WebPlan{Dir: filepath.Clean(dir), Env: envList(f.Web.Env)}
	if w.Port, err = port("web", o.WebPort, getenv("FLASHPOINT_WEB_PORT"), f.Web.Port, DefaultWebPort); err != nil {
		return nil, err
	}
	if w.Port == p.APIPort {
		return nil, fmt.Errorf("the API and web ports are both %d", w.Port)
	}
	if f.Web.Command != "" {
		cmd := strings.NewReplacer("{port}", strconv.Itoa(w.Port), "{api_url}", p.APIURL()).Replace(f.Web.Command)
		w.Argv = []string{"sh", "-c", cmd}
		w.PortOnArgv = strings.Contains(f.Web.Command, "{port}")
	} else {
		w.PackageManager = PackageManager(w.Dir, root)
		if w.Argv, w.PortOnArgv, err = WebCommand(w.Dir, w.PackageManager, w.Port); err != nil {
			return nil, err
		}
	}
	p.Web = w
	return p, nil
}

// port resolves one port; 0 anywhere means "pick a free one".
func port(name string, flag *int, env string, file *int, def int) (int, error) {
	n := def
	switch {
	case flag != nil:
		n = *flag
	case env != "":
		v, err := strconv.Atoi(env)
		if err != nil {
			return 0, fmt.Errorf("%s port %q is not a number", name, env)
		}
		n = v
	case file != nil:
		n = *file
	}
	if n < 0 || n > 65535 {
		return 0, fmt.Errorf("%s port %d is out of range", name, n)
	}
	if n == 0 {
		return freePort()
	}
	return n, nil
}

func freePort() (int, error) {
	ln, err := net.Listen("tcp", ":0")
	if err != nil {
		return 0, err
	}
	defer ln.Close()
	return ln.Addr().(*net.TCPAddr).Port, nil
}

// ParseSignal accepts "SIGINT", "INT" or "int".
func ParseSignal(s string) (syscall.Signal, error) {
	switch strings.TrimPrefix(strings.ToUpper(s), "SIG") {
	case "INT":
		return syscall.SIGINT, nil
	case "TERM":
		return syscall.SIGTERM, nil
	case "HUP":
		return syscall.SIGHUP, nil
	case "QUIT":
		return syscall.SIGQUIT, nil
	case "KILL":
		return syscall.SIGKILL, nil
	}
	return 0, fmt.Errorf("unknown stop signal %q (use SIGINT, SIGTERM, SIGHUP, SIGQUIT or SIGKILL)", s)
}

func envList(m map[string]string) []string {
	var env []string
	for k, v := range m {
		env = append(env, k+"="+v)
	}
	sort.Strings(env)
	return env
}

func first(s ...string) string {
	for _, v := range s {
		if v != "" {
			return v
		}
	}
	return ""
}

func or(d, def time.Duration) time.Duration {
	if d > 0 {
		return d
	}
	return def
}

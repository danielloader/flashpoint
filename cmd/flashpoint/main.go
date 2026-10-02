// Command flashpoint runs a Go HTTP server and a Vite app together, rebuilds
// the server on save while the old one keeps serving, then restarts it.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"runtime/debug"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/danielloader/flashpoint/internal/config"
	"github.com/danielloader/flashpoint/internal/event"
	"github.com/danielloader/flashpoint/internal/plain"
	"github.com/danielloader/flashpoint/internal/proc"
	"github.com/danielloader/flashpoint/internal/runner"
	"github.com/danielloader/flashpoint/internal/tui"
)

// version is set by the release build; go install falls back to build info.
var version = ""

// Exit codes.
const (
	exitOK    = 0
	exitError = 1
	exitUsage = 2
	exitPort  = 3
	// For `flashpoint reload`.
	exitNotRunning = 4
	exitTimeout    = 124
)

func main() {
	if len(os.Args) > 1 && os.Args[1] == proc.ShimArg {
		os.Exit(proc.RunShim(os.Args[2:]))
	}
	if len(os.Args) > 1 && os.Args[1] == "reload" {
		os.Exit(reload(os.Args[2:], os.Stdout, os.Stderr))
	}
	if len(os.Args) > 1 && os.Args[1] == "logs" {
		os.Exit(logs(os.Args[2:], os.Stdout, os.Stderr))
	}
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))
}

func run(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("flashpoint", flag.ContinueOnError)
	fs.SetOutput(stderr)
	var (
		o        config.Overrides
		dir      = fs.String("C", ".", "run in `dir` (the project root is found from there)")
		noTUI    = fs.Bool("no-tui", false, "print labelled lines instead of the TUI (the default when not on a terminal or under CI)")
		showVers = fs.Bool("version", false, "print the version and exit")
	)
	fs.StringVar(&o.Main, "main", "", "the main `package` to build, e.g. ./cmd/server")
	fs.Func("api-port", "the `port` your Go server listens on (default 8080)", portFlag(&o.APIPort))
	fs.Func("web-port", "the `port` your Vite dev server listens on (default 5173)", portFlag(&o.WebPort))
	fs.StringVar(&o.WebDir, "web-dir", "", "the Vite app's `dir` (default: found by its vite.config)")
	fs.BoolVar(&o.NoWeb, "no-web", false, "run the API only")
	fs.StringVar(&o.Logs.Dir, "log-dir", "", "write api.log, web.log, flashpoint.log and all.log in `dir` (agents: .flashpoint/logs)")
	fs.StringVar(&o.Logs.API, "log-api", "", "tee the API's output to `path`")
	fs.StringVar(&o.Logs.Web, "log-web", "", "tee the web server's output to `path`")
	fs.StringVar(&o.Logs.Flashpoint, "log-flashpoint", "", "write flashpoint's build, restart and reload events to `path`")
	fs.StringVar(&o.Logs.All, "log-all", "", "write every stream, interleaved and labelled, to `path`")
	logTimes := fs.Bool("log-timestamps", true, "prefix each log file line with an RFC 3339 timestamp")
	fs.BoolVar(&o.Logs.Truncate, "log-truncate", false, "truncate the log files at start instead of appending")
	fs.Func("log-max-size", "rotate a log file to .1 past `size` (default 10MB)", func(v string) error {
		n, err := config.ParseSize(v)
		o.Logs.MaxSize.Bytes = n
		return err
	})
	watchFiles := fs.Bool("watch", true, "rebuild on file changes; with --watch=false, only on SIGUSR1 or `flashpoint reload`")
	fs.Usage = func() {
		fmt.Fprintf(stderr, `flashpoint: hot reload for a Go HTTP server and a Vite app.

Usage:
  flashpoint [flags] [-- server args]
  flashpoint reload [--wait] [--timeout 60s]   rebuild the running instance
  flashpoint logs api|web|flashpoint|all [-n 100] [--since-build]

Flags:
`)
		fs.PrintDefaults()
		fmt.Fprintf(stderr, `
Configuration lives in an optional %s at the project root.
Docs: https://github.com/danielloader/flashpoint
`, config.FileName)
	}
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return exitOK
		}
		return exitUsage
	}
	if *showVers {
		fmt.Fprintln(stdout, "flashpoint", versionString())
		return exitOK
	}
	o.Args = fs.Args()
	o.NoWatch = !*watchFiles
	fs.Visit(func(f *flag.Flag) {
		if f.Name == "log-timestamps" {
			o.LogTimes = logTimes
		}
	})
	for _, p := range []*string{&o.Logs.Dir, &o.Logs.API, &o.Logs.Web, &o.Logs.Flashpoint, &o.Logs.All} {
		if *p != "" {
			*p, _ = filepath.Abs(*p)
		}
	}

	root, err := config.FindRoot(*dir)
	if err != nil {
		return fail(stderr, exitError, err)
	}
	file, _, err := config.Load(root)
	if err != nil {
		return fail(stderr, exitUsage, err)
	}
	plan, err := config.Resolve(root, file, o)
	if err != nil {
		return fail(stderr, exitError, err)
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM, syscall.SIGHUP)
	defer stop()
	go func() {
		<-ctx.Done()
		// A second signal ends flashpoint at once; the shims still stop the
		// children when their pipes close.
		stop()
	}()

	if !*noTUI && tui.Supported(os.Stdin, os.Stdout) {
		err = tui.Run(ctx, plan, versionString(), func(ctx context.Context, sink event.Sink, ctl <-chan runner.Control) error {
			return runner.Run(ctx, plan, sink, ctl, runner.Options{Color: true})
		})
	} else {
		color := isTerminal(os.Stdout) && os.Getenv("NO_COLOR") == ""
		sink := plain.New(stdout, color)
		sink.Line(event.Line{Source: event.System, Kind: event.Info, Text: describe(plan)})
		err = runner.Run(ctx, plan, sink, nil, runner.Options{Color: color})
		if err == nil {
			sink.Line(event.Line{Source: event.System, Kind: event.Info, Text: "stopped"})
		}
	}
	var pe *runner.PortError
	switch {
	case errors.As(err, &pe):
		return fail(stderr, exitPort, err)
	case err != nil:
		return fail(stderr, exitError, err)
	}
	return exitOK
}

func describe(p *config.Plan) string {
	s := "api " + p.APIDisplayURL()
	if p.Web != nil {
		s += " · web " + p.WebURL()
	}
	return s
}

func portFlag(dst **int) func(string) error {
	return func(s string) error {
		n, err := strconv.Atoi(s)
		if err != nil {
			return fmt.Errorf("%q is not a port", s)
		}
		*dst = &n
		return nil
	}
}

func fail(w io.Writer, code int, err error) int {
	fmt.Fprintf(w, "flashpoint: %v\n", err)
	return code
}

func versionString() string {
	if version != "" {
		return version
	}
	if bi, ok := debug.ReadBuildInfo(); ok && bi.Main.Version != "" && bi.Main.Version != "(devel)" {
		return bi.Main.Version
	}
	return "dev"
}

func isTerminal(f *os.File) bool {
	fi, err := f.Stat()
	return err == nil && fi.Mode()&os.ModeCharDevice != 0
}

// reload asks the flashpoint running for this project to rebuild, over its
// control socket.
func reload(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("flashpoint reload", flag.ContinueOnError)
	fs.SetOutput(stderr)
	dir := fs.String("C", ".", "the project, as for flashpoint")
	wait := fs.Bool("wait", false, "wait for the rebuild: exit 0 once the API is ready, 1 if the build fails")
	timeout := fs.Duration("timeout", 60*time.Second, "give up waiting after this long (exit 124)")
	web := fs.Bool("web", false, "restart the web dev server instead")
	fs.Usage = func() {
		fmt.Fprintf(stderr, "Usage: flashpoint reload [--wait] [--timeout 60s] [--web] [-C dir]\n\n")
		fs.PrintDefaults()
	}
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return exitOK
		}
		return exitUsage
	}
	root, err := config.FindRoot(*dir)
	if err != nil {
		return fail(stderr, exitNotRunning, err)
	}
	path := "/reload"
	if *web {
		path = "/restart-web"
	}
	res, err := runner.Request(root, path, *wait, *timeout)
	switch {
	case errors.Is(err, runner.ErrNotRunning):
		return fail(stderr, exitNotRunning, fmt.Errorf("%w (%s)", err, root))
	case errors.Is(err, runner.ErrTimeout):
		return fail(stderr, exitTimeout, fmt.Errorf("%w after %s", err, *timeout))
	case err != nil:
		return fail(stderr, exitError, err)
	}
	if !res.OK {
		for _, l := range res.Errors {
			fmt.Fprintln(stderr, l)
		}
		return fail(stderr, exitError, fmt.Errorf("%s: %s", strings.ReplaceAll(res.State, "_", " "), res.Detail))
	}
	if res.Detail != "" {
		fmt.Fprintln(stdout, res.Detail)
	} else {
		fmt.Fprintln(stdout, res.State)
	}
	return exitOK
}

// logs prints the tail of one stream from the running instance.
func logs(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("flashpoint logs", flag.ContinueOnError)
	fs.SetOutput(stderr)
	dir := fs.String("C", ".", "the project, as for flashpoint")
	n := fs.Int("n", 100, "lines to print; 0 prints all that are kept")
	since := fs.Bool("since-build", false, "only lines since the last build started")
	fs.Usage = func() {
		fmt.Fprintf(stderr, "Usage: flashpoint logs api|web|flashpoint|all [-n 100] [--since-build] [-C dir]\n\n")
		fs.PrintDefaults()
	}
	// The stream may come before or after the flags.
	var stream string
	if len(args) > 0 && !strings.HasPrefix(args[0], "-") {
		stream, args = args[0], args[1:]
	}
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return exitOK
		}
		return exitUsage
	}
	if stream == "" && fs.NArg() > 0 {
		stream = fs.Arg(0)
	}
	if stream == "" {
		fs.Usage()
		return exitUsage
	}
	root, err := config.FindRoot(*dir)
	if err != nil {
		return fail(stderr, exitNotRunning, err)
	}
	c, err := runner.Client(root)
	if err != nil {
		return fail(stderr, exitNotRunning, fmt.Errorf("%w (%s)", err, root))
	}
	c.Timeout = 10 * time.Second
	url := fmt.Sprintf("http://flashpoint/logs/%s?n=%d", stream, *n)
	if *since {
		url += "&since_build=1"
	}
	resp, err := c.Get(url)
	if err != nil {
		return fail(stderr, exitError, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		b, _ := io.ReadAll(resp.Body)
		return fail(stderr, exitUsage, errors.New(strings.TrimSpace(string(b))))
	}
	io.Copy(stdout, resp.Body)
	return exitOK
}

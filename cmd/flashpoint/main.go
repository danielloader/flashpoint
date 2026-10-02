// Command flashpoint runs a Go HTTP server and a Vite app together, rebuilds
// the server on save and swaps it in without refusing a connection.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"runtime/debug"
	"strconv"
	"syscall"

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
)

func main() {
	if len(os.Args) > 1 && os.Args[1] == proc.ShimArg {
		os.Exit(proc.RunShim(os.Args[2:]))
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
	fs.Func("api-port", "the API's `port` (default 8080, or $FLASHPOINT_API_PORT); 0 picks a free one", portFlag(&o.APIPort))
	fs.Func("web-port", "the web app's `port` (default 5173, or $FLASHPOINT_WEB_PORT); 0 picks a free one", portFlag(&o.WebPort))
	fs.StringVar(&o.WebDir, "web-dir", "", "the Vite app's `dir` (default: found by its vite.config)")
	fs.BoolVar(&o.NoWeb, "no-web", false, "run the API only")
	fs.Usage = func() {
		fmt.Fprintf(stderr, `flashpoint: hot reload for a Go HTTP server and a Vite app.

Usage:
  flashpoint [flags] [-- server args]

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

	root, err := config.FindRoot(*dir)
	if err != nil {
		return fail(stderr, exitError, err)
	}
	file, _, err := config.Load(root)
	if err != nil {
		return fail(stderr, exitUsage, err)
	}
	plan, err := config.Resolve(root, file, o, os.Getenv)
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

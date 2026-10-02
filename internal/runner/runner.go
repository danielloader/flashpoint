// Package runner runs the API and the web app and keeps them current.
package runner

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"sync"

	"github.com/danielloader/flashpoint/internal/config"
	"github.com/danielloader/flashpoint/internal/event"
	"github.com/danielloader/flashpoint/internal/logfile"
	"github.com/danielloader/flashpoint/internal/proc"
	"github.com/danielloader/flashpoint/internal/watch"
)

// Control is a request from the front end.
type Control int

const (
	Rebuild Control = iota
	RestartWeb
)

// Options tune Run for the front end.
type Options struct {
	// Color asks children for coloured output (FORCE_COLOR) though their
	// stdout is a pipe.
	Color bool
}

// Run supervises the project until ctx ends. It returns an error only for a
// failure to start, such as a port already in use (a *PortError).
func Run(ctx context.Context, plan *config.Plan, sink event.Sink, ctl <-chan Control, opts Options) error {
	tee, err := logfile.New(logfile.Options{
		Paths:      plan.Logs.Paths,
		Timestamps: plan.Logs.Timestamps,
		Truncate:   plan.Logs.Truncate,
		MaxSize:    plan.Logs.MaxSize,
	}, nil)
	if err != nil {
		return err
	}
	defer tee.Close()
	log := &logger{sink: multi{sink, tee}}
	tee.SetWarn(func(err error) { log.errorf(event.System, "%v; carrying on without it", err) })
	for _, name := range logfile.Streams {
		if p := plan.Logs.Paths[name]; p != "" {
			log.infof(event.System, "%s log: %s", name, rel(plan.Root, p))
		}
	}
	log.update(func(s *event.Status) {
		s.API, s.APIURL, s.WebURL = event.APIStarting, plan.APIDisplayURL(), plan.WebURL()
		if plan.Web != nil {
			s.Web = event.WebStarting
		}
	})

	if plan.Web != nil {
		if err := checkFree("web", plan.Web.Port); err != nil {
			return err
		}
	}
	ctlr, err := openControl(plan.Root)
	if err != nil {
		return err
	}
	defer ctlr.close()
	var (
		w      *watch.Watcher
		set    *watch.Set
		relist = func() error { return nil }
	)
	if plan.Watch {
		w, set, err = watch.New(watch.Options{
			Root:       plan.Root,
			Main:       plan.Main,
			BuildFlags: plan.BuildFlags,
			Include:    plan.Include,
			Exclude:    append([]string{".flashpoint/**"}, plan.Exclude...),
			Debounce:   plan.Debounce,
			OnError:    func(err error) { log.errorf(event.System, "watch: %v", err) },
		})
		if err != nil {
			return err
		}
		defer w.Close()
		relist = func() error { _, err := w.Relist(); return err }
		log.infof(event.API, "watching %d packages of %s in %d directories", set.Packages, plan.Main, len(set.Dirs))
	}
	log.update(func(s *event.Status) { s.Watching = plan.Watch })

	var apiDials, webDials dialCounter
	state, err := stateDir(plan.Root)
	if err != nil {
		return err
	}
	api := &apiServer{
		plan:   plan,
		log:    log,
		bin:    filepath.Join(state, binName(plan)),
		exited: make(chan *proc.Proc, 4),
		tail:   &tail{},
		dials:  &apiDials,
		logs:   tee.Paths(),
	}
	if err := checkFree("API", plan.APIPort); err != nil {
		return err
	}
	api.env = apiEnv(plan)

	var web *webServer
	if plan.Web != nil {
		web = &webServer{plan: plan.Web, log: log, env: webEnv(plan, opts.Color), dials: &webDials}
		if _, err := os.Stat(filepath.Join(plan.Web.Dir, "node_modules")); err != nil && plan.Web.PackageManager != "" {
			log.errorf(event.Web, "no node_modules in %s; run %s install", rel(plan.Root, plan.Web.Dir), plan.Web.PackageManager)
		}
		web.start(ctx)
	}

	q := newQueue()
	q.push(trigger{force: true})
	if w != nil {
		go func() {
			for files := range w.Changes {
				q.push(trigger{files: files})
			}
		}()
	}
	restartWeb := func(reason string) {
		if web == nil {
			return
		}
		log.infof(event.Web, "restart requested (%s)", reason)
		go web.restart(ctx)
	}
	go func() {
		for {
			select {
			case <-ctx.Done():
				return
			case c := <-ctl:
				switch c {
				case Rebuild:
					q.push(trigger{reasons: []string{"key"}, force: true})
				case RestartWeb:
					restartWeb("key")
				}
			case r := <-ctlr.requests:
				switch r.kind {
				case reqReload:
					q.push(trigger{reasons: []string{r.reason}, waiters: waiters(r.done)})
				case reqRestartWeb:
					restartWeb(r.reason)
					if r.done != nil {
						r.done <- Result{OK: web != nil, State: "queued"}
					}
				}
			}
		}
	}()
	ctlr.serve(ctx, func() event.Status {
		s := log.status()
		s.ProbeDials = apiDials.n.Load() + webDials.n.Load()
		return s
	}, tee)
	if !plan.Watch {
		log.infof(event.API, "file watching is off; reload with: kill -USR1 %d, or flashpoint reload", os.Getpid())
	}

	var wg sync.WaitGroup
	if web != nil {
		wg.Go(func() {
			<-ctx.Done()
			web.stop()
		})
	}
	api.loop(ctx, q, relist)
	wg.Wait()
	log.update(func(s *event.Status) {
		s.API, s.Serving = event.APIOffline, false
		if s.Web != event.WebNone {
			s.Web = event.WebDown
		}
	})
	return nil
}

// multi sends events to several sinks.
type multi []event.Sink

func (m multi) Line(l event.Line) {
	for _, s := range m {
		s.Line(l)
	}
}

func (m multi) Status(st event.Status) {
	for _, s := range m {
		s.Status(st)
	}
}

// apiEnv is the environment the server would have without flashpoint, plus
// api.env from the config. flashpoint sets no ports or URLs: the app keeps
// its own configuration.
func apiEnv(plan *config.Plan) []string {
	return append(os.Environ(), plan.Env...)
}

func webEnv(plan *config.Plan, color bool) []string {
	env := append(os.Environ(), plan.Web.Env...)
	if color {
		// The dev server's stdout is a pipe; keep its colours for the TUI.
		env = append(env, "FORCE_COLOR=1")
	}
	return env
}

// stateDir is a per-checkout directory for the build output, outside the
// project so it never needs a .gitignore entry.
func stateDir(root string) (string, error) {
	base := os.Getenv("FLASHPOINT_CACHE_DIR")
	if base == "" {
		var err error
		if base, err = os.UserCacheDir(); err != nil {
			base = os.TempDir()
		}
		base = filepath.Join(base, "flashpoint")
	}
	sum := sha256.Sum256([]byte(root))
	dir := filepath.Join(base, filepath.Base(root)+"-"+hex.EncodeToString(sum[:])[:10])
	return dir, os.MkdirAll(dir, 0o755)
}

func binName(plan *config.Plan) string {
	name := filepath.Base(filepath.Join(plan.Root, plan.Main))
	if name == "." || name == "/" {
		name = "server"
	}
	return name
}

func rel(root, p string) string {
	if r, err := filepath.Rel(root, p); err == nil {
		return r
	}
	return p
}

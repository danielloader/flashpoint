// Package runner runs the API and the web app and keeps them current.
package runner

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"

	"github.com/danielloader/flashpoint/internal/config"
	"github.com/danielloader/flashpoint/internal/event"
	"github.com/danielloader/flashpoint/internal/logfile"
	"github.com/danielloader/flashpoint/internal/proc"
	"github.com/danielloader/flashpoint/internal/watch"
	"github.com/joho/godotenv"
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
			Include:    append(slices.Clone(plan.Include), envPatterns(plan)...),
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
	if api.env, err = apiEnv(plan); err != nil {
		return err
	}
	for _, name := range plan.EnvFiles {
		api.envFiles = append(api.envFiles, rel(plan.Root, name))
	}

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

// apiEnv is the environment the server would have without flashpoint, then
// the api.env_file files in order, then api.env from the config. A missing
// env file is skipped. flashpoint sets no ports or URLs: the app keeps its
// own configuration.
func apiEnv(plan *config.Plan) ([]string, error) {
	env := os.Environ()
	for _, name := range plan.EnvFiles {
		b, err := os.ReadFile(name)
		if errors.Is(err, fs.ErrNotExist) {
			continue
		}
		if err != nil {
			return nil, err
		}
		m, err := godotenv.UnmarshalBytes(b)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", rel(plan.Root, name), err)
		}
		for _, k := range slices.Sorted(maps.Keys(m)) {
			env = append(env, k+"="+m[k])
		}
	}
	vars := envMap(env)
	return append(env, expandEnv(plan.Env, func(k string) string { return vars[k] })...), nil
}

// envMap resolves an environment list; a later entry wins, as for exec.
func envMap(env []string) map[string]string {
	m := make(map[string]string, len(env))
	for _, kv := range env {
		k, v, _ := strings.Cut(kv, "=")
		m[k] = v
	}
	return m
}

// envDiff names the variables that differ between two environments.
func envDiff(a, b []string) []string {
	ma, mb := envMap(a), envMap(b)
	var out []string
	for k, v := range mb {
		if old, ok := ma[k]; !ok || old != v {
			out = append(out, k)
		}
	}
	for k := range ma {
		if _, ok := mb[k]; !ok {
			out = append(out, k)
		}
	}
	slices.Sort(out)
	return out
}

func webEnv(plan *config.Plan, color bool) []string {
	env := append(os.Environ(), expandEnv(plan.Web.Env, os.Getenv)...)
	if color {
		// The dev server's stdout is a pipe; keep its colours for the TUI.
		env = append(env, "FORCE_COLOR=1")
	}
	return env
}

// expandEnv expands $VAR, ${VAR} and ${VAR:-default} in each KEY=value
// against the caller's environment, so a config value can be a default
// that the caller overrides. A plain value always wins.
func expandEnv(kvs []string, getenv func(string) string) []string {
	out := make([]string, len(kvs))
	for i, kv := range kvs {
		k, v, _ := strings.Cut(kv, "=")
		out[i] = k + "=" + os.Expand(v, func(name string) string {
			if n, def, ok := strings.Cut(name, ":-"); ok {
				if val := getenv(n); val != "" {
					return val
				}
				return def
			}
			return getenv(name)
		})
	}
	return out
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

// envPatterns are the env files as watch patterns, so a change to one
// reaches the build loop.
func envPatterns(plan *config.Plan) []string {
	var out []string
	for _, name := range plan.EnvFiles {
		out = append(out, filepath.ToSlash(rel(plan.Root, name)))
	}
	return out
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

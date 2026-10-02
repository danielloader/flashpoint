// Package runner runs the API and the web app and keeps them current.
package runner

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"

	"github.com/danielloader/flashpoint/internal/config"
	"github.com/danielloader/flashpoint/internal/event"
	"github.com/danielloader/flashpoint/internal/proc"
	"github.com/danielloader/flashpoint/internal/watch"
)

// ListenPackage is the helper a server imports to take flashpoint's socket.
const ListenPackage = "github.com/danielloader/flashpoint/listen"

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
	log := &logger{sink: sink}
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
	w, set, err := watch.New(watch.Options{
		Root:       plan.Root,
		Main:       plan.Main,
		BuildFlags: plan.BuildFlags,
		Include:    plan.Include,
		Exclude:    plan.Exclude,
		Debounce:   plan.Debounce,
		OnError:    func(err error) { log.errorf(event.System, "watch: %v", err) },
	})
	if err != nil {
		return err
	}
	defer w.Close()
	log.infof(event.API, "watching %d packages of %s in %d directories", set.Packages, plan.Main, len(set.Dirs))

	state, err := stateDir(plan.Root)
	if err != nil {
		return err
	}
	api := &apiServer{
		plan:   plan,
		log:    log,
		bin:    filepath.Join(state, binName(plan)),
		reload: filepath.Join(state, "reload"),
		exited: make(chan *proc.Proc, 4),
		tail:   &tail{},
	}
	// It must exist for the Vite plugin's watcher to see it change.
	os.WriteFile(api.reload, nil, 0o644)
	if err := checkFree("API", plan.APIPort); err != nil {
		return err
	}
	if set.Imports[ListenPackage] {
		ln, err := net.Listen("tcp", net.JoinHostPort(plan.APIHost, strconv.Itoa(plan.APIPort)))
		if err != nil {
			return &PortError{Name: "API", Port: plan.APIPort, Err: err}
		}
		f, err := ln.(*net.TCPListener).File()
		ln.Close()
		if err != nil {
			return err
		}
		defer f.Close()
		api.ln = f
		log.update(func(s *event.Status) { s.Handoff = true })
	} else {
		log.infof(event.API, "the server binds its own port, so a restart refuses connections briefly; import %s to hand it flashpoint's socket", ListenPackage)
	}
	api.env = apiEnv(plan, api.ln != nil)

	var web *webServer
	if plan.Web != nil {
		web = &webServer{plan: plan.Web, log: log, env: webEnv(plan, api.reload, opts.Color)}
		if !plan.Web.PortOnArgv {
			log.infof(event.Web, "the dev script is not plain vite, so the port is only in FLASHPOINT_WEB_PORT; make sure it reads it")
		}
		if _, err := os.Stat(filepath.Join(plan.Web.Dir, "node_modules")); err != nil && plan.Web.PackageManager != "" {
			log.errorf(event.Web, "no node_modules in %s; run %s install", rel(plan.Root, plan.Web.Dir), plan.Web.PackageManager)
		}
		web.start(ctx)
	}

	q := newQueue()
	q.push(nil, true)
	go func() {
		for files := range w.Changes {
			q.push(files, false)
		}
	}()
	go func() {
		for {
			select {
			case <-ctx.Done():
				return
			case c := <-ctl:
				switch c {
				case Rebuild:
					q.push(nil, true)
				case RestartWeb:
					if web != nil {
						go web.restart(ctx)
					}
				}
			}
		}
	}()

	var wg sync.WaitGroup
	if web != nil {
		wg.Go(func() {
			<-ctx.Done()
			web.stop()
		})
	}
	api.loop(ctx, q, func() error {
		_, err := w.Relist()
		return err
	})
	wg.Wait()
	log.update(func(s *event.Status) {
		s.API, s.Serving = event.APIOffline, false
		if s.Web != event.WebNone {
			s.Web = event.WebDown
		}
	})
	return nil
}

func apiEnv(plan *config.Plan, handoff bool) []string {
	env := clean(os.Environ(), "LISTEN_FDS", "LISTEN_PID", "LISTEN_FDNAMES", "FLASHPOINT_PREFLIGHT")
	env = append(env, plan.Env...)
	env = append(env,
		"FLASHPOINT=1",
		plan.PortEnv+"="+strconv.Itoa(plan.APIPort),
		"FLASHPOINT_API_URL="+plan.APIURL(),
	)
	if plan.Web != nil {
		env = append(env, "FLASHPOINT_WEB_URL="+plan.WebURL())
	}
	if handoff {
		env = append(env, "LISTEN_FDS=1", "LISTEN_FDNAMES=flashpoint")
	}
	return env
}

func webEnv(plan *config.Plan, reload string, color bool) []string {
	env := clean(os.Environ(), "LISTEN_FDS", "LISTEN_PID", "LISTEN_FDNAMES")
	env = append(env, plan.Web.Env...)
	env = append(env,
		"FLASHPOINT=1",
		"FLASHPOINT_API_URL="+plan.APIURL(),
		"FLASHPOINT_WEB_PORT="+strconv.Itoa(plan.Web.Port),
		"FLASHPOINT_RELOAD_FILE="+reload,
	)
	if color {
		env = append(env, "FORCE_COLOR=1")
	}
	return env
}

func clean(env []string, drop ...string) []string {
	out := env[:0:0]
outer:
	for _, kv := range env {
		for _, d := range drop {
			if strings.HasPrefix(kv, d+"=") {
				continue outer
			}
		}
		out = append(out, kv)
	}
	return out
}

// stateDir is a per-checkout directory for the build output, outside the
// project so it never needs a .gitignore entry.
func stateDir(root string) (string, error) {
	base, err := os.UserCacheDir()
	if err != nil {
		base = os.TempDir()
	}
	sum := sha256.Sum256([]byte(root))
	dir := filepath.Join(base, "flashpoint", filepath.Base(root)+"-"+hex.EncodeToString(sum[:])[:10])
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

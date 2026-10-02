package runner

import (
	"context"
	"fmt"
	"sync"
	"syscall"
	"time"

	"github.com/danielloader/flashpoint/internal/config"
	"github.com/danielloader/flashpoint/internal/event"
	"github.com/danielloader/flashpoint/internal/proc"
)

// webServer runs the Vite dev server beside the API. Vite does its own
// hot-module reload; flashpoint only starts it, labels its output and makes
// sure it goes when flashpoint does.
type webServer struct {
	plan  *config.WebPlan
	log   *logger
	env   []string
	dials *dialCounter

	mu sync.Mutex
	p  *proc.Proc
}

func (w *webServer) start(ctx context.Context) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.log.update(func(s *event.Status) { s.Web, s.WebDetail = event.WebStarting, "" })
	p, err := proc.Start(proc.Spec{
		Path:       w.plan.Argv[0],
		Args:       w.plan.Argv[1:],
		Dir:        w.plan.Dir,
		Env:        w.env,
		StopSignal: syscall.SIGTERM,
		Grace:      5 * time.Second,
		Line:       func(s string) { w.log.line(event.Web, event.Output, s) },
	})
	if err != nil {
		w.log.errorf(event.Web, "start: %v", err)
		w.log.update(func(s *event.Status) { s.Web, s.WebDetail = event.WebDown, err.Error() })
		return
	}
	w.p = p
	go w.monitor(ctx, p)
}

// monitor marks the server ready once its port accepts, and down when it
// exits.
func (w *webServer) monitor(ctx context.Context, p *proc.Proc) {
	// One dial per attempt until the port accepts, then none: the dev
	// server's own exit is what marks it down.
	poll := backoff{d: 100 * time.Millisecond, max: 2 * time.Second}
	deadline := time.Now().Add(webReadyTimeout)
	hinted := false
	t := time.NewTimer(poll.next())
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-p.Done():
			if p.Stopping() {
				return
			}
			detail := fmt.Sprintf("exited with code %d", p.ExitCode())
			w.log.errorf(event.Web, "%s (w restarts it)", detail)
			w.log.update(func(s *event.Status) { s.Web, s.WebDetail = event.WebDown, detail })
			return
		case <-t.C:
			if dialable(ctx, w.dials, w.plan.Port) {
				w.log.infof(event.Web, "ready on http://localhost:%d", w.plan.Port)
				w.log.update(func(s *event.Status) { s.Web, s.WebDetail = event.WebReady, "" })
				continue // the timer is not reset: no more dials, only the exit
			}
			if !hinted && time.Now().After(deadline) {
				hinted = true
				hint := fmt.Sprintf("Web not listening on :%d (check --web-port)", w.plan.Port)
				w.log.errorf(event.Web, "%s after %s", hint, webReadyTimeout)
				w.log.update(func(s *event.Status) { s.WebDetail = hint })
			}
			t.Reset(poll.next())
		}
	}
}

// webReadyTimeout is how long the dev server may take to accept on its port
// before flashpoint says it is not listening there.
const webReadyTimeout = 30 * time.Second

func (w *webServer) stop() {
	w.mu.Lock()
	p := w.p
	w.mu.Unlock()
	p.Stop()
}

func (w *webServer) restart(ctx context.Context) {
	w.log.infof(event.Web, "restarting")
	w.stop()
	w.start(ctx)
}

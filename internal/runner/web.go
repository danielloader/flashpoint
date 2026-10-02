package runner

import (
	"context"
	"fmt"
	"net"
	"strconv"
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
	addr := net.JoinHostPort("localhost", strconv.Itoa(w.plan.Port))
	// One dial per attempt until the port accepts, then none: Vite's own
	// exit is what marks it down.
	poll := backoff{d: 100 * time.Millisecond, max: time.Second}
	t := time.NewTimer(poll.next())
	defer t.Stop()
	for ready := false; ; {
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
			if ready {
				continue
			}
			dctx, cancel := context.WithTimeout(ctx, 200*time.Millisecond)
			c, err := w.dials.dial(dctx, "tcp", addr)
			cancel()
			if err != nil {
				t.Reset(poll.next())
			} else {
				c.Close()
				ready = true
				w.log.infof(event.Web, "ready on http://localhost:%d", w.plan.Port)
				w.log.update(func(s *event.Status) { s.Web, s.WebDetail = event.WebReady, "" })
			}
		}
	}
}

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

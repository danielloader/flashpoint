package runner

import (
	"context"
	"net"
	"net/http"
	"os"
	"time"
)

// offline answers on the held socket while no server does, so a request
// after a failed build gets a 503 that says why instead of hanging in the
// accept queue.
type offline struct {
	srv *http.Server
}

func serveOffline(f *os.File, body func() string) *offline {
	ln, err := net.FileListener(f)
	if err != nil {
		return nil
	}
	srv := &http.Server{
		Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "text/plain; charset=utf-8")
			w.Header().Set("Retry-After", "1")
			w.Header().Set("X-Flashpoint", "offline")
			w.WriteHeader(http.StatusServiceUnavailable)
			w.Write([]byte(body()))
		}),
		ReadHeaderTimeout: 5 * time.Second,
	}
	go srv.Serve(ln)
	return &offline{srv: srv}
}

// stop closes its copy of the socket; the socket itself stays open, so
// connections from here on queue for the next server.
func (o *offline) stop() {
	if o == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	o.srv.Shutdown(ctx)
}

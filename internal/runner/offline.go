package runner

import (
	"net"
	"net/http"
	"time"
)

// offline answers on the API port while no server runs, after a failed
// build or a crash, so a request gets a 503 that says why instead of a
// refused connection. It is closed before the next server starts.
type offline struct {
	srv *http.Server
}

// serveOffline binds addr, or does nothing if something else holds it.
func serveOffline(addr, body string) *offline {
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return nil
	}
	srv := &http.Server{
		Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "text/plain; charset=utf-8")
			w.Header().Set("Retry-After", "1")
			w.Header().Set("X-Flashpoint", "offline")
			w.WriteHeader(http.StatusServiceUnavailable)
			w.Write([]byte(body))
		}),
		ReadHeaderTimeout: 5 * time.Second,
	}
	go srv.Serve(ln)
	return &offline{srv: srv}
}

// stop releases the port; Close, not Shutdown, so it is free at once.
func (o *offline) stop() {
	if o == nil {
		return
	}
	o.srv.Close()
}

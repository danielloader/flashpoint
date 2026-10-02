package runner

import (
	"context"
	"net"
	"net/http"
	"sync/atomic"
	"time"
)

// dialCounter counts the connections flashpoint itself opens, so tests (and
// GET /status) can prove it never churns through ports.
type dialCounter struct{ n atomic.Int64 }

func (d *dialCounter) dial(ctx context.Context, network, addr string) (net.Conn, error) {
	d.n.Add(1)
	var nd net.Dialer
	return nd.DialContext(ctx, network, addr)
}

// newProbeClient is the one HTTP client the readiness probe uses: keep-alive
// on, so polling an up server reuses a connection.
func newProbeClient(d *dialCounter) *http.Client {
	return &http.Client{
		Timeout: 2 * time.Second,
		Transport: &http.Transport{
			DialContext:         d.dial,
			MaxIdleConnsPerHost: 1,
			IdleConnTimeout:     30 * time.Second,
		},
	}
}

// backoff is a poll interval that starts at min and grows to max.
type backoff struct{ d, max time.Duration }

func (b *backoff) next() time.Duration {
	d := b.d
	b.d = min(b.max, b.d*3/2)
	return d
}

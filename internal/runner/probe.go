package runner

import (
	"context"
	"net"
	"strconv"
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

// dialable reports whether something accepts on localhost:port, over IPv4
// or IPv6, as the app may bind either.
func dialable(ctx context.Context, d *dialCounter, port int) bool {
	ctx, cancel := context.WithTimeout(ctx, 200*time.Millisecond)
	defer cancel()
	c, err := d.dial(ctx, "tcp", net.JoinHostPort("localhost", strconv.Itoa(port)))
	if err != nil {
		return false
	}
	c.Close()
	return true
}

// backoff is a poll interval that starts at min and grows to max.
type backoff struct{ d, max time.Duration }

func (b *backoff) next() time.Duration {
	d := b.d
	b.d = min(b.max, b.d*3/2)
	return d
}

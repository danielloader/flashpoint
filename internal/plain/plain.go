// Package plain prints labelled lines for CI logs, agents and pipes:
//
//	api │ listening
//	api ▸ ready in 1.4s (build 1.3s, restart 30ms)
package plain

import (
	"fmt"
	"io"
	"strings"
	"sync"

	"github.com/charmbracelet/x/ansi"

	"github.com/danielloader/flashpoint/internal/event"
)

// Printer is an event.Sink that writes to an io.Writer.
type Printer struct {
	mu    sync.Mutex
	w     io.Writer
	color bool
}

// New returns a Printer; color keeps children's colours and adds its own.
func New(w io.Writer, color bool) *Printer { return &Printer{w: w, color: color} }

const (
	red   = "\x1b[31m"
	dim   = "\x1b[2m"
	cyan  = "\x1b[36m"
	mag   = "\x1b[35m"
	reset = "\x1b[0m"
)

// Line implements event.Sink.
func (p *Printer) Line(l event.Line) {
	if l.Kind == event.Marker {
		return
	}
	text := l.Text
	if !p.color {
		text = ansi.Strip(text)
	}
	label := l.Source.String()
	sep := "│"
	if l.Kind == event.Info || l.Kind == event.Error {
		sep = "▸"
	}
	if p.color {
		c := cyan
		if l.Source == event.Web {
			c = mag
		} else if l.Source == event.System {
			c = dim
		}
		label = c + label + reset
		switch l.Kind {
		case event.Error, event.Diagnostic:
			text = red + text + reset
		case event.Info:
			sep = c + sep + reset
		}
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	fmt.Fprintf(p.w, "%s %s %s\n", label, sep, strings.TrimRight(text, "\r"))
}

// Status implements event.Sink; the runner already logs every transition.
func (p *Printer) Status(event.Status) {}

package runner

import (
	"fmt"
	"sync"
	"sync/atomic"
	"time"

	"github.com/danielloader/flashpoint/internal/event"
)

// logger numbers lines and keeps the status every front end is sent.
type logger struct {
	sink event.Sink
	seq  atomic.Uint64

	mu sync.Mutex
	st event.Status
}

func (l *logger) line(src event.Source, kind event.Kind, text string) uint64 {
	n := l.seq.Add(1)
	l.sink.Line(event.Line{Seq: n, Time: time.Now(), Source: src, Kind: kind, Text: text})
	return n
}

func (l *logger) infof(src event.Source, format string, args ...any) {
	l.line(src, event.Info, fmt.Sprintf(format, args...))
}

func (l *logger) errorf(src event.Source, format string, args ...any) uint64 {
	return l.line(src, event.Error, fmt.Sprintf(format, args...))
}

func (l *logger) update(f func(*event.Status)) {
	l.mu.Lock()
	f(&l.st)
	st := l.st
	l.mu.Unlock()
	l.sink.Status(st)
}

func (l *logger) status() event.Status {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.st
}

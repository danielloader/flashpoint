package tui

import (
	"context"
	"os"
	"os/exec"
	"runtime"
	"sync"
	"time"

	tea "charm.land/bubbletea/v2"

	"github.com/danielloader/flashpoint/internal/config"
	"github.com/danielloader/flashpoint/internal/event"
	"github.com/danielloader/flashpoint/internal/runner"
)

// Supported reports whether the TUI can run: both ends are a terminal, and
// this is not CI or a dumb terminal.
func Supported(in, out *os.File) bool {
	if os.Getenv("CI") != "" || os.Getenv("TERM") == "dumb" {
		return false
	}
	return isTerminal(in) && isTerminal(out)
}

func isTerminal(f *os.File) bool {
	fi, err := f.Stat()
	return err == nil && fi.Mode()&os.ModeCharDevice != 0
}

// RunFunc runs the project, sending events to sink, until ctx ends.
type RunFunc func(ctx context.Context, sink event.Sink, ctl <-chan runner.Control) error

// Run shows the TUI while run supervises the project.
func Run(ctx context.Context, plan *config.Plan, version string, run RunFunc) error {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	ctl := make(chan runner.Control, 4)
	m := newModel(version, plan.Web != nil, ctl, cancel, openURL)
	p := tea.NewProgram(m, tea.WithContext(context.WithoutCancel(ctx)), tea.WithoutSignalHandler())
	sink := newSink(p)
	go sink.flush(ctx)
	go func() {
		err := run(ctx, sink, ctl)
		sink.drain()
		p.Send(doneMsg{err: err})
	}()
	// A signal (not a key) ends ctx: show "stopping" while the runner stops.
	go func() {
		<-ctx.Done()
		p.Send(stoppingMsg{})
	}()
	_, err := p.Run()
	cancel()
	if err != nil {
		return err
	}
	return m.err
}

type stoppingMsg struct{}

// sink batches lines so a chatty child costs one render per 50ms, not one
// per line.
type sink struct {
	p   *tea.Program
	mu  sync.Mutex
	buf []event.Line
}

func newSink(p *tea.Program) *sink { return &sink{p: p} }

func (s *sink) Line(l event.Line) {
	s.mu.Lock()
	s.buf = append(s.buf, l)
	s.mu.Unlock()
}

func (s *sink) Status(st event.Status) {
	s.drain()
	s.p.Send(statusMsg(st))
}

func (s *sink) drain() {
	s.mu.Lock()
	buf := s.buf
	s.buf = nil
	s.mu.Unlock()
	if len(buf) > 0 {
		s.p.Send(linesMsg(buf))
	}
}

func (s *sink) flush(ctx context.Context) {
	t := time.NewTicker(50 * time.Millisecond)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			s.drain()
		}
	}
}

func openURL(url string) {
	name := "xdg-open"
	if runtime.GOOS == "darwin" {
		name = "open"
	}
	cmd := exec.Command(name, url)
	if cmd.Start() == nil {
		go cmd.Wait()
	}
}

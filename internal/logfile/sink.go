package logfile

import (
	"fmt"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/charmbracelet/x/ansi"

	"github.com/danielloader/flashpoint/internal/event"
)

// Streams, as the files, `flashpoint logs` and the control API name them.
const (
	API        = "api"
	Web        = "web"
	Flashpoint = "flashpoint"
	All        = "all"
)

// Streams lists every stream name.
var Streams = []string{API, Web, Flashpoint, All}

// Options say where each stream goes; an empty path keeps it in memory only.
type Options struct {
	Paths      map[string]string
	Timestamps bool
	Truncate   bool
	MaxSize    int64
}

// Sink is an event.Sink that routes lines to the streams: their files, if
// any, and a ring of recent lines each for `flashpoint logs`.
type Sink struct {
	opts  Options
	files map[string]*File
	warn  func(error)

	mu    sync.Mutex
	rings map[string]*ring
}

const ringLines = 5000

// New opens the configured files, writing a session header to each.
func New(opts Options, warn func(error)) (*Sink, error) {
	if warn == nil {
		warn = func(error) {}
	}
	s := &Sink{opts: opts, files: map[string]*File{}, warn: warn, rings: map[string]*ring{}}
	for _, name := range Streams {
		s.rings[name] = &ring{}
		path := opts.Paths[name]
		if path == "" {
			continue
		}
		f, err := Open(path, opts.Truncate, opts.MaxSize, warn)
		if err != nil {
			s.Close()
			return nil, fmt.Errorf("log %s: %w", name, err)
		}
		s.files[name] = f
		fmt.Fprintf(f, "=== flashpoint session started %s (pid %d)\n", time.Now().Format(time.RFC3339), os.Getpid())
	}
	return s, nil
}

// SetWarn sets what is told of a write error (once per file).
func (s *Sink) SetWarn(warn func(error)) {
	for _, f := range s.files {
		f.mu.Lock()
		f.warn = warn
		f.mu.Unlock()
	}
}

// Paths returns the files in use, by stream.
func (s *Sink) Paths() map[string]string {
	out := map[string]string{}
	for name, f := range s.files {
		out[name] = f.Path()
	}
	return out
}

// streams says which streams a line belongs to.
func streams(l event.Line) []string {
	out := []string{All}
	switch {
	case l.Source == event.API && (l.Kind == event.Output || l.Kind == event.Diagnostic || l.Kind == event.Marker):
		out = append(out, API)
	case l.Source == event.Web && l.Kind == event.Output:
		out = append(out, Web)
	}
	if l.Kind != event.Output {
		out = append(out, Flashpoint)
	}
	return out
}

// Line implements event.Sink.
func (s *Sink) Line(l event.Line) {
	text := ansi.Strip(strings.TrimRight(l.Text, "\r"))
	for _, name := range streams(l) {
		body := text
		if name == All || (name == Flashpoint && l.Kind != event.Marker) {
			sep := "│"
			if l.Kind == event.Info || l.Kind == event.Error {
				sep = "▸"
			}
			body = fmt.Sprintf("%s %s %s", l.Source, sep, text)
		}
		s.mu.Lock()
		s.rings[name].add(body, l.Kind == event.Marker && strings.Contains(text, " started"))
		s.mu.Unlock()
		if f := s.files[name]; f != nil {
			if s.opts.Timestamps {
				body = l.Time.Format("2006-01-02T15:04:05.000Z07:00") + " " + body
			}
			f.Write([]byte(body + "\n"))
		}
	}
}

// Status implements event.Sink.
func (s *Sink) Status(event.Status) {}

// Tail returns the last n lines of a stream (all of them for n <= 0), or
// only those since the last build started.
func (s *Sink) Tail(name string, n int, sinceBuild bool) ([]string, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	r, ok := s.rings[name]
	if !ok {
		return nil, false
	}
	return r.tail(n, sinceBuild), true
}

// Close flushes and closes every file.
func (s *Sink) Close() {
	for _, f := range s.files {
		f.Close()
	}
}

type ring struct {
	lines     []string
	lastBuild int // index of the last build's start marker, or -1
	has       bool
}

func (r *ring) add(line string, buildStart bool) {
	r.lines = append(r.lines, line)
	if buildStart {
		r.lastBuild, r.has = len(r.lines)-1, true
	}
	if over := len(r.lines) - ringLines; over > ringLines/5 {
		r.lines = append([]string(nil), r.lines[over:]...)
		r.lastBuild -= over
		if r.lastBuild < 0 {
			r.has = false
		}
	}
}

func (r *ring) tail(n int, sinceBuild bool) []string {
	lines := r.lines
	if sinceBuild && r.has {
		lines = lines[r.lastBuild:]
	}
	if n > 0 && len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	return append([]string(nil), lines...)
}

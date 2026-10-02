// Package logfile tees flashpoint's streams into files an agent can tail:
// one per stream, ANSI stripped, timestamped, rotated at a size cap.
package logfile

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"sync"
)

// File is a log file that appends whole lines. Write never blocks on the
// disk: lines go through a buffered queue to a writer goroutine, and if the
// disk falls behind, lines are dropped and counted rather than stall the
// child whose output this is.
type File struct {
	path    string
	maxSize int64
	mu      sync.Mutex
	partial []byte
	warn    func(error) // called once, on the first write error

	q       chan []byte
	done    chan struct{}
	dropped int // under mu

	// Owned by the writer goroutine.
	f      *os.File
	size   int64
	warned bool
}

const queueLen = 4096

// Open opens path for appending (or truncates it), creating its directory.
// maxSize > 0 rotates the file to path.1 when it would grow past it.
func Open(path string, truncate bool, maxSize int64, warn func(error)) (*File, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, err
	}
	flags := os.O_CREATE | os.O_WRONLY | os.O_APPEND
	if truncate {
		flags |= os.O_TRUNC
	}
	f, err := os.OpenFile(path, flags, 0o644)
	if err != nil {
		return nil, err
	}
	return newFile(path, f, maxSize, warn), nil
}

func newFile(path string, f *os.File, maxSize int64, warn func(error)) *File {
	if warn == nil {
		warn = func(error) {}
	}
	l := &File{path: path, maxSize: maxSize, warn: warn, f: f, q: make(chan []byte, queueLen), done: make(chan struct{})}
	if fi, err := f.Stat(); err == nil && fi.Mode().IsRegular() {
		l.size = fi.Size()
	}
	go l.run()
	return l
}

// Path is the file's path.
func (l *File) Path() string { return l.path }

// Write implements io.Writer: it queues each complete line and keeps a
// trailing partial line until the rest arrives.
func (l *File) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	n := len(p)
	for len(p) > 0 {
		i := bytes.IndexByte(p, '\n')
		if i < 0 {
			l.partial = append(l.partial, p...)
			break
		}
		line := append(l.partial, p[:i+1]...)
		l.partial = nil
		l.enqueue(line)
		p = p[i+1:]
	}
	return n, nil
}

func (l *File) enqueue(line []byte) {
	select {
	case l.q <- line:
	default:
		l.dropped++
	}
}

func (l *File) run() {
	defer close(l.done)
	for line := range l.q {
		l.mu.Lock()
		dropped := l.dropped
		l.dropped = 0
		l.mu.Unlock()
		if dropped > 0 {
			l.write([]byte(fmt.Sprintf("--- flashpoint dropped %d lines: the disk fell behind\n", dropped)))
		}
		l.write(line)
	}
	l.f.Close()
}

func (l *File) write(line []byte) {
	if l.maxSize > 0 && l.size+int64(len(line)) > l.maxSize && l.size > 0 {
		l.rotate()
	}
	n, err := l.f.Write(line)
	l.size += int64(n)
	if err != nil {
		l.fail(err)
	}
}

// fail reports the first write error; later ones would only repeat it.
func (l *File) fail(err error) {
	if l.warned {
		return
	}
	l.warned = true
	l.mu.Lock()
	warn := l.warn
	l.mu.Unlock()
	warn(fmt.Errorf("log %s: %w", l.path, err))
}

// rotate keeps one old file: path.1.
func (l *File) rotate() {
	l.f.Close()
	if err := os.Rename(l.path, l.path+".1"); err != nil {
		l.fail(fmt.Errorf("rotate: %w", err))
	}
	f, err := os.OpenFile(l.path, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o644)
	if err != nil {
		l.fail(err)
		// Carry on into the void rather than stop the stream.
		l.f, _ = os.OpenFile(os.DevNull, os.O_WRONLY, 0)
	} else {
		l.f = f
	}
	l.size = 0
}

// Close flushes any partial line and the queue, then closes the file.
func (l *File) Close() {
	l.mu.Lock()
	if len(l.partial) > 0 {
		l.enqueue(append(l.partial, '\n'))
		l.partial = nil
	}
	l.mu.Unlock()
	close(l.q)
	<-l.done
}

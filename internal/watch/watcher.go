package watch

import (
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/fsnotify/fsnotify"
)

// Options configure a Watcher.
type Options struct {
	Root       string
	Main       string
	BuildFlags []string
	Include    []string // extra globs, relative to Root
	Exclude    []string // globs never to rebuild for
	Debounce   time.Duration
	OnError    func(error)
}

// Watcher sends a batch of changed files on Changes after each quiet spell.
type Watcher struct {
	Changes <-chan []string

	opts   Options
	fs     *fsnotify.Watcher
	events chan string

	mu  sync.Mutex
	set *Set
}

// New lists the build and starts watching it.
func New(opts Options) (*Watcher, *Set, error) {
	fw, err := fsnotify.NewWatcher()
	if err != nil {
		return nil, nil, err
	}
	if opts.OnError == nil {
		opts.OnError = func(error) {}
	}
	w := &Watcher{opts: opts, fs: fw, events: make(chan string, 256)}
	set, err := w.Relist()
	if err != nil {
		fw.Close()
		return nil, nil, err
	}
	w.Changes = Debounce(w.events, opts.Debounce)
	go w.run()
	return w, set, nil
}

// Relist asks go list again, after a build, so a new import or go:embed is
// watched from then on. A directory that dropped out stays watched; it costs
// nothing but a few file descriptors.
func (w *Watcher) Relist() (*Set, error) {
	set, err := List(w.opts.Root, w.opts.Main, w.opts.BuildFlags)
	if err != nil {
		return nil, err
	}
	for _, pattern := range w.opts.Include {
		w.globDirs(set, pattern)
	}
	for d := range set.Dirs {
		// A no-op for a directory already watched; re-adding covers one that
		// was removed and recreated, as a branch switch does.
		if err := w.fs.Add(d); err != nil && !os.IsNotExist(err) {
			w.opts.OnError(err)
		}
	}
	w.mu.Lock()
	w.set = set
	w.mu.Unlock()
	return set, nil
}

// globDirs adds every directory an include pattern could match in.
func (w *Watcher) globDirs(set *Set, pattern string) {
	start := filepath.Join(w.opts.Root, filepath.FromSlash(base(pattern)))
	recursive := strings.Contains(pattern, "**")
	filepath.WalkDir(start, func(p string, d fs.DirEntry, err error) error {
		if err != nil || !d.IsDir() {
			return nil
		}
		if p != start && skipDir(d.Name()) {
			return filepath.SkipDir
		}
		set.Dirs[p] = true
		if !recursive && p != start {
			// One level of wildcard directories is enough for a/*/b-style
			// patterns; deeper needs "**".
			if strings.Count(rel(w.opts.Root, p), "/")+1 >= strings.Count(pattern, "/") {
				return filepath.SkipDir
			}
		}
		return nil
	})
}

func skipDir(name string) bool {
	return name == "node_modules" || name == "vendor" || strings.HasPrefix(name, ".")
}

func rel(root, p string) string {
	r, err := filepath.Rel(root, p)
	if err != nil {
		return p
	}
	return filepath.ToSlash(r)
}

// Relevant reports whether a change to name should trigger a build.
func (w *Watcher) Relevant(name string) bool {
	w.mu.Lock()
	defer w.mu.Unlock()
	return relevant(w.set, w.opts, name)
}

func relevant(set *Set, opts Options, name string) bool {
	r := rel(opts.Root, name)
	for _, p := range opts.Exclude {
		if Match(p, r) {
			return false
		}
	}
	if set.Files[name] {
		return true
	}
	// A new source file, unless it is one go build ignores: tests, and names
	// starting with "." or "_" (which also covers most editors' temp files).
	if base := filepath.Base(name); set.PkgDirs[filepath.Dir(name)] && strings.HasSuffix(base, ".go") &&
		!strings.HasSuffix(base, "_test.go") && !strings.HasPrefix(base, ".") && !strings.HasPrefix(base, "_") {
		return true
	}
	for _, p := range opts.Include {
		if Match(p, r) {
			return true
		}
	}
	return false
}

func (w *Watcher) run() {
	defer close(w.events)
	for {
		select {
		case ev, ok := <-w.fs.Events:
			if !ok {
				return
			}
			if ev.Op == fsnotify.Chmod || !w.Relevant(ev.Name) {
				continue
			}
			w.events <- rel(w.opts.Root, ev.Name)
		case err, ok := <-w.fs.Errors:
			if !ok {
				return
			}
			w.opts.OnError(err)
		}
	}
}

// Close stops watching; Changes closes once the last batch is out.
func (w *Watcher) Close() error { return w.fs.Close() }

// Package config reads flashpoint.toml and fills in what it leaves out by
// looking at the project: the Go main package, the Vite app and its package
// manager.
package config

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/pelletier/go-toml/v2"
)

// FileName is the optional config file at the project root.
const FileName = "flashpoint.toml"

// File is flashpoint.toml. Every field is optional.
type File struct {
	API   API   `toml:"api"`
	Web   Web   `toml:"web"`
	Watch Watch `toml:"watch"`
	Logs  Logs  `toml:"logs"`
}

// Logs tees each stream into a file. Paths are relative to the project root.
type Logs struct {
	// Dir writes api.log, web.log, flashpoint.log and all.log there.
	Dir        string `toml:"dir"`
	API        string `toml:"api"`
	Web        string `toml:"web"`
	Flashpoint string `toml:"flashpoint"`
	All        string `toml:"all"`
	Timestamps *bool  `toml:"timestamps"`
	Truncate   bool   `toml:"truncate"`
	MaxSize    Size   `toml:"max_size"`
}

// Size reads "10MB"-style strings (B, KB, MB, GB; powers of 1024).
type Size struct{ Bytes int64 }

// UnmarshalText implements encoding.TextUnmarshaler.
func (s *Size) UnmarshalText(b []byte) error {
	n, err := ParseSize(string(b))
	s.Bytes = n
	return err
}

// ParseSize parses "10MB", "512KB", "1GB" or a plain byte count.
func ParseSize(v string) (int64, error) {
	t := strings.ToUpper(strings.TrimSpace(v))
	mult := int64(1)
	for _, u := range []struct {
		suffix string
		mult   int64
	}{{"GB", 1 << 30}, {"MB", 1 << 20}, {"KB", 1 << 10}, {"B", 1}} {
		if strings.HasSuffix(t, u.suffix) {
			t, mult = strings.TrimSpace(strings.TrimSuffix(t, u.suffix)), u.mult
			break
		}
	}
	n, err := strconv.ParseInt(t, 10, 64)
	if err != nil || n < 0 {
		return 0, fmt.Errorf("size %q: use e.g. 10MB", v)
	}
	return n * mult, nil
}

// API configures the Go server.
type API struct {
	// Main is the main package to build, e.g. "./cmd/server".
	Main string `toml:"main"`
	// Port is the port your server listens on (flashpoint does not set it).
	Port *int `toml:"port"`
	// BuildFlags are passed to go build after flashpoint's own.
	BuildFlags []string `toml:"build_flags"`
	// Args are passed to the server.
	Args []string          `toml:"args"`
	Env  map[string]string `toml:"env"`
	// ReadyTimeout is how long a new server may take to accept on Port
	// before flashpoint says it is not listening there.
	ReadyTimeout Duration `toml:"ready_timeout"`
	StopSignal   string   `toml:"stop_signal"`
	StopTimeout  Duration `toml:"stop_timeout"`
}

// Web configures the Vite dev server.
type Web struct {
	Enabled *bool  `toml:"enabled"`
	Dir     string `toml:"dir"`
	// Command replaces `<package manager> run dev`. It runs under sh -c.
	Command string `toml:"command"`
	// Port is the port your dev server listens on (flashpoint does not set it).
	Port *int              `toml:"port"`
	Env  map[string]string `toml:"env"`
}

// Watch adds to and takes from the set of files that trigger a rebuild.
type Watch struct {
	// Enabled = false turns file watching off: rebuilds happen only on
	// SIGUSR1 or `flashpoint reload`.
	Enabled  *bool    `toml:"enabled"`
	Include  []string `toml:"include"`
	Exclude  []string `toml:"exclude"`
	Debounce Duration `toml:"debounce"`
}

// Duration reads "150ms"-style strings.
type Duration struct{ time.Duration }

// UnmarshalText implements encoding.TextUnmarshaler.
func (d *Duration) UnmarshalText(b []byte) error {
	v, err := time.ParseDuration(string(b))
	if err != nil {
		return err
	}
	d.Duration = v
	return nil
}

// Parse decodes a flashpoint.toml, rejecting unknown keys so a typo is not
// silently ignored.
func Parse(b []byte) (File, error) {
	var f File
	dec := toml.NewDecoder(bytes.NewReader(b))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&f); err != nil {
		var sm *toml.StrictMissingError
		if errors.As(err, &sm) {
			return f, fmt.Errorf("unknown key: %s", sm.String())
		}
		var de *toml.DecodeError
		if errors.As(err, &de) {
			row, col := de.Position()
			return f, fmt.Errorf("line %d, column %d: %s", row, col, de.Error())
		}
		return f, err
	}
	return f, nil
}

// FindRoot returns the nearest directory at or above dir that holds
// flashpoint.toml or go.mod.
func FindRoot(dir string) (string, error) {
	dir, err := filepath.Abs(dir)
	if err != nil {
		return "", err
	}
	for d := dir; ; d = filepath.Dir(d) {
		for _, name := range []string{FileName, "go.mod"} {
			if _, err := os.Stat(filepath.Join(d, name)); err == nil {
				return d, nil
			}
		}
		if filepath.Dir(d) == d {
			return "", fmt.Errorf("no go.mod or %s in %s or any parent directory", FileName, dir)
		}
	}
}

// Load reads root/flashpoint.toml, or returns the zero File if there is none.
func Load(root string) (File, bool, error) {
	b, err := os.ReadFile(filepath.Join(root, FileName))
	if errors.Is(err, os.ErrNotExist) {
		return File{}, false, nil
	}
	if err != nil {
		return File{}, false, err
	}
	f, err := Parse(b)
	if err != nil {
		return f, true, fmt.Errorf("%s: %w", FileName, err)
	}
	return f, true, nil
}

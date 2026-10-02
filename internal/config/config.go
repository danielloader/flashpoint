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
}

// API configures the Go server.
type API struct {
	// Main is the main package to build, e.g. "./cmd/server".
	Main string `toml:"main"`
	// Port the API listens on; 0 picks a free one.
	Port *int `toml:"port"`
	// Host to bind; empty means all interfaces.
	Host string `toml:"host"`
	// BuildFlags are passed to go build after flashpoint's own.
	BuildFlags []string `toml:"build_flags"`
	// Args are passed to the server.
	Args []string          `toml:"args"`
	Env  map[string]string `toml:"env"`
	// Health is the path polled until the server answers; default "/".
	Health string `toml:"health"`
	// PortEnv names the variable that carries the port to the server.
	PortEnv     string   `toml:"port_env"`
	StopSignal  string   `toml:"stop_signal"`
	StopTimeout Duration `toml:"stop_timeout"`
}

// Web configures the Vite dev server.
type Web struct {
	Enabled *bool  `toml:"enabled"`
	Dir     string `toml:"dir"`
	// Command replaces the detected one. It runs under sh -c, with {port}
	// and {api_url} replaced.
	Command string            `toml:"command"`
	Port    *int              `toml:"port"`
	Env     map[string]string `toml:"env"`
}

// Watch adds to and takes from the set of files that trigger a rebuild.
type Watch struct {
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

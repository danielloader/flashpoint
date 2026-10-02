package config

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
)

// webDirs are searched in order for a Vite config when web.dir is not set.
var webDirs = []string{".", "web", "frontend", "ui", "client"}

var viteConfigs = []string{"vite.config.ts", "vite.config.mts", "vite.config.js", "vite.config.mjs", "vite.config.cts", "vite.config.cjs"}

// DetectMain finds the main package to build: the module root if it is one,
// else the single main package under ./cmd, else the single main package in
// the module.
func DetectMain(root string) (string, error) {
	for _, pattern := range []string{".", "./cmd/...", "./..."} {
		mains, err := mainPackages(root, pattern)
		if err != nil {
			return "", err
		}
		switch len(mains) {
		case 0:
			continue
		case 1:
			return mains[0], nil
		default:
			return "", fmt.Errorf("found %d main packages (%s); pick one with --main or api.main in %s", len(mains), strings.Join(mains, ", "), FileName)
		}
	}
	return "", fmt.Errorf("no main package in %s; pass --main ./path/to/cmd", root)
}

func mainPackages(root, pattern string) ([]string, error) {
	cmd := exec.Command("go", "list", "-e", "-f", "{{if eq .Name \"main\"}}{{.Dir}}{{end}}", pattern)
	cmd.Dir = root
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		// A pattern that matches nothing is not an error worth reporting.
		if strings.Contains(stderr.String(), "matched no packages") {
			return nil, nil
		}
		return nil, fmt.Errorf("go list %s: %v\n%s", pattern, err, stderr.String())
	}
	var mains []string
	sc := bufio.NewScanner(bytes.NewReader(out))
	for sc.Scan() {
		dir := strings.TrimSpace(sc.Text())
		if dir == "" {
			continue
		}
		rel, err := filepath.Rel(root, dir)
		if err != nil {
			continue
		}
		if rel == "." {
			mains = append(mains, ".")
		} else {
			mains = append(mains, "./"+filepath.ToSlash(rel))
		}
	}
	sort.Strings(mains)
	return mains, nil
}

// DetectWebDir returns the first of root, web/, frontend/, ui/ and client/
// that has a Vite config, or "" when none does.
func DetectWebDir(root string) string {
	for _, d := range webDirs {
		if HasViteConfig(filepath.Join(root, d)) {
			return filepath.Join(root, d)
		}
	}
	return ""
}

// HasViteConfig reports whether dir holds a vite.config.* file.
func HasViteConfig(dir string) bool {
	for _, name := range viteConfigs {
		if _, err := os.Stat(filepath.Join(dir, name)); err == nil {
			return true
		}
	}
	return false
}

// PackageManager picks npm, pnpm, yarn or bun for dir: package.json's
// packageManager field first, then the nearest lockfile between dir and root
// (a workspace keeps its lockfile at the top).
func PackageManager(dir, root string) string {
	if pkg, err := readPackageJSON(dir); err == nil && pkg.PackageManager != "" {
		name, _, _ := strings.Cut(pkg.PackageManager, "@")
		switch name {
		case "npm", "pnpm", "yarn", "bun":
			return name
		}
	}
	locks := []struct{ file, pm string }{
		{"pnpm-lock.yaml", "pnpm"},
		{"yarn.lock", "yarn"},
		{"bun.lock", "bun"},
		{"bun.lockb", "bun"},
		{"package-lock.json", "npm"},
	}
	for d := dir; ; d = filepath.Dir(d) {
		for _, l := range locks {
			if _, err := os.Stat(filepath.Join(d, l.file)); err == nil {
				return l.pm
			}
		}
		if d == root || filepath.Dir(d) == d || !strings.HasPrefix(d, root) {
			return "npm"
		}
	}
}

type packageJSON struct {
	PackageManager string            `json:"packageManager"`
	Scripts        map[string]string `json:"scripts"`
}

func readPackageJSON(dir string) (packageJSON, error) {
	var p packageJSON
	b, err := os.ReadFile(filepath.Join(dir, "package.json"))
	if err != nil {
		return p, err
	}
	return p, json.Unmarshal(b, &p)
}

// WebCommand is the argv that starts the dev server in dir with pm: its
// "dev" script, exactly as `<pm> run dev` would run it.
func WebCommand(dir, pm string) ([]string, error) {
	pkg, err := readPackageJSON(dir)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", filepath.Join(dir, "package.json"), err)
	}
	if _, ok := pkg.Scripts["dev"]; !ok {
		return nil, fmt.Errorf("%s has no \"dev\" script; add one or set web.command in %s", filepath.Join(dir, "package.json"), FileName)
	}
	return []string{pm, "run", "dev"}, nil
}

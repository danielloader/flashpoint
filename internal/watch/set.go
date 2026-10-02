// Package watch reports saves to the files a Go build reads, as `go list
// -deps` names them, plus any extra globs from the config.
package watch

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"path/filepath"
	"strings"
)

// Set is what one `go list -deps` says the build reads.
type Set struct {
	// Files are the source, embed and module files to watch by name.
	Files map[string]bool
	// PkgDirs are package directories: a new .go file there is a new source.
	PkgDirs map[string]bool
	// Dirs are the directories to watch.
	Dirs map[string]bool
	// Imports are the import paths of every package in the build.
	Imports  map[string]bool
	Packages int
}

type listedModule struct {
	Path    string
	Dir     string
	GoMod   string
	Main    bool
	Replace *listedModule
	Version string
}

type listedPackage struct {
	ImportPath string
	Dir        string
	Standard   bool
	Module     *listedModule
	GoFiles    []string
	CgoFiles   []string
	CFiles     []string
	CXXFiles   []string
	HFiles     []string
	SFiles     []string
	SysoFiles  []string
	EmbedFiles []string
}

// List runs go list for pkg in root. flags are the build flags, since tags
// change which files a build reads.
func List(root, pkg string, flags []string) (*Set, error) {
	args := append([]string{"list", "-e", "-deps", "-json=ImportPath,Dir,Standard,Module,GoFiles,CgoFiles,CFiles,CXXFiles,HFiles,SFiles,SysoFiles,EmbedFiles"}, listFlags(flags)...)
	cmd := exec.Command("go", append(args, pkg)...)
	cmd.Dir = root
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("go list: %v: %s", err, strings.TrimSpace(stderr.String()))
	}
	return parse(bytes.NewReader(out), root)
}

// listFlags keeps the build flags go list understands and that change what
// it reports; -o, -ldflags and the like do not.
func listFlags(flags []string) []string {
	var keep []string
	for i := 0; i < len(flags); i++ {
		f := flags[i]
		name, _, hasValue := strings.Cut(strings.TrimLeft(f, "-"), "=")
		switch name {
		case "tags", "mod", "modfile", "overlay", "pgo":
			keep = append(keep, f)
			if !hasValue && i+1 < len(flags) {
				i++
				keep = append(keep, flags[i])
			}
		}
	}
	return keep
}

// parse reads go list's JSON stream. Only packages from the main module (or
// a workspace module, or a replace pointing at a local directory) are
// watched: the standard library and the module cache do not change under an
// edit.
func parse(r io.Reader, root string) (*Set, error) {
	s := &Set{
		Files:   map[string]bool{},
		PkgDirs: map[string]bool{},
		Dirs:    map[string]bool{root: true},
		Imports: map[string]bool{},
	}
	for _, name := range []string{"go.mod", "go.sum", "go.work", "go.work.sum"} {
		s.Files[filepath.Join(root, name)] = true
	}
	dec := json.NewDecoder(r)
	for {
		var p listedPackage
		if err := dec.Decode(&p); errors.Is(err, io.EOF) {
			break
		} else if err != nil {
			return nil, err
		}
		s.Imports[p.ImportPath] = true
		mod := local(p.Module)
		if p.Standard || p.Dir == "" || mod == nil {
			continue
		}
		s.Packages++
		if mod.GoMod != "" {
			s.Files[mod.GoMod] = true
			s.Files[filepath.Join(filepath.Dir(mod.GoMod), "go.sum")] = true
			s.Dirs[filepath.Dir(mod.GoMod)] = true
		}
		s.PkgDirs[p.Dir] = true
		s.Dirs[p.Dir] = true
		for _, set := range [][]string{p.GoFiles, p.CgoFiles, p.CFiles, p.CXXFiles, p.HFiles, p.SFiles, p.SysoFiles, p.EmbedFiles} {
			for _, f := range set {
				f = filepath.Join(p.Dir, f)
				s.Files[f] = true
				s.Dirs[filepath.Dir(f)] = true
			}
		}
	}
	return s, nil
}

// local returns the module whose files are edited in place: the main module,
// or the target of a replace by a directory. Nil for anything else.
func local(m *listedModule) *listedModule {
	switch {
	case m == nil:
		return nil
	case m.Main:
		return m
	case m.Replace != nil && m.Replace.Version == "" && m.Replace.Dir != "":
		return m.Replace
	}
	return nil
}

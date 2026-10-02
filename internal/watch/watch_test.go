package watch

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
)

func TestMatch(t *testing.T) {
	cases := []struct {
		pat, name string
		want      bool
	}{
		{"config/*.yaml", "config/app.yaml", true},
		{"config/*.yaml", "config/sub/app.yaml", false},
		{"config/**/*.yaml", "config/app.yaml", true},
		{"config/**/*.yaml", "config/a/b/app.yaml", true},
		{"**/*.sql", "db/migrations/001.sql", true},
		{"**/*.sql", "001.sql", true},
		{"internal/gen/**", "internal/gen/api.go", true},
		{"internal/gen/**", "internal/other.go", false},
		{"templates/index.html", "templates/index.html", true},
	}
	for _, c := range cases {
		if got := Match(c.pat, c.name); got != c.want {
			t.Errorf("Match(%q, %q) = %v", c.pat, c.name, got)
		}
	}
}

func TestBase(t *testing.T) {
	for pat, want := range map[string]string{
		"config/**/*.yaml":     "config",
		"**/*.sql":             "",
		"templates/index.html": "templates",
		"a/b/*/c":              "a/b",
	} {
		if got := base(pat); got != want {
			t.Errorf("base(%q) = %q, want %q", pat, got, want)
		}
	}
}

const listed = `
{"ImportPath":"fmt","Dir":"/go/src/fmt","Standard":true,"GoFiles":["print.go"]}
{"ImportPath":"github.com/x/dep","Dir":"/mod/github.com/x/dep@v1.0.0","Module":{"Path":"github.com/x/dep","Version":"v1.0.0","Dir":"/mod/github.com/x/dep@v1.0.0"},"GoFiles":["dep.go"]}
{"ImportPath":"github.com/x/patched","Dir":"/src/patched","Module":{"Path":"github.com/x/patched","Version":"v1.2.0","Replace":{"Path":"../patched","Dir":"/src/patched","GoMod":"/src/patched/go.mod"}},"GoFiles":["p.go"]}
{"ImportPath":"example.com/app/internal/web","Dir":"/src/app/internal/web","Module":{"Path":"example.com/app","Main":true,"Dir":"/src/app","GoMod":"/src/app/go.mod"},"GoFiles":["web.go"],"EmbedFiles":["static/index.html"]}
{"ImportPath":"example.com/app","Dir":"/src/app","Module":{"Path":"example.com/app","Main":true,"Dir":"/src/app","GoMod":"/src/app/go.mod"},"GoFiles":["main.go"]}
`

func TestParseKeepsOnlyLocalModules(t *testing.T) {
	s, err := parse(strings.NewReader(listed), "/src/app")
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range []string{
		"/src/app/main.go",
		"/src/app/internal/web/web.go",
		"/src/app/internal/web/static/index.html",
		"/src/app/go.mod",
		"/src/app/go.sum",
		"/src/patched/p.go",
		"/src/patched/go.mod",
	} {
		if !s.Files[f] {
			t.Errorf("missing %s", f)
		}
	}
	for _, f := range []string{"/go/src/fmt/print.go", "/mod/github.com/x/dep@v1.0.0/dep.go"} {
		if s.Files[f] {
			t.Errorf("should not watch %s", f)
		}
	}
	if !s.Dirs["/src/app/internal/web/static"] {
		t.Error("embed dir not watched")
	}
	if s.Packages != 3 {
		t.Errorf("packages = %d", s.Packages)
	}
	if !s.Imports["github.com/x/dep"] {
		t.Error("imports should list every package")
	}
}

func TestRelevant(t *testing.T) {
	s, _ := parse(strings.NewReader(listed), "/src/app")
	opts := Options{Root: "/src/app", Include: []string{"config/**/*.yaml"}, Exclude: []string{"internal/web/static/**"}}
	cases := map[string]bool{
		"/src/app/main.go":                        true,
		"/src/app/new.go":                         true, // a new file in a package
		"/src/app/main_test.go":                   false,
		"/src/app/.!1234!main.go":                 false, // sed -i's temp file
		"/src/app/_scratch.go":                    false,
		"/src/app/README.md":                      false,
		"/src/app/config/dev/app.yaml":            true,
		"/src/app/internal/web/static/index.html": false, // excluded
		"/src/app/web/src/App.tsx":                false,
	}
	for name, want := range cases {
		if got := relevant(s, opts, name); got != want {
			t.Errorf("relevant(%s) = %v", name, got)
		}
	}
}

func TestListFlags(t *testing.T) {
	got := listFlags([]string{"-tags=dev", "-ldflags", "-X a=b", "-mod", "vendor", "-race"})
	if want := []string{"-tags=dev", "-mod", "vendor"}; !slices.Equal(got, want) {
		t.Fatalf("got %q", got)
	}
}

func TestDebounceCoalescesABurst(t *testing.T) {
	in := make(chan string)
	out := Debounce(in, 40*time.Millisecond)
	for range 5 {
		in <- "a.go"
		in <- "b.go"
		time.Sleep(10 * time.Millisecond)
	}
	select {
	case b := <-out:
		if !slices.Equal(b, []string{"a.go", "b.go"}) {
			t.Fatalf("batch %q", b)
		}
	case <-time.After(time.Second):
		t.Fatal("no batch")
	}
	select {
	case b := <-out:
		t.Fatalf("second batch %q", b)
	case <-time.After(80 * time.Millisecond):
	}
	close(in)
	if _, ok := <-out; ok {
		t.Fatal("out should close")
	}
}

func TestDebounceWaitsForQuiet(t *testing.T) {
	in := make(chan string)
	out := Debounce(in, 50*time.Millisecond)
	start := time.Now()
	// Events every 20ms for 200ms: no batch until they stop.
	for range 10 {
		in <- "a.go"
		time.Sleep(20 * time.Millisecond)
	}
	<-out
	if el := time.Since(start); el < 200*time.Millisecond {
		t.Fatalf("fired after %v, during the burst", el)
	}
}

func TestDebounceFoldsUnreadBatches(t *testing.T) {
	in := make(chan string)
	out := Debounce(in, 10*time.Millisecond)
	in <- "a.go"
	time.Sleep(40 * time.Millisecond)
	in <- "b.go"
	time.Sleep(40 * time.Millisecond)
	if b := <-out; !slices.Equal(b, []string{"a.go", "b.go"}) {
		t.Fatalf("batch %q", b)
	}
}

func write(t *testing.T, path, body string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestWatcherEndToEnd(t *testing.T) {
	root, _ := filepath.EvalSymlinks(t.TempDir())
	write(t, filepath.Join(root, "go.mod"), "module example.com/w\n\ngo 1.26\n")
	write(t, filepath.Join(root, "main.go"), "package main\n\nimport _ \"example.com/w/lib\"\n\nfunc main() {}\n")
	write(t, filepath.Join(root, "lib", "lib.go"), "package lib\n")
	write(t, filepath.Join(root, "other", "other.go"), "package other\n")
	w, set, err := New(Options{Root: root, Main: ".", Debounce: 30 * time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	defer w.Close()
	if set.Packages != 2 {
		t.Fatalf("packages = %d", set.Packages)
	}
	expect := func(want ...string) {
		t.Helper()
		select {
		case b := <-w.Changes:
			if len(want) == 0 {
				t.Fatalf("unexpected change %q", b)
			}
			if !slices.Equal(b, want) {
				t.Fatalf("changes %q, want %q", b, want)
			}
		case <-time.After(400 * time.Millisecond):
			if len(want) > 0 {
				t.Fatalf("no change, want %q", want)
			}
		}
	}
	write(t, filepath.Join(root, "lib", "lib.go"), "package lib\n\nvar X = 1\n")
	expect("lib/lib.go")
	write(t, filepath.Join(root, "lib", "lib_test.go"), "package lib\n")
	write(t, filepath.Join(root, "other", "other.go"), "package other\n\nvar Y = 1\n")
	write(t, filepath.Join(root, "notes.txt"), "x")
	expect()
	write(t, filepath.Join(root, "lib", "more.go"), "package lib\n")
	expect("lib/more.go")
}

package logfile

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/danielloader/flashpoint/internal/event"
)

func read(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func TestWriteJoinsLinesAcrossChunks(t *testing.T) {
	path := filepath.Join(t.TempDir(), "x.log")
	f, err := Open(path, false, 0, nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, chunk := range []string{"hel", "lo\nwor", "ld\n", "tail"} {
		f.Write([]byte(chunk))
	}
	f.Close()
	if got := read(t, path); got != "hello\nworld\ntail\n" {
		t.Fatalf("got %q", got)
	}
}

func TestRotation(t *testing.T) {
	path := filepath.Join(t.TempDir(), "x.log")
	f, _ := Open(path, false, 20, nil)
	for _, l := range []string{"0123456789\n", "abcdefghij\n", "klmnopqrst\n"} {
		f.Write([]byte(l))
	}
	f.Close()
	if got := read(t, path); got != "klmnopqrst\n" {
		t.Fatalf("current %q", got)
	}
	if got := read(t, path+".1"); got != "abcdefghij\n" {
		t.Fatalf("rotated %q", got)
	}
}

func TestTruncateAndAppend(t *testing.T) {
	path := filepath.Join(t.TempDir(), "x.log")
	os.WriteFile(path, []byte("old\n"), 0o644)
	f, _ := Open(path, false, 0, nil)
	f.Write([]byte("new\n"))
	f.Close()
	if got := read(t, path); got != "old\nnew\n" {
		t.Fatalf("append: %q", got)
	}
	f, _ = Open(path, true, 0, nil)
	f.Write([]byte("only\n"))
	f.Close()
	if got := read(t, path); got != "only\n" {
		t.Fatalf("truncate: %q", got)
	}
}

func TestWriteFailureWarnsOnceAndDoesNotBlock(t *testing.T) {
	// A pipe nobody reads fills after 64KB and then blocks its writer: the
	// stand-in for a stalled disk.
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	var warned atomic.Int32
	f := newFile("stalled", w, 0, func(error) { warned.Add(1) })
	line := []byte(strings.Repeat("x", 99) + "\n")
	start := time.Now()
	for range 20000 {
		f.Write(line)
	}
	if el := time.Since(start); el > 2*time.Second {
		t.Fatalf("writes blocked for %v", el)
	}
	// Now make every write fail.
	w.Close()
	r.Close()
	go f.Close()
	deadline := time.Now().Add(5 * time.Second)
	for warned.Load() == 0 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if n := warned.Load(); n != 1 {
		t.Fatalf("warned %d times, want once", n)
	}
}

func TestSinkRoutesStripsAndMarks(t *testing.T) {
	dir := t.TempDir()
	paths := map[string]string{}
	for _, s := range Streams {
		paths[s] = filepath.Join(dir, s+".log")
	}
	var warns []error
	s, err := New(Options{Paths: paths, Timestamps: true}, func(e error) { warns = append(warns, e) })
	if err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 10, 2, 15, 4, 5, 0, time.UTC)
	for _, l := range []event.Line{
		{Time: now, Source: event.API, Kind: event.Marker, Text: "--- build #1 started (start)"},
		{Time: now, Source: event.API, Kind: event.Output, Text: "\x1b[32mlistening\x1b[0m"},
		{Time: now, Source: event.API, Kind: event.Info, Text: "ready in 1.2s"},
		{Time: now, Source: event.API, Kind: event.Marker, Text: "--- build #1 ok in 1.2s"},
		{Time: now, Source: event.Web, Kind: event.Output, Text: "VITE ready"},
		{Time: now, Source: event.API, Kind: event.Marker, Text: "--- build #2 started (main.go)"},
		{Time: now, Source: event.API, Kind: event.Diagnostic, Text: "./main.go:1:1: oops"},
	} {
		s.Line(l)
	}
	tail, _ := s.Tail(API, 0, true)
	s.Close()

	api := read(t, paths[API])
	if !strings.Contains(api, "2026-10-02T15:04:05.000Z listening\n") || strings.Contains(api, "\x1b") {
		t.Errorf("api.log:\n%s", api)
	}
	if strings.Contains(api, "VITE") || strings.Contains(api, "ready in 1.2s") {
		t.Errorf("api.log has other streams:\n%s", api)
	}
	if !strings.Contains(api, "--- build #1 ok in 1.2s") || !strings.Contains(api, "./main.go:1:1: oops") {
		t.Errorf("api.log lacks markers or errors:\n%s", api)
	}
	if web := read(t, paths[Web]); !strings.HasSuffix(web, " VITE ready\n") || strings.Contains(web, "listening") {
		t.Errorf("web.log:\n%s", web)
	}
	fp := read(t, paths[Flashpoint])
	if !strings.Contains(fp, "api ▸ ready in 1.2s") || !strings.Contains(fp, "--- build #2 started") || strings.Contains(fp, "listening") {
		t.Errorf("flashpoint.log:\n%s", fp)
	}
	if all := read(t, paths[All]); !strings.Contains(all, "api │ listening") || !strings.Contains(all, "web │ VITE ready") {
		t.Errorf("all.log:\n%s", all)
	}
	if len(tail) != 2 || !strings.HasPrefix(tail[0], "--- build #2 started") {
		t.Errorf("since build: %q", tail)
	}
	if len(warns) > 0 {
		t.Errorf("warnings: %v", errors.Join(warns...))
	}
}

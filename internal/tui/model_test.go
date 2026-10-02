package tui

import (
	"os"
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"

	"github.com/danielloader/flashpoint/internal/event"
	"github.com/danielloader/flashpoint/internal/runner"
)

func testModel() (*model, chan runner.Control, *[]string) {
	ctl := make(chan runner.Control, 4)
	var opened []string
	m := newModel("v0.1.0", true, ctl, func() {}, func(u string) { opened = append(opened, u) })
	m.Update(tea.WindowSizeMsg{Width: 100, Height: 20})
	return m, ctl, &opened
}

var seq uint64

func line(src event.Source, kind event.Kind, text string) event.Line {
	seq++
	return event.Line{Seq: seq, Source: src, Kind: kind, Text: text}
}

func press(m *model, keys ...string) {
	for _, k := range keys {
		var msg tea.KeyPressMsg
		switch k {
		case "enter":
			msg = tea.KeyPressMsg{Code: tea.KeyEnter}
		case "esc":
			msg = tea.KeyPressMsg{Code: tea.KeyEscape}
		default:
			r := []rune(k)[0]
			msg = tea.KeyPressMsg{Code: r, Text: k}
		}
		m.Update(msg)
	}
}

func screen(m *model) string { return ansi.Strip(m.render()) }

func TestTabsFilterTheirSource(t *testing.T) {
	m, _, _ := testModel()
	m.Update(linesMsg{line(event.API, event.Output, "api says hi"), line(event.Web, event.Output, "vite says hi")})
	if s := screen(m); !strings.Contains(s, "api │ api says hi") || !strings.Contains(s, "web │ vite says hi") {
		t.Fatalf("All tab:\n%s", s)
	}
	press(m, "1")
	if s := screen(m); !strings.Contains(s, "api says hi") || strings.Contains(s, "vite says hi") {
		t.Fatalf("API tab:\n%s", s)
	}
	press(m, "2")
	if s := screen(m); strings.Contains(s, "api says hi") || !strings.Contains(s, "vite says hi") {
		t.Fatalf("Web tab:\n%s", s)
	}
}

func TestFilter(t *testing.T) {
	m, _, _ := testModel()
	m.Update(linesMsg{line(event.API, event.Output, "GET /users 200"), line(event.API, event.Output, "GET /orders 500")})
	press(m, "/", "o", "r", "d", "enter")
	s := screen(m)
	if strings.Contains(s, "/users") || !strings.Contains(s, "/orders") || !strings.Contains(s, "filter: ord") {
		t.Fatalf("filtered:\n%s", s)
	}
	press(m, "esc")
	if !strings.Contains(screen(m), "/users") {
		t.Fatal("esc should clear the filter")
	}
}

func TestScrolledViewStaysPut(t *testing.T) {
	m, _, _ := testModel()
	for i := range 100 {
		m.Update(linesMsg{line(event.API, event.Output, "line "+string(rune('a'+i%26)))})
	}
	press(m, "k", "k", "k")
	before := screen(m)
	m.Update(linesMsg{line(event.API, event.Output, "new line")})
	if after := screen(m); strings.Contains(after, "new line") || logArea(after) != logArea(before) {
		t.Fatal("a reader scrolled up should not be moved by new lines")
	}
	press(m, "G")
	if !strings.Contains(screen(m), "new line") {
		t.Fatal("G should follow again")
	}
}

func logArea(s string) string {
	rows := strings.Split(s, "\n")
	return strings.Join(rows[1:len(rows)-2], "\n")
}

func TestClear(t *testing.T) {
	m, _, _ := testModel()
	m.Update(linesMsg{line(event.API, event.Output, "api line"), line(event.Web, event.Output, "web line")})
	press(m, "1", "c", "3")
	if s := screen(m); strings.Contains(s, "api line") || !strings.Contains(s, "web line") {
		t.Fatalf("after clearing API:\n%s", s)
	}
}

func TestBuildFailureAndJump(t *testing.T) {
	m, _, _ := testModel()
	m.Update(linesMsg{line(event.API, event.Output, "serving")})
	errLine := line(event.API, event.Diagnostic, "./main.go:12:2: undefined: foo")
	m.Update(linesMsg{errLine})
	for range 40 {
		m.Update(linesMsg{line(event.Web, event.Output, "web noise")})
	}
	m.Update(statusMsg(event.Status{API: event.APIBuildFailed, APIDetail: "./main.go:12:2: undefined: foo", ErrorSeq: errLine.Seq, APIURL: "http://localhost:8080"}))
	s := screen(m)
	if !strings.Contains(s, "API build failed") || !strings.Contains(s, "✗ ./main.go:12:2: undefined: foo") || !strings.Contains(s, "e jump to error") {
		t.Fatalf("status:\n%s", s)
	}
	press(m, "e")
	if m.tab != tabAPI || !strings.Contains(screen(m), "undefined: foo") {
		t.Fatalf("jump:\n%s", screen(m))
	}
}

func TestKeysSendControls(t *testing.T) {
	m, ctl, opened := testModel()
	m.Update(statusMsg(event.Status{APIURL: "http://localhost:8080", WebURL: "http://localhost:5173"}))
	press(m, "r", "w", "o")
	if c := <-ctl; c != runner.Rebuild {
		t.Fatalf("r sent %v", c)
	}
	if c := <-ctl; c != runner.RestartWeb {
		t.Fatalf("w sent %v", c)
	}
	if len(*opened) != 1 || (*opened)[0] != "http://localhost:5173" {
		t.Fatalf("opened %v", *opened)
	}
}

func TestClickTabAndLink(t *testing.T) {
	m, _, opened := testModel()
	m.Update(statusMsg(event.Status{API: event.APIReady, APIURL: "http://localhost:8080", Web: event.WebReady, WebURL: "http://localhost:5173"}))
	m.render()
	api := m.tabZones[0]
	m.Update(tea.MouseClickMsg{X: api.x0 + 1, Y: 0, Button: tea.MouseLeft})
	if m.tab != tabAPI {
		t.Fatalf("tab = %v", m.tab)
	}
	m.render()
	web := m.linkZones[1]
	m.Update(tea.MouseClickMsg{X: web.x0, Y: m.height - 2, Button: tea.MouseLeft})
	if len(*opened) != 1 || (*opened)[0] != "http://localhost:5173" {
		t.Fatalf("opened %v", *opened)
	}
}

func TestStatusLinksAreHyperlinks(t *testing.T) {
	m, _, _ := testModel()
	m.Update(statusMsg(event.Status{API: event.APIReady, APIURL: "http://localhost:8080", Web: event.WebReady, WebURL: "http://localhost:5173", LastReady: 1400 * time.Millisecond}))
	out := m.render()
	if !strings.Contains(out, "\x1b]8;;http://localhost:5173") {
		t.Fatal("no OSC 8 link to the web app")
	}
	if !strings.Contains(ansi.Strip(out), "rebuilt in 1.4s") {
		t.Fatal("no rebuild time")
	}
}

func TestQuit(t *testing.T) {
	cancelled := false
	m := newModel("v", false, make(chan runner.Control, 1), func() { cancelled = true }, func(string) {})
	m.Update(tea.WindowSizeMsg{Width: 80, Height: 10})
	press(m, "q")
	if !cancelled || !strings.Contains(screen(m), "stopping") {
		t.Fatal("q should stop the runner and say so")
	}
	if _, cmd := m.Update(tea.KeyPressMsg{Code: 'q', Text: "q"}); cmd == nil {
		t.Fatal("a second q should quit at once")
	}
}

func TestSanitize(t *testing.T) {
	cases := map[string]string{
		"\x1b[32mgreen\x1b[0m": "\x1b[32mgreen\x1b[0m",
		"\x1b[2J\x1b[Hcleared": "cleared",
		"50%\r100%":            "100%",
		"a\tb":                 "a    b",
		"\x1b]0;title\x07text": "text",
		"bell\x07":             "bell",
	}
	for in, want := range cases {
		if got := sanitize(in); got != want {
			t.Errorf("sanitize(%q) = %q, want %q", in, got, want)
		}
	}
}

// TestDemoFrame renders a representative screen. With FLASHPOINT_FRAME set
// it writes the frame there, which is how docs/demo.png is made.
func TestDemoFrame(t *testing.T) {
	m := newModel("v0.3.0", true, make(chan runner.Control, 1), func() {}, func(string) {})
	m.Update(tea.WindowSizeMsg{Width: 110, Height: 28})
	add := func(src event.Source, kind event.Kind, text string) { m.Update(linesMsg{line(src, kind, text)}) }
	add(event.API, event.Info, "watching 14 packages of ./cmd/server in 16 directories")
	add(event.Web, event.Output, "> vite --port 5173 --strictPort")
	add(event.API, event.Output, "2026/10/02 15:02:32 listening on [::]:8080")
	add(event.API, event.Info, "ready in 1.4s (build 1.3s, restart 42ms)")
	add(event.Web, event.Output, "\x1b[32m  VITE v8.3.2\x1b[39m  ready in \x1b[1m145\x1b[22m ms")
	add(event.Web, event.Output, "  \x1b[32m➜\x1b[39m  \x1b[1mLocal\x1b[22m:   \x1b[36mhttp://localhost:5173/\x1b[39m")
	add(event.Web, event.Info, "ready on http://localhost:5173")
	add(event.API, event.Output, "2026/10/02 15:03:10 GET /api/hello 200 312µs")
	add(event.Web, event.Output, "\x1b[2m15:03:41\x1b[22m \x1b[36m\x1b[1m[vite]\x1b[22m\x1b[39m \x1b[32m(client)\x1b[39m \x1b[32mhmr update \x1b[39m\x1b[2m/src/App.tsx\x1b[22m")
	add(event.API, event.Info, "internal/api/hello.go changed; building")
	add(event.API, event.Output, "2026/10/02 15:04:02 stopped")
	add(event.API, event.Output, "2026/10/02 15:04:02 listening on [::]:8080")
	add(event.API, event.Info, "ready in 1.2s (build 1.1s, restart 38ms)")
	add(event.API, event.Output, "2026/10/02 15:04:05 GET /api/hello 200 287µs")
	add(event.API, event.Info, "internal/api/hello.go changed; building")
	add(event.API, event.Error, "build failed in 0.4s; still serving the last good build")
	errLine := line(event.API, event.Diagnostic, "./internal/api/hello.go:21:9: undefined: greeting")
	m.Update(linesMsg{errLine})
	add(event.API, event.Output, "2026/10/02 15:04:20 GET /api/hello 200 301µs")
	m.Update(statusMsg(event.Status{
		API: event.APIBuildFailed, APIDetail: "./internal/api/hello.go:21:9: undefined: greeting", ErrorSeq: errLine.Seq,
		Serving: true, Watching: true, LastReady: 1400 * time.Millisecond,
		Web: event.WebReady, APIURL: "http://localhost:8080", WebURL: "http://localhost:5173",
	}))
	out := m.render()
	if !strings.Contains(ansi.Strip(out), "Web ready") {
		t.Fatal(ansi.Strip(out))
	}
	if path := os.Getenv("FLASHPOINT_FRAME"); path != "" {
		if err := os.WriteFile(path, []byte(out), 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

func TestWatchOffIsShown(t *testing.T) {
	m, _, _ := testModel()
	m.Update(statusMsg(event.Status{API: event.APIReady, APIURL: "http://localhost:8080"}))
	if !strings.Contains(screen(m), "watch: off · reload: signal") {
		t.Fatal(screen(m))
	}
	m.Update(statusMsg(event.Status{API: event.APIReady, Watching: true, APIURL: "http://localhost:8080"}))
	if strings.Contains(screen(m), "watch: off") {
		t.Fatal("shown while watching")
	}
}

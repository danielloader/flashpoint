package plain

import (
	"bytes"
	"testing"

	"github.com/danielloader/flashpoint/internal/event"
)

func TestLines(t *testing.T) {
	var b bytes.Buffer
	p := New(&b, false)
	p.Line(event.Line{Source: event.API, Kind: event.Output, Text: "\x1b[32mlistening\x1b[0m"})
	p.Line(event.Line{Source: event.API, Kind: event.Info, Text: "ready in 2.1s (build 1.3s, swap 0.1s)"})
	p.Line(event.Line{Source: event.Web, Kind: event.Output, Text: "VITE ready"})
	p.Line(event.Line{Source: event.System, Kind: event.Info, Text: "stopping"})
	want := "api │ listening\napi ▸ ready in 2.1s (build 1.3s, swap 0.1s)\nweb │ VITE ready\nflashpoint ▸ stopping\n"
	if b.String() != want {
		t.Fatalf("got\n%s\nwant\n%s", b.String(), want)
	}
}

// Package tui is flashpoint's terminal UI: tabs of API, web and combined
// logs, and a status bar with the URLs and the state of each process.
package tui

import (
	"strings"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"

	"github.com/danielloader/flashpoint/internal/event"
	"github.com/danielloader/flashpoint/internal/runner"
)

type tab int

const (
	tabAPI tab = iota
	tabWeb
	tabAll
)

var tabNames = [...]string{"API", "Web", "All"}

const (
	maxLines  = 20000
	wheelStep = 3
)

type entry struct {
	event.Line
	text  string // sanitised, colours kept
	lower string // plain lower-case text, for the filter
}

func (e entry) in(t tab) bool {
	switch t {
	case tabAPI:
		return e.Source == event.API
	case tabWeb:
		return e.Source == event.Web
	}
	return true
}

type (
	linesMsg  []event.Line
	statusMsg event.Status
	doneMsg   struct{ err error }
	tickMsg   time.Time
)

type zone struct {
	x0, x1 int
	tab    tab
	url    string
}

type model struct {
	width, height int

	tab    tab
	lines  []entry
	offset [3]int // entries scrolled up from the bottom; 0 follows

	filter    string
	filtering bool
	input     string

	status   event.Status
	webShown bool // a web app exists, so the Web tab means something
	help     bool
	mouse    bool
	quitting bool
	err      error
	version  string
	now      time.Time
	since    time.Time // when the current build started

	ctl    chan<- runner.Control
	cancel func()
	open   func(url string)

	tabZones  []zone
	linkZones []zone
}

func newModel(version string, hasWeb bool, ctl chan<- runner.Control, cancel func(), open func(string)) *model {
	m := &model{version: version, webShown: hasWeb, mouse: true, ctl: ctl, cancel: cancel, open: open, now: time.Now()}
	if !hasWeb {
		m.tab = tabAPI
	} else {
		m.tab = tabAll
	}
	return m
}

func (m *model) Init() tea.Cmd { return tick() }

func tick() tea.Cmd {
	return tea.Tick(100*time.Millisecond, func(t time.Time) tea.Msg { return tickMsg(t) })
}

func (m *model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.width, m.height = msg.Width, msg.Height
	case linesMsg:
		m.add(msg)
	case statusMsg:
		prev := m.status.API
		m.status = event.Status(msg)
		if m.status.API == event.APIBuilding && prev != event.APIBuilding {
			m.since = time.Now()
		}
	case tickMsg:
		m.now = time.Time(msg)
		return m, tick()
	case doneMsg:
		m.err = msg.err
		return m, tea.Quit
	case stoppingMsg:
		m.quitting = true
	case tea.KeyPressMsg:
		return m, m.key(msg)
	case tea.MouseClickMsg:
		m.click(msg.Mouse())
	case tea.MouseWheelMsg:
		switch msg.Mouse().Button {
		case tea.MouseWheelUp:
			m.scroll(wheelStep)
		case tea.MouseWheelDown:
			m.scroll(-wheelStep)
		}
	}
	return m, nil
}

func (m *model) add(ls []event.Line) {
	for _, l := range ls {
		text := sanitize(l.Text)
		e := entry{Line: l, text: text, lower: strings.ToLower(ansi.Strip(text))}
		m.lines = append(m.lines, e)
		// Hold the view still for a reader scrolled up.
		for t := range m.offset {
			if m.offset[t] > 0 && e.in(tab(t)) && m.matches(e) {
				m.offset[t]++
			}
		}
	}
	if len(m.lines) > maxLines+maxLines/4 {
		m.lines = append([]entry(nil), m.lines[len(m.lines)-maxLines:]...)
	}
}

func (m *model) matches(e entry) bool {
	return m.filter == "" || strings.Contains(e.lower, strings.ToLower(m.filter))
}

// visible is the current tab's lines after the filter.
func (m *model) visible() []entry {
	var out []entry
	for _, e := range m.lines {
		if e.in(m.tab) && m.matches(e) {
			out = append(out, e)
		}
	}
	return out
}

func (m *model) logHeight() int { return max(1, m.height-3) }

func (m *model) scroll(n int) {
	o := m.offset[m.tab] + n
	o = min(o, max(0, len(m.visible())-1))
	m.offset[m.tab] = max(0, o)
}

func (m *model) quit() tea.Cmd {
	if m.quitting {
		// A second request: leave now. The shims stop the children when
		// flashpoint's pipes to them close.
		return tea.Quit
	}
	m.quitting = true
	m.cancel()
	return nil
}

func (m *model) send(c runner.Control) {
	select {
	case m.ctl <- c:
	default:
	}
}

func (m *model) key(k tea.KeyPressMsg) tea.Cmd {
	s := k.String()
	if s == "ctrl+c" {
		return m.quit()
	}
	if m.filtering {
		switch s {
		case "enter":
			m.filtering = false
		case "esc":
			m.filtering, m.filter, m.input = false, "", ""
		case "backspace":
			if r := []rune(m.input); len(r) > 0 {
				m.input = string(r[:len(r)-1])
			}
			m.filter = m.input
		default:
			if k.Text != "" {
				m.input += k.Text
				m.filter = m.input
			}
		}
		m.offset[m.tab] = 0
		return nil
	}
	if m.help {
		m.help = false
		if s == "q" {
			return m.quit()
		}
		return nil
	}
	switch s {
	case "q":
		return m.quit()
	case "?":
		m.help = true
	case "1":
		m.tab = tabAPI
	case "2":
		m.tab = tabWeb
	case "w":
		m.send(runner.RestartWeb)
	case "3":
		m.tab = tabAll
	case "tab", "right", "l":
		m.tab = (m.tab + 1) % 3
	case "shift+tab", "left", "h":
		m.tab = (m.tab + 2) % 3
	case "up", "k":
		m.scroll(1)
	case "down", "j":
		m.scroll(-1)
	case "pgup", "ctrl+u", "b":
		m.scroll(m.logHeight() - 1)
	case "pgdown", "ctrl+d", "space":
		m.scroll(-(m.logHeight() - 1))
	case "home", "g":
		m.scroll(len(m.lines))
	case "end", "G", "f":
		m.offset[m.tab] = 0
	case "/":
		m.filtering, m.input = true, m.filter
	case "esc":
		m.filter = ""
	case "c":
		m.clear()
	case "r":
		m.send(runner.Rebuild)
	case "o":
		if m.status.WebURL != "" {
			m.open(m.status.WebURL)
		} else {
			m.open(m.status.APIURL)
		}
	case "O":
		m.open(m.status.APIURL)
	case "e":
		m.jumpToError()
	case "m":
		m.mouse = !m.mouse
	}
	return nil
}

func (m *model) clear() {
	kept := m.lines[:0]
	for _, e := range m.lines {
		if !e.in(m.tab) {
			kept = append(kept, e)
		}
	}
	m.lines = kept
	m.offset = [3]int{}
}

// jumpToError shows the API tab from the first line of the last failed
// build's output.
func (m *model) jumpToError() {
	if m.status.ErrorSeq == 0 {
		return
	}
	m.tab, m.filter = tabAPI, ""
	vis := m.visible()
	for i, e := range vis {
		if e.Seq == m.status.ErrorSeq {
			bottom := min(len(vis)-1, i+m.logHeight()-1)
			m.offset[tabAPI] = len(vis) - 1 - bottom
			return
		}
	}
}

func (m *model) click(ms tea.Mouse) {
	if ms.Button != tea.MouseLeft {
		return
	}
	if ms.Y == 0 {
		for _, z := range m.tabZones {
			if ms.X >= z.x0 && ms.X < z.x1 {
				m.tab = z.tab
			}
		}
		return
	}
	if ms.Y == m.height-2 {
		for _, z := range m.linkZones {
			if ms.X >= z.x0 && ms.X < z.x1 {
				m.open(z.url)
			}
		}
	}
}

package tui

import (
	"fmt"
	"strings"
	"time"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/x/ansi"

	"github.com/danielloader/flashpoint/internal/event"
)

var (
	amber = lipgloss.Color("#E3A008")
	green = lipgloss.Color("#3FB950")
	red   = lipgloss.Color("#F85149")
	cyan  = lipgloss.Color("#58A6FF")
	pink  = lipgloss.Color("#D2A8FF")
	grey  = lipgloss.Color("#8B949E")

	sDim    = lipgloss.NewStyle().Foreground(grey)
	sRed    = lipgloss.NewStyle().Foreground(red)
	sBrand  = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("#0D1117")).Background(amber).Padding(0, 1)
	sTab    = lipgloss.NewStyle().Padding(0, 1).Foreground(grey)
	sTabOn  = lipgloss.NewStyle().Padding(0, 1).Bold(true).Foreground(lipgloss.Color("#0D1117")).Background(cyan)
	sLink   = lipgloss.NewStyle().Underline(true)
	sKey    = lipgloss.NewStyle().Bold(true)
	sBorder = lipgloss.NewStyle().Border(lipgloss.RoundedBorder()).BorderForeground(grey).Padding(1, 3)
)

func srcStyle(s event.Source) lipgloss.Style {
	switch s {
	case event.API:
		return lipgloss.NewStyle().Foreground(cyan)
	case event.Web:
		return lipgloss.NewStyle().Foreground(pink)
	}
	return sDim
}

func (m *model) View() tea.View {
	v := tea.NewView(m.render())
	v.AltScreen = true
	v.WindowTitle = "flashpoint"
	if m.mouse {
		v.MouseMode = tea.MouseModeCellMotion
	}
	return v
}

func (m *model) render() string {
	if m.width == 0 || m.height == 0 {
		return ""
	}
	rows := make([]string, 0, m.height)
	rows = append(rows, m.header())
	body := m.logRows()
	if m.help {
		body = m.helpRows()
	}
	rows = append(rows, body...)
	rows = append(rows, m.statusRow(), m.hintRow())
	return strings.Join(rows, "\n")
}

func (m *model) header() string {
	brand := sBrand.Render("⚡ flashpoint")
	x := lipgloss.Width(brand) + 1
	parts := []string{brand, ""}
	m.tabZones = m.tabZones[:0]
	for i, name := range tabNames {
		t := tab(i)
		if t == tabWeb && !m.webShown {
			continue
		}
		label := fmt.Sprintf("%d %s", i+1, name)
		st := sTab
		if t == m.tab {
			st = sTabOn
		}
		r := st.Render(label)
		w := lipgloss.Width(r)
		m.tabZones = append(m.tabZones, zone{x0: x, x1: x + w, tab: t})
		parts = append(parts, r)
		x += w + 1
	}
	left := strings.Join(parts, " ")
	right := sDim.Render(m.version + "  ? help ")
	return fill(left, right, m.width)
}

// logRows renders the bottom of the current tab's lines, wrapped, from the
// scroll offset up.
func (m *model) logRows() []string {
	h := m.logHeight()
	vis := m.visible()
	end := len(vis) - m.offset[m.tab]
	var rows []string
	for i := end - 1; i >= 0 && len(rows) < h; i-- {
		wrapped := m.renderEntry(vis[i])
		rows = append(wrapped, rows...)
	}
	if len(rows) > h {
		rows = rows[len(rows)-h:]
	}
	if len(vis) == 0 {
		msg := "waiting for output…"
		if m.filter != "" {
			msg = fmt.Sprintf("no lines match %q", m.filter)
		}
		rows = []string{sDim.Render("  " + msg)}
	}
	for len(rows) < h {
		rows = append(rows, "")
	}
	return rows
}

func (m *model) renderEntry(e entry) []string {
	prefix := ""
	if m.tab == tabAll {
		label := e.Source.String()
		if e.Source == event.System {
			label = "fp"
		}
		prefix = srcStyle(e.Source).Render(fmt.Sprintf("%-3s", label)) + sDim.Render(" │ ")
	}
	text := e.text
	switch e.Kind {
	case event.Info:
		text = srcStyle(e.Source).Render("▸ ") + text
	case event.Error:
		text = sRed.Render("▸ " + ansi.Strip(text))
	case event.Diagnostic:
		text = sRed.Render(ansi.Strip(text))
	}
	pw := lipgloss.Width(prefix)
	width := max(10, m.width-pw)
	lines := strings.Split(ansi.Hardwrap(text, width, true), "\n")
	indent := strings.Repeat(" ", pw)
	for i, l := range lines {
		if i == 0 {
			lines[i] = prefix + l
		} else {
			lines[i] = indent + l
		}
	}
	return lines
}

func dot(c lipgloss.Style) string { return c.Render("●") }

func (m *model) statusRow() string {
	st := m.status
	m.linkZones = m.linkZones[:0]
	var b strings.Builder
	x := 0
	put := func(s string) {
		b.WriteString(s)
		x += lipgloss.Width(s)
	}
	link := func(url string) {
		text := strings.TrimPrefix(url, "http://")
		r := sLink.Hyperlink(url).Render(text)
		m.linkZones = append(m.linkZones, zone{x0: x, x1: x + len(text), url: url})
		put(r)
	}

	put(" ")
	ac, al := m.apiState()
	put(dot(ac) + " API " + ac.Render(al) + "  ")
	link(st.APIURL)
	switch {
	case st.API == event.APIBuilding && st.Serving:
		put(sDim.Render("  serving the last build"))
	case st.LastReady > 0 && st.API == event.APIReady:
		put(sDim.Render("  rebuilt in " + event.Seconds(st.LastReady)))
	}
	if st.Web != event.WebNone {
		put(sDim.Render("   │   "))
		wc, wl := webState(st.Web)
		put(dot(wc) + " Web " + wc.Render(wl) + "  ")
		link(st.WebURL)
	}

	var right string
	if o := m.offset[m.tab]; o > 0 {
		right = sDim.Render(fmt.Sprintf("↑ %d more  G follow ", o))
	} else {
		right = sDim.Render("following ")
	}
	if !st.Watching && st.API != event.APIStarting {
		right = sDim.Render("watch: off · reload: signal  ") + right
	}
	if m.filter != "" && !m.filtering {
		right = lipgloss.NewStyle().Foreground(amber).Render("filter: "+m.filter) + "  " + right
	}
	return fill(b.String(), right, m.width)
}

func (m *model) apiState() (lipgloss.Style, string) {
	switch m.status.API {
	case event.APIStarting:
		return lipgloss.NewStyle().Foreground(amber), "starting"
	case event.APIBuilding:
		return lipgloss.NewStyle().Foreground(amber), "building " + elapsed(m.now.Sub(m.since))
	case event.APIReady:
		return lipgloss.NewStyle().Foreground(green), "ready"
	case event.APIBuildFailed:
		return lipgloss.NewStyle().Foreground(red), "build failed"
	case event.APICrashed:
		return lipgloss.NewStyle().Foreground(red), "crashed"
	}
	return lipgloss.NewStyle().Foreground(red), "offline"
}

func webState(s event.WebState) (lipgloss.Style, string) {
	switch s {
	case event.WebStarting:
		return lipgloss.NewStyle().Foreground(amber), "starting"
	case event.WebReady:
		return lipgloss.NewStyle().Foreground(green), "ready"
	}
	return lipgloss.NewStyle().Foreground(red), "down"
}

func elapsed(d time.Duration) string {
	if d < 0 {
		d = 0
	}
	return fmt.Sprintf("%.1fs", d.Seconds())
}

func (m *model) hintRow() string {
	switch {
	case m.quitting:
		return " " + lipgloss.NewStyle().Foreground(amber).Render("stopping…") + sDim.Render("  (ctrl+c again to leave now)")
	case m.filtering:
		return " " + sKey.Render("/") + " " + m.input + "█" + sDim.Render("   enter keep · esc clear")
	case m.status.API == event.APIBuildFailed || m.status.API == event.APICrashed || (m.status.API == event.APIOffline && m.status.APIDetail != ""):
		detail := m.status.APIDetail
		hint := ""
		if m.status.API == event.APIBuildFailed {
			hint = "  e jump to error"
		}
		room := m.width - lipgloss.Width(hint) - 4
		return " " + sRed.Render("✗ "+ansi.Truncate(detail, max(10, room), "…")) + sDim.Render(hint)
	case m.status.WebDetail != "" && m.status.Web != event.WebReady:
		return " " + sRed.Render("✗ "+ansi.Truncate(m.status.WebDetail, max(10, m.width-4), "…"))
	}
	keys := []string{"r rebuild", "o open", "/ filter", "c clear", "tab switch", "? help", "q quit"}
	for i, k := range keys {
		key, rest, _ := strings.Cut(k, " ")
		keys[i] = sKey.Render(key) + sDim.Render(" "+rest)
	}
	return " " + ansi.Truncate(strings.Join(keys, sDim.Render("  ·  ")), m.width-1, "")
}

func (m *model) helpRows() []string {
	keys := [][2]string{
		{"1 2 3 / tab", "API, Web and All logs (or click a tab)"},
		{"↑ ↓  pgup pgdn", "scroll (or the mouse wheel)"},
		{"g  G", "top; bottom and follow"},
		{"/", "filter lines; esc clears"},
		{"c", "clear this tab"},
		{"e", "jump to the last build error"},
		{"r", "rebuild and restart the API now"},
		{"w", "restart the web dev server"},
		{"o  O", "open the web app; open the API"},
		{"m", "mouse capture on/off (off lets the terminal select text)"},
		{"q  ctrl+c", "stop everything and quit"},
	}
	var b strings.Builder
	b.WriteString(sKey.Render("flashpoint "+m.version) + "\n\n")
	for _, k := range keys {
		b.WriteString(fmt.Sprintf("%s  %s\n", sKey.Render(fmt.Sprintf("%-15s", k[0])), sDim.Render(k[1])))
	}
	b.WriteString("\n\n" + sDim.Render("any key closes this"))
	box := sBorder.Render(b.String())
	placed := lipgloss.Place(m.width, m.logHeight(), lipgloss.Center, lipgloss.Center, box)
	return strings.Split(placed, "\n")
}

// fill puts right at the right edge of a width-wide row.
func fill(left, right string, width int) string {
	gap := width - lipgloss.Width(left) - lipgloss.Width(right)
	if gap < 1 {
		return ansi.Truncate(left, width, "")
	}
	return left + strings.Repeat(" ", gap) + right
}

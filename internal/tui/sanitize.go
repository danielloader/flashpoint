package tui

import (
	"regexp"
	"strings"
)

var (
	csi = regexp.MustCompile(`\x1b\[[0-9;:?<=>]*[ -/]*[@-~]`)
	osc = regexp.MustCompile(`\x1b\][^\x07\x1b]*(\x07|\x1b\\)`)
	esc = regexp.MustCompile(`\x1b[@-Z\\-_]`)
)

// sanitize keeps a child's colours (SGR) and drops every other control
// sequence: a cursor move or screen clear meant for a terminal of its own
// would wreck the log view. A carriage return keeps only what was drawn last,
// as a progress line would look.
func sanitize(s string) string {
	if i := strings.LastIndexByte(strings.TrimRight(s, "\r"), '\r'); i >= 0 {
		s = s[i+1:]
	}
	s = strings.TrimRight(s, "\r")
	if strings.IndexByte(s, 0x1b) >= 0 {
		s = osc.ReplaceAllString(s, "")
		s = csi.ReplaceAllStringFunc(s, func(m string) string {
			if strings.HasSuffix(m, "m") {
				return m
			}
			return ""
		})
		s = esc.ReplaceAllString(s, "")
	}
	s = strings.ReplaceAll(s, "\t", "    ")
	return strings.Map(func(r rune) rune {
		if r < 0x20 && r != 0x1b {
			return -1
		}
		return r
	}, s)
}

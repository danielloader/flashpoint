package watch

import (
	"path"
	"strings"
)

// Match reports whether name, a slash-separated path relative to the
// project root, matches pattern. Pattern segments use path.Match syntax, and
// a "**" segment matches any number of segments, including none.
func Match(pattern, name string) bool {
	return match(strings.Split(pattern, "/"), strings.Split(name, "/"))
}

func match(pat, name []string) bool {
	for len(pat) > 0 {
		if pat[0] == "**" {
			for i := 0; i <= len(name); i++ {
				if match(pat[1:], name[i:]) {
					return true
				}
			}
			return false
		}
		if len(name) == 0 {
			return false
		}
		if ok, err := path.Match(pat[0], name[0]); err != nil || !ok {
			return false
		}
		pat, name = pat[1:], name[1:]
	}
	return len(name) == 0
}

// base is the directory a pattern can match under: its segments before the
// first one with a wildcard.
func base(pattern string) string {
	var fixed []string
	for _, seg := range strings.Split(pattern, "/") {
		if strings.ContainsAny(seg, "*?[\\") {
			break
		}
		fixed = append(fixed, seg)
	}
	// The last fixed segment of a wildcard-free pattern is the file itself.
	if len(fixed) == len(strings.Split(pattern, "/")) && len(fixed) > 0 {
		fixed = fixed[:len(fixed)-1]
	}
	return strings.Join(fixed, "/")
}

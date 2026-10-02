//go:build race

package main

// The shim is flashpoint itself, so under -race its slower start is part of
// every restart gap.
func init() {
	buildFlags = append(buildFlags, "-race")
	maxOutage *= 2
}

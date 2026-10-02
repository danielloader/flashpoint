//go:build !darwin

package runner

import "time"

// preflight is only needed on macOS, which assesses each new binary on its
// first exec.
func preflight(bin, dir string) time.Duration { return 0 }

//go:build !unix

package proc

import (
	"fmt"
	"os"
)

// RunShim is unix-only; flashpoint does not support this platform yet.
func RunShim([]string) int {
	fmt.Fprintln(os.Stderr, "flashpoint: this platform is not supported")
	return 2
}

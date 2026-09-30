//go:build !darwin && !linux && !freebsd && !openbsd && !netbsd && !dragonfly

package mention

import (
	"fmt"
	"os"
)

func openSelection(path string) (*os.File, error) {
	return nil, fmt.Errorf("mention: safe file selection is unavailable on this platform")
}

// Package tempdir selects the OS-owned scratch directory used by ghg.
package tempdir

import (
	"os"
	"path/filepath"
	"runtime"
)

// Base returns a system temporary directory that cannot resolve to ghg's
// working directory through a relative TMPDIR-style environment value.
func Base() string {
	switch runtime.GOOS {
	case "darwin":
		return "/private/tmp"
	case "linux":
		return "/tmp"
	}
	base := filepath.Clean(os.TempDir())
	if filepath.IsAbs(base) {
		return base
	}
	return filepath.Join(string(filepath.Separator), "tmp")
}

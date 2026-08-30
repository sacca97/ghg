//go:build !darwin && !linux

package sandbox

import "os"

func rootOwned(os.FileInfo) bool { return false }

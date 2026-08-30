//go:build !darwin && !linux

package tools

import "os"

func cachePathOwned(os.FileInfo) bool { return true }

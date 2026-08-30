//go:build darwin || linux

package tools

import (
	"os"
	"syscall"
)

func cachePathOwned(info os.FileInfo) bool {
	stat, ok := info.Sys().(*syscall.Stat_t)
	return ok && uint32(os.Geteuid()) == stat.Uid
}

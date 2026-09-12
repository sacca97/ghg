//go:build darwin || linux

package sys

import (
	"os"
	"syscall"
)

func OwnedBy(info os.FileInfo, uid uint32) bool {
	stat, ok := info.Sys().(*syscall.Stat_t)
	return ok && stat.Uid == uid
}

func RootOwned(info os.FileInfo) bool { return OwnedBy(info, 0) }

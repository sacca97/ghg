//go:build !darwin && !dragonfly && !freebsd && !linux && !netbsd && !openbsd

package sys

import "os"

func FileLocksSupported() bool           { return false }
func TryLockFile(*os.File) (bool, error) { return false, nil }
func LockFile(*os.File) error            { return nil }
func UnlockFile(*os.File) error          { return nil }

//go:build !darwin && !dragonfly && !freebsd && !linux && !netbsd && !openbsd

package auth

import "os"

func lockFile(*os.File) error {
	return nil
}

func unlockFile(*os.File) error {
	return nil
}

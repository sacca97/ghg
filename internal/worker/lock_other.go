//go:build !darwin && !dragonfly && !freebsd && !linux && !netbsd && !openbsd

package worker

import "os"

func lockFile(*os.File) error {
	return ErrUnsupported
}

func unlockFile(*os.File) error {
	return nil
}

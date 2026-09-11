//go:build !darwin && !dragonfly && !freebsd && !linux && !netbsd && !openbsd

package worker

import "os/exec"

func isolateProcess(*exec.Cmd) {}

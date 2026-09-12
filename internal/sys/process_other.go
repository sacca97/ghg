//go:build !darwin && !dragonfly && !freebsd && !linux && !netbsd && !openbsd

package sys

import "os/exec"

func IsolateProcess(*exec.Cmd) {}

//go:build darwin || dragonfly || freebsd || linux || netbsd || openbsd

package sys

import (
	"os/exec"
	"syscall"
)

func IsolateProcess(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
}

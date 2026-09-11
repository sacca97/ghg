//go:build darwin || dragonfly || freebsd || linux || netbsd || openbsd

package worker

import (
	"os/exec"
	"syscall"
)

// isolateProcess gives the worker its own process group so terminal signals
// (Ctrl-C, SIGHUP) aimed at the launching TUI or bridge cannot take down a
// worker that was deliberately detached. Stop signals the worker pid only;
// the worker's own children manage their own groups.
func isolateProcess(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
}

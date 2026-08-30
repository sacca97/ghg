package tools

import (
	"context"
	"errors"
	"fmt"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"syscall"
	"time"

	"github.com/sacca97/ghg/internal/sandbox"
)

const hookOutputLimit = 8 << 10

func clonePostEditHooks(in []PostEditHook) []PostEditHook {
	if in == nil {
		return nil
	}
	out := make([]PostEditHook, len(in))
	for i, hook := range in {
		out[i] = hook
		out[i].Command = append([]string(nil), hook.Command...)
		out[i].Extensions = append([]string(nil), hook.Extensions...)
	}
	return out
}

// HookReport is returned only for a noisy or unsuccessful hook. A successful
// silent hook deliberately contributes no tool output.
type HookReport struct {
	Command       string
	Status        string
	Stdout        string
	Stderr        string
	OmittedStdout int64
	OmittedStderr int64
}

func (r HookReport) note(runtime *ToolRuntime) string {
	command := r.Command
	if runtime != nil {
		command = runtime.RedactText(command)
	}
	var b strings.Builder
	fmt.Fprintf(&b, "postEdit %s: %s", command, r.Status)
	if r.Stdout != "" {
		fmt.Fprintf(&b, "\nstdout:\n%s", redactHookOutput(runtime, r.Stdout))
	}
	if r.Stderr != "" {
		fmt.Fprintf(&b, "\nstderr:\n%s", redactHookOutput(runtime, r.Stderr))
	}
	if r.OmittedStdout > 0 || r.OmittedStderr > 0 {
		fmt.Fprintf(&b, "\n... output omitted (stdout=%d bytes, stderr=%d bytes)", r.OmittedStdout, r.OmittedStderr)
	}
	return b.String()
}

func redactHookOutput(runtime *ToolRuntime, output string) string {
	if runtime == nil {
		return output
	}
	return runtime.RedactText(output)
}

// RunPostEditHooks invokes matching configured hooks once with the complete,
// sorted mutation batch. Hook failures are reports, not mutation failures:
// publication has already happened and callers reread disk afterward.
func (r *ToolRuntime) RunPostEditHooks(ctx context.Context, paths []string) []HookReport {
	if r == nil || len(r.PostEditHooks) == 0 || len(paths) == 0 {
		return nil
	}
	paths = uniqueHookPaths(paths)
	var reports []HookReport
	for _, hook := range r.PostEditHooks {
		matched := matchingHookPaths(hook, paths)
		if len(matched) == 0 {
			continue
		}
		report := runPostEditHook(ctx, r, hook, matched)
		if report.Status != "ok" || report.Stdout != "" || report.Stderr != "" || report.OmittedStdout > 0 || report.OmittedStderr > 0 {
			reports = append(reports, report)
		}
	}
	return reports
}

func uniqueHookPaths(paths []string) []string {
	seen := make(map[string]struct{}, len(paths))
	out := make([]string, 0, len(paths))
	for _, path := range paths {
		path = filepath.Clean(path)
		if path == "." {
			continue
		}
		if _, ok := seen[path]; ok {
			continue
		}
		seen[path] = struct{}{}
		out = append(out, path)
	}
	sort.Strings(out)
	return out
}

func matchingHookPaths(hook PostEditHook, paths []string) []string {
	if len(hook.Extensions) == 0 {
		return append([]string(nil), paths...)
	}
	allowed := make(map[string]struct{}, len(hook.Extensions))
	for _, ext := range hook.Extensions {
		allowed[strings.ToLower(ext)] = struct{}{}
	}
	var out []string
	for _, path := range paths {
		if _, ok := allowed[strings.ToLower(filepath.Ext(path))]; ok {
			out = append(out, path)
		}
	}
	return out
}

func runPostEditHook(ctx context.Context, runtime *ToolRuntime, hook PostEditHook, paths []string) HookReport {
	report := HookReport{Command: strings.Join(hook.Command, " "), Status: "failed"}
	if len(hook.Command) == 0 {
		report.Status = "invalid command"
		return report
	}
	workspace := ""
	if runtime.Policy != nil {
		workspace = runtime.Policy.Workspace()
	}
	wrapped, err := runtime.WrapCommand(sandbox.CommandSpec{
		Program: hook.Command[0],
		Args:    append(append([]string(nil), hook.Command[1:]...), paths...),
		Dir:     workspace,
		Env:     runtime.ChildEnv(nil),
	})
	if err != nil {
		report.Status = "sandbox: " + err.Error()
		return report
	}
	timeout := hook.Timeout
	if timeout <= 0 {
		timeout = 10 * time.Second
	}
	commandCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	cmd := exec.Command(wrapped.Program, wrapped.Args...)
	cmd.Dir, cmd.Env = wrapped.Dir, wrapped.Env
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	stdout, stderr := &hookOutput{}, &hookOutput{}
	cmd.Stdout, cmd.Stderr = stdout, stderr
	if err := cmd.Start(); err != nil {
		report.Status = "start: " + err.Error()
		return report
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	select {
	case err = <-done:
		if err != nil {
			report.Status = "exit: " + err.Error()
		} else {
			report.Status = "ok"
		}
	case <-commandCtx.Done():
		killHookProcess(cmd)
		<-done
		if errors.Is(commandCtx.Err(), context.DeadlineExceeded) {
			report.Status = "timeout"
		} else {
			report.Status = "cancelled"
		}
	}
	report.Stdout, report.OmittedStdout = stdout.String()
	report.Stderr, report.OmittedStderr = stderr.String()
	return report
}

type hookOutput struct {
	data  []byte
	total int64
}

func (o *hookOutput) Write(p []byte) (int, error) {
	o.total += int64(len(p))
	o.data = append(o.data, p...)
	if len(o.data) > hookOutputLimit {
		o.data = append([]byte(nil), o.data[len(o.data)-hookOutputLimit:]...)
	}
	return len(p), nil
}

func (o *hookOutput) String() (string, int64) {
	omitted := o.total - int64(len(o.data))
	if omitted < 0 {
		omitted = 0
	}
	return string(o.data), omitted
}

func killHookProcess(cmd *exec.Cmd) {
	if cmd.Process != nil {
		_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
		_ = cmd.Process.Kill()
	}
}

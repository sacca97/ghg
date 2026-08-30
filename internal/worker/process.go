package worker

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"sync"
)

type Process struct {
	cmd      *exec.Cmd
	waitOnce sync.Once
	waitDone chan struct{}
	waitErr  error
}

func Launch(ctx context.Context, executable string, env map[string]string) (*Process, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if strings.TrimSpace(executable) == "" {
		return nil, fmt.Errorf("worker executable is empty")
	}
	cmd := exec.CommandContext(ctx, executable)
	values := make(map[string]string)
	for _, pair := range os.Environ() {
		key, value, ok := strings.Cut(pair, "=")
		if ok {
			values[key] = value
		}
	}
	for key, value := range env {
		if key == "" || strings.ContainsRune(key, '=') || strings.ContainsRune(value, 0) {
			return nil, fmt.Errorf("invalid worker environment entry %q", key)
		}
		values[key] = value
	}
	cmd.Env = make([]string, 0, len(values))
	for key, value := range values {
		cmd.Env = append(cmd.Env, key+"="+value)
	}
	devNull, err := os.OpenFile(os.DevNull, os.O_WRONLY, 0)
	if err != nil {
		return nil, fmt.Errorf("open worker output sink: %w", err)
	}
	cmd.Stdin = nil
	cmd.Stdout = devNull
	cmd.Stderr = devNull
	if err := cmd.Start(); err != nil {
		devNull.Close()
		return nil, fmt.Errorf("start worker: %w", err)
	}
	_ = devNull.Close()
	return &Process{cmd: cmd, waitDone: make(chan struct{})}, nil
}

func (p *Process) PID() int {
	if p == nil || p.cmd == nil || p.cmd.Process == nil {
		return 0
	}
	return p.cmd.Process.Pid
}

func (p *Process) Wait() error {
	if p == nil || p.cmd == nil {
		return nil
	}
	p.waitOnce.Do(func() {
		p.waitErr = p.cmd.Wait()
		close(p.waitDone)
	})
	<-p.waitDone
	return p.waitErr
}

func (p *Process) Stop() error {
	if p == nil || p.cmd == nil || p.cmd.Process == nil {
		return nil
	}
	return p.cmd.Process.Signal(os.Interrupt)
}

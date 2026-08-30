package worker

import (
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
)

var (
	ErrAlreadyRunning = errors.New("worker is already running")
	ErrInvalidSession = errors.New("invalid worker session id")
	ErrUnsupported    = errors.New("worker runtime is unsupported on this platform")
)

type Runtime struct {
	BaseDir    string
	SessionID  string
	Dir        string
	SocketPath string
	LockPath   string
	StatePath  string
	PromptPath string
}

func NewRuntime(baseDir, sessionID string) (Runtime, error) {
	if !validSessionID(sessionID) {
		return Runtime{}, ErrInvalidSession
	}
	if baseDir == "" {
		return Runtime{}, fmt.Errorf("worker runtime base directory is empty")
	}
	absBase, err := filepath.Abs(baseDir)
	if err != nil {
		return Runtime{}, fmt.Errorf("resolve worker runtime base: %w", err)
	}
	runtime := runtimePaths(absBase, sessionID)
	runDir := filepath.Join(absBase, "run")
	for _, path := range []string{runDir, runtime.Dir} {
		if err := os.MkdirAll(path, 0700); err != nil {
			return Runtime{}, fmt.Errorf("create worker runtime directory: %w", err)
		}
		if err := os.Chmod(path, 0700); err != nil {
			return Runtime{}, fmt.Errorf("restrict worker runtime directory: %w", err)
		}
	}
	return runtime, nil
}

func runtimePaths(baseDir, sessionID string) Runtime {
	dir := filepath.Join(baseDir, "run", sessionID)
	return Runtime{
		BaseDir:    baseDir,
		SessionID:  sessionID,
		Dir:        dir,
		SocketPath: filepath.Join(dir, "worker.sock"),
		LockPath:   filepath.Join(dir, "worker.lock"),
		StatePath:  filepath.Join(dir, "state.json"),
		PromptPath: filepath.Join(dir, "prompt.txt"),
	}
}

func (r Runtime) Acquire() (*Lock, error) {
	file, err := os.OpenFile(r.LockPath, os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return nil, fmt.Errorf("open worker lock: %w", err)
	}
	if err := file.Chmod(0600); err != nil {
		file.Close()
		return nil, fmt.Errorf("restrict worker lock: %w", err)
	}
	if err := lockFile(file); err != nil {
		file.Close()
		if errors.Is(err, ErrAlreadyRunning) {
			return nil, ErrAlreadyRunning
		}
		return nil, err
	}
	return &Lock{file: file}, nil
}

func (r Runtime) Listen() (net.Listener, error) {
	if existing, err := os.Lstat(r.SocketPath); err == nil {
		if existing.Mode()&os.ModeSocket == 0 {
			return nil, fmt.Errorf("worker socket path is not a socket")
		}
		if err := os.Remove(r.SocketPath); err != nil {
			return nil, fmt.Errorf("remove stale worker socket: %w", err)
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return nil, fmt.Errorf("inspect worker socket: %w", err)
	}
	listener, err := net.Listen("unix", r.SocketPath)
	if err != nil {
		return nil, fmt.Errorf("listen for worker clients: %w", err)
	}
	if err := os.Chmod(r.SocketPath, 0600); err != nil {
		listener.Close()
		os.Remove(r.SocketPath)
		return nil, fmt.Errorf("restrict worker socket: %w", err)
	}
	return listener, nil
}

func (r Runtime) RemoveSocket() error {
	err := os.Remove(r.SocketPath)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	return err
}

func (r Runtime) RemovePrompt() error {
	err := os.Remove(r.PromptPath)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	return err
}

type Lock struct {
	file *os.File
}

func (l *Lock) Close() error {
	if l == nil || l.file == nil {
		return nil
	}
	err := unlockFile(l.file)
	closeErr := l.file.Close()
	l.file = nil
	if err != nil {
		return err
	}
	return closeErr
}

func validSessionID(id string) bool {
	if id == "" || len(id) > MaxSessionIDBytes || id == "." || id == ".." {
		return false
	}
	for _, ch := range id {
		if ch >= 'a' && ch <= 'z' || ch >= 'A' && ch <= 'Z' || ch >= '0' && ch <= '9' || ch == '-' || ch == '_' {
			continue
		}
		return false
	}
	return true
}

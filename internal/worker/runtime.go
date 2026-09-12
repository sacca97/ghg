package worker

import (
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/sacca97/ghg/internal/sys"
)

var (
	ErrAlreadyRunning = errors.New("worker is already running")
	ErrInvalidSession = errors.New("invalid worker session id")
	ErrUnsupported    = errors.New("worker runtime is unsupported on this platform")
)

type State string

const (
	StateIdle            State = "idle"
	StateRunning         State = "running"
	StateWaitingApproval State = "waiting_for_approval"
	StateWaitingQuestion State = "waiting_for_question"
	StateStopping        State = "stopping"
	StateInterrupted     State = "interrupted"
)

type StateRecord struct {
	SessionID string    `json:"session_id"`
	State     State     `json:"state"`
	Detached  bool      `json:"detached,omitempty"`
	Role      string    `json:"role,omitempty"`
	Mode      string    `json:"mode,omitempty"`
	PID       int       `json:"pid,omitempty"`
	UpdatedAt time.Time `json:"updated_at"`
	Detail    string    `json:"detail,omitempty"`
}

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
	if !sys.FileLocksSupported() {
		file.Close()
		return nil, ErrUnsupported
	}
	locked, err := sys.TryLockFile(file)
	if err != nil {
		file.Close()
		return nil, err
	}
	if !locked {
		file.Close()
		return nil, ErrAlreadyRunning
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
	return removeIfExists(r.SocketPath)
}

func (r Runtime) RemovePrompt() error {
	return removeIfExists(r.PromptPath)
}

func (r Runtime) WriteState(record StateRecord) error {
	if record.SessionID == "" {
		record.SessionID = r.SessionID
	}
	if record.SessionID != r.SessionID || !validSessionID(record.SessionID) {
		return ErrInvalidSession
	}
	if record.State == "" {
		return errors.New("worker state is empty")
	}
	record.UpdatedAt = record.UpdatedAt.UTC()
	if record.UpdatedAt.IsZero() {
		record.UpdatedAt = time.Now().UTC()
	}
	data, err := json.Marshal(record)
	if err != nil {
		return fmt.Errorf("marshal worker state: %w", err)
	}
	return writeFileAtomic(r.Dir, ".state-*", r.StatePath, "worker state", append(data, '\n'))
}

func (r Runtime) ReadState() (StateRecord, error) {
	data, err := os.ReadFile(r.StatePath)
	if err != nil {
		return StateRecord{}, err
	}
	var record StateRecord
	if err := json.Unmarshal(data, &record); err != nil {
		return StateRecord{}, fmt.Errorf("decode worker state: %w", err)
	}
	if record.SessionID != r.SessionID || !validSessionID(record.SessionID) {
		return StateRecord{}, ErrInvalidSession
	}
	return record, nil
}

func (r Runtime) RemoveState() error {
	return removeIfExists(r.StatePath)
}

func (r Runtime) WritePrompt(prompt string) error {
	return writeFileAtomic(r.Dir, ".prompt-*", r.PromptPath, "worker prompt", []byte(prompt))
}

func (r Runtime) ReadPrompt() (string, error) {
	data, err := os.ReadFile(r.PromptPath)
	if err != nil {
		return "", err
	}
	return string(data), nil
}

// Live reports if an active worker holds the process runtime lock.
// Ignores leftover sockets or state files from terminated processes.
func (r Runtime) Live() bool {
	lock, err := r.Acquire()
	if errors.Is(err, ErrAlreadyRunning) {
		return true
	}
	if err == nil {
		_ = lock.Close()
	}
	return false
}

type Lock struct {
	file *os.File
}

func (l *Lock) Close() error {
	if l == nil || l.file == nil {
		return nil
	}
	err := sys.UnlockFile(l.file)
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

func writeFileAtomic(dir, pattern, path, label string, data []byte) error {
	tmp, err := os.CreateTemp(dir, pattern)
	if err != nil {
		return fmt.Errorf("create %s: %w", label, err)
	}
	tmpName := tmp.Name()
	defer func() {
		_ = tmp.Close()
		_ = os.Remove(tmpName)
	}()
	if err := tmp.Chmod(0600); err != nil {
		return fmt.Errorf("restrict %s: %w", label, err)
	}
	if _, err := tmp.Write(data); err != nil {
		return fmt.Errorf("write %s: %w", label, err)
	}
	if err := tmp.Sync(); err != nil {
		return fmt.Errorf("sync %s: %w", label, err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("close %s: %w", label, err)
	}
	if err := os.Rename(tmpName, path); err != nil {
		return fmt.Errorf("publish %s: %w", label, err)
	}
	return nil
}

func removeIfExists(path string) error {
	err := os.Remove(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	return err
}

func ListStates(baseDir string) ([]StateRecord, error) {
	absBase, err := filepath.Abs(baseDir)
	if err != nil {
		return nil, err
	}
	entries, err := os.ReadDir(filepath.Join(absBase, "run"))
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var out []StateRecord
	for _, entry := range entries {
		if !entry.IsDir() || !validSessionID(entry.Name()) {
			continue
		}
		runtime := runtimePaths(absBase, entry.Name())
		record, err := runtime.ReadState()
		if err != nil {
			continue
		}
		out = append(out, record)
	}
	slices.SortFunc(out, func(a, b StateRecord) int {
		if n := a.UpdatedAt.Compare(b.UpdatedAt); n != 0 {
			return n
		}
		return strings.Compare(a.SessionID, b.SessionID)
	})
	return out, nil
}

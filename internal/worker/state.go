package worker

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"
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

func (r Runtime) ReadPrompt() (string, error) {
	data, err := os.ReadFile(r.PromptPath)
	if err != nil {
		return "", err
	}
	return string(data), nil
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

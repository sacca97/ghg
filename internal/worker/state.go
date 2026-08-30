package worker

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"time"
)

type State string

const (
	StateIdle            State = "idle"
	StateRunning         State = "running"
	StateWaitingApproval State = "waiting_for_approval"
	StateStopping        State = "stopping"
	StateInterrupted     State = "interrupted"
)

type StateRecord struct {
	SessionID string    `json:"session_id"`
	State     State     `json:"state"`
	Detached  bool      `json:"detached,omitempty"`
	Role      string    `json:"role,omitempty"`
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
	tmp, err := os.CreateTemp(r.Dir, ".state-*")
	if err != nil {
		return fmt.Errorf("create worker state: %w", err)
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName)
	if err := tmp.Chmod(0600); err != nil {
		tmp.Close()
		return fmt.Errorf("restrict worker state: %w", err)
	}
	if _, err := tmp.Write(append(data, '\n')); err != nil {
		tmp.Close()
		return fmt.Errorf("write worker state: %w", err)
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return fmt.Errorf("sync worker state: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("close worker state: %w", err)
	}
	if err := os.Rename(tmpName, r.StatePath); err != nil {
		return fmt.Errorf("publish worker state: %w", err)
	}
	return nil
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

func (r Runtime) WritePrompt(prompt string) error {
	tmp, err := os.CreateTemp(r.Dir, ".prompt-*")
	if err != nil {
		return fmt.Errorf("create worker prompt: %w", err)
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName)
	if err := tmp.Chmod(0600); err != nil {
		tmp.Close()
		return fmt.Errorf("restrict worker prompt: %w", err)
	}
	if _, err := tmp.WriteString(prompt); err != nil {
		tmp.Close()
		return fmt.Errorf("write worker prompt: %w", err)
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return fmt.Errorf("sync worker prompt: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("close worker prompt: %w", err)
	}
	if err := os.Rename(tmpName, r.PromptPath); err != nil {
		return fmt.Errorf("publish worker prompt: %w", err)
	}
	return nil
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
			if errors.Is(err, os.ErrNotExist) {
				continue
			}
			return nil, err
		}
		out = append(out, record)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].UpdatedAt.Equal(out[j].UpdatedAt) {
			return out[i].SessionID < out[j].SessionID
		}
		return out[i].UpdatedAt.Before(out[j].UpdatedAt)
	})
	return out, nil
}

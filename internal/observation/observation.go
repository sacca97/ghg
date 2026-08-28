// Package observation records the exact file bytes a read issued to the
// model. Stateful edits use those observations as their authorization
// boundary; a hash is deliberately not the authority.
package observation

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"os"
	"strings"
	"sync"
	"time"
)

// Record is one bounded, complete-line file observation. Content contains the
// exact file bytes for StartLine..EndLine, including their original line
// endings; it never contains a truncated line or model-added line numbers.
// Complete reports whether the requested read reached its line limit rather
// than the byte ceiling; even a byte-limited record still contains only whole
// lines and can authorize edits within the returned range.
type Record struct {
	ID          string    `json:"id"`
	SessionID   string    `json:"session_id,omitempty"`
	Path        string    `json:"path"`
	StartLine   int       `json:"start_line"`
	EndLine     int       `json:"end_line"`
	NextOffset  int       `json:"next_offset"`
	IssuedBytes int       `json:"issued_bytes"`
	Content     string    `json:"content"`
	Complete    bool      `json:"complete"`
	CreatedAt   time.Time `json:"created_at"`
}

// Store is the durable session boundary for observations. The session package
// implements it without importing the tools package.
type Store interface {
	SaveObservation(ctx context.Context, sessionID string, record Record) error
	LoadObservation(ctx context.Context, sessionID, id string) (Record, error)
}

// Registry provides live observations and an optional durable mirror. It is
// safe for concurrent tool calls and keeps no package-global current session.
type Registry struct {
	mu         sync.Mutex
	records    map[string]Record
	persistent Store
}

// NewRegistry creates an empty observation registry.
func NewRegistry() *Registry { return &Registry{records: make(map[string]Record)} }

// SetPersistent installs a durable mirror for future observations.
func (r *Registry) SetPersistent(store Store) {
	if r == nil {
		return
	}
	r.mu.Lock()
	r.persistent = store
	r.mu.Unlock()
}

// BindSession persists observations made before the first session row was
// created. The TUI creates a session after the first turn, so this bridge is
// needed for a read in that first turn to remain recoverable after restart.
func (r *Registry) BindSession(ctx context.Context, sessionID string) error {
	if r == nil || strings.TrimSpace(sessionID) == "" {
		return nil
	}
	r.mu.Lock()
	store := r.persistent
	var pending []Record
	for key, record := range r.records {
		if strings.HasPrefix(key, "\x00") {
			copyRecord := record
			copyRecord.SessionID = sessionID
			r.records[sessionKey(sessionID, record.ID)] = copyRecord
			delete(r.records, key)
			pending = append(pending, copyRecord)
		}
	}
	r.mu.Unlock()
	if store == nil {
		return nil
	}
	for _, record := range pending {
		if err := store.SaveObservation(ctx, sessionID, record); err != nil {
			return err
		}
	}
	return nil
}

// Save records an observation and mirrors it when sessionID is non-empty.
func (r *Registry) Save(ctx context.Context, sessionID string, record Record) error {
	if r == nil {
		return errors.New("observation registry is nil")
	}
	if strings.TrimSpace(record.ID) == "" {
		return errors.New("observation id is required")
	}
	if strings.TrimSpace(record.Path) == "" {
		return errors.New("observation path is required")
	}
	if record.StartLine <= 0 || record.EndLine < record.StartLine {
		return errors.New("observation line range is invalid")
	}
	if record.CreatedAt.IsZero() {
		record.CreatedAt = time.Now().UTC()
	}
	record.SessionID = sessionID
	r.mu.Lock()
	r.records[sessionKey(sessionID, record.ID)] = record
	store := r.persistent
	r.mu.Unlock()
	if store != nil && strings.TrimSpace(sessionID) != "" {
		return store.SaveObservation(ctx, sessionID, record)
	}
	return nil
}

// Load returns the live record first and then falls back to the durable store.
func (r *Registry) Load(ctx context.Context, sessionID, id string) (Record, error) {
	if r == nil {
		return Record{}, errors.New("observation registry is nil")
	}
	r.mu.Lock()
	record, ok := r.records[sessionKey(sessionID, id)]
	store := r.persistent
	r.mu.Unlock()
	if ok {
		return record, nil
	}
	if store == nil || strings.TrimSpace(sessionID) == "" {
		return Record{}, os.ErrNotExist
	}
	record, err := store.LoadObservation(ctx, sessionID, id)
	if err != nil {
		return Record{}, err
	}
	r.mu.Lock()
	r.records[sessionKey(sessionID, id)] = record
	r.mu.Unlock()
	return record, nil
}

func sessionKey(sessionID, id string) string { return sessionID + "\x00" + id }

// NewID returns a short opaque observation identifier.
func NewID() string {
	var raw [8]byte
	if _, err := rand.Read(raw[:]); err == nil {
		return "obs-" + hex.EncodeToString(raw[:])
	}
	return "obs-" + hex.EncodeToString([]byte(time.Now().UTC().Format("150405.000000000")))
}

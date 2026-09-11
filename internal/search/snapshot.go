// Package search owns stable snapshots, the fuzzy file index, and the bounded,
// filesystem-free structural query seam. Filesystem traversal, ranking,
// pagination rendering, and observation issuance remain in internal/tools.
package search

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"os"
	"slices"
	"strings"
	"sync"
	"time"
)

// Item is one stable search result. Line is zero for path-only results such as
// glob and find_files.
type Item struct {
	Path          string `json:"path"`
	Line          int    `json:"line,omitempty"`
	Text          string `json:"text,omitempty"`
	StartColumn   int    `json:"start_column,omitempty"`
	EndLine       int    `json:"end_line,omitempty"`
	EndColumn     int    `json:"end_column,omitempty"`
	StartByte     int    `json:"start_byte,omitempty"`
	EndByte       int    `json:"end_byte,omitempty"`
	Pattern       int    `json:"pattern,omitempty"`
	ObservationID string `json:"-"`
}

// Snapshot is the bounded result set behind a pagination cursor. It is
// immutable after Save; keeping the full bounded set here makes later pages
// independent of worktree changes.
type Snapshot struct {
	ID        string    `json:"id"`
	Kind      string    `json:"kind"`
	Items     []Item    `json:"items"`
	Patterns  []string  `json:"patterns,omitempty"`
	Complete  bool      `json:"complete"`
	Reason    string    `json:"reason,omitempty"`
	CreatedAt time.Time `json:"created_at"`
}

// Store persists snapshots under a session boundary. The session package
// implements this interface; Registry supplies an in-memory copy so a new
// session can paginate before its first database save.
type Store interface {
	SaveSearchSnapshot(ctx context.Context, sessionID string, snapshot Snapshot) error
	LoadSearchSnapshot(ctx context.Context, sessionID, id string) (Snapshot, error)
}

// Registry keeps live snapshots and optionally mirrors them into a durable
// session store. A session id is passed per operation so background and
// foreground agents can safely share the registry.
type Registry struct {
	mu         sync.Mutex
	snapshots  map[string]Snapshot
	persistent Store
}

// NewRegistry creates an empty snapshot registry.
func NewRegistry() *Registry {
	return &Registry{snapshots: make(map[string]Snapshot)}
}

// SetPersistent installs the durable session store used by future saves.
func (r *Registry) SetPersistent(store Store) {
	if r == nil {
		return
	}
	r.mu.Lock()
	r.persistent = store
	r.mu.Unlock()
}

// BindSession persists snapshots made before a session row existed. This is
// what lets an interactive first turn use a cursor and still recover it after
// the TUI creates the session at the end of that turn.
func (r *Registry) BindSession(ctx context.Context, sessionID string) error {
	if r == nil || strings.TrimSpace(sessionID) == "" {
		return nil
	}
	r.mu.Lock()
	store := r.persistent
	var pending []Snapshot
	for key, snapshot := range r.snapshots {
		if strings.HasPrefix(key, "\x00") {
			copySnapshot := cloneSnapshot(snapshot)
			destination := sessionKey(sessionID, snapshot.ID)
			if _, exists := r.snapshots[destination]; exists {
				// Keep the already-bound snapshot authoritative; never overwrite
				// a live or durable-session entry with pending state.
				delete(r.snapshots, key)
				continue
			}
			r.snapshots[destination] = copySnapshot
			delete(r.snapshots, key)
			pending = append(pending, copySnapshot)
		}
	}
	r.evictOldest(sessionID)
	r.mu.Unlock()
	if store == nil {
		return nil
	}
	for _, snapshot := range pending {
		if err := store.SaveSearchSnapshot(ctx, sessionID, snapshot); err != nil {
			return err
		}
	}
	return nil
}

const maxLiveSnapshotsPerSession = 16

// Save stores a snapshot in memory and mirrors it when sessionID is set.
func (r *Registry) Save(ctx context.Context, sessionID string, snapshot Snapshot) error {
	if r == nil {
		return errors.New("search snapshot registry is nil")
	}
	if strings.TrimSpace(snapshot.ID) == "" {
		return errors.New("search snapshot id is required")
	}
	if snapshot.CreatedAt.IsZero() {
		snapshot.CreatedAt = time.Now().UTC()
	}
	snapshot = cloneSnapshot(snapshot)
	r.mu.Lock()
	r.snapshots[sessionKey(sessionID, snapshot.ID)] = snapshot
	r.evictOldest(sessionID)
	store := r.persistent
	r.mu.Unlock()
	if store != nil && strings.TrimSpace(sessionID) != "" {
		return store.SaveSearchSnapshot(ctx, sessionID, snapshot)
	}
	return nil
}

// ponytail: O(n) eviction is intentional while the per-session limit is 16;
// replace with an LRU only if this bound grows materially.
func (r *Registry) evictOldest(sessionID string) {
	prefix := sessionID + "\x00"
	for {
		count := 0
		for key := range r.snapshots {
			if strings.HasPrefix(key, prefix) {
				count++
			}
		}
		if count <= maxLiveSnapshotsPerSession {
			return
		}
		candidates := make(map[string]Snapshot, count)
		for key, snapshot := range r.snapshots {
			if strings.HasPrefix(key, prefix) {
				candidates[key] = snapshot
			}
		}
		oldest := oldestKey(candidates, func(snapshot Snapshot) time.Time {
			return snapshot.CreatedAt
		}, false)
		if oldest == "" {
			return
		}
		delete(r.snapshots, oldest)
	}
}

// Load returns a live snapshot first, then asks the durable store for it.
func (r *Registry) Load(ctx context.Context, sessionID, id string) (Snapshot, error) {
	if r == nil {
		return Snapshot{}, errors.New("search snapshot registry is nil")
	}
	r.mu.Lock()
	snapshot, ok := r.snapshots[sessionKey(sessionID, id)]
	store := r.persistent
	r.mu.Unlock()
	if ok {
		return cloneSnapshot(snapshot), nil
	}
	if store == nil || strings.TrimSpace(sessionID) == "" {
		return Snapshot{}, os.ErrNotExist
	}
	snapshot, err := store.LoadSearchSnapshot(ctx, sessionID, id)
	if err != nil {
		return Snapshot{}, err
	}
	r.mu.Lock()
	r.snapshots[sessionKey(sessionID, id)] = cloneSnapshot(snapshot)
	r.evictOldest(sessionID)
	r.mu.Unlock()
	return snapshot, nil
}

func sessionKey(sessionID, id string) string { return sessionID + "\x00" + id }

func cloneSnapshot(in Snapshot) Snapshot {
	in.Items = slices.Clone(in.Items)
	in.Patterns = slices.Clone(in.Patterns)
	return in
}

// NewID returns a short opaque id suitable for a model-facing cursor. It is
// intentionally not a content hash: two searches of the same file can carry
// different bounded result sets.
func NewID(prefix string) string {
	var raw [8]byte
	if _, err := rand.Read(raw[:]); err == nil {
		return prefix + "-" + hex.EncodeToString(raw[:])
	}
	return prefix + "-" + hex.EncodeToString([]byte(time.Now().UTC().Format("150405.000000000")))
}

func oldestKey[V any](entries map[string]V, created func(V) time.Time, keyIsTiebreaker bool) string {
	oldestKey := ""
	var oldest time.Time
	for key, entry := range entries {
		at := created(entry)
		if oldestKey == "" || at.Before(oldest) || (keyIsTiebreaker && at.Equal(oldest) && key < oldestKey) {
			oldestKey, oldest = key, at
		}
	}
	return oldestKey
}

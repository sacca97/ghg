package search

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestRegistryScopesSnapshotsBySession(t *testing.T) {
	registry := NewRegistry()
	snapshot := Snapshot{ID: "grep-1", Kind: "grep", Items: []Item{{Path: "a.go", Line: 1}}, Complete: true}
	if err := registry.Save(nil, "one", snapshot); err != nil {
		t.Fatal(err)
	}
	if _, err := registry.Load(nil, "two", snapshot.ID); err == nil {
		t.Fatal("snapshot leaked across session boundary")
	}
	got, err := registry.Load(nil, "one", snapshot.ID)
	if err != nil || len(got.Items) != 1 || got.Items[0].Path != "a.go" {
		t.Fatalf("scoped snapshot = %+v, %v", got, err)
	}
}

func TestRegistrySnapshotEviction(t *testing.T) {
	registry := NewRegistry()
	for i := 1; i <= 20; i++ {
		snap := Snapshot{
			ID:        filepath.Base(string(rune('a'+i-1)) + "-snap"),
			Kind:      "grep",
			Items:     []Item{{Path: "file.go", Line: i}},
			CreatedAt: time.Now().Add(time.Duration(i) * time.Minute),
		}
		if err := registry.Save(nil, "sess-1", snap); err != nil {
			t.Fatal(err)
		}
	}
	// Oldest snapshots (1 to 4) should be evicted
	if _, err := registry.Load(nil, "sess-1", "a-snap"); err == nil {
		t.Fatal("expected oldest snapshot 'a-snap' to be evicted")
	}
	// Newer snapshots should be present
	if _, err := registry.Load(nil, "sess-1", "t-snap"); err != nil {
		t.Fatalf("expected newest snapshot 't-snap' to be present: %v", err)
	}
}

type memorySnapshotStore struct {
	snapshots map[string]Snapshot
}

func (s *memorySnapshotStore) SaveSearchSnapshot(_ context.Context, sessionID string, snapshot Snapshot) error {
	if s.snapshots == nil {
		s.snapshots = make(map[string]Snapshot)
	}
	s.snapshots[sessionKey(sessionID, snapshot.ID)] = cloneSnapshot(snapshot)
	return nil
}

func (s *memorySnapshotStore) LoadSearchSnapshot(_ context.Context, sessionID, id string) (Snapshot, error) {
	snapshot, ok := s.snapshots[sessionKey(sessionID, id)]
	if !ok {
		return Snapshot{}, os.ErrNotExist
	}
	return cloneSnapshot(snapshot), nil
}

func countSessionSnapshots(registry *Registry, sessionID string) int {
	registry.mu.Lock()
	defer registry.mu.Unlock()
	prefix := sessionID + "\x00"
	count := 0
	for key := range registry.snapshots {
		if strings.HasPrefix(key, prefix) {
			count++
		}
	}
	return count
}

func TestRegistryCapsLoadedAndBoundSnapshots(t *testing.T) {
	store := &memorySnapshotStore{snapshots: make(map[string]Snapshot)}
	for i := 0; i < maxLiveSnapshotsPerSession+1; i++ {
		id := "loaded-" + string(rune('a'+i))
		store.snapshots[sessionKey("sess", id)] = Snapshot{ID: id, CreatedAt: time.Unix(int64(i), 0)}
	}
	registry := NewRegistry()
	registry.SetPersistent(store)
	for i := 0; i < maxLiveSnapshotsPerSession+1; i++ {
		id := "loaded-" + string(rune('a'+i))
		if _, err := registry.Load(nil, "sess", id); err != nil {
			t.Fatal(err)
		}
	}
	if got := countSessionSnapshots(registry, "sess"); got != maxLiveSnapshotsPerSession {
		t.Fatalf("loaded snapshots = %d, want %d", got, maxLiveSnapshotsPerSession)
	}

	pending := NewRegistry()
	pending.mu.Lock()
	for i := 0; i < maxLiveSnapshotsPerSession; i++ {
		existingID := "existing-" + string(rune('a'+i))
		pendingID := "pending-" + string(rune('a'+i))
		pending.snapshots[sessionKey("sess", existingID)] = Snapshot{ID: existingID, CreatedAt: time.Unix(int64(i), 0)}
		pending.snapshots[sessionKey("", pendingID)] = Snapshot{ID: pendingID, CreatedAt: time.Unix(int64(i+maxLiveSnapshotsPerSession), 0)}
	}
	pending.mu.Unlock()
	if err := pending.BindSession(nil, "sess"); err != nil {
		t.Fatal(err)
	}
	if got := countSessionSnapshots(pending, "sess"); got != maxLiveSnapshotsPerSession {
		t.Fatalf("bound snapshots = %d, want %d", got, maxLiveSnapshotsPerSession)
	}

	duplicate := NewRegistry()
	duplicate.mu.Lock()
	duplicate.snapshots[sessionKey("sess", "same")] = Snapshot{ID: "same", Items: []Item{{Path: "bound.go"}}}
	duplicate.snapshots[sessionKey("", "same")] = Snapshot{ID: "same", Items: []Item{{Path: "pending.go"}}}
	duplicate.mu.Unlock()
	if err := duplicate.BindSession(nil, "sess"); err != nil {
		t.Fatal(err)
	}
	got, err := duplicate.Load(nil, "sess", "same")
	if err != nil || len(got.Items) != 1 || got.Items[0].Path != "bound.go" {
		t.Fatalf("duplicate bind replaced destination: %+v, %v", got, err)
	}
}

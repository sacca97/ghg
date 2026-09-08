package search

import (
	"path/filepath"
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

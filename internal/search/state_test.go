package search

import (
	"os"
	"path/filepath"
	"testing"
)

func TestFuzzyFilesScoresEveryCandidateBeforeLimit(t *testing.T) {
	root := t.TempDir()
	for i := 0; i < 40; i++ {
		path := filepath.Join(root, "archive", filepath.Base("note-"+string(rune('a'+i%26))+".txt"))
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, nil, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	strong := filepath.Join(root, "src", "roadmap.md")
	if err := os.MkdirAll(filepath.Dir(strong), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(strong, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	InvalidateFileIndex(root)
	hits := FuzzyFiles(root, "roadmap", 1)
	if len(hits) != 1 || hits[0] != "src/roadmap.md" {
		t.Fatalf("late strong match was cut off: %v", hits)
	}
}

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

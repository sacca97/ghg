package search

import (
	"os"
	"path/filepath"
	"testing"
	"time"
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

func TestFuzzyFilesTopKEqualsFullSortPrefix(t *testing.T) {
	root := t.TempDir()
	sampleFiles := []string{
		"cmd/ghg/main.go",
		"cmd/ghg/run.go",
		"internal/agent/agent.go",
		"internal/agent/task.go",
		"internal/agent/background.go",
		"internal/search/state.go",
		"internal/search/state_test.go",
		"internal/sandbox/policy.go",
		"internal/sandbox/backend.go",
		"internal/tui/tui.go",
		"internal/tui/input.go",
		"internal/tools/search.go",
		"internal/tools/read.go",
		"pkg/util/helper.go",
		"README.md",
		"roadmap.md",
	}
	for _, rel := range sampleFiles {
		full := filepath.Join(root, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte("content"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	InvalidateFileIndex(root)

	queries := []string{"", "go", "agent", "state", "search", "main", "md", "nonexistent"}
	limits := []int{1, 3, 5, 8, 20}

	for _, q := range queries {
		all := FuzzyFiles(root, q, 0)
		for _, lim := range limits {
			limited := FuzzyFiles(root, q, lim)
			expectedLen := min(lim, len(all))
			if len(limited) != expectedLen {
				t.Fatalf("query %q limit %d: got %d hits, want %d", q, lim, len(limited), expectedLen)
			}
			for i := 0; i < expectedLen; i++ {
				if limited[i] != all[i] {
					t.Fatalf("query %q limit %d at index %d: got %q, want %q", q, lim, i, limited[i], all[i])
				}
			}
		}
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


package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestProjectInstructionsTrustAndBounds(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "AGENTS.md")
	if err := os.WriteFile(path, []byte("run task check\n\nkeep this note"), 0o644); err != nil {
		t.Fatal(err)
	}

	if got := ProjectInstructions(root, false); got != "" {
		t.Fatalf("untrusted project instructions must be absent: %q", got)
	}
	got := ProjectInstructions(root, true)
	if !strings.Contains(got, "<project_instructions>") || !strings.Contains(got, "run task check") {
		t.Fatalf("trusted instructions missing from prompt block: %q", got)
	}

	if err := os.WriteFile(path, []byte{}, 0o644); err != nil {
		t.Fatal(err)
	}
	if got := ProjectInstructions(root, true); got != "" {
		t.Fatalf("empty instructions should be absent: %q", got)
	}

	if err := os.WriteFile(path, []byte(strings.Repeat("x", maxProjectInstructions+1)), 0o644); err != nil {
		t.Fatal(err)
	}
	if got := ProjectInstructions(root, true); got != "" {
		t.Fatal("oversized instructions must not enter the prompt")
	}
}

func TestProjectInstructionsRejectsSymlink(t *testing.T) {
	root := t.TempDir()
	target := filepath.Join(t.TempDir(), "AGENTS.md")
	if err := os.WriteFile(target, []byte("outside"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, filepath.Join(root, "AGENTS.md")); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	if got := ProjectInstructions(root, true); got != "" {
		t.Fatalf("symlinked project instructions must be absent: %q", got)
	}
}

func TestProjectGoalMaxRounds(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	dir := t.TempDir()

	if n := ProjectGoalMaxRounds(dir); n != 0 {
		t.Fatalf("no override should be 0, got %d", n)
	}
	if err := SetProjectGoalMaxRounds(dir, 42); err != nil {
		t.Fatal(err)
	}
	if n := ProjectGoalMaxRounds(dir); n != 42 {
		t.Fatalf("override should be 42, got %d", n)
	}
	other := t.TempDir()
	if n := ProjectGoalMaxRounds(other); n != 0 {
		t.Fatalf("other project should be 0, got %d", n)
	}
	if err := SetProjectGoalMaxRounds(dir, 0); err != nil {
		t.Fatal(err)
	}
	if n := ProjectGoalMaxRounds(dir); n != 0 {
		t.Fatalf("cleared override should be 0, got %d", n)
	}
}

func TestGoalMaxRoundsConfigRoundTrip(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	cfg := Default()
	cfg.GoalMaxRounds = 250
	if err := cfg.Save(); err != nil {
		t.Fatal(err)
	}
	loaded, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if loaded.GoalMaxRounds != 250 {
		t.Fatalf("goalMaxRounds should round-trip as 250, got %d", loaded.GoalMaxRounds)
	}
}

package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/sacca97/ghg/internal/config"
	"github.com/sacca97/ghg/internal/llm"
	"github.com/sacca97/ghg/internal/session"
)

// The system prompt always carries the built-in operating rules (the safety
// rails); ~/.ghg/me.md appends the user's standing instructions after them.
func TestSystemPromptAppendsUserMe(t *testing.T) {
	home := t.TempDir()
	t.Setenv("GHG_HOME", home)

	p := systemPrompt()
	if !strings.Contains(p, "never force-push") {
		t.Fatal("built-in operating rules must always be present")
	}
	if !strings.Contains(p, "verify it from the relevant source instead of guessing") {
		t.Fatal("embedded prompt must include the verification rule")
	}
	if strings.Contains(p, "Standing instructions") {
		t.Fatal("a fresh install (all-comments me.md) appends nothing")
	}

	os.WriteFile(filepath.Join(home, "me.md"), []byte("- Always pnpm, never npm.\n"), 0o644)
	p = systemPrompt()
	if !strings.Contains(p, "never force-push") {
		t.Fatal("built-in rules survive a user me.md")
	}
	if !strings.Contains(p, "Standing instructions from the user") || !strings.Contains(p, "Always pnpm") {
		t.Fatalf("user instructions should append:\n%s", p)
	}
}

func TestSystemPromptAppendsTrustedProjectInstructions(t *testing.T) {
	home := t.TempDir()
	root := t.TempDir()
	t.Setenv("GHG_HOME", home)
	t.Chdir(root)
	if err := config.Trust(root); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(home, "me.md"), []byte("prefer task test\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "AGENTS.md"), []byte("run task check\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	p := systemPrompt()
	if !strings.Contains(p, "<project_instructions>") || !strings.Contains(p, "run task check") {
		t.Fatalf("trusted AGENTS.md should be in the system prompt:\n%s", p)
	}
	base := strings.Index(p, "You are an expert coding assistant")
	cwd := strings.Index(p, "Current working directory: "+root)
	me := strings.Index(p, "Standing instructions from the user")
	project := strings.Index(p, "<project_instructions>")
	if base < 0 || cwd < base || me < cwd || project < me {
		t.Fatalf("prompt blocks out of order: base=%d cwd=%d me=%d project=%d", base, cwd, me, project)
	}

	if got := systemPromptForProject(false); strings.Contains(got, "run task check") {
		t.Fatal("untrusted project instructions must not be added")
	}
}

func TestSystemPromptPrefersBoundedExplorationTools(t *testing.T) {
	prompt := systemPrompt()
	for _, fragment := range []string{"Prefer grep for text", "glob for exact paths", "find_files for fuzzy paths", "read with offset/limit", "Reserve bash for builds, tests, git"} {
		if !strings.Contains(prompt, fragment) {
			t.Errorf("system prompt lacks %q", fragment)
		}
	}
}

func TestContinueSessionIDUsesCurrentDirectory(t *testing.T) {
	home := t.TempDir()
	root := t.TempDir()
	t.Setenv("GHG_HOME", home)
	t.Chdir(root)
	dir, err := config.Dir()
	if err != nil {
		t.Fatal(err)
	}
	st, err := session.Open(filepath.Join(dir, "sessions.db"))
	if err != nil {
		t.Fatal(err)
	}
	id, err := st.Create(root, "model", "provider")
	if err != nil {
		st.Close()
		t.Fatal(err)
	}
	if err := st.Save(id, 1, []llm.Message{{Role: "system"}, {Role: "user", Content: "continue me"}}, "model", "provider"); err != nil {
		st.Close()
		t.Fatal(err)
	}
	if err := st.Close(); err != nil {
		t.Fatal(err)
	}

	got, err := continueSessionID()
	if err != nil || got != id {
		t.Fatalf("continue session: %q, %v", got, err)
	}
}

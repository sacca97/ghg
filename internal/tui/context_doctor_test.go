package tui

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/sacca97/ghg/internal/config"
	"github.com/sacca97/ghg/internal/mcp"
	"github.com/sacca97/ghg/internal/skills"
)

func TestDoctorFreshSession(t *testing.T) {
	m := tasksModel("http://unused")
	m.sysPrompt = "You are an expert coding assistant operating inside ghg. "
	disabled := false
	m.mcpMgr = mcp.NewManager(map[string]mcp.ServerConfig{
		"off":     {Command: []string{"true"}, Enabled: &disabled},
		"invalid": {},
	})
	out := m.doctorReport()
	for _, want := range []string{
		"context audit",
		"system prompt (base)",
		"skills (",
		"tool schemas (",
		"mcp: off",
		"disabled",
		"mcp: invalid",
		"TOTAL injected",
		"Trim:",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q in:\n%s", want, out)
		}
	}
}

func TestDoctorCommandWired(t *testing.T) {
	m := tasksModel("http://unused")
	m.command("/context-doctor")
	if len(m.blocks) == 0 || !strings.Contains(m.blocks[len(m.blocks)-1].text, "context audit") {
		t.Fatalf("/context-doctor produced no report: %+v", m.blocks)
	}
	// No /doctor shorthand: the full name is the command.
	before := len(m.blocks)
	m.command("/doctor")
	if !strings.Contains(m.blocks[len(m.blocks)-1].text, "unknown command") {
		t.Error("/doctor should not exist as a shorthand")
	}
	_ = before
}

// TestDoctorSkillSources pins the attribution: each skill named in "biggest:"
// carries the directory it was discovered from, home-relative for ~/.ghg
// and ./-relative for the project dir — answering "where does this skill come
// from?" without leaving the report.
func TestDoctorSkillSources(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	proj := t.TempDir()
	t.Chdir(proj)

	writeSkill := func(dir, name, desc string) {
		t.Helper()
		sd := filepath.Join(dir, name)
		if err := os.MkdirAll(sd, 0o755); err != nil {
			t.Fatal(err)
		}
		body := "---\nname: " + name + "\ndescription: " + desc + "\n---\n"
		if err := os.WriteFile(filepath.Join(sd, "SKILL.md"), []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	writeSkill(filepath.Join(proj, ".agents", "skills"), "proj-skill", "from the project")
	writeSkill(filepath.Join(home, ".ghg", "skills"), "user-skill", "from the user dir")

	m := tasksModel("http://unused")
	m.skillScan = func() []skills.Skill { return skills.Scan(skills.DefaultDirs()...) }
	out := m.doctorReport()
	if !strings.Contains(out, "proj-skill") || !strings.Contains(out, "user-skill") {
		t.Fatalf("both skills should be named:\n%s", out)
	}
	if !strings.Contains(out, "proj-skill ~") || !strings.Contains(out, "(./.agents/skills)") {
		t.Errorf("project skill should point at ./.agents/skills:\n%s", out)
	}
	if !strings.Contains(out, "(~/.ghg/skills)") {
		t.Errorf("user skill should point at ~/.ghg/skills:\n%s", out)
	}
}

func TestDoctorProjectInstructions(t *testing.T) {
	home := t.TempDir()
	root := t.TempDir()
	t.Setenv("GHG_HOME", home)
	t.Chdir(root)
	if err := config.Trust(root); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "AGENTS.md"), []byte("run task check\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	m := tasksModel("http://unused")
	m.sysPrompt = "base\n\n" + config.ProjectInstructions(root, true)
	out := m.doctorReport()
	if !strings.Contains(out, "project instructions (AGENTS.md)") || !strings.Contains(out, "trusted project") {
		t.Fatalf("trusted project instructions should be audited:\n%s", out)
	}

	// A fresh model in an untrusted directory must not report or count the file.
	other := t.TempDir()
	t.Chdir(other)
	m.sysPrompt = "base"
	out = m.doctorReport()
	if strings.Contains(out, "project instructions (AGENTS.md)") {
		t.Fatalf("untrusted project instructions should be absent:\n%s", out)
	}
}

// TestShortSkillsDir pins the compaction rules: home → ~, cwd → ./, anything
// else stays absolute.
func TestShortSkillsDir(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	wd := t.TempDir()
	t.Chdir(wd)
	cases := map[string]string{
		filepath.Join(home, ".ghg", "skills"):                      "~/.ghg/skills",
		filepath.Join(wd, ".agents", "skills"):                     "./.agents/skills",
		filepath.Join(string(filepath.Separator), "opt", "skills"): "/opt/skills",
	}
	for dir, want := range cases {
		if got := shortSkillsDir(dir); got != want {
			t.Errorf("shortSkillsDir(%q) = %q, want %q", dir, got, want)
		}
	}
}

func TestTok(t *testing.T) {
	if tok(350) != "350" || tok(4848) != "4.8k" {
		t.Errorf("tok: %q %q", tok(350), tok(4848))
	}
}

package tui

import (
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/sacca97/ghg/internal/models"
	"github.com/sacca97/ghg/internal/session"
)

func (m *model) dispatches(name string) bool {
	before := len(m.blocks)
	m.command(name)
	for _, b := range m.blocks[before:] {
		if strings.Contains(b.text, "unknown command") {
			return false
		}
	}
	return true
}

// Every slash command in the registry must route through the dispatch switch
// to a real handler — the "registry says it exists but the switch 404s" drift
// class. The probe runs the bare command on a scratch model and fails if the
// transcript reports an unknown command.
func TestRegistryEntriesDispatch(t *testing.T) {
	for _, e := range slashRegistry() {
		if !compactCmdModel().dispatches(e.Name) {
			t.Errorf("%s is in the registry but the command switch doesn't handle it", e.Name)
		}
	}
}

// /help renders from the registry: every entry's hint (and the shell escape)
// must appear in its output.
func TestHelpContainsEveryRegistryHint(t *testing.T) {
	help := helpText()
	for _, e := range registry {
		if !strings.Contains(help, e.Hint) {
			t.Errorf("/help missing hint for %s: %q", e.Name, e.Hint)
		}
		if !strings.Contains(help, e.Name) {
			t.Errorf("/help missing command name %s", e.Name)
		}
	}
	// and the actual /help command prints it
	m := compactCmdModel()
	m.command("/help")
	if !strings.Contains(m.blocks[len(m.blocks)-1].text, "/compact") {
		t.Fatalf("/help output missing registry content: %q", m.blocks[len(m.blocks)-1].text)
	}
}

// The settings's slash-command rows take their description from the registry:
// for every row whose hint is a slash name, the rendered description must
// contain the registry hint.
func TestPaletteListsRegistryCommands(t *testing.T) {
	m := compactCmdModel()
	m.openPalette()
	rows := 0
	for _, it := range m.settings.all {
		if it.dynHint == nil || it.dynDesc == nil {
			continue
		}
		hint := it.dynHint(m)
		if !strings.HasPrefix(hint, "/") || strings.ContainsAny(hint, " ·<") {
			continue // keybind-only or usage-form hints ("/model · tab")
		}
		e := registryFind(hint)
		if e == nil {
			t.Errorf("settings row %q hints %q, which is not in the registry", it.title, hint)
			continue
		}
		if !strings.Contains(it.dynDesc(m), e.Hint) {
			t.Errorf("settings row %q desc %q doesn't come from the registry hint %q", it.title, it.dynDesc(m), e.Hint)
		}
		rows++
	}
	if rows < 8 {
		t.Fatalf("expected the settings to surface registry commands, found %d rows", rows)
	}
}

// The completion table is derived from the registry, never hand-maintained.
func TestCompletionMatchesRegistry(t *testing.T) {
	slash := slashRegistry()
	if len(commands) != len(slash) {
		t.Fatalf("completion table has %d entries, registry has %d slash commands", len(commands), len(slash))
	}
	for _, e := range slash {
		found := false
		for _, c := range commands {
			if c.Text == e.Name {
				found = true
				if c.Desc != e.Hint {
					t.Errorf("completion desc for %s = %q, registry hint is %q", e.Name, c.Desc, e.Hint)
				}
			}
		}
		if !found {
			t.Errorf("%s missing from the completion table", e.Name)
		}
	}
}

// TestEnvReportCollectsWhitelist: the bundle names the ghg/terminal/system
// facts and reads the whitelisted env vars.
func TestEnvReportCollectsWhitelist(t *testing.T) {
	t.Setenv("TERM", "xterm-256color")
	t.Setenv("TERM_PROGRAM", "ghostty")
	t.Setenv("TERM_PROGRAM_VERSION", "1.2.3")
	t.Setenv("COLORTERM", "truecolor")
	t.Setenv("COLORFGBG", "15;0")
	t.Setenv("TMUX", "") // outside tmux: no tmux row
	t.Setenv("SHELL", "/bin/zsh")
	t.Setenv("LC_ALL", "en_US.UTF-8")
	t.Setenv("SSH_TTY", "")
	t.Setenv("SSH_CONNECTION", "")

	Version = "1.4.0-test"
	defer func() { Version = "dev" }()

	m := &model{modelName: "gpt-5", provName: "openai",
		mouseOn: true, sessionID: "abc123", width: 120, height: 40}
	r := m.envReport()

	got := map[string]string{}
	for _, row := range r.rows {
		got[row.key] = row.val
	}
	want := map[string]string{
		"ghg":          "1.4.0-test",
		"model":        "gpt-5",
		"provider":     "openai",
		"TERM":         "xterm-256color",
		"TERM_PROGRAM": "ghostty 1.2.3",
		"COLORTERM":    "truecolor",
		"COLORFGBG":    "15;0",
		"SHELL":        "/bin/zsh",
		"locale":       "en_US.UTF-8",
		"size":         "120x40",
		"mouse":        "on",
		"session":      "abc123",
	}
	for k, v := range want {
		if got[k] != v {
			t.Errorf("row %q = %q, want %q", k, got[k], v)
		}
	}
	for _, k := range []string{"os", "go", "uname"} {
		if got[k] == "" {
			t.Errorf("row %q missing", k)
		}
	}
	// outside tmux / ssh: those rows are absent, not empty
	if _, ok := got["tmux"]; ok {
		t.Errorf("tmux row present outside tmux: %q", got["tmux"])
	}
	if _, ok := got["ssh"]; ok {
		t.Errorf("ssh row present without SSH_TTY/SSH_CONNECTION")
	}
}

// TestEnvReportTmuxAndSSH: inside tmux over ssh the tmux row carries the
// server version and ssh is flagged.
func TestEnvReportTmuxAndSSH(t *testing.T) {
	t.Setenv("TMUX", "/tmp/tmux-1000/default,1234,0")
	t.Setenv("SSH_TTY", "/dev/pts/3")
	m := &model{width: 80, height: 24}
	r := m.envReport()
	got := map[string]string{}
	for _, row := range r.rows {
		got[row.key] = row.val
	}
	if got["ssh"] != "yes" {
		t.Errorf("ssh = %q, want yes", got["ssh"])
	}
	// tmux -V runs only when tmux is on PATH; either way the row exists.
	if _, ok := got["tmux"]; !ok {
		t.Error("tmux row missing inside tmux")
	}
}

// TestEnvReportNoSecrets: secret-shaped env vars never enter the bundle —
// the whitelist reads only its named keys.
func TestEnvReportNoSecrets(t *testing.T) {
	t.Setenv("OPENAI_API_KEY", "sk-supersecret-123")
	t.Setenv("ANTHROPIC_API_KEY", "sk-ant-supersecret")
	t.Setenv("GITHUB_TOKEN", "ghp_supersecret")
	t.Setenv("AWS_SECRET_ACCESS_KEY", "aws-supersecret")

	m := &model{width: 80, height: 24}
	r := m.envReport()
	for _, s := range []string{r.snippet, r.link} {
		for _, secret := range []string{"supersecret", "sk-", "ghp_"} {
			if strings.Contains(s, secret) {
				t.Errorf("secret material %q leaked into bundle:\n%s", secret, s)
			}
		}
	}
}

// TestReportSnippetFenced: the copy-paste form is a fenced code block with
// aligned rows and no OSC 8 escape sequences (clipboard-safe verbatim).
func TestReportSnippetFenced(t *testing.T) {
	m := &model{modelName: "m1", width: 100, height: 30}
	r := m.envReport()
	if !strings.HasPrefix(r.snippet, "```\n") || !strings.HasSuffix(r.snippet, "```") {
		t.Errorf("snippet not fenced: %q", r.snippet)
	}
	if strings.ContainsRune(r.snippet, 0x1b) {
		t.Error("snippet contains ESC — hyperlinks/styling must not leak into the paste form")
	}
	if !strings.Contains(r.snippet, "ghg ") || !strings.Contains(r.snippet, "model") || !strings.Contains(r.snippet, "m1") {
		t.Errorf("snippet missing rows:\n%s", r.snippet)
	}
}

// TestIssueURL: the link targets the ghg repo's new-issue page, round-trips
// through url.Parse, and its body carries the skeleton plus the env bundle.
func TestIssueURL(t *testing.T) {
	snippet := "```\nghg 1.2.3\nTERM xterm\n```"
	link := issueURL(snippet)
	u, err := url.Parse(link)
	if err != nil {
		t.Fatalf("link does not parse: %v", err)
	}
	if u.Scheme+"://"+u.Host+u.Path != issueBase {
		t.Errorf("link target = %s://%s%s, want %s", u.Scheme, u.Host, u.Path, issueBase)
	}
	body := u.Query().Get("body")
	for _, want := range []string{"### What happened", "### Expected", "### Environment", snippet} {
		if !strings.Contains(body, want) {
			t.Errorf("issue body missing %q:\n%s", want, body)
		}
	}
	if len(link) > 8000 { // GitHub's practical URL ceiling
		t.Errorf("link too long: %d bytes", len(link))
	}
}

// TestReportBlock: the transcript block pairs the clickable link with the
// snippet; the link is OSC 8 (terminal owns the click).
func TestReportBlock(t *testing.T) {
	m := &model{modelName: "m1", provName: "p1", width: 90, height: 30}
	b := m.reportBlock()
	if !strings.Contains(b, "\x1b]8;;"+issueBase) {
		t.Error("block missing OSC 8 hyperlink to the new-issue page")
	}
	if !strings.Contains(b, "open a prefilled GitHub issue") {
		t.Error("block missing the link label")
	}
	if !strings.Contains(b, "```\n") {
		t.Error("block missing the fenced snippet")
	}
}

// TestReportCommandAppendsOneBlock: /report appends exactly one transcript
// block (headless, like /context-doctor).
func TestReportCommandAppendsOneBlock(t *testing.T) {
	m := &model{width: 80, height: 24}
	before := len(m.blocks)
	if _, cmd := m.command("/report"); cmd != nil {
		t.Error("/report should not return a tea.Cmd")
	}
	if len(m.blocks) != before+1 {
		t.Fatalf("blocks grew by %d, want 1", len(m.blocks)-before)
	}
	if !strings.Contains(m.blocks[before].text, issueBase) {
		t.Error("appended block does not carry the issue link")
	}
}

// TestReportIsBusySafe: /report is read-only, so it runs mid-turn instead of
// being queued as a message.
func TestReportIsBusySafe(t *testing.T) {
	if !busyCmd("/report") {
		t.Error("/report should be safe while busy")
	}
}

func TestReviewWithoutTargetShowsUsage(t *testing.T) {
	m := compactCmdModel()
	m.command("/review")
	var found bool
	for _, blk := range m.blocks {
		if strings.Contains(blk.text, "usage: /review <target or instructions>") {
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("expected usage text, got blocks: %+v", m.blocks)
	}
}

func TestSchedulePersistence(t *testing.T) {
	t.Setenv("GHG_HOME", t.TempDir())
	st, err := session.Open(filepath.Join(t.TempDir(), "s.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	m := compactCmdModel()
	m.store = st

	id, err := st.Create(cwd(), m.modelName, m.provName)
	if err != nil {
		t.Fatal(err)
	}
	m.sessionID = id

	m.scheduleCommand([]string{"@every", "10m", "check the deploy status"})
	m.scheduleCommand([]string{"@at", time.Now().Add(time.Hour).UTC().Format(time.RFC3339), "one-shot reminder"})

	tasks := st.Schedules(m.sessionID)
	if len(tasks) != 2 {
		t.Fatalf("two tasks stored, got %d", len(tasks))
	}
	if tasks[0].Schedule != "@every 10m0s" || tasks[0].Prompt != "check the deploy status" {
		t.Fatalf("task 1: %+v", tasks[0])
	}
	if !strings.HasPrefix(tasks[1].Schedule, "@at ") {
		t.Fatalf("task 2: %+v", tasks[1])
	}

	// cancel removes it
	m.scheduleCommand([]string{"cancel", "1"})
	if tasks := st.Schedules(m.sessionID); len(tasks) != 1 {
		t.Fatalf("after cancel: %d tasks", len(tasks))
	}
}

// TestStartupReportSkillsAndWarnings: the report names loaded skills, flags a
// description that exceeds maxDesc (truncated in the system prompt), and
// flags a SKILL.md that fails to parse — pi's [Skill conflicts] block.
func TestStartupReportSkillsAndWarnings(t *testing.T) {
	dir := t.TempDir()
	mkSkill := func(name, desc string) {
		d := filepath.Join(dir, ".agents", "skills", name)
		os.MkdirAll(d, 0o755)
		os.WriteFile(filepath.Join(d, "SKILL.md"), []byte("---\nname: "+name+"\ndescription: "+desc+"\n---\n"), 0o644)
	}
	mkSkill("good", "fine")
	mkSkill("wordy", strings.Repeat("x", 1100)) // over the spec's 1024
	// A SKILL.md with no frontmatter = parse problem.
	bad := filepath.Join(dir, ".agents", "skills", "broken")
	os.MkdirAll(bad, 0o755)
	os.WriteFile(filepath.Join(bad, "SKILL.md"), []byte("no frontmatter here"), 0o644)

	wd, _ := os.Getwd()
	os.Chdir(dir)
	defer os.Chdir(wd)

	m := compactCmdModel()
	m.startupReport()
	if len(m.blocks) == 0 {
		t.Fatal("no report rendered")
	}
	out := m.blocks[0].text
	if m.skillsLoaded != 2 {
		t.Errorf("loaded skill count: got %d, want 2", m.skillsLoaded)
	}
	if strings.Contains(out, "skills: 2 loaded") {
		t.Errorf("loaded count should move to the header, not the startup report:\n%s", out)
	}
	head := strings.SplitN(m.View(), "\n", 2)[0]
	if !strings.Contains(head, "skills: 2 loaded") {
		t.Errorf("header missing loaded count: %q", head)
	}
	if !strings.Contains(out, "wordy") || !strings.Contains(out, "exceeds 1024") {
		t.Errorf("missing truncation warning:\n%s", out)
	}
	if !strings.Contains(out, "broken") {
		t.Errorf("missing parse problem:\n%s", out)
	}
}

// TestStartupReportSilent: nothing loaded, nothing said.
func TestStartupReportSilent(t *testing.T) {
	dir := t.TempDir()
	wd, _ := os.Getwd()
	os.Chdir(dir)
	defer os.Chdir(wd)
	t.Setenv("HOME", t.TempDir()) // no ~/.ghg/skills either

	m := compactCmdModel()
	m.startupReport()
	if len(m.blocks) != 0 {
		t.Errorf("expected silence, got %q", m.blocks[0].text)
	}
}

func shellModel() *model {
	m := &model{
		input: newInput(),
	}
	m.width = 80
	m.input.SetWidth(m.width - 2)
	return m
}

func TestRunShellEmptyIsANote(t *testing.T) {
	m := shellModel()
	m.runShell("!")
	if len(m.messages) != 0 {
		t.Fatalf("bare ! should not touch the conversation: %v", m.messages)
	}
	if b := lastBlock(m); !strings.Contains(b, "! <command>") {
		t.Fatalf("bare ! should print a usage note: %q", b)
	}
}

func TestSeedTranscriptRendersShellMessage(t *testing.T) {
	m := shellModel()
	msgs := []models.Message{{Role: "user", Content: "$ ls\nfoo.go bar.go"}}
	m.seedTranscript(msgs, 1)
	if b := lastBlock(m); !strings.Contains(b, "$ ls") {
		t.Fatalf("a resumed shell message should render: %q", b)
	}
}

// "! " with only spaces after the bang → usage note, no message
func TestBangWhitespaceOnlyIsANote(t *testing.T) {
	m := shellModel()
	m.input.SetValue("!   ")
	m = press(t, m, tea.KeyMsg{Type: tea.KeyEnter})
	if len(m.messages) != 0 {
		t.Fatalf("whitespace-only ! should be the usage note: %v", m.messages)
	}
}

// "!" not at offset 0 (e.g. pasted mid-line) — idle path trims and checks prefix
func TestBangNotAtStartQueuesAsMessage(t *testing.T) {
	m := busyQueueModel() // busy: plain text queues instead of submitting (no provider needed)
	m.input.SetValue("say ! loudly")
	m = press(t, m, tea.KeyMsg{Type: tea.KeyEnter})
	if len(m.queue) != 1 || m.queue[0] != "say ! loudly" {
		t.Fatalf("mid-string ! must queue as a plain message: %v", m.queue)
	}
	if len(m.messages) != 0 {
		t.Fatal("mid-string ! must not trigger the shell escape")
	}
}

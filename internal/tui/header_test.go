package tui

import (
	"strings"
	"testing"

	"github.com/charmbracelet/x/ansi"

	"github.com/sacca97/ghg/internal/agent"
	"github.com/sacca97/ghg/internal/llm"
)

func TestFmtTok(t *testing.T) {
	for in, want := range map[int]string{
		0: "0", 999: "999", 1000: "1.0k", 12345: "12.3k",
		1_000_000: "1.0M", 1_234_567: "1.2M",
	} {
		if got := fmtTok(in); got != want {
			t.Errorf("fmtTok(%d) = %q, want %q", in, got, want)
		}
	}
}

// Context size and route controls live in the bottom status bar; the header is
// reserved for the app identity and loaded-skill count.
func TestHeaderKeepsContextInStatus(t *testing.T) {
	m := compactCmdModel()
	m.agent = agent.New(testBackend("https://x", "k"), "kimi-k3-fast", 100, "sys")
	m.agent.ContextLimit = 100000
	m.follow = true
	m.agent.AddUsage(llm.Usage{PromptTokens: 12345, CompletionTokens: 678})
	m.agent.AddUsage(llm.Usage{ // cached tokens accumulate through the details struct
		PromptTokens: 1,
		PromptTokensDetails: &struct {
			CachedTokens int `json:"cached_tokens"`
		}{CachedTokens: 4000},
	})
	m.width = 200 // wide enough for the full status context block
	head := strings.SplitN(m.View(), "\n", 2)[0]
	for _, unwanted := range []string{"12.3k", "4.0k", "678", "ctx", "tok"} {
		if strings.Contains(head, unwanted) {
			t.Errorf("header should not contain context details %q: %q", unwanted, head)
		}
	}
	status := m.statusView()
	if !strings.Contains(status, "ctx ") || !strings.Contains(status, "/100.0k") {
		t.Errorf("status missing context size: %q", status)
	}
	for _, unwanted := range []string{"↓", "↑", "tok", "12.3k", "4.0k", "678"} {
		if strings.Contains(status, unwanted) {
			t.Errorf("status should not contain request token usage %q: %q", unwanted, status)
		}
	}
	if strings.Contains(head, "⚡") || strings.Contains(head, "kimi-k3-fast") || strings.Contains(head, shortCWD()) {
		t.Errorf("header should not repeat the route or working directory: %q", head)
	}
}

func TestHeaderContainsOnlyAppAndSkillCount(t *testing.T) {
	m := compactCmdModel()
	m.skillsLoaded = 33
	m.goal = "finish the release"
	m.follow = false
	m.width = 120

	head := strings.SplitN(ansi.Strip(m.View()), "\n", 2)[0]
	if got, want := strings.TrimSpace(head), "ghg · skills: 33 loaded"; got != want {
		t.Fatalf("header = %q, want exactly %q", got, want)
	}
}

func TestHeaderShowsLoadedSkillCount(t *testing.T) {
	m := compactCmdModel()
	m.skillsLoaded = 33
	head := strings.SplitN(m.View(), "\n", 2)[0]
	if !strings.Contains(head, " ghg · skills: 33 loaded") {
		t.Fatalf("header should show the loaded skill count next to ghg: %q", head)
	}
}

// The header omits the context block entirely and leaves route details to the
// bottom status line.
func TestHeaderOmitsContext(t *testing.T) {
	m := compactCmdModel()
	m.width = 120
	head := strings.SplitN(m.View(), "\n", 2)[0]
	if strings.Contains(head, "⣿") {
		t.Errorf("header should not contain a context block: %q", head)
	}
	if strings.Contains(head, "⚡") || strings.Contains(head, "kimi-k3-fast") {
		t.Errorf("header should omit effort and route controls: %q", head)
	}
}

func TestHeaderOmitsEffortControl(t *testing.T) {
	m := compactCmdModel()
	head := strings.SplitN(m.View(), "\n", 2)[0]
	if strings.Contains(head, "⚡") || strings.Contains(head, "effort") {
		t.Fatalf("header should not render a reasoning control: %q", head)
	}
}

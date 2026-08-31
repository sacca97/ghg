package tui

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/charmbracelet/x/ansi"

	"github.com/sacca97/ghg/internal/agent"
	"github.com/sacca97/ghg/internal/config"
	"github.com/sacca97/ghg/internal/llm"
)

// statusModel builds a model with an agent so statusView has data.
func statusModel() *model {
	m := newGrowModel()
	m.agent = &agent.Agent{}
	return m
}

// The status box always renders below the input with directory, model,
// mode, provider, and context size — regardless of scroll or state.
func TestStatusLineAlwaysShown(t *testing.T) {
	m := statusModel()
	m.width = 120
	m.modelName = "kimi-k3-fast"
	m.provName = "inference"
	m.agent.Effort = "high"
	m.agent.ContextLimit = 128000
	m.agent.Messages = []llm.Message{{Role: "system", Content: "system prompt"}}

	v := m.View()
	for _, want := range []string{"kimi-k3-fast", "(high)", "execute", "inference", "ctx 8/128.0k"} {
		if !strings.Contains(v, want) {
			t.Errorf("status box should show %q\n--- view tail ---\n%s", want, tailLines(v, 8))
		}
	}
	// the directory is present (compacted to its last segments) — assert
	// against the actual cwd via shortCWD, not a hardcoded checkout name:
	// tests run from internal/tui regardless of the repo folder's name.
	if dir := shortCWD(); !strings.Contains(v, dir) {
		t.Errorf("status box should show the working directory %q\n%s", dir, tailLines(v, 8))
	}
}

func TestContextStatusUsesLatestReportedRequest(t *testing.T) {
	m := statusModel()
	m.agent.ContextLimit = 128000
	m.agent.Messages = []llm.Message{
		{Role: "system", Content: strings.Repeat("x", 40000)},
		{Role: "assistant", Content: "first", Usage: &llm.Usage{PromptTokens: 100, CompletionTokens: 20}},
		{Role: "user", Content: "next"},
		{Role: "assistant", Content: "latest", Usage: &llm.Usage{PromptTokens: 300, CompletionTokens: 40}},
	}

	if got, want := m.contextStatus(), "ctx 340/128.0k"; got != want {
		t.Fatalf("context status = %q, want %q", got, want)
	}
}

func TestContextStatusEstimatesActiveTokensWithoutReportedResponse(t *testing.T) {
	m := statusModel()
	m.agent.ContextLimit = 128000
	m.agent.Messages = []llm.Message{{Role: "system", Content: strings.Repeat("x", 40000)}}

	if got, want := m.contextStatus(), "ctx 10.0k/128.0k"; got != want {
		t.Fatalf("context status = %q, want %q", got, want)
	}
}

// With no conversation yet the context reads zero, and effort remains a
// separate control.
func TestStatusLineDefaults(t *testing.T) {
	m := statusModel()
	m.width = 120
	m.modelName = "m"
	m.provName = "p"

	v := m.View()
	if !strings.Contains(v, "ctx 0") {
		t.Errorf("empty session should read ctx 0\n%s", tailLines(v, 6))
	}
	if !strings.Contains(v, "│ m │ (off) │ execute │") {
		t.Errorf("effort off should remain a separate indicator\n%s", tailLines(v, 6))
	}
	if !strings.Contains(v, "│ m │ (off) │ execute │ p │") {
		t.Errorf("model, mode, and provider should appear\n%s", tailLines(v, 6))
	}
}

func TestStatusBoxHasBordersAndDelimiters(t *testing.T) {
	m := statusModel()
	m.width = 120
	m.modelName = "model"
	m.provName = "provider"

	lines := strings.Split(ansi.Strip(m.statusView()), "\n")
	if len(lines) != statusBoxRows {
		t.Fatalf("status box should have %d rows, got %d: %q", statusBoxRows, len(lines), lines)
	}
	if !strings.HasPrefix(lines[0], "┌") || !strings.HasSuffix(lines[0], "┐") {
		t.Errorf("top row should be bordered: %q", lines[0])
	}
	if !strings.HasPrefix(lines[1], "│") || !strings.HasSuffix(lines[1], "│") {
		t.Errorf("content row should be bordered: %q", lines[1])
	}
	if !strings.HasPrefix(lines[2], "└") || !strings.HasSuffix(lines[2], "┘") {
		t.Errorf("bottom row should be bordered: %q", lines[2])
	}
	if got := strings.Count(lines[1], "│"); got != 7 {
		t.Errorf("content row should delimit all six cells, got %d vertical bars: %q", got, lines[1])
	}
	for i, line := range lines {
		if got := ansi.StringWidth(line); got > m.width {
			t.Errorf("status row %d is wider than terminal: %d > %d", i, got, m.width)
		}
	}
}

func TestBottomStatusHitboxMatchesRenderedRow(t *testing.T) {
	m := statusModel()
	tm, _ := m.Update(mkWinSize(100, 30))
	m = tm.(*model)

	lines := strings.Split(ansi.Strip(m.View()), "\n")
	renderedRow := -1
	for i, line := range lines {
		if strings.Contains(line, "execute") && strings.Contains(line, "ctx") {
			renderedRow = i
			break
		}
	}
	if renderedRow < 0 {
		t.Fatalf("could not find the rendered status row:\n%s", m.View())
	}
	if renderedRow != statusInfoRow(m.height) {
		t.Fatalf("status hitbox row=%d, rendered row=%d; the bottom controls would not receive clicks", statusInfoRow(m.height), renderedRow)
	}
}

func TestStatusBoxFitsNarrowWidths(t *testing.T) {
	for _, width := range []int{1, 2, 6, 10, 40} {
		t.Run(fmt.Sprintf("width-%d", width), func(t *testing.T) {
			m := statusModel()
			m.width = width
			lines := strings.Split(ansi.Strip(m.statusView()), "\n")
			if len(lines) != statusBoxRows {
				t.Fatalf("status box should have %d rows, got %d: %q", statusBoxRows, len(lines), lines)
			}
			for i, line := range lines {
				if got := ansi.StringWidth(line); got > width {
					t.Errorf("status row %d is wider than terminal: %d > %d (%q)", i, got, width, line)
				}
			}
		})
	}
}

func TestStatusModelSlotStaysFixedAcrossRoleChanges(t *testing.T) {
	m := compactCmdModel()
	m.width = 120
	short := "short-model"
	long := "a-deliberately-long-selected-model"
	m.cfg.Roles = map[string]config.RoleConfig{
		config.RoleDefault: {Model: short, Provider: "inference"},
		config.RoleSmart:   {Model: long, Provider: "inference"},
		config.RoleFast:    {Model: short, Provider: "inference"},
		config.RoleTiny:    {Model: short, Provider: "inference"},
	}
	m.cfg.Models[short] = config.Model{Providers: []string{"inference"}}
	m.cfg.Models[long] = config.Model{Providers: []string{"inference"}}

	m.modelName = short
	m.agent.Role = config.RoleDefault
	_ = m.View()
	effortX, modeX, modelW := m.statusEffortX, m.statusModeX, m.statusModelW
	if modelW != len(long) {
		t.Fatalf("model slot should reserve the longest selected role model: got %d, want %d", modelW, len(long))
	}

	m.modelName = long
	m.agent.Role = config.RoleSmart
	_ = m.View()
	if m.statusEffortX != effortX || m.statusModeX != modeX || m.statusModelW != modelW {
		t.Fatalf("following status controls moved with the model: short=(%d,%d,%d), long=(%d,%d,%d)", effortX, modeX, modelW, m.statusEffortX, m.statusModeX, m.statusModelW)
	}

	m.width = 40
	_ = m.View()
	if m.statusEffortW == 0 || m.statusModeW == 0 {
		t.Fatalf("narrow status should retain effort and mode controls: effort=%d mode=%d", m.statusEffortW, m.statusModeW)
	}
	if m.statusModelW >= len(long) {
		t.Fatalf("narrow status should truncate the model slot, got width %d", m.statusModelW)
	}
}

// Request usage is no longer rendered in the status segment; context size is
// the useful persistent value there instead.
func TestStatusLineOmitsTokenUsage(t *testing.T) {
	m := statusModel()
	m.modelName = "m"
	m.provName = "p"
	u := llm.Usage{PromptTokens: 10000, CompletionTokens: 500}
	u.PromptTokensDetails = &struct {
		CachedTokens int `json:"cached_tokens"`
	}{CachedTokens: 4000}
	m.agent.AddUsage(u)

	got := m.statusView()
	if strings.ContainsAny(got, "↓↑") || strings.Contains(got, "tok") {
		t.Errorf("status should no longer show directional token usage: %q", got)
	}
	if !strings.Contains(got, "ctx 0") {
		t.Errorf("status should show context size instead: %q", got)
	}
}

func TestBusyStatsLeavesTokenUsageToStatus(t *testing.T) {
	m := statusModel()
	m.turnStart = time.Unix(100, 0)
	m.now = func() time.Time { return time.Unix(101, 0) }
	m.agent.AddUsage(llm.Usage{PromptTokens: 12000, CompletionTokens: 800})

	got := m.busyStats()
	if strings.Contains(got, "tok") || strings.Contains(got, "%") || strings.Contains(got, "12.0k") {
		t.Fatalf("busy line should not render token usage: %q", got)
	}
	if !strings.Contains(got, "0:01") {
		t.Fatalf("busy line should retain elapsed time: %q", got)
	}
}

// The status box is the last content rows before the bottom padding, sitting
// below the input even when the esc/quit warnings or completion menu show.
func TestStatusLineBelowInputAndWarnings(t *testing.T) {
	m := statusModel()
	m.modelName = "m"
	m.provName = "p"
	m.escClr = true // draft-clear warning armed

	v := m.View()
	lines := strings.Split(strings.TrimRight(v, "\n"), "\n")
	var inputRow, statusRow int
	for i, l := range lines {
		if strings.Contains(l, "Ask ghg anything") {
			inputRow = i
		}
		if strings.Contains(l, "ctx 0") {
			statusRow = i
		}
	}
	if statusRow <= inputRow {
		t.Fatalf("status box should sit below the input (input=%d status=%d)\n%s", inputRow, statusRow, v)
	}
}

// Exactly one blank line separates the status box from whatever is above it,
// and the three-row box is the final content (no blank line below).
func TestStatusLineSpacing(t *testing.T) {
	m := statusModel()
	m.width = 120
	m.modelName = "m"
	m.provName = "p"

	lines := strings.Split(m.View(), "\n")
	var statusRow = -1
	for i, l := range lines {
		if strings.Contains(l, "ctx 0") {
			statusRow = i
		}
	}
	if statusRow < 1 {
		t.Fatalf("status box not found\n%s", m.View())
	}
	if lines[statusRow-2] != "" {
		t.Errorf("want one blank line above the status box, got %q", lines[statusRow-2])
	}
	for i, want := range []string{"┌", "│", "└"} {
		if !strings.HasPrefix(ansi.Strip(lines[statusRow-1+i]), want) {
			t.Errorf("status box row %d should start with %q, got %q", i, want, lines[statusRow-1+i])
		}
	}
	// the bottom border is the last row, with nothing below it
	if statusRow+1 != len(lines)-1 {
		t.Errorf("status box should be the last rows (bottom row %d of %d lines)", statusRow+1, len(lines)-1)
	}
}

// tailLines returns the last n lines of s, for failure output.
func tailLines(s string, n int) string {
	lines := strings.Split(strings.TrimRight(s, "\n"), "\n")
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	return strings.Join(lines, "\n")
}

// Cost appears in the context segment when the provider's catalog advertises
// pricing for the current model, and is hidden otherwise.
func TestStatusLineShowsCost(t *testing.T) {
	m := statusModel()
	m.modelName = "m"
	m.provName = "p"
	m.agent.Model = "priced"
	m.catalogs = map[string]config.Catalog{
		"p": {Models: []config.ModelInfoLite{{ID: "priced", InPrice: 1e-6, OutPrice: 5e-6, CacheReadPrice: 1e-7}}},
	}
	u := llm.Usage{PromptTokens: 10000, CompletionTokens: 1000}
	u.PromptTokensDetails = &struct {
		CachedTokens int `json:"cached_tokens"`
	}{CachedTokens: 8000}
	m.agent.AddUsage(u)

	// (10k-8k)*1e-6 + 8k*1e-7 + 1k*5e-6 = 0.0078
	if got := m.statusView(); !strings.Contains(got, "$0.0078") {
		t.Errorf("cost should show in the spend: %q", got)
	}
}

func TestStatusLineHidesCostWithoutPricing(t *testing.T) {
	m := statusModel()
	m.modelName = "m"
	m.provName = "p"
	m.agent.Model = "unpriced"
	m.catalogs = map[string]config.Catalog{
		"p": {Models: []config.ModelInfoLite{{ID: "unpriced"}}},
	}
	m.agent.AddUsage(llm.Usage{PromptTokens: 10000, CompletionTokens: 1000})

	if got := m.statusView(); strings.Contains(got, "$") {
		t.Errorf("unpriced model should hide cost: %q", got)
	}

	// no catalog for the provider at all (startup fetch still in flight)
	m.catalogs = nil
	if got := m.statusView(); strings.Contains(got, "$") {
		t.Errorf("missing catalog should hide cost: %q", got)
	}
}

func TestFmtCost(t *testing.T) {
	if got := fmtCost(0.0134); got != "$0.0134" {
		t.Errorf("sub-dollar: %q", got)
	}
	if got := fmtCost(12.345); got != "$12.35" {
		t.Errorf("over a dollar: %q", got)
	}
}

// The /models fetch → catalog → cost pipeline keeps per-variant pricing
// distinct (kimi-k3-fast bills higher than kimi-k3) with nothing hardcoded:
// rates come from the provider's response body alone.
func TestSessionCostUsesFetchedPricing(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"data":[
			{"id":"kimi-k3","pricing":{"prompt":"0.000003","completion":"0.000015","input_cache_read":"0.0000003"}},
			{"id":"kimi-k3-fast","pricing":{"prompt":"0.0000045","completion":"0.0000225","input_cache_read":"0.00000045"}}
		]}`))
	}))
	defer srv.Close()

	infos, err := llm.New(srv.URL, "k").Models(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	lites := make([]config.ModelInfoLite, len(infos))
	for i, mi := range infos {
		lites[i] = config.ModelInfoLite{ID: mi.ID}
		if mi.Pricing != nil {
			lites[i].InPrice, lites[i].OutPrice, lites[i].CacheReadPrice = mi.Pricing.Rates()
		}
	}
	m := statusModel()
	m.provName = "inference"
	m.catalogs = map[string]config.Catalog{"inference": {Models: lites}}
	u := llm.Usage{PromptTokens: 31100, CompletionTokens: 360}
	u.PromptTokensDetails = &struct {
		CachedTokens int `json:"cached_tokens"`
	}{CachedTokens: 20700}
	m.agent.AddUsage(u)

	m.agent.Model = "kimi-k3-fast"
	fast, ok := m.sessionCost()
	if !ok {
		t.Fatal("fast variant should be priced")
	}
	m.agent.Model = "kimi-k3"
	std, ok := m.sessionCost()
	if !ok {
		t.Fatal("standard variant should be priced")
	}
	if fast <= std {
		t.Errorf("kimi-k3-fast cost %v should exceed kimi-k3 %v", fast, std)
	}
	// exact: (31100-20700)*4.5e-6 + 20700*4.5e-7 + 360*22.5e-6
	if want := 0.064215; fast != want {
		t.Errorf("kimi-k3-fast cost = %v, want %v", fast, want)
	}
}

func TestWorkerSnapshotContextTokensOverridesStaleShadowAgent(t *testing.T) {
	m := statusModel()
	m.agent.ContextLimit = 128000
	// Shadow agent has ~10k tokens worth of text
	m.agent.Messages = []llm.Message{{Role: "system", Content: strings.Repeat("x", 40000)}}

	// Direct execution would say 10.0k
	if got, want := m.contextStatus(), "ctx 10.0k/128.0k"; got != want {
		t.Fatalf("pre-worker context status = %q, want %q", got, want)
	}

	// Apply worker snapshot with authoritative 80617 tokens
	m.applyWorkerSnapshot(workerSnapshot{
		State:         "running",
		ContextTokens: 80617,
		ContextLimit:  128000,
	})

	// Should reflect authoritative worker count (~80.6k)
	if got, want := m.contextStatus(), "ctx 80.6k/128.0k"; got != want {
		t.Fatalf("worker snapshot context status = %q, want %q", got, want)
	}
}

func TestWorkerUsageAndCompactionSynchronizeContextTokens(t *testing.T) {
	m := statusModel()
	m.agent.ContextLimit = 128000
	m.workerState = "running"
	m.workerContextTokens = 1000

	if got, want := m.contextStatus(), "ctx 1.0k/128.0k"; got != want {
		t.Fatalf("initial context status = %q, want %q", got, want)
	}

	// Worker sends usage event for 50k prompt + 2k completion
	usageJSON, err := json.Marshal(llm.Usage{PromptTokens: 50000, CompletionTokens: 2000})
	if err != nil {
		t.Fatal(err)
	}
	m.workerEvent(workerEvent{Kind: "usage", Data: usageJSON})

	if got, want := m.contextStatus(), "ctx 52.0k/128.0k"; got != want {
		t.Fatalf("post-usage context status = %q, want %q", got, want)
	}

	// Worker completes compaction, returning compacted messages (~1k tokens)
	compactJSON, err := json.Marshal(workerCompactResult{
		Messages: []llm.Message{
			{Role: "system", Content: "sys"},
			{Role: "user", Content: "summary"},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	m.workerEvent(workerEvent{Kind: "compact_done", Data: compactJSON})

	// Context tokens recomputed from returned compacted messages
	if m.workerContextTokens >= 52000 || m.workerContextTokens == 0 {
		t.Fatalf("expected compacted tokens < 52k, got %d", m.workerContextTokens)
	}
	if got := m.contextStatus(); strings.Contains(got, "52.0k") {
		t.Fatalf("expected context status to decrease after compaction, got %q", got)
	}
}

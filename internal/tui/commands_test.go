package tui

import (
	"context"
	"encoding/json"
	"errors"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/x/ansi"
	"github.com/sacca97/ghg/internal/agent"
	"github.com/sacca97/ghg/internal/auth"
	"github.com/sacca97/ghg/internal/config"
	"github.com/sacca97/ghg/internal/goal"
	"github.com/sacca97/ghg/internal/mcp"
	"github.com/sacca97/ghg/internal/memory"
	"github.com/sacca97/ghg/internal/models"
	"github.com/sacca97/ghg/internal/session"
	"github.com/sacca97/ghg/internal/skills"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func authTestModel(t *testing.T) *model {
	t.Helper()
	home := t.TempDir()
	t.Setenv("GHG_HOME", home)
	cfg, err := config.Load()
	if err != nil {
		t.Fatal(err)
	}
	profiles, err := models.Load(models.LoadOptions{UserDir: filepath.Join(home, "providers")})
	if err != nil {
		t.Fatal(err)
	}
	m := &model{
		input:    newInput(),
		cfg:      cfg,
		profiles: profiles,
		queueSel: -1,
	}
	m.width = 80
	m.input.SetWidth(m.width - 2)
	return m
}

func authResolved(t *testing.T, m *model, id string) models.Resolved {
	t.Helper()
	resolved, err := auth.ResolveProfile(m.profiles, id)
	if err != nil {
		t.Fatal(err)
	}
	return resolved
}

// transcriptText concatenates every appended block's text for assertions.
func (m *model) transcriptText() string {
	var sb strings.Builder
	for _, b := range m.blocks {
		sb.WriteString(b.text + "\n")
	}
	return sb.String()
}

func TestAuthCommandListsProfilesAndStatus(t *testing.T) {
	m := authTestModel(t)
	resolved := authResolved(t, m, "generic-openai")
	if err := m.cfg.UpsertProviderKey("generic-openai", resolved, "sk-generic", false); err != nil {
		t.Fatal(err)
	}

	m.authCommand(nil)
	out := m.transcriptText()
	if !strings.Contains(out, "provider profiles:") || !strings.Contains(out, "anthropic") {
		t.Fatalf("bare /auth should list profile IDs:\n%s", out)
	}
	if !strings.Contains(out, "generic-openai") || !strings.Contains(out, "configured") {
		t.Fatalf("configured profile should be marked:\n%s", out)
	}
	if strings.Contains(out, "sk-generic") {
		t.Fatal("profile listing leaked a key")
	}
}

func TestAuthCommandUnknownProviderListsAvailable(t *testing.T) {
	m := authTestModel(t)
	m.authCommand([]string{"not-a-profile"})
	out := m.transcriptText()
	if !strings.Contains(out, "unknown provider") || !strings.Contains(out, "anthropic") {
		t.Fatalf("unknown profile should list available IDs:\n%s", out)
	}
}

func TestAuthCommandRejectsAuthNone(t *testing.T) {
	m := authTestModel(t)
	resolved := authResolved(t, m, "generic-openai")
	resolved.Auth.Kind = models.AuthNone
	m.applyAuthResult(authResultMsg{name: "generic-openai", profile: resolved, key: "sk-nope"})
	if !strings.Contains(m.transcriptText(), "takes no API key") {
		t.Fatalf("auth:none result should be refused:\n%s", m.transcriptText())
	}
}

func TestAuthCommandBareOpensMaskedPrompt(t *testing.T) {
	m := authTestModel(t)
	m.authCommand([]string{"anthropic"})
	if m.namePrompt == nil {
		t.Fatal("bare /auth <id> should open the key prompt")
	}
	if !m.namePrompt.mask {
		t.Error("key prompt must mask input")
	}
	if !strings.Contains(m.namePrompt.label, "Anthropic") {
		t.Errorf("prompt should use profile display name: %q", m.namePrompt.label)
	}
	if got := m.namePrompt.maskedValue("sk-anthropic-secret"); strings.Contains(got, "secret") {
		t.Errorf("masked render leaked the key: %q", got)
	}

	m.closeNamePrompt()
	if m.namePrompt != nil {
		t.Error("esc should close the prompt")
	}
	if m.cfg.AnyProviderConfigured() {
		t.Error("cancelled prompt must not configure a provider")
	}
}

func TestAuthResultGoodKeyConfiguresAndSeedsOnlyOnLivePath(t *testing.T) {
	m := authTestModel(t)
	resolved := authResolved(t, m, "generic-openai")
	m.applyAuthResult(authResultMsg{
		name:      "generic-openai",
		profile:   resolved,
		key:       "sk-generic-good",
		validated: true,
		models:    []models.ModelInfo{{ID: "gpt-test", ContextLength: 128000, InputModalities: []string{"text", "image"}}},
	})

	p, ok := m.cfg.Providers["generic-openai"]
	if !ok {
		t.Fatal("provider should be configured after a good auth")
	}
	if p.APIKey != "sk-generic-good" || p.Profile != "generic-openai" {
		t.Errorf("literal profile key not stored: %+v", p)
	}
	if cats := config.LoadCatalogs(); len(cats) != 0 {
		t.Error("dispatch-level auth must not write the live catalog cache")
	}
	out := m.transcriptText()
	if !strings.Contains(out, "generic-openai configured") {
		t.Errorf("success should be reported:\n%s", out)
	}
	if strings.Contains(out, "sk-generic-good") {
		t.Error("the key must never appear in the transcript")
	}
}

func TestAuthResultColdStartUsesFastRole(t *testing.T) {
	m := authTestModel(t)
	resolved := authResolved(t, m, "generic-openai")
	legacyDefault := m.cfg.DefaultModel
	m.cfg.Models = map[string]config.Model{
		"fast-model": {Providers: []string{"generic-openai"}, Context: 128000},
	}
	m.cfg.Roles = map[string]config.RoleConfig{
		config.RoleFast: {Provider: "generic-openai", Model: "fast-model"},
	}

	m.applyAuthResult(authResultMsg{
		name:      "generic-openai",
		profile:   resolved,
		key:       "sk-fast",
		validated: true,
	})

	if m.agent == nil {
		t.Fatal("auth should promote a cold TUI to an agent")
	}
	if m.agent.Role != config.RoleFast || m.modelName != "fast-model" || m.provName != "generic-openai" {
		t.Fatalf("cold auth route = role %q, model %q, provider %q; want fast-model @ generic-openai", m.agent.Role, m.modelName, m.provName)
	}
	if m.cfg.DefaultModel != legacyDefault {
		t.Fatalf("role-driven auth should not replace legacy defaultModel, got %q (want %q)", m.cfg.DefaultModel, legacyDefault)
	}
}

func TestAuthResultBadKeyWritesNothing(t *testing.T) {
	m := authTestModel(t)
	m.applyAuthResult(authResultMsg{
		name: "generic-openai",
		err:  errors.New("provider \"generic-openai\" validation failed: 401 invalid key"),
	})

	if m.cfg.AnyProviderConfigured() {
		t.Error("a rejected key must not configure a provider")
	}
	if cats := config.LoadCatalogs(); len(cats) != 0 {
		t.Error("a rejected key must not write the catalog")
	}
	if !strings.Contains(m.transcriptText(), "rejected") {
		t.Errorf("failure should be reported:\n%s", m.transcriptText())
	}
}

func TestAuthResultUnvalidatedRequiresConfirmation(t *testing.T) {
	m := authTestModel(t)
	resolved := authResolved(t, m, "generic-openai")
	m.applyAuthResult(authResultMsg{
		name:        "generic-openai",
		profile:     resolved,
		key:         "sk-unvalidated",
		unvalidated: true,
	})
	if m.cfg.AnyProviderConfigured() {
		t.Fatal("unvalidated key must not be saved before confirmation")
	}
	if !strings.Contains(m.transcriptText(), "key not stored") {
		t.Fatalf("nil-program test should decline unvalidated storage:\n%s", m.transcriptText())
	}

	m.applyAuthResult(authResultMsg{
		name:        "generic-openai",
		profile:     resolved,
		key:         "sk-unvalidated",
		unvalidated: true,
		confirmed:   true,
	})
	if got := m.cfg.Providers["generic-openai"].APIKey; got != "sk-unvalidated" {
		t.Fatalf("confirmed unvalidated key not saved: %q", got)
	}
}

func TestAuthResultRekeysLiveSession(t *testing.T) {
	m := authTestModel(t)
	resolved := authResolved(t, m, "generic-openai")
	if err := m.cfg.UpsertProviderKey("generic-openai", resolved, "sk-generic-old", false); err != nil {
		t.Fatal(err)
	}
	m.cfg.Models["gpt-test"] = config.Model{Providers: []string{"generic-openai"}, ID: "gpt-test", Context: 128000}
	m.modelName, m.provName = "gpt-test", "generic-openai"
	ag, _, _, err := agent.NewConfigured(agent.BuildOptions{
		Config: m.cfg, Profiles: m.profiles, Model: m.modelName, Provider: m.provName,
		Role: config.RoleDefault, SystemPrompt: "sys",
	})
	if err != nil {
		t.Fatal(err)
	}
	m.agent = ag
	m.agent.Messages = append(m.agent.Messages, models.Message{Role: "user", Content: "hi", Authored: true})

	m.applyAuthResult(authResultMsg{
		name:      "generic-openai",
		profile:   resolved,
		key:       "sk-generic-new",
		validated: true,
	})

	if got := m.cfg.Providers["generic-openai"].Key(); got != "sk-generic-new" {
		t.Errorf("live provider should be rebuilt with the new key, got %q", got)
	}
	if len(m.agent.Messages) != 2 || m.agent.Messages[1].Content != "hi" {
		t.Error("history must survive the hot-swap")
	}
}

func TestBuildAgentWithProfilesOptionalDegradesOnlyForMissingCredentials(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("GHG_HOME", t.TempDir())
	t.Setenv("INFERENCE_API_KEY", "")
	profiles, err := models.Load(models.LoadOptions{UserDir: filepath.Join(t.TempDir(), "providers")})
	if err != nil {
		t.Fatal(err)
	}

	cfg := config.Default()
	ag, modelName, providerName, err := agent.NewConfigured(agent.BuildOptions{
		Config: cfg, Profiles: profiles, SystemPrompt: "sys", AllowMissingCredentials: true,
	})
	if err != nil {
		t.Fatalf("missing key should be a degraded start: %v", err)
	}
	if ag != nil || modelName != cfg.DefaultModel || providerName != "inference" {
		t.Fatalf("unexpected degraded route: agent=%v model=%q provider=%q", ag, modelName, providerName)
	}

	if _, _, _, err := agent.NewConfigured(agent.BuildOptions{
		Config: cfg, Profiles: profiles, Model: "does-not-exist", SystemPrompt: "sys", AllowMissingCredentials: true,
	}); err == nil {
		t.Fatal("an explicit unknown model must remain a hard error")
	}

	const missingEnv = "GHG_TEST_MISSING_PROVIDER_KEY"
	t.Setenv(missingEnv, "")
	broken := &config.Config{
		DefaultModel: "m",
		Providers: map[string]config.Provider{
			"p": {Profile: "generic-openai", BaseURL: "https://example.test/v1", API: string(models.ProtocolOpenAIChatCompletions), APIKey: "$" + missingEnv},
		},
		Models: map[string]config.Model{
			"m": {Providers: []string{"p"}, ID: "m"},
		},
	}
	if _, _, _, err := agent.NewConfigured(agent.BuildOptions{
		Config: broken, Profiles: profiles, SystemPrompt: "sys", AllowMissingCredentials: true,
	}); err == nil || !strings.Contains(err.Error(), missingEnv) {
		t.Fatalf("broken secret reference must remain a hard error naming the reference: %v", err)
	}
}

func TestColdTUICommandsRemainSafeAndActionable(t *testing.T) {
	m := authTestModel(t)
	m.agent = nil
	m.modelName = m.cfg.DefaultModel
	m.provName = "inference"
	m.width, m.height = 80, 24

	note := m.degradedProviderNote()
	if note != "No provider has been configured — run /auth" {
		t.Fatalf("degraded note should be short and actionable: %q", note)
	}
	m.startupReport()
	if !strings.Contains(m.transcriptText(), note) {
		t.Fatalf("startup should report the degraded path:\n%s", m.transcriptText())
	}

	for _, command := range []string{
		"/help",
		"/auth",
		"/model",
		"/context-doctor",
		"/goal",
		"/goal draft",
		"/goal clear",
		"/goal-from-context",
		"/effort low",
		"/compact",
		"/compact retry",
		"/compact log",
		"/tasks",
		"/tasks missing",
		"/fork",
		"/resume missing",
		"/clear",
	} {
		m.command(command)
		if m.busy {
			t.Fatalf("cold command %q unexpectedly started work", command)
		}
	}

	m.openPalette()
	if got := m.View(); got == "" {
		t.Fatal("cold settings should render")
	}
	m.settings = nil
	m.openPaletteOn("reasoning effort")
	if got := m.View(); got == "" {
		t.Fatal("cold reasoning panel should render")
	}
	m.paletteKey(tea.KeyMsg{Type: tea.KeyEnter})

	m.submit("a turn without credentials")
	if m.busy {
		t.Fatal("cold submission must not set busy")
	}
	if !strings.Contains(m.transcriptText(), "No provider has been configured — run /auth") {
		t.Fatalf("cold submission should repeat the onboarding note:\n%s", m.transcriptText())
	}
	if _, cmd := m.Update(tea.WindowSizeMsg{Width: 80, Height: 24}); cmd != nil {
		// Window sizing is synchronous for a headless model; a non-nil command
		// would still be harmless, but the assertion keeps this path explicit.
		_ = cmd
	}
	if got := m.View(); got == "" {
		t.Fatal("cold TUI should remain renderable")
	}
}

func TestAuthResultPromotesColdTUIToFirstAgent(t *testing.T) {
	m := authTestModel(t)
	m.agent = nil
	m.modelName = m.cfg.DefaultModel
	m.provName = "inference"
	resolved := authResolved(t, m, "generic-openai")

	m.applyAuthResult(authResultMsg{
		name:      "generic-openai",
		profile:   resolved,
		key:       "sk-first-agent",
		validated: true,
	})
	if m.agent == nil {
		t.Fatal("successful auth should create the first live agent")
	}
	if m.provName != "generic-openai" || m.modelName != m.cfg.DefaultModel {
		t.Fatalf("first agent should use the authenticated route: %q @ %q", m.modelName, m.provName)
	}
	if m.cfg.DefaultProvider != "generic-openai" {
		t.Fatalf("first auth should make the selected provider the next-start route, got %q", m.cfg.DefaultProvider)
	}
}

// Keys must never enter input history (idle or busy), never queue as a chat
// message while the agent is busy, and the masked prompt's esc-esc draft
// stash must not record them either.
func TestAuthKeyNeverLeaksIntoHistoryOrQueue(t *testing.T) {
	m := authTestModel(t)

	m.input.SetValue("/auth generic-openai sk-generic-idle")
	tm, _ := m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	m = tm.(*model)
	for _, h := range m.hist {
		if strings.Contains(h, "sk-generic-idle") {
			t.Fatalf("idle submit recorded a key in history: %v", m.hist)
		}
	}

	m.busy = true
	m.input.SetValue("/auth generic-openai sk-generic-busy")
	tm, _ = m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	m = tm.(*model)
	m.busy = false
	for _, q := range m.queue {
		if strings.Contains(q, "sk-generic-busy") {
			t.Fatalf("busy submit queued a key as a chat message: %v", m.queue)
		}
	}
	for _, h := range m.hist {
		if strings.Contains(h, "sk-generic-busy") {
			t.Fatalf("busy submit recorded a key in history: %v", m.hist)
		}
	}
	if !busyCmd("/auth generic-openai sk-generic-x") {
		t.Error("/auth must be a busyCmd — queueing an inline key sends it to the model")
	}

	m.authCommand([]string{"anthropic"})
	m.input.SetValue("sk-anthropic-masked")
	tm, _ = m.Update(tea.KeyMsg{Type: tea.KeyEsc})
	m = tm.(*model)
	if m.namePrompt != nil {
		t.Fatal("esc should close the masked prompt")
	}
	for _, h := range m.hist {
		if strings.Contains(h, "sk-anthropic-masked") {
			t.Fatalf("masked prompt cancel recorded a key in history: %v", m.hist)
		}
	}
	if m.escClr {
		t.Error("esc-esc clear must not arm after a masked prompt")
	}
}

func TestAuthCompletionUsesProfileIDs(t *testing.T) {
	m := authTestModel(t)
	_, cands := completions("/auth an", nil, nil, m.authProviderCands(), nil, nil)
	if len(cands) != 1 || cands[0].Text != "anthropic" {
		t.Fatalf("auth completion should use profile IDs: %+v", cands)
	}
}

// The masked render shows the bullet mask with the textarea's prompt, never
// the key.
func TestAuthMaskedRender(t *testing.T) {
	m := authTestModel(t)
	m.agent = &agent.Agent{}
	m.authCommand([]string{"anthropic"})
	m.input.SetValue("sk-anthropic-hidden")
	out := m.View()
	if strings.Contains(out, "sk-anthropic-hidden") {
		t.Fatalf("masked prompt leaked the key into the view")
	}
	if !strings.Contains(out, "┃ "+strings.Repeat("•", len("sk-anthropic-hidden"))) {
		t.Errorf("masked prompt should render bullets after the prompt:\n%s", out)
	}
}

// ModelInfoLites carries context/output caps, reasoning efforts, vision
// modalities, and pricing from the provider's /models into the cache shape.
func TestCatalogLites(t *testing.T) {
	lites := config.ModelInfoLites([]models.ModelInfo{
		{ID: "gpt-test", ContextLength: 400000, MaxCompletionTokens: 128000,
			ReasoningEfforts: []string{"low", "high"}, InputModalities: []string{"text", "image"},
			Pricing: &models.Pricing{Prompt: "0.00000125", Completion: "0.00001", InputCacheRead: "0.000000125"}},
		{ID: "text-only", ContextLength: 131072},
	})
	if len(lites) != 2 {
		t.Fatalf("want 2 lites, got %d", len(lites))
	}
	a := lites[0]
	if a.ContextLength != 400000 || a.MaxCompletionTokens != 128000 {
		t.Errorf("caps not carried: %+v", a)
	}
	if len(a.ReasoningEfforts) != 2 || len(a.InputModalities) != 2 {
		t.Errorf("efforts/modalities not carried: %+v", a)
	}
	if a.InPrice == 0 || a.OutPrice == 0 || a.CacheReadPrice == 0 {
		t.Errorf("pricing not parsed: %+v", a)
	}
	if b := lites[1]; b.InPrice != 0 || b.OutPrice != 0 || len(b.InputModalities) != 0 {
		t.Errorf("pricing-less model should stay zero-rated: %+v", b)
	}
}

func TestTUIBackendConstructionOAuth(t *testing.T) {
	m := authTestModel(t)
	resolved := authResolved(t, m, "codex-subscription")

	backend, err := auth.NewBackend(resolved, "", "", 0)
	if err != nil {
		t.Fatalf("newBackend error: %v", err)
	}
	responsesBackend, ok := backend.(*models.OpenAIResponsesClient)
	if !ok {
		t.Fatalf("expected *models.OpenAIResponsesClient, got %T", backend)
	}
	if responsesBackend == nil {
		t.Fatal("expected non-nil OpenAIResponsesClient")
	}
	if responsesBackend.Authorizer == nil {
		t.Error("expected Authorizer to be installed on OAuth backend")
	}
}

func TestApplyOAuthResultReportsDiscoveryFailure(t *testing.T) {
	m := authTestModel(t)
	resolved := authResolved(t, m, "codex-subscription")

	m.applyOAuthResult(authOAuthResultMsg{
		name:     "codex-subscription",
		profile:  resolved,
		modelErr: errors.New("connection refused"),
	})

	transcript := m.transcriptText()
	if !strings.Contains(transcript, "configured.") {
		t.Errorf("expected configured message, got: %s", transcript)
	}
	if !strings.Contains(transcript, "model discovery failed: connection refused") {
		t.Errorf("expected discovery failure message, got: %s", transcript)
	}
	if !strings.Contains(transcript, "/model refresh") {
		t.Errorf("expected /model refresh hint, got: %s", transcript)
	}
}

func compactCmdModel() *model {
	// NOTE: any test that drives setEffort/switchModel/compactCommand writes
	// through cfg.Save(); TestMain points GHG_HOME at a scratch dir so
	// those writes can never reach the real ~/.ghg/config.json.
	// serve the compaction summary so a bare /compact completes in-test
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"choices":[{"message":{"content":"sim"}}]}`))
	}))
	m := &model{
		input:   newInput(),
		mouseOn: true, // matches the Run() default (wheel scroll + app selection)
		agent:   agent.New(testBackend(srv.URL, "k"), "kimi-k3-fast", 100, "sys"),
		cfg: &config.Config{
			DefaultModel: "kimi-k3-fast",
			Providers:    map[string]config.Provider{"inference": {BaseURL: "https://x", APIKey: "k"}},
			Models: map[string]config.Model{
				"kimi-k3-fast": {Providers: []string{"inference"}},
				"glm-5.2-fast": {Providers: []string{"inference"}},
				// the built-in compaction default, routable on inference
				config.DefaultCompactModel: {Providers: []string{"inference"}},
			},
		},
		modelName: "kimi-k3-fast",
		provName:  "inference",
		catalogs: map[string]config.Catalog{
			"inference": {Models: []config.ModelInfoLite{
				{ID: "kimi-k3-fast", ContextLength: 131072},
			}},
		},
	}
	m.width = 80
	m.input.SetWidth(78)
	return m
}

// Regression guard for the config corruption bug: running a persistence
// command from a test must write under the isolated GHG_HOME, never the
// user's real ~/.ghg.
func TestCompactCommandNeverTouchesRealHome(t *testing.T) {
	m := compactCmdModel()
	m.compactCommand([]string{"glm-5.2-fast"}) // triggers cfg.Save()
	dir := os.Getenv("GHG_HOME")
	if dir == "" || dir == filepath.Join(os.Getenv("HOME"), ".ghg") {
		t.Fatalf("tests must run with an isolated GHG_HOME, got %q", dir)
	}
	if _, err := os.Stat(filepath.Join(dir, "config.json")); err != nil {
		t.Fatalf("expected the save to land under GHG_HOME: %v", err)
	}
}

func TestCompactCommandSelectsModel(t *testing.T) {
	m := compactCmdModel()
	blocks := len(m.blocks)
	m.compactCommand([]string{"glm-5.2-fast"})
	if m.compactModel != "glm-5.2-fast" || m.compactProv != "" {
		t.Fatalf("compact model state: %q @ %q", m.compactModel, m.compactProv)
	}
	if m.agent.CompactModel != "glm-5.2-fast" || m.agent.CompactBackend == nil {
		t.Fatalf("agent should summarize with glm-5.2-fast on its own client")
	}
	if m.cfg.CompactModel != "glm-5.2-fast" {
		t.Fatalf("config should persist the pick, got %q", m.cfg.CompactModel)
	}
	m.compactCommand([]string{"off"})
	if m.compactModel != "" || m.agent.CompactModel != config.DefaultCompactModel || m.agent.CompactBackend == nil {
		t.Fatalf("off should restore the default compaction model: %q", m.compactModel)
	}
	if len(m.blocks) != blocks {
		t.Fatalf("successful compaction model changes should not append routine notes, got %v", m.blocks)
	}
}

// An empty compactModel resolves the built-in default from the config at
// apply time, so users who never picked one compact on deepseek-v4-flash.
func TestCompactModelEmptyResolvesDefault(t *testing.T) {
	m := compactCmdModel()
	m.applyCompactModel()
	if m.agent.CompactModel != config.DefaultCompactModel || m.agent.CompactBackend == nil {
		t.Fatalf("empty compactModel should resolve the default, got %q", m.agent.CompactModel)
	}
}

func TestCompactModelUsesTinyRole(t *testing.T) {
	m := compactCmdModel()
	m.cfg.Roles = map[string]config.RoleConfig{
		config.RoleTiny: {Model: "glm-5.2-fast", Provider: "inference"},
	}
	m.applyCompactModel()
	if m.agent.CompactModel != "glm-5.2-fast" || m.agent.CompactBackend == nil {
		t.Fatalf("tiny role should provide the compaction route, got %q / %T", m.agent.CompactModel, m.agent.CompactBackend)
	}
}

// When the default model isn't in the user's config, the override clears and
// compaction falls back to the conversation's own model — no error note.
func TestCompactModelDefaultFallsBack(t *testing.T) {
	m := compactCmdModel()
	delete(m.cfg.Models, config.DefaultCompactModel)
	blocks := len(m.blocks)
	m.applyCompactModel()
	if m.agent.CompactBackend != nil || m.agent.CompactModel != "" {
		t.Fatal("unresolvable default should fall back to the current model")
	}
	if len(m.blocks) != blocks {
		t.Fatal("a missing default should not nag — only picked models earn an error note")
	}
}

func TestCompactCommandRejectsUnknownModel(t *testing.T) {
	m := compactCmdModel()
	m.compactCommand([]string{"nope"})
	if m.compactModel != "" || m.agent.CompactModel != "" {
		t.Fatal("unknown model must not become the compaction model")
	}
	if !strings.Contains(m.blocks[len(m.blocks)-1].text, "unknown model") {
		t.Fatalf("expected an error note, got %v", m.blocks)
	}
}

func TestContextLimitFromCatalog(t *testing.T) {
	m := compactCmdModel()
	if got := m.contextLimitFor("inference", "kimi-k3-fast"); got != 131072 {
		t.Fatalf("contextLimitFor: %d", got)
	}
	if got := m.contextLimitFor("inference", "unknown"); got != 0 {
		t.Fatalf("unknown model: %d", got)
	}
	// a fresh /models fetch re-resolves the agent's limit
	cats := map[string]config.Catalog{
		"inference": {Models: []config.ModelInfoLite{{ID: "kimi-k3-fast", ContextLength: 262144}}},
	}
	m.updateCatalogs(cats)
	if m.agent.ContextLimit != 262144 {
		t.Fatalf("agent limit should follow the catalog, got %d", m.agent.ContextLimit)
	}
}

func TestContextLimitFallsBackToModelsDev(t *testing.T) {
	t.Setenv("GHG_HOME", t.TempDir())
	if err := config.SaveModelsDev(config.ModelsDevCache{
		FetchedAt: time.Now(),
		Providers: map[string]map[string]int{"opencode": {"grok-4": 131072}},
	}); err != nil {
		t.Fatal(err)
	}
	profiles, err := models.Load(models.LoadOptions{UserDir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	m := &model{
		cfg: &config.Config{
			Providers: map[string]config.Provider{"opencode": {Profile: "opencode"}},
		},
		profiles:  profiles,
		modelName: "grok-4",
		provName:  "opencode",
		catalogs: map[string]config.Catalog{
			"opencode": {Models: []config.ModelInfoLite{{ID: "grok-4"}}},
		},
	}
	if got := m.contextLimitFor("opencode", "grok-4"); got != 131072 {
		t.Fatalf("models.dev context = %d, want 131072", got)
	}

	// Provider metadata remains authoritative when it is present.
	m.catalogs["opencode"] = config.Catalog{Models: []config.ModelInfoLite{{ID: "grok-4", ContextLength: 262144}}}
	if got := m.contextLimitFor("opencode", "grok-4"); got != 262144 {
		t.Fatalf("provider context = %d, want 262144", got)
	}
}

// Bare /compact with no history reports there's nothing to fold rather than
// touching the compaction-model selection. (The busy path is exercised
// end-to-end in the running TUI; here m.prog is nil so we stay on the
// synchronous error branch.)
func TestCompactBareKeepsSelection(t *testing.T) {
	m := compactCmdModel()
	m.compactModel, m.compactProv = "glm-5.2-fast", ""
	m.applyCompactModel()
	m.busy = true // busy path: synchronous, never starts the goroutine
	m.command("/compact")
	if m.compactModel != "glm-5.2-fast" || m.agent.CompactModel != "glm-5.2-fast" {
		t.Fatal("bare /compact must not change the compaction-model selection")
	}
}

func TestCompactThresholdFor(t *testing.T) {
	cases := []struct {
		pct  int
		want float64
	}{
		{0, 0.4},   // unset → built-in default
		{70, 0.7},  // user preference
		{5, 0.1},   // clamped to the floor
		{99, 0.9},  // clamped to the ceiling
		{-30, 0.1}, // garbage clamps too
	}
	for _, tc := range cases {
		cfg := &config.Config{CompactPct: tc.pct}
		if got := config.CompactThreshold(cfg); got != tc.want {
			t.Errorf("CompactThreshold(%d) = %v, want %v", tc.pct, got, tc.want)
		}
	}
}

func TestSetCompactPct(t *testing.T) {
	m := compactCmdModel()
	m.agent.CompactThreshold = 0.5

	m.setCompactPct(60)
	if m.agent.CompactThreshold != 0.6 || m.cfg.CompactPct != 60 || m.compactPct() != 60 {
		t.Fatalf("setCompactPct(60): agent=%v cfg=%d", m.agent.CompactThreshold, m.cfg.CompactPct)
	}

	m.setCompactPct(120) // clamps to the 90 ceiling
	if m.agent.CompactThreshold != 0.9 || m.cfg.CompactPct != 90 {
		t.Fatalf("setCompactPct(120) should clamp to 90: agent=%v cfg=%d", m.agent.CompactThreshold, m.cfg.CompactPct)
	}
	m.setCompactPct(0) // clamps to the 10 floor
	if m.agent.CompactThreshold != 0.1 || m.cfg.CompactPct != 10 {
		t.Fatalf("setCompactPct(0) should clamp to 10: agent=%v cfg=%d", m.agent.CompactThreshold, m.cfg.CompactPct)
	}
}

func TestEffortCycleAndParse(t *testing.T) {
	got := ""
	for _, want := range []string{"low", "medium", "high", "", "low"} {
		got = nextEffort(defaultEfforts, got)
		if got != want {
			t.Fatalf("cycle: got %q want %q", got, want)
		}
	}
	if nextEffort(defaultEfforts, "bogus") != "" {
		t.Fatal("unknown level should reset to off")
	}
	if effortLabel("") != "off" || effortLabel("high") != "high" {
		t.Fatal("labels")
	}
	for in, want := range map[string]string{"off": "", "low": "low", "high": "high"} {
		if lv, ok := parseEffort(defaultEfforts, in); !ok || lv != want {
			t.Fatalf("parse %q: %q %v", in, lv, ok)
		}
	}
	if _, ok := parseEffort(defaultEfforts, "ultra"); ok {
		t.Fatal("invalid level accepted")
	}
}

func TestEffortCompletion(t *testing.T) {
	_, cs := completions("/effort h", nil, nil, nil, nil, nil)
	if len(cs) != 1 || cs[0].Text != "high" {
		t.Fatalf("effort completion: %v", texts(cs))
	}
}

func TestEffortsForAdvertisedLevels(t *testing.T) {
	m := &model{
		provName: "inference",
		agent:    &agent.Agent{Model: "deepseek-v4-flash"},
		catalogs: map[string]config.Catalog{
			"inference": {Models: []config.ModelInfoLite{
				{ID: "deepseek-v4-flash", ReasoningEfforts: []string{"low", "high", "max"}, ReasoningToggle: true},
				{ID: "claude-opus-5", ReasoningEfforts: []string{"none", "minimal", "low", "medium", "high", "xhigh", "max"}},
				{ID: "gemini-3.5-flash"}, // no reasoning_efforts
				{ID: "toggle-only", ReasoningKnown: true, ReasoningToggle: true},
				{ID: "no-controls", ReasoningKnown: true},
			}},
		},
	}
	if got := m.effortsFor(); len(got) != 4 || got[0] != "" || got[3] != "max" {
		t.Fatalf("advertised levels: %v", got)
	}
	if next := nextEffort(m.effortsFor(), "high"); next != "max" {
		t.Fatalf("cycle should reach max: %q", next)
	}
	for _, e := range m.effortsFor() {
		if e == "on" {
			t.Fatal("a model with graded efforts should not add a duplicate on state")
		}
	}
	if _, ok := parseEffort(m.effortsFor(), "medium"); ok {
		t.Fatal("medium should be rejected for deepseek")
	}

	// "none" collapses into off ("")
	m.agent.Model = "claude-opus-5"
	got := m.effortsFor()
	if got[0] != "" || len(got) != 7 {
		t.Fatalf("claude levels: %v", got)
	}
	for _, e := range got {
		if e == "none" {
			t.Fatalf("none should map to off: %v", got)
		}
	}

	// no advertised levels → defaults
	m.agent.Model = "gemini-3.5-flash"
	if got := m.effortsFor(); len(got) != len(defaultEfforts) {
		t.Fatalf("gemini should fall back to defaults: %v", got)
	}

	m.agent.Model = "toggle-only"
	if got := m.effortsFor(); len(got) != 2 || got[0] != "" || got[1] != "on" {
		t.Fatalf("toggle-only model should expose off/on: %v", got)
	}
	m.agent.Model = "no-controls"
	if got := m.effortsFor(); len(got) != 1 || got[0] != "" {
		t.Fatalf("known model without controls should expose off only: %v", got)
	}

	// unknown provider → defaults
	m.provName = "elsewhere"
	if got := m.effortsFor(); len(got) != len(defaultEfforts) {
		t.Fatalf("missing catalog should fall back to defaults: %v", got)
	}
}

// bare /effort opens the level selector (settings panel) so the user can
// scroll ↑/↓ and pick — cycling blindly hides the choices.
func TestEffortBareOpensSelector(t *testing.T) {
	m := compactCmdModel()
	m.command("/effort")
	if m.settings == nil {
		t.Fatal("bare /effort should open the settings")
	}
	pp := m.settings.top()
	if pp == nil || pp.kind != panelEffort {
		t.Fatalf("expected the effort panel, got %+v", pp)
	}
	if len(pp.levels) != len(defaultEfforts) || pp.levels[pp.lidx] != m.agent.Effort {
		t.Fatalf("effort panel should list the model's levels on the current one: %v @%d", pp.levels, pp.lidx)
	}
	// scroll down to low and apply with enter
	tm, _ := m.paletteKey(tea.KeyMsg{Type: tea.KeyDown})
	m = tm.(*model)
	tm, _ = m.paletteKey(tea.KeyMsg{Type: tea.KeyEnter})
	m = tm.(*model)
	if m.agent.Effort != "low" {
		t.Fatalf("selecting low in the selector should apply it, got %q", m.agent.Effort)
	}
	// the selector came from /effort, not ctrl+p: commit-and-close, don't
	// strand the user on a settings root they never opened
	if m.settings != nil {
		t.Fatal("enter in a directly-opened selector should close the settings")
	}
}

// A user-picked effort is both the new global default (config.json) and the
// live session's restore value (sessions.db); a reconciliation reset touches
// only the session.
func TestSetEffortPersistsGlobalAndSession(t *testing.T) {
	m := compactCmdModel()
	m.cfg.DefaultEffort = "medium"
	m.agent.Effort = "medium"
	st, err := session.Open(filepath.Join(t.TempDir(), "s.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	m.store = st
	id, err := st.Create("/tmp", m.modelName, m.provName)
	if err != nil {
		t.Fatal(err)
	}
	m.sessionID = id

	m.setEffort("low") // the user picks a level
	if m.agent.Effort != "low" {
		t.Fatalf("agent effort: %q", m.agent.Effort)
	}
	reloaded, err := config.Load()
	if err != nil {
		t.Fatal(err)
	}
	if reloaded.DefaultEffort != "low" {
		t.Fatalf("global default should follow the pick, got %q", reloaded.DefaultEffort)
	}
	meta, _, err := st.Load(id)
	if err != nil {
		t.Fatal(err)
	}
	if meta.Effort != "low" {
		t.Fatalf("session row should carry the pick, got %q", meta.Effort)
	}

	// a reconciliation (catalog refresh drops the level) must not rewrite the
	// user's global default, only the live session
	m.resetEffort("")
	reloaded, _ = config.Load()
	if reloaded.DefaultEffort != "low" {
		t.Fatalf("reset must not touch the global default, got %q", reloaded.DefaultEffort)
	}
	meta, _, _ = st.Load(id)
	if meta.Effort != "" {
		t.Fatalf("session row should track the reset, got %q", meta.Effort)
	}
}

// Resume restores the session's own effort; a row that pre-dates per-session
// effort ("") inherits the current default and is stamped on the next save.
func TestResumeRestoresEffort(t *testing.T) {
	m := compactCmdModel()
	m.cfg.DefaultEffort = "medium"
	m.agent.Effort = "medium"
	st, err := session.Open(filepath.Join(t.TempDir(), "s.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	m.store = st

	id, err := st.Create("/tmp", m.modelName, m.provName)
	if err != nil {
		t.Fatal(err)
	}
	msgs := []models.Message{{Role: "system", Content: "sys"}, {Role: "user", Content: "q", Authored: true}}
	if err := st.Save(id, 1, msgs, m.modelName, m.provName); err != nil {
		t.Fatal(err)
	}

	// session chose high; the global default drifting to low must not matter
	if err := st.SetEffort(id, "high"); err != nil {
		t.Fatal(err)
	}
	m.agent.Effort = "low"
	if err := m.resume(id); err != nil {
		t.Fatal(err)
	}
	if m.agent.Effort != "high" {
		t.Fatalf("resume should restore the session effort, got %q", m.agent.Effort)
	}

	// a legacy row (no effort) inherits the current default…
	id2, err := st.Create("/tmp", m.modelName, m.provName)
	if err != nil {
		t.Fatal(err)
	}
	if err := st.Save(id2, 1, msgs, m.modelName, m.provName); err != nil {
		t.Fatal(err)
	}
	m.agent.Effort = "low"
	if err := m.resume(id2); err != nil {
		t.Fatal(err)
	}
	if m.agent.Effort != "low" {
		t.Fatalf("legacy row should inherit the current default, got %q", m.agent.Effort)
	}
	// …and the next persist stamps it so a later default change can't leak in
	m.persist()
	meta, _, _ := st.Load(id2)
	if meta.Effort != "low" {
		t.Fatalf("persist should stamp the inherited effort, got %q", meta.Effort)
	}
}

// Usage totals persist with the session: resume restores them (so the status
// line shows the real spend, not 0/0) and the next save keeps them. Legacy
// rows read zero and get stamped on the first save after resume.
func TestResumeRestoresUsage(t *testing.T) {
	m := compactCmdModel()
	st, err := session.Open(filepath.Join(t.TempDir(), "s.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	m.store = st

	id, err := st.Create("/tmp", m.modelName, m.provName)
	if err != nil {
		t.Fatal(err)
	}
	msgs := []models.Message{{Role: "system", Content: "sys"}, {Role: "user", Content: "q", Authored: true}}
	if err := st.Save(id, 1, msgs, m.modelName, m.provName); err != nil {
		t.Fatal(err)
	}
	if err := st.SetUsage(id, 12000, 8000, 1500); err != nil {
		t.Fatal(err)
	}

	if err := m.resume(id); err != nil {
		t.Fatal(err)
	}
	u := m.agent.Usage()
	if u.PromptTokens != 12000 || u.Cached() != 8000 || u.CompletionTokens != 1500 {
		t.Fatalf("resume should restore usage, got in=%d cached=%d out=%d", u.PromptTokens, u.Cached(), u.CompletionTokens)
	}

	// new spend accumulates on top of the restored totals…
	m.agent.AddUsage(models.Usage{PromptTokens: 3000, CompletionTokens: 500})
	// …and persists: the stored row is absolute, so a compaction (now a
	// recorded event, no rewrite) can't zero it
	m.persist()
	meta, _, _ := st.Load(id)
	if meta.UsageIn != 15000 || meta.UsageOut != 2000 {
		t.Fatalf("persist should store cumulative totals, got in=%d out=%d", meta.UsageIn, meta.UsageOut)
	}

	// legacy row (no usage columns stamped): totals are reconstructed from the
	// per-message usage stored on assistant messages, then stamped on the next
	// persist so reconstruction happens once
	id2, err := st.Create("/tmp", m.modelName, m.provName)
	if err != nil {
		t.Fatal(err)
	}
	cu := models.Usage{PromptTokens: 1000, CompletionTokens: 200,
		PromptTokensDetails: &struct {
			CachedTokens int `json:"cached_tokens"`
		}{CachedTokens: 600}}
	legacy := []models.Message{
		{Role: "system", Content: "sys"},
		{Role: "user", Content: "q1", Authored: true},
		{Role: "assistant", Content: "a1", Usage: &cu},
		{Role: "user", Content: "q2", Authored: true},
		{Role: "assistant", Content: "a2", Usage: &models.Usage{PromptTokens: 500, CompletionTokens: 100}},
	}
	if err := st.Save(id2, 1, legacy, m.modelName, m.provName); err != nil {
		t.Fatal(err)
	}
	if err := m.resume(id2); err != nil {
		t.Fatal(err)
	}
	u2 := m.agent.Usage()
	if u2.PromptTokens != 1500 || u2.Cached() != 600 || u2.CompletionTokens != 300 {
		t.Fatalf("legacy row should reconstruct usage from messages, got in=%d cached=%d out=%d",
			u2.PromptTokens, u2.Cached(), u2.CompletionTokens)
	}
	m.persist()
	meta2, _, _ := st.Load(id2)
	if meta2.UsageIn != 1500 || meta2.UsageCached != 600 || meta2.UsageOut != 300 {
		t.Fatalf("persist should stamp the reconstructed totals, got %+v", meta2)
	}

	// a session with no usage anywhere (pre-usage tracking) stays zero
	id3, err := st.Create("/tmp", m.modelName, m.provName)
	if err != nil {
		t.Fatal(err)
	}
	if err := st.Save(id3, 1, msgs, m.modelName, m.provName); err != nil {
		t.Fatal(err)
	}
	if err := m.resume(id3); err != nil {
		t.Fatal(err)
	}
	if u := m.agent.Usage(); u.PromptTokens != 0 || u.CompletionTokens != 0 {
		t.Fatalf("usage-free session should start at zero, got %+v", u)
	}
}

func TestUpdateCatalogsResetsUnsupportedEffort(t *testing.T) {
	t.Setenv("HOME", t.TempDir()) // keep setEffort's cfg.Save() away from the real config
	m := &model{
		cfg:      &config.Config{},
		provName: "inference",
		agent:    &agent.Agent{Model: "deepseek-v4-flash", Effort: "medium"},
	}
	m.updateCatalogs(map[string]config.Catalog{
		"inference": {Models: []config.ModelInfoLite{
			{ID: "deepseek-v4-flash", ReasoningEfforts: []string{"low", "high", "max"}},
		}},
	})
	if m.agent.Effort != "" {
		t.Fatalf("unsupported effort should reset to off, got %q", m.agent.Effort)
	}

	// a supported effort survives the refresh
	m.agent.Effort = "high"
	m.updateCatalogs(map[string]config.Catalog{
		"inference": {Models: []config.ModelInfoLite{
			{ID: "deepseek-v4-flash", ReasoningEfforts: []string{"low", "high", "max"}},
		}},
	})
	if m.agent.Effort != "high" {
		t.Fatalf("supported effort should survive, got %q", m.agent.Effort)
	}
}

func TestModelSwitchDefaultsToMaxEffort(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	m := compactCmdModel()
	m.catalogs = map[string]config.Catalog{
		"inference": {Models: []config.ModelInfoLite{
			{ID: "deepseek-v4-flash", ReasoningEfforts: []string{"low", "high", "max"}},
			{ID: "toggle-only", ReasoningKnown: true, ReasoningToggle: true},
			{ID: "no-controls", ReasoningKnown: true},
		}},
	}
	m.cfg.Models["deepseek-v4-flash"] = config.Model{Providers: []string{"inference"}}
	m.cfg.Models["toggle-only"] = config.Model{Providers: []string{"inference"}}
	m.cfg.Models["no-controls"] = config.Model{Providers: []string{"inference"}}

	m.switchModel("deepseek-v4-flash", "inference")
	if m.agent.Effort != "max" {
		t.Fatalf("expected max effort for deepseek-v4-flash, got %q", m.agent.Effort)
	}

	m.switchModel("toggle-only", "inference")
	if m.agent.Effort != "on" {
		t.Fatalf("expected on effort for toggle-only, got %q", m.agent.Effort)
	}

	m.switchModel("no-controls", "inference")
	if m.agent.Effort != "" {
		t.Fatalf("expected empty effort for no-controls, got %q", m.agent.Effort)
	}
}

// A prose claim never completes an active goal. Completion must arrive as a
// validated update_goal result carrying a verification note.
func TestGoalLoopRequiresStructuredCompletion(t *testing.T) {
	m := &model{goal: "ship the thing", goalRounds: 0}
	m.goalTurnFinished(turnDoneMsg{final: "Verified: all checks pass."}, false)
	record, ok := m.currentGoalRecord()
	if !ok || record.Status != agent.GoalStatusActive {
		t.Fatalf("prose must not complete the goal: %+v", record)
	}
	if record.Rounds != 1 {
		t.Fatalf("rounds = %d, want 1", record.Rounds)
	}
}

func TestGoalLoopEndsOnStructuredCompletion(t *testing.T) {
	m := &model{goal: "ship the thing", goalRounds: 1}
	record, _ := m.currentGoalRecord()
	updates := []agent.GoalUpdate{{
		GoalID:   record.ID,
		Status:   agent.GoalStatusComplete,
		Progress: "tests and the release check passed",
	}}
	if m.goalTurnFinished(turnDoneMsg{final: "done", goalUpdates: updates}, false) {
		t.Fatal("complete goal must not submit a continuation")
	}
	got, ok := m.currentGoalRecord()
	if !ok || got.Status != agent.GoalStatusComplete {
		t.Fatalf("goal status = %+v, want complete", got)
	}
	if m.goal != "" {
		t.Fatalf("completed goal must leave the active compatibility mirror empty: %q", m.goal)
	}
}

func TestResumePausesActiveGoalUntilExplicitResume(t *testing.T) {
	m := compactCmdModel()
	st, err := session.Open(filepath.Join(t.TempDir(), "s.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	m.store = st

	id, err := st.Create("/tmp", "m", "p")
	if err != nil {
		t.Fatal(err)
	}
	record := agent.NewGoal("finish the migration")
	record.ID = "goal-resume"
	if err := st.CheckpointGoal(id, record); err != nil {
		t.Fatal(err)
	}
	if err := m.resume(id); err != nil {
		t.Fatal(err)
	}
	got, ok, err := st.LoadGoal(id)
	if err != nil || !ok || got.Status != agent.GoalStatusPaused {
		t.Fatalf("resume should pause active work: %+v %v %v", got, ok, err)
	}
	if m.goal != "" {
		t.Fatalf("paused goal must not remain active in the compatibility mirror: %q", m.goal)
	}
	if !m.resumeGoal() {
		t.Fatal("explicit resume should reactivate a paused goal")
	}
	got, ok, err = st.LoadGoal(id)
	if err != nil || !ok || got.Status != agent.GoalStatusActive {
		t.Fatalf("explicit resume status: %+v %v %v", got, ok, err)
	}
}

func TestGoalHelpers(t *testing.T) {
	p := goal.ContinuePrompt("ship the feature")
	if !strings.Contains(p, "ship the feature") || !strings.Contains(p, "update_goal") {
		t.Fatalf("prompt: %q", p)
	}
	record := agent.NewGoal("ship the feature")
	if err := (agent.GoalUpdate{GoalID: record.ID, Status: agent.GoalStatusComplete, Progress: "verified"}).Validate(record.ID); err != nil {
		t.Fatalf("complete update should validate: %v", err)
	}
	if err := (agent.GoalUpdate{GoalID: record.ID, Status: agent.GoalStatusComplete}).Validate(record.ID); err == nil {
		t.Fatal("completion without verification should fail")
	}
}

// lastBlock returns the last transcript block's text.
func lastBlock(m *model) string {
	if len(m.blocks) == 0 {
		return ""
	}
	return m.blocks[len(m.blocks)-1].text
}

func TestGoalMaxRoundsResolution(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	m := modelCmdModel()

	if n := m.goalMaxRounds(); n != config.DefaultGoalMaxRounds {
		t.Fatalf("default should be %d, got %d", config.DefaultGoalMaxRounds, n)
	}
	m.cfg.GoalMaxRounds = 250
	if n := m.goalMaxRounds(); n != 250 {
		t.Fatalf("global config should win, got %d", n)
	}
	// project override beats the global default
	wd, _ := os.Getwd()
	if err := config.SetProjectGoalMaxRounds(wd, 42); err != nil {
		t.Fatal(err)
	}
	if n := m.goalMaxRounds(); n != 42 {
		t.Fatalf("project override should win, got %d", n)
	}
	if err := config.SetProjectGoalMaxRounds(wd, 0); err != nil {
		t.Fatal(err)
	}
}

func TestGoalRoundsCommand(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	m := modelCmdModel()

	// bare reports the effective cap and source
	m.command("/goal rounds")
	if out := lastBlock(m); !strings.Contains(out, "100") || !strings.Contains(out, "built-in default") {
		t.Fatalf("bare report: %q", out)
	}
	// project override
	m.command("/goal rounds 42")
	if n := m.goalMaxRounds(); n != 42 {
		t.Fatalf("project override: %d", n)
	}
	if out := lastBlock(m); !strings.Contains(out, "this project") {
		t.Fatalf("project set message: %q", out)
	}
	// global default is set, but the project override still wins and says so
	m.command("/goal rounds 250 --global")
	if m.cfg.GoalMaxRounds != 250 {
		t.Fatalf("global not saved on cfg: %d", m.cfg.GoalMaxRounds)
	}
	if n := m.goalMaxRounds(); n != 42 {
		t.Fatalf("project should still win: %d", n)
	}
	if out := lastBlock(m); !strings.Contains(out, "overrides it with 42") {
		t.Fatalf("override note: %q", out)
	}
	// clearing the project override falls back to the global value
	m.command("/goal rounds default")
	if n := m.goalMaxRounds(); n != 250 {
		t.Fatalf("after clearing override should be 250, got %d", n)
	}
	// clearing the global falls back to the built-in
	m.command("/goal rounds default --global")
	if n := m.goalMaxRounds(); n != config.DefaultGoalMaxRounds {
		t.Fatalf("after clearing global should be %d, got %d", config.DefaultGoalMaxRounds, n)
	}
	// garbage is rejected without changing anything
	m.command("/goal rounds nope")
	if out := lastBlock(m); !strings.Contains(out, "positive number") {
		t.Fatalf("bad input: %q", out)
	}
}

// goalFromContextModel builds a headless model whose provider serves one
// canned chat-completion body (or status) to the goal-formulation call.
// m.prog stays nil, so the command's goroutine runs but never p.Sends; tests
// poll m.goal / m.busy directly.
func goalFromContextModel(t *testing.T, status int, body string) *model {
	t.Helper()
	return goalFromContextModelCapture(t, status, body, nil)
}

// goalFromContextModelCapture is goalFromContextModel plus a hook that
// receives the raw request body of every call the command makes.
func goalFromContextModelCapture(t *testing.T, status int, body string, capture func([]byte)) *model {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if capture != nil {
			b, _ := io.ReadAll(r.Body)
			capture(b)
		}
		w.WriteHeader(status)
		w.Write([]byte(body))
	}))
	t.Cleanup(srv.Close)
	m := compactCmdModel()
	m.agent = agent.New(testBackend(srv.URL, "k"), "kimi-k3-fast", 100, "sys")
	return m
}

func TestGoalFromContextPrompt(t *testing.T) {
	call := models.ToolCall{}
	call.Function.Name = "bash"
	call.Function.Arguments = `{"cmd":"go test ./..."}`
	tail := []models.Message{
		{Role: "user", Content: "make the tests green"},
		{Role: "assistant", Content: "I'll fix the flaky test and run go test.", ToolCalls: []models.ToolCall{call}},
	}
	p := agent.BuildGoalFromContextPrompt(tail)
	for _, want := range []string{"make the tests green", "flaky test", "assistant called bash(", "ONLY the goal"} {
		if !strings.Contains(p, want) {
			t.Fatalf("prompt missing %q:\n%s", want, p)
		}
	}

	// window selection: system excluded; n caps the tail, short history wins
	msgs := []models.Message{
		{Role: "system", Content: "sys"},
		{Role: "user", Content: "old"},
		{Role: "assistant", Content: "older"},
		{Role: "user", Content: "recent ask"},
		{Role: "assistant", Content: "recent reply"},
	}
	got, err := agent.GoalFromContextMessages(msgs, 2)
	if err != nil || len(got) != 2 || got[0].Content != "recent ask" || got[1].Content != "recent reply" {
		t.Fatalf("window: %v %v", got, err)
	}
	// n larger than the history clamps to everything after the system prompt
	got, err = agent.GoalFromContextMessages(msgs, 50)
	if err != nil || len(got) != 4 || got[0].Content != "old" {
		t.Fatalf("clamped window: %v %v", got, err)
	}
	// n <= 0 means the default window
	got, err = agent.GoalFromContextMessages(msgs, 0)
	if err != nil || len(got) != 4 {
		t.Fatalf("default window: %v %v", got, err)
	}
	if _, err := agent.GoalFromContextMessages(msgs[:2], 8); err == nil {
		t.Fatal("two conversation messages required")
	}
}

func TestGoalFromContextSetsGoal(t *testing.T) {
	m := goalFromContextModel(t, 200, `{"choices":[{"message":{"content":"fix the flaky test and verify with go test"}}]}`)
	m.agent.Messages = []models.Message{
		{Role: "system", Content: "sys"},
		{Role: "user", Content: "tests are flaky"},
		{Role: "assistant", Content: "I'll fix them."},
	}
	m.command("/goal-from-context") // headless: runs the formulation inline
	if m.goal != "fix the flaky test and verify with go test" {
		t.Fatalf("goal: %q", m.goal)
	}
	if m.busy {
		t.Fatal("busy must clear when the inline formulation returns")
	}
	// the transcript must say how many messages were distilled
	found := false
	for _, b := range m.blocks {
		if strings.Contains(b.text, "formulating goal from the last 2 messages") {
			found = true
		}
	}
	if !found {
		t.Fatalf("expected the message count in the note, blocks: %v", m.blocks)
	}
}

func TestGoalFromContextWindowArg(t *testing.T) {
	var req []byte
	m := goalFromContextModelCapture(t, 200,
		`{"choices":[{"message":{"content":"the goal"}}]}`, func(b []byte) { req = b })
	m.agent.Messages = []models.Message{
		{Role: "system", Content: "sys"},
		{Role: "user", Content: "ancient context"},
		{Role: "assistant", Content: "ancient reply"},
		{Role: "user", Content: "recent ask"},
		{Role: "assistant", Content: "recent reply"},
	}
	m.command("/goal-from-context 2")
	if m.goal != "the goal" {
		t.Fatalf("goal: %q", m.goal)
	}
	// the formulation prompt must contain only the last 2 messages
	body := string(req)
	if !strings.Contains(body, "recent ask") || strings.Contains(body, "ancient context") {
		t.Fatalf("window not honored in the request:\n%s", body)
	}
	// and the note reports the distilled window
	found := false
	for _, b := range m.blocks {
		if strings.Contains(b.text, "formulating goal from the last 2 messages") {
			found = true
		}
	}
	if !found {
		t.Fatalf("expected the message count in the note, blocks: %v", m.blocks)
	}
}

func TestGoalFromContextMaxTokens(t *testing.T) {
	var req struct {
		MaxTokens int `json:"max_tokens"`
	}
	m := goalFromContextModelCapture(t, 200,
		`{"choices":[{"message":{"content":"the goal"}}]}`,
		func(b []byte) { json.Unmarshal(b, &req) })
	m.agent.Messages = []models.Message{
		{Role: "system", Content: "sys"},
		{Role: "user", Content: "u"},
		{Role: "assistant", Content: "a"},
	}
	m.command("/goal-from-context")
	if req.MaxTokens != 8192 {
		t.Fatalf("the formulation call must allow detailed goals, max_tokens=%d", req.MaxTokens)
	}
}

func TestGoalFromContextBadCount(t *testing.T) {
	m := goalFromContextModel(t, 200, `{"choices":[{"message":{"content":"x"}}]}`)
	m.agent.Messages = []models.Message{
		{Role: "system", Content: "sys"},
		{Role: "user", Content: "u"},
		{Role: "assistant", Content: "a"},
	}
	for _, cmd := range []string{"/goal-from-context nope", "/goal-from-context 1"} {
		m.command(cmd)
		if m.busy {
			t.Fatalf("%s: no formulation call should start", cmd)
		}
		if out := lastBlock(m); !strings.Contains(out, "usage: /goal-from-context") {
			t.Fatalf("%s: expected a usage note, got %q", cmd, out)
		}
	}
}

func TestGoalFromContextErrorLeavesGoalUntouched(t *testing.T) {
	m := goalFromContextModel(t, 500, `{"error":"boom"}`)
	m.agent.Messages = []models.Message{
		{Role: "system", Content: "sys"},
		{Role: "user", Content: "tests are flaky"},
		{Role: "assistant", Content: "I'll fix them."},
	}
	m.command("/goal-from-context")
	if m.goal != "" {
		t.Fatalf("failed formulation must not set a goal, got %q", m.goal)
	}
	if m.busy {
		t.Fatal("busy must clear after a failed formulation")
	}
	if out := lastBlock(m); !strings.Contains(out, "goal-from-context failed") {
		t.Fatalf("expected a failure note, got %q", out)
	}
}

func TestGoalFromContextNeedsHistory(t *testing.T) {
	m := goalFromContextModel(t, 200, `{"choices":[{"message":{"content":"x"}}]}`)
	m.agent.Messages = []models.Message{{Role: "system", Content: "sys"}}
	m.command("/goal-from-context")
	if m.busy {
		t.Fatal("no formulation call should start without history")
	}
	if out := lastBlock(m); !strings.Contains(out, "not enough context") {
		t.Fatalf("expected a needs-history note, got %q", out)
	}
}

// The live-path message handler: on failure it clears busy/cancel itself and
// must NOT submit (no trailing turnDoneMsg, so a paused goal's loop cannot
// re-engage); on success it sets the goal and hands busy to the new turn.
func TestGoalFromContextMsgHandler(t *testing.T) {
	m := goalFromContextModel(t, 200, `{"choices":[{"message":{"content":"x"}}]}`)
	m.busy = true
	m.cancel = func() {}
	m.goal = "paused old goal"
	m.goalRounds = 20 // exhausted
	tm, cmd := m.Update(goalFromContextMsg{err: errors.New("boom")})
	m = tm.(*model)
	if cmd != nil {
		t.Fatal("a failed formulation must not submit anything")
	}
	if m.busy || m.cancel != nil {
		t.Fatal("the msg handler must clear busy/cancel on failure")
	}
	if m.goal != "paused old goal" {
		t.Fatalf("old goal must survive untouched, got %q", m.goal)
	}
	if out := lastBlock(m); !strings.Contains(out, "goal-from-context failed") {
		t.Fatalf("expected a failure note, got %q", out)
	}

	// esc-cancel reads as an interrupt note, not an error
	m.busy, m.cancel = true, func() {}
	tm, _ = m.Update(goalFromContextMsg{err: context.Canceled})
	m = tm.(*model)
	if m.busy || !strings.Contains(lastBlock(m), "(interrupted)") {
		t.Fatalf("cancelled formulation should interrupt cleanly: busy=%v last=%q", m.busy, lastBlock(m))
	}

	// success: goal trimmed, set, and submitted — busy stays owned by the
	// new turn. The submit's turn goroutine p.Sends on a nil prog, so it
	// must run to completion (or fail) without touching the assertions.
	m2 := goalFromContextModel(t, 200, `{"choices":[{"message":{"content":"x"}}]}`)
	m2.busy = true
	m2.cancel = func() {}
	m2.agent.Messages = []models.Message{{Role: "system", Content: "sys"}}
	tm2, cmd2 := m2.Update(goalFromContextMsg{goal: "  ship it  "})
	m2 = tm2.(*model)
	if cmd2 == nil {
		t.Fatal("a successful formulation must submit the goal (start the turn)")
	}
	if !m2.busy {
		t.Fatal("busy must stay set — it belongs to the submitted turn now")
	}
	if m2.goal != "ship it" {
		t.Fatalf("goal should be trimmed and set, got %q", m2.goal)
	}
	found := false
	for _, b := range m2.blocks {
		if strings.Contains(b.text, "◎ goal set: ship it") {
			found = true
		}
	}
	if !found {
		t.Fatal("expected a goal-set note in the transcript")
	}
}

func TestGoalFromContextBusyRefuses(t *testing.T) {
	m := goalFromContextModel(t, 200, `{"choices":[{"message":{"content":"x"}}]}`)
	m.agent.Messages = []models.Message{
		{Role: "system", Content: "sys"},
		{Role: "user", Content: "a"},
		{Role: "assistant", Content: "b"},
	}
	m.busy = true
	m.command("/goal-from-context")
	if out := lastBlock(m); !strings.Contains(out, "busy") {
		t.Fatalf("expected a busy note, got %q", out)
	}
	if m.goal != "" {
		t.Fatal("busy refusal must not touch the goal")
	}
}

// The feature end-to-end: remember writes a markdown bullet to
// ~/.ghg/memory.md, the next turn's system prompt injects it, /memory lists
// it, and deleting by number stops the injection.
func TestMemoryEndToEnd(t *testing.T) {
	home := t.TempDir()
	t.Setenv("GHG_HOME", home)

	m := compactCmdModel()

	// 1. the model remembers a fact (installation scope)
	callTool := func(name, args string) string {
		t.Helper()
		for _, tool := range m.agent.Tools {
			if tool.Def.Function.Name == name {
				out, err := tool.Run(t.Context(), []byte(args))
				if err != nil {
					return "Error: " + err.Error()
				}
				return out
			}
		}
		t.Fatal(name + " not registered")
		return ""
	}
	if out := callTool("remember", `{"text":"user prefers pnpm over npm"}`); strings.HasPrefix(out, "Error:") {
		t.Fatal(out)
	}

	// 2. it's a plain markdown bullet in ~/.ghg/memory.md
	data, err := os.ReadFile(filepath.Join(home, "memory.md"))
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != "- [ ] user prefers pnpm over npm\n" {
		t.Fatalf("memory.md:\n%s", data)
	}

	// 3. the next turn injects it into the system prompt
	m.prepareTurn("hello")
	sys := m.agent.Messages[0].Content
	if !strings.Contains(sys, "user prefers pnpm over npm") || !strings.Contains(sys, "<memory>") {
		t.Fatalf("memory not injected into the system prompt:\n%s", sys)
	}

	// 4. /memory lists it
	m.memoryCommand(nil)
	var listed bool
	for _, b := range m.blocks {
		r := ansi.Strip(b.render(m.width))
		if strings.Contains(r, "installation") && strings.Contains(r, "user prefers pnpm") {
			listed = true
		}
	}
	if !listed {
		t.Fatal("/memory should list the installation entry")
	}

	// 5. delete it by number without leaving the TUI; injection stops
	m.memoryCommand([]string{"1"})
	data, _ = os.ReadFile(filepath.Join(home, "memory.md"))
	if !strings.Contains(string(data), "- [x] user prefers pnpm") {
		t.Fatalf("entry should be struck, not deleted:\n%s", data)
	}
	m.prepareTurn("hello again")
	if strings.Contains(m.agent.Messages[0].Content, "pnpm") {
		t.Fatal("struck entries must stop being injected")
	}
}

// Session-scoped memory lives under sessions/<id>.memory.md and only appears
// while that session is active.
func TestSessionMemoryScope(t *testing.T) {
	home := t.TempDir()
	t.Setenv("GHG_HOME", home)

	m := compactCmdModel()
	m.agent.SetSessionID("sess1")
	if err := memory.Session("sess1").Remember("this repo uses ./scripts/ship.sh to deploy"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(home, "sessions", "sess1.memory.md")); err != nil {
		t.Fatal("session memory should live under sessions/")
	}
	m.sessionID = "sess1"
	m.prepareTurn("hi")
	if !strings.Contains(m.agent.Messages[0].Content, "ship.sh") {
		t.Fatal("session memory should inject while the session is active")
	}
}

func TestBottomStatusControlsCycleModelAndMode(t *testing.T) {
	m := compactCmdModel()
	m.width, m.height = 100, 30
	m.cfg.Roles = map[string]config.RoleConfig{
		config.RoleDefault: {Model: "default-model", Provider: "inference"},
		config.RoleSmart:   {Model: "smart-model", Provider: "inference"},
		config.RoleFast:    {Model: "fast-model", Provider: "inference"},
		config.RoleTiny:    {Model: "tiny-model", Provider: "inference"},
	}
	for _, name := range []string{"default-model", "smart-model", "fast-model", "tiny-model"} {
		m.cfg.Models[name] = config.Model{Providers: []string{"inference"}}
	}
	m.modelName = "tiny-model"
	m.agent.Role = config.RoleTiny

	clickModel := func() {
		t, _ := m.Update(tea.MouseMsg{
			Action: tea.MouseActionPress, Button: tea.MouseButtonLeft,
			X: m.statusModelX + m.statusModelW/2, Y: statusInfoRow(m.height),
		})
		m = t.(*model)
		_ = m.View()
	}
	_ = m.View()
	clickModel()
	if m.settings != nil || m.uiMode() != uiModeExecute || m.agent.Role != config.RoleSmart || m.modelName != "smart-model" {
		t.Fatalf("model click should select smart without changing execute mode, got %q/%q/%q", m.uiMode(), m.agent.Role, m.modelName)
	}
	clickModel()
	if m.agent.Role != config.RoleDefault || m.modelName != "default-model" || m.uiMode() != uiModeExecute {
		t.Fatalf("model click should select default without changing execute mode, got %q/%q/%q", m.uiMode(), m.agent.Role, m.modelName)
	}
	clickModel()
	if m.agent.Role != config.RoleFast || m.modelName != "fast-model" {
		t.Fatalf("model click should select fast, got %q/%q", m.agent.Role, m.modelName)
	}
	clickModel()
	if m.agent.Role != config.RoleTiny || m.modelName != "tiny-model" {
		t.Fatalf("model click should select tiny, got %q/%q", m.agent.Role, m.modelName)
	}

	clickMode := func() {
		t, _ := m.Update(tea.MouseMsg{
			Action: tea.MouseActionPress, Button: tea.MouseButtonLeft,
			X: m.statusModeX + m.statusModeW/2, Y: statusInfoRow(m.height),
		})
		m = t.(*model)
		_ = m.View()
	}
	clickMode()
	if m.settings != nil || m.uiMode() != uiModePlan || m.agent.Role != config.RoleTiny {
		t.Fatalf("mode click should cycle execute → plan without changing role, got %q/%q", m.uiMode(), m.agent.Role)
	}
	clickModel()
	if m.uiMode() != uiModePlan || m.agent.Role != config.RoleSmart || m.modelName != "smart-model" {
		t.Fatalf("model click should work in plan mode, got %q/%q/%q", m.uiMode(), m.agent.Role, m.modelName)
	}
	clickMode()
	if m.uiMode() != uiModeExecute || m.agent.Role != config.RoleSmart || m.modelName != "smart-model" {
		t.Fatalf("second mode click should wrap plan → execute without changing role, got %q/%q/%q", m.uiMode(), m.agent.Role, m.modelName)
	}
}

func TestModeSelectionPreservesModel(t *testing.T) {
	m := compactCmdModel()
	origRole := m.agent.Role
	origModel := m.modelName
	if err := m.setMode(uiModePlan); err != nil {
		t.Fatal(err)
	}
	if m.uiMode() != uiModePlan || m.agent.Role != origRole || m.modelName != origModel {
		t.Fatalf("plan mode should preserve model and role, got mode %q role %q", m.uiMode(), m.agent.Role)
	}
	if err := m.setMode(uiModeExecute); err != nil {
		t.Fatal(err)
	}
	if m.uiMode() != uiModeExecute || m.agent.Role != origRole || m.modelName != origModel {
		t.Fatalf("execute mode should preserve model and role, got mode %q role %q", m.uiMode(), m.agent.Role)
	}
}

func TestBottomModeClickCyclesWithoutOpeningPalette(t *testing.T) {
	m := compactCmdModel()
	m.width, m.height = 100, 30
	_ = m.View()
	tm, _ := m.Update(tea.MouseMsg{
		Action: tea.MouseActionPress, Button: tea.MouseButtonLeft,
		X: m.statusModeX + m.statusModeW/2, Y: statusInfoRow(m.height),
	})
	m = tm.(*model)
	if m.settings != nil {
		t.Fatal("clicking the bottom mode should not open a selector")
	}
	if m.uiMode() != uiModePlan {
		t.Fatalf("mode click did not activate plan: %q", m.uiMode())
	}
	if got := m.statusView(); !strings.Contains(got, " plan ") {
		t.Fatalf("status should expose the selected mode: %q", got)
	}
}

func TestBottomStatusEffortIsSeparateAndClickable(t *testing.T) {
	m := compactCmdModel()
	m.width, m.height = 100, 30
	m.agent.Effort = "high"
	_ = m.View()
	if got := m.statusView(); !strings.Contains(got, "│ kimi-k3-fast │ (high) │ execute │") {
		t.Fatalf("status should render effort as a separate segment: %q", got)
	}

	tm, _ := m.Update(tea.MouseMsg{
		Action: tea.MouseActionPress, Button: tea.MouseButtonLeft,
		X: m.statusEffortX + m.statusEffortW/2, Y: statusInfoRow(m.height),
	})
	m = tm.(*model)
	if m.settings != nil || m.agent.Effort != "" || m.uiMode() != uiModeExecute {
		t.Fatalf("effort click should cycle high → off without opening a selector or changing mode: %q/%q", m.agent.Effort, m.uiMode())
	}

	_ = m.View()
	tm, _ = m.Update(tea.MouseMsg{
		Action: tea.MouseActionPress, Button: tea.MouseButtonLeft,
		X: m.statusEffortX + m.statusEffortW/2, Y: statusInfoRow(m.height),
	})
	m = tm.(*model)
	if m.agent.Effort != "low" {
		t.Fatalf("effort click should cycle off → low, got %q", m.agent.Effort)
	}
}

func TestAvailableModelItemsRequireConfiguredProvider(t *testing.T) {
	m := &model{cfg: &config.Config{
		Providers: map[string]config.Provider{
			"ready":   {BaseURL: "https://ready.example", APIKey: "key"},
			"missing": {BaseURL: "https://missing.example"},
		},
		Models: map[string]config.Model{
			"shared":       {Providers: []string{"missing", "ready"}},
			"only-missing": {Providers: []string{"missing"}},
		},
	}}
	items := m.availableModelItems()
	if len(items) != 1 || items[0].model != "shared" || items[0].provider != "ready" {
		t.Fatalf("available routes should exclude providers without keys: %+v", items)
	}
}

func TestPaletteMouseClicksActivateRootAndPanelRows(t *testing.T) {
	m := compactCmdModel()
	m.width, m.height = 100, 30
	m.openPalette()

	rows, positions := m.paletteRootRows()
	modelIndex := -1
	for i, it := range m.settings.items {
		if it.title == "Model" {
			modelIndex = i
			break
		}
	}
	if modelIndex < 0 {
		t.Fatal("settings should contain Model")
	}
	if positions[modelIndex] >= len(rows) {
		t.Fatalf("Model row position %d is outside %d rendered rows", positions[modelIndex], len(rows))
	}

	tm, _ := m.Update(tea.MouseMsg{
		Action: tea.MouseActionPress, Button: tea.MouseButtonLeft,
		X: 5, Y: 1 + m.paletteRootListStart() + positions[modelIndex],
	})
	m = tm.(*model)
	if m.settings == nil || m.settings.top() == nil || m.settings.top().kind != panelRole {
		t.Fatal("clicking the Model row should open the role panel")
	}

	// Click the plan role. The panel's title and separator occupy the first
	// two settings rows; the role rows follow them.
	tm, _ = m.Update(tea.MouseMsg{
		Action: tea.MouseActionPress, Button: tea.MouseButtonLeft,
		X: 5, Y: 1 + 2 + 1,
	})
	m = tm.(*model)
	if m.settings == nil || m.settings.top() == nil || m.settings.top().kind != panelModel || m.settings.top().role != config.RoleSmart {
		t.Fatal("clicking the smart role should open its model panel")
	}

	// The first model row is directly below the model-panel title/separator.
	tm, _ = m.Update(tea.MouseMsg{
		Action: tea.MouseActionPress, Button: tea.MouseButtonLeft,
		X: 5, Y: 1 + 2,
	})
	m = tm.(*model)
	if m.cfg.Roles[config.RoleSmart].Model == "" {
		t.Fatal("clicking a role model should persist the selected route")
	}
}

func modelCmdModel() *model {
	m := &model{
		input: newInput(),
		agent: &agent.Agent{},
		cfg: &config.Config{
			DefaultModel: "kimi-k3-fast",
			Providers:    map[string]config.Provider{"inference": {BaseURL: "https://x", APIKey: "k"}},
			Models: map[string]config.Model{
				"kimi-k3-fast": {Providers: []string{"inference"}},
				"glm-5.2-fast": {Providers: []string{"inference"}},
			},
		},
		modelName: "kimi-k3-fast",
		provName:  "inference",
	}
	m.width = 80
	m.input.SetWidth(78)
	return m
}

func typeStr(t *testing.T, m *model, s string) *model {
	t.Helper()
	for _, r := range s {
		tm, _ := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{r}})
		m = tm.(*model)
	}
	return m
}

// Regression: typing /model and pressing enter must open the interactive
// picker, NOT insert a newline. (The newline bug was KeyCtrlM == KeyEnter
// being forwarded to the textarea; this guards against its return.)
func TestModelBareEnterOpensPicker(t *testing.T) {
	m := modelCmdModel()
	m = typeStr(t, m, "/model")
	if m.menu == nil {
		t.Fatal("typing /model should focus the completion menu")
	}
	tm, _ := m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	m = tm.(*model)
	if m.settings == nil || m.settings.top() == nil || m.settings.top().kind != panelRole {
		t.Fatalf("/model + enter should open the role picker; input=%q LineCount=%d", m.input.Value(), m.input.LineCount())
	}
	if m.input.Value() != "" || m.input.LineCount() != 1 {
		t.Errorf("enter must not leave a newline in the input: value=%q LineCount=%d", m.input.Value(), m.input.LineCount())
	}
}

// The ctrl+p settings's first suggestion is Model; enter drills into its
// interactive panel without leaving the settings.
func TestModelPaletteEnterOpensPicker(t *testing.T) {
	m := modelCmdModel()
	tm, _ := m.key(tea.KeyMsg{Type: tea.KeyCtrlP})
	m = tm.(*model)
	if m.settings == nil {
		t.Fatal("ctrl+p should open the command settings")
	}
	if len(m.settings.items) == 0 || m.settings.items[0].title != "Model" {
		t.Fatalf("first suggestion should be Model, got %+v", m.settings.items)
	}
	tm, _ = m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	m = tm.(*model)
	pp := m.settings.top()
	if pp == nil || pp.kind != panelRole {
		t.Fatalf("settings Model + enter should push the model-role panel; input=%q", m.input.Value())
	}
	if len(pp.list) != 4 || pp.list[1] != "smart" {
		t.Fatalf("model-role panel should list default, smart, fast, tiny: %+v", pp.list)
	}
	tm, _ = m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	m = tm.(*model)
	if pp := m.settings.top(); pp == nil || pp.kind != panelModel {
		t.Fatal("selecting a role should open its model panel")
	}
	if len(m.settings.top().items) == 0 {
		t.Fatal("model panel should list the configured routes")
	}
}

// Selecting a model name completes it on the first enter; the second enter
// submits. Neither may insert a newline into the input.
func TestModelArgEnterNeverNewlines(t *testing.T) {
	m := modelCmdModel()
	m = typeStr(t, m, "/model glm")
	if m.menu == nil {
		t.Fatal("expected model-name completion menu")
	}
	tm, _ := m.Update(tea.KeyMsg{Type: tea.KeyEnter}) // complete the name
	m = tm.(*model)
	if m.input.LineCount() != 1 {
		t.Fatalf("completing a model name must not newline: value=%q", m.input.Value())
	}
	if m.input.Value() == "/model glm" {
		t.Fatalf("enter should have accepted the completion, still %q", m.input.Value())
	}
}

type planTestBackend struct {
	mu     sync.Mutex
	stream chan models.Request
	done   chan struct{}
}

func (b *planTestBackend) Stream(_ context.Context, req models.Request, sink models.EventSink) (models.Message, models.Usage, error) {
	if b.stream != nil {
		b.stream <- req
	}
	if sink.OnText != nil {
		sink.OnText("plan text\n<proposed_plan>\n# Plan: ship it\n1. write code\n2. run tests\n</proposed_plan>")
	}
	if b.done != nil {
		b.done <- struct{}{}
	}
	return models.Message{Role: "assistant", Content: "plan text\n<proposed_plan>\n# Plan: ship it\n1. write code\n2. run tests\n</proposed_plan>"}, models.Usage{}, nil
}

func (b *planTestBackend) Complete(_ context.Context, req models.Request) (models.Message, models.Usage, error) {
	return models.Message{Role: "assistant", Content: "done"}, models.Usage{}, nil
}

func TestPlanThenExecuteConversational(t *testing.T) {
	b := &planTestBackend{
		stream: make(chan models.Request, 2),
		done:   make(chan struct{}),
	}
	m := &model{
		agent: agent.New(b, "model", 100, "sys"),
		input: newInput(),
	}

	// 1. /plan switches to plan mode and submits goal
	m.command("/plan ship it")
	if m.uiMode() != uiModePlan {
		t.Fatalf("expected mode plan, got %q", m.uiMode())
	}
	if !m.agent.PlanMode {
		t.Fatal("expected agent.PlanMode to be true")
	}
	<-b.done
	for i := 0; i < 100 && len(m.agent.MessagesSnapshot()) < 3; i++ {
		time.Sleep(2 * time.Millisecond)
	}

	// Simulate turn completion
	finalMsg := "Here is the plan:\n<proposed_plan>\n# Plan: ship it\n1. write code\n2. run tests\n</proposed_plan>"
	m.Update(turnDoneMsg{final: finalMsg})

	if m.proposedPlanMD == "" || !strings.Contains(m.proposedPlanMD, "# Plan: ship it") {
		t.Fatalf("expected proposedPlanMD to contain plan, got: %q", m.proposedPlanMD)
	}

	// 2. /execute switches to execute mode and submits approved plan
	m.command("/execute")
	if m.uiMode() != uiModeExecute {
		t.Fatalf("expected mode execute, got %q", m.uiMode())
	}
	if m.agent.PlanMode {
		t.Fatal("expected agent.PlanMode to be false after /execute")
	}
	<-b.done
	for i := 0; i < 100 && len(m.agent.MessagesSnapshot()) < 5; i++ {
		time.Sleep(2 * time.Millisecond)
	}
}

func TestExecuteWithoutPlanFails(t *testing.T) {
	m := &model{
		agent: agent.New(&planTestBackend{}, "model", 100, "sys"),
		input: newInput(),
	}

	m.command("/execute")
	var foundErr bool
	for _, blk := range m.blocks {
		if strings.Contains(blk.text, "no plan to execute") {
			foundErr = true
			break
		}
	}
	if !foundErr {
		t.Fatalf("expected 'no plan to execute' error, got blocks: %+v", m.blocks)
	}
}

func TestPlanCommandPreservesModel(t *testing.T) {
	b := &planTestBackend{}
	m := &model{
		agent:     agent.New(b, "custom-model", 100, "sys"),
		modelName: "custom-model",
		provName:  "custom-prov",
		input:     newInput(),
	}
	m.agent.Role = config.RoleFast

	m.planCommand("/plan")
	if m.modelName != "custom-model" || m.provName != "custom-prov" || m.agent.Role != config.RoleFast {
		t.Fatalf("bare /plan changed model or role: %q/%q/%q", m.modelName, m.provName, m.agent.Role)
	}
	if m.uiMode() != uiModePlan || !m.agent.PlanMode {
		t.Fatalf("expected plan mode, got uiMode=%q planMode=%v", m.uiMode(), m.agent.PlanMode)
	}
}

func TestBuildAgentUsesModelRoutesAndModelAPIOverride(t *testing.T) {
	t.Setenv("GHG_HOME", t.TempDir())
	profiles := routeTestProfiles(t)
	cfg := &config.Config{
		Providers: map[string]config.Provider{
			"route-test": {
				Profile: "route-test",
				APIKey:  "test-key",
			},
		},
		Models: map[string]config.Model{
			"anthropic-model": {Providers: []string{"route-test"}, ID: "minimax-m3", Context: 1000},
			"responses-model": {Providers: []string{"route-test"}, ID: "grok-4.6", Context: 1000},
			"override-model":  {Providers: []string{"route-test"}, ID: "minimax-m3", API: string(models.ProtocolOpenAIChatCompletions), Context: 1000},
		},
		Roles: map[string]config.RoleConfig{
			config.RoleDefault: {Model: "override-model", Provider: "route-test"},
			config.RoleSmart:   {Model: "anthropic-model", Provider: "route-test"},
			config.RoleFast:    {Model: "override-model", Provider: "route-test"},
			config.RoleTiny:    {Model: "anthropic-model", Provider: "route-test"},
		},
	}

	ag, _, _, err := agent.NewConfigured(agent.BuildOptions{
		Config: cfg, Profiles: profiles, Model: "anthropic-model", Role: config.RoleDefault, SystemPrompt: "system",
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := ag.Backend.(*models.AnthropicClient); !ok {
		t.Fatalf("route-matched model backend = %T, want *models.AnthropicClient", ag.Backend)
	}

	ag, _, _, err = agent.NewConfigured(agent.BuildOptions{
		Config: cfg, Profiles: profiles, Model: "override-model", Role: config.RoleDefault, SystemPrompt: "system",
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := ag.Backend.(*models.Client); !ok {
		t.Fatalf("model API override backend = %T, want *models.Client", ag.Backend)
	}

	ag, _, _, err = agent.NewConfigured(agent.BuildOptions{
		Config: cfg, Profiles: profiles, Model: "responses-model", Role: config.RoleDefault, SystemPrompt: "system",
	})
	if err != nil {
		t.Fatalf("Responses route should build: %v", err)
	}
	if _, ok := ag.Backend.(*models.OpenAIResponsesClient); !ok {
		t.Fatalf("Responses route backend = %T, want *models.OpenAIResponsesClient", ag.Backend)
	}
}

func TestBuildAgentForRoleUsesRoleRoute(t *testing.T) {
	profiles := routeTestProfiles(t)
	cfg := &config.Config{
		Providers: map[string]config.Provider{
			"route-test": {Profile: "route-test", APIKey: "test-key"},
		},
		Models: map[string]config.Model{
			"default-model": {Providers: []string{"route-test"}, ID: "minimax-m3", Context: 1000},
			"smart-model":   {Providers: []string{"route-test"}, ID: "minimax-m3", Context: 1000},
			"fast-model":    {Providers: []string{"route-test"}, ID: "qwen3-coder", Context: 1000},
			"tiny-model":    {Providers: []string{"route-test"}, ID: "minimax-m3", Context: 1000},
		},
		Roles: map[string]config.RoleConfig{
			config.RoleDefault: {Model: "default-model", Provider: "route-test"},
			config.RoleSmart:   {Model: "smart-model", Provider: "route-test"},
			config.RoleFast:    {Model: "fast-model", Provider: "route-test"},
			config.RoleTiny:    {Model: "tiny-model", Provider: "route-test"},
		},
	}

	for _, tc := range []struct {
		role, model string
		wantType    any
	}{
		{config.RoleSmart, "smart-model", (*models.AnthropicClient)(nil)},
		{config.RoleFast, "fast-model", (*models.Client)(nil)},
		{config.RoleTiny, "tiny-model", (*models.AnthropicClient)(nil)},
	} {
		ag, model, prov, err := agent.NewConfiguredForRole(cfg, profiles, tc.role, "system", false)
		if err != nil {
			t.Fatalf("%s: %v", tc.role, err)
		}
		if model != tc.model || prov != "route-test" || ag.Role != tc.role {
			t.Fatalf("%s route = %q @ %q, agent role %q", tc.role, model, prov, ag.Role)
		}
		switch tc.wantType.(type) {
		case *models.AnthropicClient:
			if _, ok := ag.Backend.(*models.AnthropicClient); !ok {
				t.Fatalf("%s backend = %T, want Anthropic", tc.role, ag.Backend)
			}
		case *models.Client:
			if _, ok := ag.Backend.(*models.Client); !ok {
				t.Fatalf("%s backend = %T, want OpenAI", tc.role, ag.Backend)
			}
		}
	}
}

func TestBuildAgentUsesModelsDevContextFallback(t *testing.T) {
	t.Setenv("GHG_HOME", t.TempDir())
	if err := config.SaveModelsDev(config.ModelsDevCache{
		Providers: map[string]map[string]int{"route-test": {"minimax-m3": 131072}},
	}); err != nil {
		t.Fatal(err)
	}
	profiles := routeTestProfiles(t)
	cfg := &config.Config{
		Providers: map[string]config.Provider{
			"route-test": {Profile: "route-test", APIKey: "test-key"},
		},
		Models: map[string]config.Model{
			"metadata-model": {Providers: []string{"route-test"}, ID: "minimax-m3"},
		},
	}

	ag, _, _, err := agent.NewConfigured(agent.BuildOptions{
		Config: cfg, Profiles: profiles, Model: "metadata-model", Role: config.RoleDefault, SystemPrompt: "system",
	})
	if err != nil {
		t.Fatal(err)
	}
	if ag.ContextLimit != 131072 {
		t.Fatalf("models.dev context = %d, want 131072", ag.ContextLimit)
	}
}

func routeTestProfiles(t *testing.T) models.Profiles {
	t.Helper()
	dir := t.TempDir()
	data := `schema: 1
id: route-test
display_name: Route Test
protocol: openai-chat-completions
base_url: http://127.0.0.1:9999/v1
auth:
  kind: bearer
  header: Authorization
  env_var: ROUTE_TEST_KEY
routes:
  - models: ["minimax-*"]
    protocol: anthropic-messages
    auth:
      kind: header
      header: x-api-key
    default_headers:
      anthropic-version: "2023-06-01"
  - models: ["grok-*"]
    protocol: openai-responses
catalog:
  kind: none
`
	if err := os.WriteFile(filepath.Join(dir, "route-test.yaml"), []byte(data), 0o600); err != nil {
		t.Fatal(err)
	}
	profiles, err := models.Load(models.LoadOptions{UserDir: dir})
	if err != nil {
		t.Fatal(err)
	}
	return profiles
}

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
			continue // keybind-only or usage-form hints ("/model · tab", "/compact <model>")
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

type reviewTestBackend struct {
	done chan struct{}
}

func (b *reviewTestBackend) Stream(_ context.Context, _ models.Request, _ models.EventSink) (models.Message, models.Usage, error) {
	if b.done != nil {
		b.done <- struct{}{}
	}
	return models.Message{Role: "assistant", Content: `{"summary":"looks great","verdict":"approve","findings":[]}`}, models.Usage{}, nil
}

func (b *reviewTestBackend) Complete(_ context.Context, _ models.Request) (models.Message, models.Usage, error) {
	return models.Message{Role: "assistant", Content: "done"}, models.Usage{}, nil
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

func TestReviewOneShotLifecycle(t *testing.T) {
	b := &reviewTestBackend{done: make(chan struct{})}
	m := &model{
		agent: agent.New(b, "test-model", 100, "sys"),
		input: newInput(),
		mode:  uiModeExecute,
	}
	m.agent.Role = config.RoleFast

	// 1. Submit /review
	m.command("/review internal/tui")
	if !m.reviewing {
		t.Fatal("expected m.reviewing to be true")
	}
	if !m.agent.ReviewMode {
		t.Fatal("expected agent.ReviewMode to be true")
	}
	if m.agent.PlanMode {
		t.Fatal("expected agent.PlanMode to be false")
	}
	if m.agent.Role != config.RoleFast {
		t.Fatalf("expected role to remain fast, got %q", m.agent.Role)
	}
	<-b.done
	for i := 0; i < 100 && len(m.agent.MessagesSnapshot()) < 3; i++ {
		time.Sleep(2 * time.Millisecond)
	}

	// 2. Complete turn with valid review JSON
	reviewJSON := `{"summary":"verified clean","verdict":"approve","findings":[]}`
	m.Update(turnDoneMsg{final: reviewJSON})

	if m.reviewing {
		t.Fatal("expected m.reviewing to be false after completion")
	}
	if m.agent.ReviewMode {
		t.Fatal("expected agent.ReviewMode to be false after completion")
	}
	if m.uiMode() != uiModeExecute {
		t.Fatalf("expected uiMode to remain execute, got %q", m.uiMode())
	}
	if m.agent.Role != config.RoleFast {
		t.Fatalf("expected role to be restored to fast, got %q", m.agent.Role)
	}

	// 3. Transcript contains rendered findings
	var renderedFound bool
	for _, blk := range m.blocks {
		if strings.Contains(blk.text, "verified clean") {
			renderedFound = true
			break
		}
	}
	if !renderedFound {
		t.Fatalf("expected rendered review in blocks, got: %+v", m.blocks)
	}
}

func TestReviewPreservesCustomModel(t *testing.T) {
	b := &reviewTestBackend{done: make(chan struct{})}
	m := &model{
		agent:     agent.New(b, "custom-model", 100, "sys"),
		modelName: "custom-model",
		provName:  "custom-prov",
		input:     newInput(),
		mode:      uiModePlan,
	}
	m.agent.Role = config.RoleDefault

	m.command("/review target")
	if m.modelName != "custom-model" || m.provName != "custom-prov" || m.agent.Role != config.RoleDefault {
		t.Fatalf("expected custom model to be preserved during review, got %q/%q/%q", m.modelName, m.provName, m.agent.Role)
	}
	<-b.done
	for i := 0; i < 100 && len(m.agent.MessagesSnapshot()) < 3; i++ {
		time.Sleep(2 * time.Millisecond)
	}
	m.Update(turnDoneMsg{final: `{"summary":"clean","verdict":"approve","findings":[]}`})
	if m.modelName != "custom-model" || m.provName != "custom-prov" || m.agent.Role != config.RoleDefault {
		t.Fatalf("expected custom model to be preserved after review, got %q/%q/%q", m.modelName, m.provName, m.agent.Role)
	}
}

// Scheduled tasks persist in the session store and survive a reload — the
// durability half of the wakeup channel.
func TestSchedulePersistence(t *testing.T) {
	t.Setenv("GHG_HOME", t.TempDir())
	st, err := session.Open(filepath.Join(t.TempDir(), "s.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	m := compactCmdModel()
	m.store = st

	// /schedule needs a session row; persist() creates one when a turn exists
	m.agent.Messages = append(m.agent.Messages, models.Message{Role: "user", Content: "q", Authored: true})
	m.persist()
	if m.sessionID == "" {
		t.Fatal("persist should create the session")
	}

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

	m := tasksModel("http://unused")
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

// TestStartupReportMCP: ready/failed/disabled servers render with the right
// glyphs in one line.
func TestStartupReportMCP(t *testing.T) {
	m := tasksModel("http://unused")
	disabled := false
	m.mcpMgr = mcp.NewManager(map[string]mcp.ServerConfig{
		"off":     {Command: []string{"true"}, Enabled: &disabled},
		"invalid": {},
	})
	m.startupReport()
	out := m.blocks[0].text
	if !strings.Contains(out, "MCP servers:") || !strings.Contains(out, "○ off") || !strings.Contains(out, "✗ invalid") {
		t.Errorf("bad mcp line:\n%s", out)
	}
}

// TestStartupReportSilent: nothing loaded, nothing said.
func TestStartupReportSilent(t *testing.T) {
	dir := t.TempDir()
	wd, _ := os.Getwd()
	os.Chdir(dir)
	defer os.Chdir(wd)
	t.Setenv("HOME", t.TempDir()) // no ~/.ghg/skills either

	m := tasksModel("http://unused")
	m.startupReport()
	if len(m.blocks) != 0 {
		t.Errorf("expected silence, got %q", m.blocks[0].text)
	}
}

func shellModel() *model {
	m := &model{
		input: newInput(),
		agent: &agent.Agent{},
	}
	m.width = 80
	m.input.SetWidth(m.width - 2)
	return m
}

func TestRunShellAppendsToolBlockAndMessage(t *testing.T) {
	m := shellModel()
	m.runShell("!echo hello")

	b := m.blocks[len(m.blocks)-1]
	if b.kind != blockTool {
		t.Fatalf("output should be a tool block, got kind %d", b.kind)
	}
	if !strings.Contains(b.text, "hello") {
		t.Fatalf("output should contain the command's stdout: %q", b.text)
	}

	if len(m.agent.Messages) != 1 {
		t.Fatalf("expected one conversation message, got %d", len(m.agent.Messages))
	}
	msg := m.agent.Messages[0]
	if msg.Role != "user" || msg.Authored {
		t.Fatalf("message should be a non-authored user message: %+v", msg)
	}
	if !strings.HasPrefix(msg.Content, "$ echo hello") {
		t.Fatalf("message should lead with the command: %q", msg.Content)
	}
	if !strings.Contains(msg.Content, "hello") {
		t.Fatalf("message should carry the output: %q", msg.Content)
	}
}

func TestRunShellEmptyIsANote(t *testing.T) {
	m := shellModel()
	m.runShell("!")
	if len(m.agent.Messages) != 0 {
		t.Fatalf("bare ! should not touch the conversation: %v", m.agent.Messages)
	}
	if b := lastBlock(m); !strings.Contains(b, "! <command>") {
		t.Fatalf("bare ! should print a usage note: %q", b)
	}
}

func TestRunShellFailingCommand(t *testing.T) {
	m := shellModel()
	m.runShell("!echo oops >&2; exit 3")
	b := lastBlock(m)
	if !strings.Contains(b, "oops") || !strings.Contains(b, "exit") {
		t.Fatalf("stderr and exit status should be captured: %q", b)
	}
}

func TestRunShellTruncatesHugeOutput(t *testing.T) {
	m := shellModel()
	m.runShell("!seq 1 200000")
	if b := lastBlock(m); !strings.Contains(b, "truncated") {
		t.Fatalf("huge output should carry a truncation marker (len %d)", len(b))
	}
}

func TestRunShellWhileBusySteers(t *testing.T) {
	// mid-turn the turn goroutine owns Messages, so the output is steered
	// (mutex-guarded) for injection at the next loop boundary instead of a
	// racy direct append
	m := shellModel()
	m.busy = true
	m.runShell("!echo mid-turn")
	if !m.busy {
		t.Fatal("runShell must not clear busy")
	}
	if len(m.agent.Messages) != 0 {
		t.Fatalf("mid-turn output must steer, not append: %v", m.agent.Messages)
	}
	// drainPending is unexported, but Turn would see it — pin via a turn on a
	// nil client being overkill; instead confirm the transcript got the block
	if b := lastBlock(m); !strings.Contains(b, "mid-turn") {
		t.Fatalf("the output block should still land in the transcript: %q", b)
	}
}

func TestEnterWhileBusyRunsShellEscape(t *testing.T) {
	m := busyQueueModel()
	m.input.SetValue("!echo hi")
	m = press(t, m, tea.KeyMsg{Type: tea.KeyEnter})
	if len(m.queue) != 0 {
		t.Fatalf("! should run now, not queue: %v", m.queue)
	}
	if len(m.agent.Messages) != 0 {
		t.Fatalf("busy output steers into the running turn, not append: %v", m.agent.Messages)
	}
	if b := lastBlock(m); !strings.Contains(b, "hi") {
		t.Fatalf("the output block should land: %q", b)
	}
	if m.hist[len(m.hist)-1] != "!echo hi" {
		t.Fatalf("the escape should be in history: %v", m.hist)
	}
}

func TestEnterIdleRunsShellEscape(t *testing.T) {
	m := shellModel()
	m.input.SetValue("!echo hi")
	m = press(t, m, tea.KeyMsg{Type: tea.KeyEnter})
	if m.busy {
		t.Fatal("! must not start a turn")
	}
	if len(m.agent.Messages) != 1 {
		t.Fatalf("the shell message should land: %d", len(m.agent.Messages))
	}
}

func TestQueueDrainExecutesShellEscape(t *testing.T) {
	m := busyQueueModel("!echo drained")
	m = press(t, m, tea.KeyMsg{Type: tea.KeyUp}) // exercise queueSel reset
	tm, _ := m.Update(turnDoneMsg{})
	m = tm.(*model)
	if len(m.queue) != 0 {
		t.Fatalf("the drained ! line should execute, not resubmit: %v", m.queue)
	}
	if m.queueSel != -1 {
		t.Fatalf("queueSel should reset, got %d", m.queueSel)
	}
	// busy just cleared, so the drained escape appends idle-style
	if len(m.agent.Messages) != 1 || !strings.Contains(m.agent.Messages[0].Content, "drained") {
		t.Fatalf("the shell message should land: %+v", m.agent.Messages)
	}
	// the queued line was already rendered in the queue view; draining must
	// not re-echo it as ❯ !echo drained
	for _, b := range m.blocks {
		if strings.Contains(b.text, "❯ !echo drained") {
			t.Fatal("drained escapes should not re-echo the command line")
		}
	}
}

func TestCdAndPwd(t *testing.T) {
	orig, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chdir(orig) })

	m := shellModel()
	m.command("/pwd")
	if b := lastBlock(m); !strings.Contains(b, orig) {
		t.Fatalf("/pwd should print the cwd: %q", b)
	}

	// macOS: t.TempDir lives under /var, a symlink to /private/var — Getwd
	// resolves it, the literal dir doesn't.
	dir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	m.command("/cd " + dir)
	if wd, _ := os.Getwd(); wd != dir {
		t.Fatalf("/cd should chdir: got %q", wd)
	}
	if b := lastBlock(m); !strings.Contains(b, dir) {
		t.Fatalf("/cd should confirm the new cwd: %q", b)
	}

	// bare /cd prints
	m.command("/cd")
	if b := lastBlock(m); !strings.Contains(b, dir) {
		t.Fatalf("bare /cd should print the cwd: %q", b)
	}

	// bad dir errors without moving
	m.command("/cd /definitely/not/a/dir")
	if b := lastBlock(m); !strings.Contains(b, "/cd:") {
		t.Fatalf("/cd should report the error: %q", b)
	}
	if wd, _ := os.Getwd(); wd != dir {
		t.Fatalf("failed /cd should not move: got %q", wd)
	}
}

func TestCdTilde(t *testing.T) {
	orig, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chdir(orig) })
	home, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("HOME", home)

	m := shellModel()
	m.command("/cd ~")
	if wd, _ := os.Getwd(); wd != home {
		t.Fatalf("/cd ~ should land in $HOME: got %q", wd)
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
	if len(m.agent.Messages) != 0 {
		t.Fatalf("whitespace-only ! should be the usage note: %v", m.agent.Messages)
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
	if len(m.agent.Messages) != 0 {
		t.Fatal("mid-string ! must not trigger the shell escape")
	}
}

// multiline command via ctrl+j
func TestRunShellMultilineCommand(t *testing.T) {
	m := shellModel()
	m.runShell("!echo a\necho b")
	if len(m.agent.Messages) != 1 || !strings.Contains(m.agent.Messages[0].Content, "b") {
		t.Fatalf("multiline command should run through bash -c: %+v", m.agent.Messages)
	}
}

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
		"context audit", "system prompt (base)", "skills (", "tool schemas (",
		"mcp: off", "disabled", "mcp: invalid", "TOTAL injected", "Trim:",
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
	before := len(m.blocks)
	m.command("/doctor")
	if !strings.Contains(m.blocks[len(m.blocks)-1].text, "unknown command") {
		t.Error("/doctor should not exist as a shorthand")
	}
	_ = before
}

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

	other := t.TempDir()
	t.Chdir(other)
	m.sysPrompt = "base"
	out = m.doctorReport()
	if strings.Contains(out, "project instructions (AGENTS.md)") {
		t.Fatalf("untrusted project instructions should be absent:\n%s", out)
	}
}

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

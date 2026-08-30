package tui

import (
	"errors"
	"path/filepath"
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/sacca97/ghg/internal/agent"
	"github.com/sacca97/ghg/internal/auth"
	"github.com/sacca97/ghg/internal/config"
	"github.com/sacca97/ghg/internal/llm"
	"github.com/sacca97/ghg/internal/provider"
)

func authTestModel(t *testing.T) *model {
	t.Helper()
	home := t.TempDir()
	t.Setenv("GHG_HOME", home)
	cfg, err := config.Load()
	if err != nil {
		t.Fatal(err)
	}
	profiles, err := provider.Load(provider.LoadOptions{UserDir: filepath.Join(home, "providers")})
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

func authResolved(t *testing.T, m *model, id string) provider.Resolved {
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
	resolved.Auth.Kind = provider.AuthNone
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
		models:    []llm.ModelInfo{{ID: "gpt-test", ContextLength: 128000, InputModalities: []string{"text", "image"}}},
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
	ag, _, _, err := buildAgentWithProfiles(m.cfg, m.modelName, m.provName, "sys", m.profiles)
	if err != nil {
		t.Fatal(err)
	}
	m.agent = ag
	m.agent.Messages = append(m.agent.Messages, llm.Message{Role: "user", Content: "hi", Authored: true})

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
	profiles, err := provider.Load(provider.LoadOptions{UserDir: filepath.Join(t.TempDir(), "providers")})
	if err != nil {
		t.Fatal(err)
	}

	cfg := config.Default()
	ag, modelName, providerName, err := buildAgentWithProfilesOptional(cfg, "", "", "sys", profiles)
	if err != nil {
		t.Fatalf("missing key should be a degraded start: %v", err)
	}
	if ag != nil || modelName != cfg.DefaultModel || providerName != "inference" {
		t.Fatalf("unexpected degraded route: agent=%v model=%q provider=%q", ag, modelName, providerName)
	}

	if _, _, _, err := buildAgentWithProfilesOptional(cfg, "does-not-exist", "", "sys", profiles); err == nil {
		t.Fatal("an explicit unknown model must remain a hard error")
	}

	const missingEnv = "GHG_TEST_MISSING_PROVIDER_KEY"
	t.Setenv(missingEnv, "")
	broken := &config.Config{
		DefaultModel: "m",
		Providers: map[string]config.Provider{
			"p": {Profile: "generic-openai", BaseURL: "https://example.test/v1", API: provider.ProtocolOpenAIChatCompletions, APIKey: "$" + missingEnv},
		},
		Models: map[string]config.Model{
			"m": {Providers: []string{"p"}, ID: "m"},
		},
	}
	if _, _, _, err := buildAgentWithProfilesOptional(broken, "", "", "sys", profiles); err == nil || !strings.Contains(err.Error(), missingEnv) {
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
	_, cands := completionsWithAuth("/auth an", nil, nil, m.authProviderCands(), nil, nil)
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
	lites := config.ModelInfoLites([]llm.ModelInfo{
		{ID: "gpt-test", ContextLength: 400000, MaxCompletionTokens: 128000,
			ReasoningEfforts: []string{"low", "high"}, InputModalities: []string{"text", "image"},
			Pricing: &llm.Pricing{Prompt: "0.00000125", Completion: "0.00001", InputCacheRead: "0.000000125"}},
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

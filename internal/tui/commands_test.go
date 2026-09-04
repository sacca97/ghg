package tui

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/sacca97/ghg/internal/agent"
	"github.com/sacca97/ghg/internal/auth"
	"github.com/sacca97/ghg/internal/config"
	"github.com/sacca97/ghg/internal/models"
	"github.com/sacca97/ghg/internal/session"
	workerwire "github.com/sacca97/ghg/internal/worker"
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

	if m.role != config.RoleFast || m.modelName != "fast-model" || m.provName != "generic-openai" {
		t.Fatalf("cold auth route = role %q, model %q, provider %q; want fast-model @ generic-openai", m.role, m.modelName, m.provName)
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
	m.messages = []models.Message{{Role: "system", Content: "sys"}, {Role: "user", Content: "hi", Authored: true}}

	m.applyAuthResult(authResultMsg{
		name:      "generic-openai",
		profile:   resolved,
		key:       "sk-generic-new",
		validated: true,
	})

	if got := m.cfg.Providers["generic-openai"].Key(); got != "sk-generic-new" {
		t.Errorf("live provider should be rebuilt with the new key, got %q", got)
	}
	if len(m.messages) != 2 || m.messages[1].Content != "hi" {
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
	m.modelName = m.cfg.DefaultModel
	m.provName = "inference"
	resolved := authResolved(t, m, "generic-openai")

	m.applyAuthResult(authResultMsg{
		name:      "generic-openai",
		profile:   resolved,
		key:       "sk-first-agent",
		validated: true,
	})
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
	m := &model{
		input:   newInput(),
		mouseOn: true, // matches the Run() default (wheel scroll + app selection)
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
		effort:    "",
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

func TestAskCommandSendsReadOnlyWorkerTurn(t *testing.T) {
	serverConn, clientConn := net.Pipe()
	defer serverConn.Close()
	defer clientConn.Close()

	m := compactCmdModel()
	m.workerClient = workerwire.NewClient(clientConn, "test-ask")

	frameCh := make(chan workerwire.Frame, 1)
	go func() {
		frame, err := workerwire.NewDecoder(serverConn).Read()
		if err == nil {
			frameCh <- frame
		}
	}()

	m.command("/ask what is the answer?")
	select {
	case frame := <-frameCh:
		var command workerwire.CommandRequest
		if err := json.Unmarshal(frame.Payload, &command); err != nil {
			t.Fatal(err)
		}
		if command.Name != workerwire.CommandInput {
			t.Fatalf("command = %q, want input", command.Name)
		}
		var input workerwire.Input
		if err := json.Unmarshal(command.Payload, &input); err != nil {
			t.Fatal(err)
		}
		if input.Input != "what is the answer?" {
			t.Fatalf("input = %q", input.Input)
		}
		if !input.AskMode || input.PlanMode || input.ReviewMode {
			t.Fatalf("unexpected mode flags: %+v", input)
		}
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for ask worker input")
	}
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
	if m.cfg.CompactModel != "glm-5.2-fast" {
		t.Fatalf("config should persist the pick, got %q", m.cfg.CompactModel)
	}
	m.compactCommand([]string{"off"})
	if m.compactModel != "" {
		t.Fatalf("off should restore the default compaction model: %q", m.compactModel)
	}
	if len(m.blocks) != blocks {
		t.Fatalf("successful compaction model changes should not append routine notes, got %v", m.blocks)
	}
}

func TestCompactCommandRejectsUnknownModel(t *testing.T) {
	m := compactCmdModel()
	m.compactCommand([]string{"nope"})
	if m.compactModel != "" {
		t.Fatal("unknown model must not become the compaction model")
	}
	if !strings.Contains(m.blocks[len(m.blocks)-1].text, "unknown model") {
		t.Fatalf("expected an error note, got %v", m.blocks)
	}
}

func TestWorkerLSPStatusAckRendersAllStates(t *testing.T) {
	m := compactCmdModel()
	payload, err := json.Marshal([]workerwire.LSPStatus{
		{Name: "gopls", Root: "/workspace", State: "connected"},
		{Name: "typescript", State: "not started"},
		{Name: "rust-analyzer", State: "failed", Error: "rust-analyzer not on PATH"},
	})
	if err != nil {
		t.Fatal(err)
	}
	m.handleWorkerFrame(workerwire.Frame{Type: workerwire.TypeAck, RequestID: "lsp-1", Payload: payload})
	last := m.blocks[len(m.blocks)-1].text
	for _, want := range []string{
		"● gopls",
		"connected (root: /workspace)",
		"○ typescript",
		"idle — starts on first matching file",
		"✗ rust-analyzer",
		"rust-analyzer not on PATH",
	} {
		if !strings.Contains(last, want) {
			t.Errorf("worker status view missing %q: %q", want, last)
		}
	}
}

func TestWorkerLSPAndMCPCommands(t *testing.T) {
	serverConn, clientConn := net.Pipe()
	defer serverConn.Close()
	defer clientConn.Close()

	m := compactCmdModel()
	m.sessionID = "test-worker-integrations"
	m.workerClient = workerwire.NewClient(clientConn, "test-worker-integrations")

	// 1. Dispatch /lsp
	readFrame := make(chan workerwire.Frame, 1)
	go func() {
		dec := workerwire.NewDecoder(serverConn)
		f, _ := dec.Read()
		readFrame <- f
	}()
	m.lspCommand(nil)
	frame := <-readFrame
	if frame.Type != workerwire.TypeCommand {
		t.Fatalf("frame type = %q, want %q", frame.Type, workerwire.TypeCommand)
	}
	var cmdReq workerwire.CommandRequest
	_ = json.Unmarshal(frame.Payload, &cmdReq)
	if cmdReq.Name != workerwire.CommandLSPStatus {
		t.Fatalf("command = %q, want %q", cmdReq.Name, workerwire.CommandLSPStatus)
	}

	// 2. Dispatch /mcp
	go func() {
		dec := workerwire.NewDecoder(serverConn)
		f, _ := dec.Read()
		readFrame <- f
	}()
	m.mcpCommand([]string{"/mcp"})
	frame = <-readFrame
	_ = json.Unmarshal(frame.Payload, &cmdReq)
	if cmdReq.Name != workerwire.CommandMCPStatus {
		t.Fatalf("command = %q, want %q", cmdReq.Name, workerwire.CommandMCPStatus)
	}

	// 3. Dispatch /mcp myserver reconnect
	go func() {
		dec := workerwire.NewDecoder(serverConn)
		f, _ := dec.Read()
		readFrame <- f
	}()
	m.mcpCommand([]string{"/mcp", "myserver", "reconnect"})
	frame = <-readFrame
	_ = json.Unmarshal(frame.Payload, &cmdReq)
	if cmdReq.Name != workerwire.CommandMCPReconnect {
		t.Fatalf("command = %q, want %q", cmdReq.Name, workerwire.CommandMCPReconnect)
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
	// a fresh /models fetch re-resolves the active limit
	cats := map[string]config.Catalog{
		"inference": {Models: []config.ModelInfoLite{{ID: "kimi-k3-fast", ContextLength: 262144}}},
	}
	m.updateCatalogs(cats)
	if m.contextLimit != 262144 {
		t.Fatalf("active limit should follow the catalog, got %d", m.contextLimit)
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
	m.setCompactPct(60)
	if m.cfg.CompactPct != 60 || m.compactPct() != 60 {
		t.Fatalf("setCompactPct(60): cfg=%d", m.cfg.CompactPct)
	}

	m.setCompactPct(120) // clamps to the 90 ceiling
	if m.cfg.CompactPct != 90 {
		t.Fatalf("setCompactPct(120) should clamp to 90: cfg=%d", m.cfg.CompactPct)
	}
	m.setCompactPct(0) // clamps to the 10 floor
	if m.cfg.CompactPct != 10 {
		t.Fatalf("setCompactPct(0) should clamp to 10: cfg=%d", m.cfg.CompactPct)
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
		provName:  "inference",
		modelName: "deepseek-v4-flash",
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
	m.modelName = "claude-opus-5"
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
	m.modelName = "gemini-3.5-flash"
	if got := m.effortsFor(); len(got) != len(defaultEfforts) {
		t.Fatalf("gemini should fall back to defaults: %v", got)
	}

	m.modelName = "toggle-only"
	if got := m.effortsFor(); len(got) != 2 || got[0] != "" || got[1] != "on" {
		t.Fatalf("toggle-only model should expose off/on: %v", got)
	}
	m.modelName = "no-controls"
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
	if len(pp.levels) != len(defaultEfforts) || pp.levels[pp.lidx] != m.effort {
		t.Fatalf("effort panel should list the model's levels on the current one: %v @%d", pp.levels, pp.lidx)
	}
	// scroll down to low and apply with enter
	tm, _ := m.paletteKey(tea.KeyMsg{Type: tea.KeyDown})
	m = tm.(*model)
	tm, _ = m.paletteKey(tea.KeyMsg{Type: tea.KeyEnter})
	m = tm.(*model)
	if m.effort != "low" {
		t.Fatalf("selecting low in the selector should apply it, got %q", m.effort)
	}
	// the selector came from /effort, not ctrl+p: commit-and-close, don't
	// strand the user on a settings root they never opened
	if m.settings != nil {
		t.Fatal("enter in a directly-opened selector should close the settings")
	}
}

// A user-picked effort updates the global default; the worker persists the
// live session value when it receives the route change.
func TestSetEffortPersistsGlobal(t *testing.T) {
	m := compactCmdModel()
	m.cfg.DefaultEffort = "medium"
	m.effort = "medium"

	m.setEffort("low") // the user picks a level
	if m.effort != "low" {
		t.Fatalf("effort: %q", m.effort)
	}
	reloaded, err := config.Load()
	if err != nil {
		t.Fatal(err)
	}
	if reloaded.DefaultEffort != "low" {
		t.Fatalf("global default should follow the pick, got %q", reloaded.DefaultEffort)
	}
	// A reconciliation must not rewrite the user's global default.
	m.resetEffort("")
	reloaded, _ = config.Load()
	if reloaded.DefaultEffort != "low" {
		t.Fatalf("reset must not touch the global default, got %q", reloaded.DefaultEffort)
	}
}

// Resume restores the session's own effort; a row that pre-dates per-session
// effort ("") inherits the current default and is stamped on the next save.
func TestResumeRestoresEffort(t *testing.T) {
	m := compactCmdModel()
	m.cfg.DefaultEffort = "medium"
	m.effort = "medium"
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
	m.effort = "low"
	if err := m.resume(id); err != nil {
		t.Fatal(err)
	}
	if m.effort != "high" {
		t.Fatalf("resume should restore the session effort, got %q", m.effort)
	}

	// a legacy row (no effort) inherits the current default…
	id2, err := st.Create("/tmp", m.modelName, m.provName)
	if err != nil {
		t.Fatal(err)
	}
	if err := st.Save(id2, 1, msgs, m.modelName, m.provName); err != nil {
		t.Fatal(err)
	}
	m.cfg.DefaultEffort = "low"
	m.effort = "low"
	if err := m.resume(id2); err != nil {
		t.Fatal(err)
	}
	if m.effort != "low" {
		t.Fatalf("legacy row should inherit the current default, got %q", m.effort)
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
	u := m.usage
	if u.PromptTokens != 12000 || u.Cached() != 8000 || u.CompletionTokens != 1500 {
		t.Fatalf("resume should restore usage, got in=%d cached=%d out=%d", u.PromptTokens, u.Cached(), u.CompletionTokens)
	}

	// new spend accumulates on top of the restored totals…
	m.usage.Add(models.Usage{PromptTokens: 3000, CompletionTokens: 500})

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
	u2 := m.usage
	if u2.PromptTokens != 1500 || u2.Cached() != 600 || u2.CompletionTokens != 300 {
		t.Fatalf("legacy row should reconstruct usage from messages, got in=%d cached=%d out=%d",
			u2.PromptTokens, u2.Cached(), u2.CompletionTokens)
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
	if u := m.usage; u.PromptTokens != 0 || u.CompletionTokens != 0 {
		t.Fatalf("usage-free session should start at zero, got %+v", u)
	}
}

func TestUpdateCatalogsResetsUnsupportedEffort(t *testing.T) {
	t.Setenv("HOME", t.TempDir()) // keep setEffort's cfg.Save() away from the real config
	m := &model{
		cfg:       &config.Config{},
		provName:  "inference",
		modelName: "deepseek-v4-flash",
		effort:    "medium",
	}
	m.updateCatalogs(map[string]config.Catalog{
		"inference": {Models: []config.ModelInfoLite{
			{ID: "deepseek-v4-flash", ReasoningEfforts: []string{"low", "high", "max"}},
		}},
	})
	if m.effort != "" {
		t.Fatalf("unsupported effort should reset to off, got %q", m.effort)
	}

	// a supported effort survives the refresh
	m.effort = "high"
	m.updateCatalogs(map[string]config.Catalog{
		"inference": {Models: []config.ModelInfoLite{
			{ID: "deepseek-v4-flash", ReasoningEfforts: []string{"low", "high", "max"}},
		}},
	})
	if m.effort != "high" {
		t.Fatalf("supported effort should survive, got %q", m.effort)
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
	if m.effort != "max" {
		t.Fatalf("expected max effort for deepseek-v4-flash, got %q", m.effort)
	}

	m.switchModel("toggle-only", "inference")
	if m.effort != "on" {
		t.Fatalf("expected on effort for toggle-only, got %q", m.effort)
	}

	m.switchModel("no-controls", "inference")
	if m.effort != "" {
		t.Fatalf("expected empty effort for no-controls, got %q", m.effort)
	}
}

func TestGoalHelpers(t *testing.T) {
	p := agent.ContinuePrompt("ship the feature")
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

func TestGoalFromContextMsgHandler(t *testing.T) {
	m := compactCmdModel()
	m.busy = true
	m.cancel = func() {}
	oldGoal := agent.NewGoal("paused old goal")
	oldGoal.Rounds = 20
	oldGoal.Status = agent.GoalStatusPaused
	m.applyGoalRecord(oldGoal)
	tm, cmd := m.Update(goalFromContextMsg{err: errors.New("boom")})
	m = tm.(*model)
	if cmd != nil {
		t.Fatal("a failed formulation must not submit anything")
	}
	if m.busy || m.cancel != nil {
		t.Fatal("the msg handler must clear busy/cancel on failure")
	}
	if m.goalRecord == nil || m.goalRecord.Objective != "paused old goal" {
		t.Fatalf("old goal must survive untouched, got %+v", m.goalRecord)
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
	m2 := compactCmdModel()
	serverConn, clientConn := net.Pipe()
	defer serverConn.Close()
	defer clientConn.Close()
	go func() {
		dec := workerwire.NewDecoder(serverConn)
		for {
			if _, err := dec.Read(); err != nil {
				return
			}
		}
	}()
	m2.workerClient = workerwire.NewClient(clientConn, "test")
	m2.busy = true
	m2.cancel = func() {}
	m2.messages = []models.Message{{Role: "system", Content: "sys"}}
	tm2, cmd2 := m2.Update(goalFromContextMsg{goal: "  ship it  "})
	m2 = tm2.(*model)
	if cmd2 == nil {
		t.Fatal("a successful formulation must submit the goal (start the turn)")
	}
	if !m2.busy {
		t.Fatal("busy must stay set — it belongs to the submitted turn now")
	}
	if m2.currentGoal() != "ship it" {
		t.Fatalf("goal should be trimmed and set, got %q", m2.currentGoal())
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
	m := compactCmdModel()
	m.messages = []models.Message{
		{Role: "system", Content: "sys"},
		{Role: "user", Content: "a"},
		{Role: "assistant", Content: "b"},
	}
	m.busy = true
	m.command("/goal-from-context")
	if out := lastBlock(m); !strings.Contains(out, "busy") {
		t.Fatalf("expected a busy note, got %q", out)
	}
	if m.currentGoal() != "" {
		t.Fatal("busy refusal must not touch the goal")
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
	m.role = config.RoleTiny

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
	if m.settings != nil || m.uiMode() != uiModeExecute || m.role != config.RoleSmart || m.modelName != "smart-model" {
		t.Fatalf("model click should select smart without changing execute mode, got %q/%q/%q", m.uiMode(), m.role, m.modelName)
	}
	clickModel()
	if m.role != config.RoleDefault || m.modelName != "default-model" || m.uiMode() != uiModeExecute {
		t.Fatalf("model click should select default without changing execute mode, got %q/%q/%q", m.uiMode(), m.role, m.modelName)
	}
	clickModel()
	if m.role != config.RoleFast || m.modelName != "fast-model" {
		t.Fatalf("model click should select fast, got %q/%q", m.role, m.modelName)
	}
	clickModel()
	if m.role != config.RoleTiny || m.modelName != "tiny-model" {
		t.Fatalf("model click should select tiny, got %q/%q", m.role, m.modelName)
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
	if m.settings != nil || m.uiMode() != uiModePlan || m.role != config.RoleTiny {
		t.Fatalf("mode click should cycle execute → plan without changing role, got %q/%q", m.uiMode(), m.role)
	}
	clickModel()
	if m.uiMode() != uiModePlan || m.role != config.RoleSmart || m.modelName != "smart-model" {
		t.Fatalf("model click should work in plan mode, got %q/%q/%q", m.uiMode(), m.role, m.modelName)
	}
	clickMode()
	if m.uiMode() != uiModeExecute || m.role != config.RoleSmart || m.modelName != "smart-model" {
		t.Fatalf("second mode click should wrap plan → execute without changing role, got %q/%q/%q", m.uiMode(), m.role, m.modelName)
	}
}

func TestModeSelectionPreservesModel(t *testing.T) {
	m := compactCmdModel()
	origRole := m.role
	origModel := m.modelName
	if err := m.setMode(uiModePlan); err != nil {
		t.Fatal(err)
	}
	if m.uiMode() != uiModePlan || m.role != origRole || m.modelName != origModel {
		t.Fatalf("plan mode should preserve model and role, got mode %q role %q", m.uiMode(), m.role)
	}
	if err := m.setMode(uiModeExecute); err != nil {
		t.Fatal(err)
	}
	if m.uiMode() != uiModeExecute || m.role != origRole || m.modelName != origModel {
		t.Fatalf("execute mode should preserve model and role, got mode %q role %q", m.uiMode(), m.role)
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
	m.effort = "high"
	_ = m.View()
	if got := m.statusView(); !strings.Contains(got, "│ kimi-k3-fast │ (high) │ execute │") {
		t.Fatalf("status should render effort as a separate segment: %q", got)
	}

	tm, _ := m.Update(tea.MouseMsg{
		Action: tea.MouseActionPress, Button: tea.MouseButtonLeft,
		X: m.statusEffortX + m.statusEffortW/2, Y: statusInfoRow(m.height),
	})
	m = tm.(*model)
	if m.settings != nil || m.effort != "" || m.uiMode() != uiModeExecute {
		t.Fatalf("effort click should cycle high → off without opening a selector or changing mode: %q/%q", m.effort, m.uiMode())
	}

	_ = m.View()
	tm, _ = m.Update(tea.MouseMsg{
		Action: tea.MouseActionPress, Button: tea.MouseButtonLeft,
		X: m.statusEffortX + m.statusEffortW/2, Y: statusInfoRow(m.height),
	})
	m = tm.(*model)
	if m.effort != "low" {
		t.Fatalf("effort click should cycle off → low, got %q", m.effort)
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

// multiline command via ctrl+j
func TestFmtTokIncludesMillions(t *testing.T) {
	if fmtTok(350) != "350" || fmtTok(4848) != "4.8k" || fmtTok(1_500_000) != "1.5M" {
		t.Errorf("fmtTok: %q %q %q", fmtTok(350), fmtTok(4848), fmtTok(1_500_000))
	}
}

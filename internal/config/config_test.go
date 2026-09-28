package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestLoadSaveDefaults(t *testing.T) {
	t.Setenv("HOME", t.TempDir())

	cfg, err := Load() // first run writes defaults
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Version != ConfigVersion || cfg.Roles[RoleDefault].Model != "" || len(cfg.Models) != 0 || cfg.Providers["inference"].BaseURL == "" || cfg.Providers["inference"].Profile != "inference" {
		t.Fatalf("defaults: %+v", cfg)
	}
	cfg.Roles = map[string]RoleConfig{RoleDefault: {Model: "glm-5.2-fast"}}
	if err := cfg.Save(); err != nil {
		t.Fatal(err)
	}
	cfg2, err := Load()
	if err != nil || cfg2.Roles[RoleDefault].Model != "glm-5.2-fast" {
		t.Fatalf("reload: %+v %v", cfg2, err)
	}
}

func TestExecutionOverridesValidateWithoutPersisting(t *testing.T) {
	cfg := &Config{}
	if err := cfg.ApplyExecutionOverrides("workspace-write", "deny", "auto"); err != nil {
		t.Fatal(err)
	}
	if cfg.Execution == nil || cfg.Execution.Approval != "auto" {
		t.Fatalf("execution overrides = %+v", cfg.Execution)
	}
	if err := cfg.ApplyExecutionOverrides("unsafe", "", ""); err == nil {
		t.Fatal("invalid sandbox override should fail")
	}
	cfg.Execution.BubblewrapPath = "bwrap"
	if err := cfg.ValidateExecution(); err == nil {
		t.Fatal("relative bubblewrap path should fail")
	}
	cfg.Execution.BubblewrapPath = "/usr/bin/bwrap"
	cfg.Execution.SecretNames = []string{"["}
	if err := cfg.ValidateExecution(); err == nil {
		t.Fatal("invalid secret name pattern should fail")
	}
}

func TestLoadRejectsBadJSON(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	os.MkdirAll(filepath.Join(home, ".ghg"), 0o700)
	os.WriteFile(filepath.Join(home, ".ghg", "config.json"), []byte("{nope"), 0o600)
	if _, err := Load(); err == nil {
		t.Fatal("expected parse error")
	}
}

func TestLoadRejectsNoncurrentConfigVersion(t *testing.T) {
	for _, tc := range []struct {
		name  string
		input string
		want  string
	}{
		{name: "missing", input: `{"providers":null,"models":null}`, want: "unsupported config version 0"},
		{name: "old", input: `{"version":1}`, want: "unsupported config version 1"},
		{name: "future", input: `{"version":3}`, want: "newer than supported"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			home := t.TempDir()
			t.Setenv("HOME", home)
			dir := filepath.Join(home, ".ghg")
			if err := os.MkdirAll(dir, 0o700); err != nil {
				t.Fatal(err)
			}
			path := filepath.Join(dir, "config.json")
			input := []byte(tc.input)
			if err := os.WriteFile(path, input, 0o600); err != nil {
				t.Fatal(err)
			}
			if _, err := Load(); err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("config should be rejected with %q: %v", tc.want, err)
			}
			if got, err := os.ReadFile(path); err != nil || string(got) != string(input) {
				t.Fatalf("unsupported config was rewritten: %v\n%s", err, got)
			}
		})
	}
}

func TestProviderKey(t *testing.T) {
	t.Setenv("HOME", t.TempDir()) // no ~/.inf fallback available
	t.Setenv("GHG_TEST_KEY", "from-env")

	if k := (Provider{APIKeyEnv: "GHG_TEST_KEY", APIKey: "literal"}).Key(); k != "from-env" {
		t.Fatalf("env should win: %q", k)
	}
	if k := (Provider{APIKeyEnv: "GHG_UNSET_VAR", APIKey: "literal"}).Key(); k != "literal" {
		t.Fatalf("literal fallback: %q", k)
	}
	if k := (Provider{BaseURL: "https://other.example.com"}).Key(); k != "" {
		t.Fatalf("no key expected: %q", k)
	}
}

func TestResolveRouting(t *testing.T) {
	cfg := &Config{
		Roles: map[string]RoleConfig{RoleDefault: {Model: "m1"}},
		Providers: map[string]Provider{
			"a": {BaseURL: "https://a", API: "openai-chat-completions"},
			"b": {BaseURL: "https://b", API: "openai-chat-completions"},
		},
		Models: map[string]Model{
			"m1": {Providers: []string{"a", "b"}, ID: "vendor/m1"},
		},
	}
	route, err := cfg.Resolve("", "")
	if err != nil || route.Provider.BaseURL != "https://a" || route.APIID != "vendor/m1" || route.ProviderName != "a" || route.ModelName != "m1" {
		t.Fatalf("default routing: %+v %v", route, err)
	}
	route, err = cfg.Resolve("m1", "b")
	if err != nil || route.Provider.BaseURL != "https://b" || route.ProviderName != "b" {
		t.Fatalf("provider override: %+v %v", route, err)
	}
	if _, err = cfg.Resolve("nope", ""); err == nil {
		t.Fatal("expected unknown model error")
	}
	if _, err = cfg.Resolve("m1", "nope"); err == nil {
		t.Fatal("expected unknown provider error")
	}
}

func TestHomeUnavailable(t *testing.T) {
	t.Setenv("HOME", "")
	if _, err := Dir(); err == nil {
		t.Fatal("expected Dir error")
	}
	if _, err := Load(); err == nil {
		t.Fatal("expected Load error")
	}
	if err := (&Config{}).Save(); err == nil {
		t.Fatal("expected Save error")
	}
}

func TestUserInstructionsSeedsTemplateAndStripsComments(t *testing.T) {
	t.Setenv("GHG_HOME", t.TempDir())

	if got := UserInstructions(); got != "" {
		t.Fatalf("a fresh seed is all comments — nothing to inject, got %q", got)
	}
	path := filepath.Join(os.Getenv("GHG_HOME"), "AGENTS.md")
	data, err := os.ReadFile(path)
	if err != nil || !strings.Contains(string(data), "# Your standing instructions") {
		t.Fatalf("seed file should exist with the template: %v\n%s", err, data)
	}

	if err := os.WriteFile(path,
		[]byte("# hi\n\n- Always pnpm.\n- Ask before force-push.\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	got := UserInstructions()
	if !strings.Contains(got, "- Always pnpm.") || strings.Contains(got, "# hi") {
		t.Fatalf("instructions should carry user lines only:\n%s", got)
	}
	if !strings.Contains(AgentsSeed, "/me opens this file") {
		t.Fatal("seed should tell the user how to edit")
	}

	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	legacy := filepath.Join(os.Getenv("GHG_HOME"), "me.md")
	if err := os.WriteFile(legacy, []byte("legacy instructions\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if got := UserInstructionsPath(); got != legacy {
		t.Fatalf("legacy instructions path = %q, want %q", got, legacy)
	}
	if got := UserInstructions(); got != "legacy instructions" {
		t.Fatalf("legacy instructions = %q", got)
	}
}

func TestLoadJSONCCommentsAndTrailingCommas(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	os.MkdirAll(filepath.Join(home, ".ghg"), 0o700)
	src := `{
  "version": 2, // current schema
  "roles": {"default": {"model": "m1", "provider": "a"},}, /* block comment */
  "providers": {
    "a": { "baseUrl": "https://a", "api": "openai-chat-completions", }, // trailing comma
  },
  "models": {
    "m1": { "providers": ["a",], "api": "anthropic-messages", "context": 1024, },
    "m2": { "providers": ["a"], "api": "openai-chat-completions", "context": 4096 },
  },
}

`
	if err := os.WriteFile(filepath.Join(home, ".ghg", "config.json"), []byte(src), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Version != ConfigVersion || cfg.Roles[RoleDefault] != (RoleConfig{Model: "m1", Provider: "a"}) {
		t.Fatalf("default role: %+v", cfg.Roles[RoleDefault])
	}
	if cfg.Models["m1"].Providers[0] != "a" || cfg.Models["m1"].API != "anthropic-messages" || cfg.Models["m1"].Context != 1024 {
		t.Fatalf("model: %+v", cfg.Models["m1"])
	}
	if cfg.Providers["a"].API != "openai-chat-completions" || cfg.Models["m2"].API != "openai-chat-completions" || cfg.Models["m2"].Context != 4096 {
		t.Fatalf("model protocol/context: %+v", cfg.Models["m2"])
	}
}

// TestMCPImportRoundTrip pins the mcpImport block's JSONC shape: absent stays
// nil (import-everything default), and a full block round-trips through
// Save/Load unchanged — including exclude beating only at policy level.
func TestMCPImportRoundTrip(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	os.MkdirAll(filepath.Join(home, ".ghg"), 0o700)
	src := `{
	  "version": 2,
	  "providers": { "a": { "baseUrl": "https://a", "api": "openai-chat-completions" } },
  "models": { "m1": { "providers": ["a"] } },
  "mcpImport": {
    "claude": { "enabled": false },
    "codex": { "enabled": true, "only": ["paper"], "exclude": ["node_repl"] }
  }
}
`
	os.WriteFile(filepath.Join(home, ".ghg", "config.json"), []byte(src), 0o600)
	cfg, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.MCPImport == nil || cfg.MCPImport.Claude == nil || cfg.MCPImport.Claude.Enabled == nil || *cfg.MCPImport.Claude.Enabled {
		t.Fatalf("claude should parse as enabled=false, got %+v", cfg.MCPImport)
	}
	if got := cfg.MCPImport.Codex.Exclude; len(got) != 1 || got[0] != "node_repl" {
		t.Fatalf("codex exclude: %+v", cfg.MCPImport.Codex)
	}
	if err := cfg.Save(); err != nil {
		t.Fatal(err)
	}
	reloaded, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if reloaded.MCPImport == nil || *reloaded.MCPImport.Claude.Enabled || reloaded.MCPImport.Codex.Only[0] != "paper" {
		t.Fatalf("mcpImport did not round-trip: %+v", reloaded.MCPImport)
	}
	// Absent block stays nil — zero-breakage default.
	if err := os.WriteFile(filepath.Join(home, ".ghg", "config.json"), []byte(`{
	  "version": 2,
	  "providers": { "a": { "baseUrl": "https://a", "api": "openai-chat-completions" } },
  "models": { "m1": { "providers": ["a"] } }
}`), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg2, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if cfg2.MCPImport != nil {
		t.Errorf("absent mcpImport must stay nil, got %+v", cfg2.MCPImport)
	}
}

// TestLoadPreservesMCPImportOnClobber: regenerating defaults after a clobber
// keeps the user's import gating (same rule as MCP servers).
func TestLoadPreservesMCPImportOnClobber(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	dir := filepath.Join(home, ".ghg")
	os.MkdirAll(dir, 0o700)
	os.WriteFile(filepath.Join(dir, "config.json"), []byte(
		`{"version":2,"providers":null,"models":null,"mcpImport":{"codex":{"enabled":false}}}`), 0o600)
	cfg, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.MCPImport == nil || cfg.MCPImport.Codex == nil || *cfg.MCPImport.Codex.Enabled {
		t.Fatalf("mcpImport must survive clobber recovery, got %+v", cfg.MCPImport)
	}
}

func TestLoadRecoversFromClobberedConfig(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	dir := filepath.Join(home, ".ghg")
	os.MkdirAll(dir, 0o700)
	p := filepath.Join(dir, "config.json")
	// a previously-clobbered config: parses fine but has no providers/models
	os.WriteFile(p, []byte(`{"version":2,"providers":null,"models":null}`), 0o600)
	// a healthy backup from before the wipe
	os.WriteFile(p+".bak", []byte(`{"version":2,"providers":{"a":{"baseUrl":"https://a","api":"openai-chat-completions"}},"models":{"m1":{"providers":["a"]}},"roles":{"default":{"model":"m1"}}}`), 0o600)

	cfg, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Roles[RoleDefault].Model != "m1" || len(cfg.Providers) != 1 {
		t.Fatalf("expected restore from .bak, got %+v", cfg)
	}
}

func TestLoadRegeneratesDefaultsWhenEmptyAndNoBackup(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	dir := filepath.Join(home, ".ghg")
	os.MkdirAll(dir, 0o700)
	os.WriteFile(filepath.Join(dir, "config.json"), []byte(`{"version":2,"providers":null,"models":null}`), 0o600)
	cfg, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Roles[RoleDefault].Model != "" || len(cfg.Models) != 0 || len(cfg.Providers) == 0 {
		t.Fatalf("expected regenerated defaults, got %+v", cfg)
	}
}

func TestSaveRefusesToClobberHealthyConfig(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	dir := filepath.Join(home, ".ghg")
	os.MkdirAll(dir, 0o700)
	p := filepath.Join(dir, "config.json")
	healthy := `{"version":2,"providers":{"a":{"baseUrl":"https://a","api":"openai-chat-completions"}},"models":{"m1":{"providers":["a"]}},"roles":{"default":{"model":"m1"}}}`
	os.WriteFile(p, []byte(healthy), 0o600)

	if err := (&Config{}).Save(); err == nil {
		t.Fatal("expected refusal to overwrite a healthy config with an empty one")
	}
	// original untouched
	data, _ := os.ReadFile(p)
	if string(data) != healthy {
		t.Fatalf("config should be unchanged, got %q", data)
	}
}

func TestSaveWritesBackupAndIsAtomic(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	cfg, err := Load() // writes defaults
	if err != nil {
		t.Fatal(err)
	}
	p, _ := path()
	first, _ := os.ReadFile(p)

	cfg.Roles = map[string]RoleConfig{RoleDefault: {Model: "glm-5.2-fast"}}
	if err := cfg.Save(); err != nil {
		t.Fatal(err)
	}
	bak, err := os.ReadFile(p + ".bak")
	if err != nil {
		t.Fatal("expected a .bak of the previous contents")
	}
	if string(bak) != string(first) {
		t.Fatalf("backup should hold the previous contents")
	}
	// no temp file left behind
	if _, err := os.Stat(p + ".tmp"); !os.IsNotExist(err) {
		t.Fatal("temp file should be renamed away")
	}
}

func TestSaveWritesJSONCHeader(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	if _, err := Load(); err != nil {
		t.Fatal(err)
	}
	p, _ := path()
	data, _ := os.ReadFile(p)
	if len(data) == 0 || data[0] != '/' {
		t.Fatalf("expected a // header comment, got:\n%s", data)
	}
	// and it still parses back via the JSONC loader
	if _, err := Load(); err != nil {
		t.Fatal(err)
	}
}

func TestLoadCatalogsAlwaysNonNil(t *testing.T) {
	t.Setenv("HOME", t.TempDir()) // no ~/.ghg/models.json exists
	cats := LoadCatalogs()
	if cats == nil {
		t.Fatal("LoadCatalogs must return a non-nil map so callers can write into it")
	}
	cats["inference"] = Catalog{} // must not panic
	if len(cats) != 1 {
		t.Fatalf("expected to hold the written entry, got %d", len(cats))
	}
}

func TestLogEventWritesAndRotates(t *testing.T) {
	t.Setenv("GHG_HOME", t.TempDir())

	LogEvent("config.save", "before=(providers=1) after=(providers=1)")
	LogEvent("catalog.fetch", "inference ok: 42 models")
	dir, _ := Dir()
	b, err := os.ReadFile(filepath.Join(dir, "ghg.log"))
	if err != nil {
		t.Fatal(err)
	}
	s := string(b)
	if !strings.Contains(s, "config.save") || !strings.Contains(s, "catalog.fetch") || !strings.Contains(s, "pid=") {
		t.Fatalf("log content: %q", s)
	}

	// rotation: oversize log rolls to ghg.log.1
	os.WriteFile(filepath.Join(dir, "ghg.log"), make([]byte, logMaxBytes+1), 0o600)
	LogEvent("config.load", "after rotation")
	if _, err := os.Stat(filepath.Join(dir, "ghg.log.1")); err != nil {
		t.Fatalf("expected rotation: %v", err)
	}
	b, _ = os.ReadFile(filepath.Join(dir, "ghg.log"))
	if !strings.Contains(string(b), "after rotation") {
		t.Fatalf("fresh log should hold the new event: %q", b)
	}
}

func TestLogEventNeverFails(t *testing.T) {
	t.Setenv("GHG_HOME", "/nonexistent-\x7f-impossible") // Dir() will fail MkdirAll
	LogEvent("config.load", "should not panic or error")
}

// The catalog reports the provider's output cap (max_completion_tokens)
// separately from the input window (context_length).
func TestCatalogMaxCompletionTokens(t *testing.T) {
	c := Catalog{Models: []ModelInfoLite{
		{ID: "a", ContextLength: 1000000, MaxCompletionTokens: 128000},
		{ID: "b", ContextLength: 200000}, // no output cap advertised
	}}
	if got := c.MaxCompletionTokens("a"); got != 128000 {
		t.Fatalf("a: %d", got)
	}
	if got := c.MaxCompletionTokens("b"); got != 0 {
		t.Fatalf("b should be 0 when unadvertised: %d", got)
	}
	if got := c.MaxCompletionTokens("nope"); got != 0 {
		t.Fatalf("unknown: %d", got)
	}
	if got := c.ContextLength("a"); got != 1000000 {
		t.Fatalf("ctx a: %d", got)
	}
}

func TestOutputConfigRoundTrip(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	dir := filepath.Join(home, ".ghg")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "config.json"), []byte(`{
	  "version": 2,
	  "providers": { "a": { "baseUrl": "https://a", "api": "openai-chat-completions" } },
  "models": { "m1": { "providers": ["a"] } },
  "outputs": { "enabled": false, "maxBytes": 4096 }
}`), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Outputs == nil || cfg.Outputs.Enabled == nil || *cfg.Outputs.Enabled || cfg.Outputs.MaxBytes != 4096 {
		t.Fatalf("output config did not parse: %+v", cfg.Outputs)
	}
	if err := cfg.Save(); err != nil {
		t.Fatal(err)
	}
	saved, err := os.ReadFile(filepath.Join(dir, "config.json"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(saved), `"outputs"`) {
		t.Fatalf("output config was not saved: %s", saved)
	}
	reloaded, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if reloaded.Outputs == nil || reloaded.Outputs.Enabled == nil || *reloaded.Outputs.Enabled || reloaded.Outputs.MaxBytes != 4096 {
		t.Fatalf("output config did not round-trip: %+v", reloaded.Outputs)
	}
}

func TestSubagentsConfigRoundTripAndDisable(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	dir := filepath.Join(home, ".ghg")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "config.json"), []byte(`{
	  "version": 2,
	  "providers": { "a": { "baseUrl": "https://a", "api": "openai-chat-completions" } },
  "models": { "m1": { "providers": ["a"] } },
  "subagents": false
}`), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Subagents == nil || *cfg.Subagents || SubagentsEnabled(cfg) {
		t.Fatalf("subagents config did not parse: %+v", cfg.Subagents)
	}
	if err := cfg.Save(); err != nil {
		t.Fatal(err)
	}
	reloaded, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if reloaded.Subagents == nil || *reloaded.Subagents || SubagentsEnabled(reloaded) {
		t.Fatalf("subagents config did not round-trip: %+v", reloaded.Subagents)
	}
	if !SubagentsEnabled(nil) {
		t.Fatalf("nil config should enable subagents by default")
	}
}

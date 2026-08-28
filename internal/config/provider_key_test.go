package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/sacca97/ghg/internal/provider"
)

func testResolvedProvider(envVar string) provider.Resolved {
	return provider.Resolved{
		Name: "opencode",
		Profile: provider.Profile{
			ID:          "opencode",
			DisplayName: "OpenCode Go",
		},
		BaseURL:  "https://opencode.example/v1",
		Protocol: provider.ProtocolOpenAIChatCompletions,
		Auth: provider.Auth{
			Kind:   provider.AuthBearer,
			Header: "Authorization",
			EnvVar: envVar,
		},
	}
}

func TestUpsertProviderKeyLiteralPreservesConfig(t *testing.T) {
	cfg := &Config{
		Providers: map[string]Provider{
			"inference": {Name: "Inference", Profile: "inference"},
		},
		Models: map[string]Model{
			"kimi-k3": {Providers: []string{"inference"}, Context: 1048576},
		},
	}
	if err := cfg.UpsertProviderKey("opencode", testResolvedProvider("OPENCODE_GO_API_KEY"), " Bearer sk-go ", false); err != nil {
		t.Fatal(err)
	}

	p, ok := cfg.Providers["opencode"]
	if !ok {
		t.Fatal("provider missing after upsert")
	}
	if p.Name != "OpenCode Go" || p.Profile != "opencode" || p.BaseURL != "https://opencode.example/v1" || p.API != provider.ProtocolOpenAIChatCompletions {
		t.Errorf("unexpected provider shape: %+v", p)
	}
	if p.APIKey != "sk-go" || p.APIKeyEnv != "" {
		t.Errorf("literal mode should set apiKey only: %+v", p)
	}
	if _, ok := cfg.Providers["inference"]; !ok || cfg.Models["kimi-k3"].Context != 1048576 {
		t.Error("upsert clobbered existing config state")
	}
}

func TestUpsertProviderKeySwitchesStorageModes(t *testing.T) {
	cfg := &Config{}
	resolved := testResolvedProvider("OPENCODE_GO_API_KEY")
	if err := cfg.UpsertProviderKey("opencode", resolved, "sk-one", false); err != nil {
		t.Fatal(err)
	}
	if err := cfg.UpsertProviderKey("opencode", resolved, "sk-two", true); err != nil {
		t.Fatal(err)
	}
	p := cfg.Providers["opencode"]
	if p.APIKey != "" || p.APIKeyEnv != "OPENCODE_GO_API_KEY" {
		t.Errorf("env mode should clear literal key: %+v", p)
	}

	if err := cfg.UpsertProviderKey("opencode", resolved, "sk-three", false); err != nil {
		t.Fatal(err)
	}
	p = cfg.Providers["opencode"]
	if p.APIKey != "sk-three" || p.APIKeyEnv != "" {
		t.Errorf("literal mode should clear env reference: %+v", p)
	}
}

func TestUpsertProviderKeyRequiresProfileEnvVar(t *testing.T) {
	cfg := &Config{Providers: map[string]Provider{"existing": {APIKey: "keep"}}}
	err := cfg.UpsertProviderKey("opencode", testResolvedProvider(""), "sk-go", true)
	if err == nil || !strings.Contains(err.Error(), "does not declare auth.env_var") {
		t.Fatalf("missing env metadata should fail: %v", err)
	}
	if _, ok := cfg.Providers["opencode"]; ok {
		t.Fatal("failed upsert must not create an entry")
	}
}

func TestAnyProviderUsableIsProfileAware(t *testing.T) {
	cfg := &Config{Providers: map[string]Provider{
		"missing-key": {Profile: "inference", APIKeyEnv: "MISSING_PROVIDER_KEY"},
		"local":       {Profile: "local"},
	}}
	t.Setenv("MISSING_PROVIDER_KEY", "")

	userDir := t.TempDir()
	writeProviderTestProfile(t, userDir, "local", provider.AuthNone)
	profiles, err := provider.Load(provider.LoadOptions{UserDir: userDir})
	if err != nil {
		t.Fatal(err)
	}
	if !cfg.AnyProviderUsable(profiles) {
		t.Fatal("auth:none provider should count as usable")
	}

	cfg.Providers = map[string]Provider{"inference": {Profile: "inference", APIKeyEnv: "INFERENCE_TEST_KEY"}}
	t.Setenv("INFERENCE_TEST_KEY", "sk-inference")
	if !cfg.AnyProviderConfigured() || !cfg.AnyProviderUsable(profiles) {
		t.Fatal("a configured provider should count as usable")
	}
}

func writeProviderTestProfile(t *testing.T, dir, id, authKind string) {
	t.Helper()
	data := "schema: 1\n" +
		"id: " + id + "\n" +
		"display_name: " + id + "\n" +
		"protocol: openai-chat-completions\n" +
		"base_url: https://local.example/v1\n" +
		"auth:\n  kind: " + authKind + "\n" +
		"default_headers: {}\n" +
		"catalog:\n  kind: none\n"
	if err := os.WriteFile(filepath.Join(dir, id+".yaml"), []byte(data), 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestTrimKey(t *testing.T) {
	for in, want := range map[string]string{
		"  sk-go-x\n":     "sk-go-x",
		"Bearer sk-go-x":  "sk-go-x",
		"Bearer  sk-go-x": "sk-go-x",
		"sk-go-x":         "sk-go-x",
		"":                "",
	} {
		if got := TrimKey(in); got != want {
			t.Errorf("TrimKey(%q) = %q, want %q", in, got, want)
		}
	}
}

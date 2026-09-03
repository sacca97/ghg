package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/sacca97/ghg/internal/config"
	"github.com/sacca97/ghg/internal/models"
)

func fakeProfileServer(t *testing.T, goodKey string) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		key := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
		if r.Method == http.MethodPost && r.URL.Path == "/chat/completions" {
			if key != goodKey {
				w.WriteHeader(http.StatusUnauthorized)
				_, _ = fmt.Fprintf(w, `{"error":{"message":"invalid key %s"}}`, key)
				return
			}
			w.WriteHeader(http.StatusBadRequest)
			_, _ = fmt.Fprint(w, `{"error":{"message":"invalid model"}}`)
			return
		}
		if r.URL.Path != "/models" {
			http.NotFound(w, r)
			return
		}
		if key != goodKey {
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = fmt.Fprintf(w, `{"error":{"message":"invalid key %s"}}`, key)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"data": []map[string]any{
				{"id": "gpt-go", "context_length": 400000, "input_modalities": []string{"text", "image"},
					"pricing": map[string]string{"prompt": "0.00000125", "completion": "0.00001"}},
				{"id": "claude-go", "context_length": 1000000, "input_modalities": []string{"text"}},
			},
		})
	}))
}

func writeAuthProfile(t *testing.T, home, id, baseURL, envVar, catalog string) {
	t.Helper()
	dir := filepath.Join(home, "providers")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	auth := "  kind: bearer\n  header: Authorization\n"
	if envVar != "" {
		auth += "  env_var: " + envVar + "\n"
	}
	data := fmt.Sprintf(`schema: 1
id: %s
display_name: %s
protocol: openai-chat-completions
base_url: %s
auth:
%sdocs:
  keys_url: https://example.com/%s/keys
default_headers:
  X-Profile-Test: enabled
catalog:
  kind: %s
capabilities:
  tools: true
`, id, id, baseURL, auth, id, catalog)
	if err := os.WriteFile(filepath.Join(dir, id+".yaml"), []byte(data), 0o600); err != nil {
		t.Fatal(err)
	}
}

func writeAuthNoneProfile(t *testing.T, home, id, baseURL string) {
	t.Helper()
	dir := filepath.Join(home, "providers")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	data := fmt.Sprintf(`schema: 1
id: %s
display_name: %s
protocol: openai-chat-completions
base_url: %s
auth:
  kind: none
default_headers: {}
catalog:
  kind: none
`, id, id, baseURL)
	if err := os.WriteFile(filepath.Join(dir, id+".yaml"), []byte(data), 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestAuthCLICustomProfileValidatesUpsertsAndSeedsCatalog(t *testing.T) {
	home := t.TempDir()
	t.Setenv("GHG_HOME", home)
	t.Setenv("INFERENCE_API_KEY", "")
	srv := fakeProfileServer(t, "sk-go-good")
	defer srv.Close()
	writeAuthProfile(t, home, "opencode", srv.URL, "OPENCODE_GO_API_KEY", "openai-models")

	if err := authCLI([]string{"opencode", "sk-go-good"}); err != nil {
		t.Fatalf("auth failed: %v", err)
	}

	cfg, err := config.Load()
	if err != nil {
		t.Fatal(err)
	}
	p, ok := cfg.Providers["opencode"]
	if !ok {
		t.Fatal("custom provider was not saved")
	}
	if p.Name != "opencode" || p.Profile != "opencode" || p.BaseURL != srv.URL || p.API != "openai-chat-completions" {
		t.Errorf("unexpected provider shape: %+v", p)
	}
	if p.APIKey != "sk-go-good" || p.APIKeyEnv != "" {
		t.Errorf("literal mode should set apiKey only: %+v", p)
	}

	cat, ok := config.LoadCatalogs()["opencode"]
	if !ok || len(cat.Models) != 2 {
		t.Fatalf("custom catalog was not prefetched: %+v", config.LoadCatalogs())
	}
	if cat.ContextLength("gpt-go") != 400000 {
		t.Errorf("catalog capabilities were not carried: %+v", cat.Models)
	}
	route, err := cfg.Resolve("claude-go", "")
	if err != nil {
		t.Fatalf("catalog model should resolve: %v", err)
	}
	if len(route.Model.Providers) != 1 || route.Model.Providers[0] != "opencode" {
		t.Errorf("catalog model route: %+v", route.Model)
	}
}

func TestAuthCLIBadKeyWritesNothingAndRedactsError(t *testing.T) {
	home := t.TempDir()
	t.Setenv("GHG_HOME", home)
	srv := fakeProfileServer(t, "sk-go-good")
	defer srv.Close()
	writeAuthProfile(t, home, "opencode", srv.URL, "OPENCODE_GO_API_KEY", "openai-models")

	secret := "sk-echo-secret"
	err := authCLI([]string{"opencode", secret})
	if err == nil {
		t.Fatal("bad key should be rejected")
	}
	if strings.Contains(err.Error(), secret) {
		t.Fatalf("validation error leaked the key: %v", err)
	}
	cfg, loadErr := config.Load()
	if loadErr != nil {
		t.Fatal(loadErr)
	}
	if _, ok := cfg.Providers["opencode"]; ok {
		t.Error("rejected key must not leave a provider entry")
	}
	if cats := config.LoadCatalogs(); len(cats) != 0 {
		t.Errorf("rejected key must not write a catalog: %+v", cats)
	}
}

func TestAuthCLIEnvModeUsesProfileVariableAndPreservesOnFailedSwitch(t *testing.T) {
	home := t.TempDir()
	t.Setenv("GHG_HOME", home)
	srv := fakeProfileServer(t, "sk-go-env")
	defer srv.Close()
	writeAuthProfile(t, home, "opencode", srv.URL, "OPENCODE_GO_API_KEY", "openai-models")
	t.Setenv("OPENCODE_GO_API_KEY", "sk-go-env")

	if err := authCLI([]string{"opencode", "--env"}); err != nil {
		t.Fatalf("env auth failed: %v", err)
	}
	cfg, _ := config.Load()
	p := cfg.Providers["opencode"]
	if p.APIKey != "" || p.APIKeyEnv != "OPENCODE_GO_API_KEY" {
		t.Errorf("env mode should set only profile env var: %+v", p)
	}

	if err := authCLI([]string{"opencode", "sk-go-literal"}); err == nil {
		t.Fatal("unexpected success for a rejected replacement key")
	}
	cfg, _ = config.Load()
	p = cfg.Providers["opencode"]
	if p.APIKey != "" || p.APIKeyEnv != "OPENCODE_GO_API_KEY" {
		t.Errorf("failed re-auth should preserve env mode: %+v", p)
	}
}

func TestAuthCLIEnvModeRequiresDeclaredVariable(t *testing.T) {
	home := t.TempDir()
	t.Setenv("GHG_HOME", home)
	writeAuthProfile(t, home, "no-env", "https://example.com/v1", "", "none")
	if err := authCLI([]string{"no-env", "--env"}); err == nil || !strings.Contains(err.Error(), "does not declare auth.env_var") {
		t.Fatalf("missing profile env var should be an error: %v", err)
	}
}

func TestAuthCLICatalogNoneUsesProbeWithoutCatalog(t *testing.T) {
	home := t.TempDir()
	t.Setenv("GHG_HOME", home)
	srv := fakeProfileServer(t, "sk-probe")
	defer srv.Close()
	writeAuthProfile(t, home, "probe-only", srv.URL, "PROBE_ONLY_KEY", "none")

	if err := authCLI([]string{"probe-only", "sk-probe"}); err != nil {
		t.Fatalf("catalog-less auth should use probe: %v", err)
	}
	cfg, _ := config.Load()
	if _, ok := cfg.Providers["probe-only"]; !ok {
		t.Fatal("probe-authenticated provider was not saved")
	}
	if cats := config.LoadCatalogs(); len(cats) != 0 {
		t.Errorf("catalog:none must not write a catalog: %+v", cats)
	}
}

func TestAuthCLIUnknownProfileListsIDs(t *testing.T) {
	home := t.TempDir()
	t.Setenv("GHG_HOME", home)
	writeAuthProfile(t, home, "opencode", "https://example.com/v1", "OPENCODE_GO_API_KEY", "none")
	err := authCLI([]string{"missing"})
	if err == nil || !strings.Contains(err.Error(), "opencode") || !strings.Contains(err.Error(), "anthropic") {
		t.Fatalf("unknown profile should list available IDs: %v", err)
	}
}

func TestAuthCLIRejectsAuthNone(t *testing.T) {
	home := t.TempDir()
	t.Setenv("GHG_HOME", home)
	writeAuthNoneProfile(t, home, "local", "http://127.0.0.1:9999/v1")
	err := authCLI([]string{"local"})
	if err == nil || !strings.Contains(err.Error(), "takes no API key") {
		t.Fatalf("auth:none profile should refuse a key: %v", err)
	}
}

func TestShellRC(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	type tc struct{ shell, want string }
	for _, c := range []tc{
		{"/bin/zsh", home + "/.zshrc"},
		{"/usr/bin/bash", home + "/.bashrc"},
		{"/bin/fish", ""},
		{"", ""},
	} {
		t.Setenv("SHELL", c.shell)
		if got := shellRC(); got != c.want {
			t.Errorf("SHELL=%q: got %q, want %q", c.shell, got, c.want)
		}
	}
}

func TestAuthOAuthCLIDiscoveryFailure(t *testing.T) {
	home := t.TempDir()
	t.Setenv("GHG_HOME", home)
	profiles, err := models.Load(models.LoadOptions{UserDir: filepath.Join(home, "providers")})
	if err != nil {
		t.Fatal(err)
	}
	resolved, err := profiles.Resolve(models.Instance{
		Name:    "codex-subscription",
		Profile: "codex-subscription",
		BaseURL: "http://127.0.0.1:9999/unreachable",
	})
	if err != nil {
		t.Fatal(err)
	}

	if err := configureOAuthPostLogin("codex-subscription", resolved); err != nil {
		t.Fatalf("configureOAuthPostLogin should succeed even when discovery fails: %v", err)
	}
	cfg, _ := config.Load()
	if _, ok := cfg.Providers["codex-subscription"]; !ok {
		t.Fatal("OAuth provider was not saved to config")
	}
}

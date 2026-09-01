package provider

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestLoadEmbeddedProfiles(t *testing.T) {
	profiles, err := Load(LoadOptions{UserDir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"anthropic", "commandcode", "generic-openai", "inference", "openrouter", "opencode"} {
		if _, ok := profiles.Lookup(id); !ok {
			t.Fatalf("embedded profile %q missing; ids=%v", id, profiles.IDs())
		}
	}
	p, ok := profiles.Lookup("openrouter")
	if !ok {
		t.Fatal("openrouter profile missing")
	}
	if p.Protocol != ProtocolOpenAIChatCompletions || p.BaseURL != "https://openrouter.ai/api/v1" {
		t.Fatalf("openrouter profile: %+v", p)
	}
	if p.Auth.Kind != AuthBearer || p.Auth.Header != "Authorization" {
		t.Fatalf("openrouter auth: %+v", p.Auth)
	}
	if p.Auth.EnvVar != "OPENROUTER_API_KEY" || p.Docs.KeysURL != "https://openrouter.ai/keys" {
		t.Fatalf("openrouter auth/docs metadata: %+v / %+v", p.Auth, p.Docs)
	}
	opencode, ok := profiles.Lookup("opencode")
	if !ok || opencode.Protocol != ProtocolOpenAIChatCompletions || !opencode.Catalog.Public || opencode.Catalog.ModelsDev != "opencode-go" || opencode.Auth.Kind != AuthBearer || len(opencode.Routes) != 2 {
		t.Fatalf("opencode profile: %+v", opencode)
	}
	if opencode.Routes[0].Protocol != ProtocolAnthropicMessages || opencode.Routes[0].Auth.Header != "x-api-key" || opencode.Routes[0].DefaultHeaders["anthropic-version"] != "2023-06-01" {
		t.Fatalf("opencode anthropic route: %+v", opencode.Routes[0])
	}
	if opencode.Routes[1].Protocol != ProtocolOpenAIResponses {
		t.Fatalf("opencode responses route: %+v", opencode.Routes[1])
	}
	commandcode, ok := profiles.Lookup("commandcode")
	if !ok {
		t.Fatal("commandcode profile missing")
	}
	if commandcode.BaseURL != "https://api.commandcode.ai/provider/v1" || commandcode.Auth.Kind != AuthBearer || commandcode.Auth.Header != "Authorization" || commandcode.Auth.EnvVar != "CMD_API_KEY" {
		t.Fatalf("commandcode profile/auth: %+v", commandcode)
	}
	if commandcode.Docs.KeysURL != "https://commandcode.ai/settings/keys" || commandcode.Catalog.Kind != CatalogOpenAIModels || !commandcode.Catalog.Public {
		t.Fatalf("commandcode docs/catalog: %+v", commandcode)
	}

	resolved, err := profiles.ResolveModel(Instance{Name: "commandcode", Profile: "commandcode"}, "claude-sonnet-4-6")
	if err != nil {
		t.Fatal(err)
	}
	if resolved.Protocol != ProtocolAnthropicMessages || resolved.Auth.Kind != AuthBearer || resolved.Auth.Header != "Authorization" || resolved.Auth.EnvVar != "CMD_API_KEY" || resolved.DefaultHeaders["anthropic-version"] != "2023-06-01" {
		t.Fatalf("commandcode Claude route: %+v", resolved)
	}

	resolved, err = profiles.ResolveModel(Instance{Name: "commandcode", Profile: "commandcode"}, "deepseek/deepseek-v4-flash")
	if err != nil {
		t.Fatal(err)
	}
	if resolved.Protocol != ProtocolOpenAIChatCompletions {
		t.Fatalf("commandcode non-Claude route: %+v", resolved)
	}
}

func TestProfilePrecedence(t *testing.T) {
	userDir, projectDir := t.TempDir(), t.TempDir()
	writeProfileFile(t, userDir, "user.yaml", profileYAML("openrouter", "https://user.example/v1"))
	writeProfileFile(t, projectDir, "project.yaml", profileYAML("openrouter", "https://project.example/v1"))

	profiles, err := Load(LoadOptions{UserDir: userDir, ProjectDir: projectDir})
	if err != nil {
		t.Fatal(err)
	}
	p, _ := profiles.Lookup("openrouter")
	if p.BaseURL != "https://user.example/v1" {
		t.Fatalf("untrusted project must not override user profile: %q", p.BaseURL)
	}

	profiles, err = Load(LoadOptions{UserDir: userDir, ProjectDir: projectDir, ProjectTrusted: true})
	if err != nil {
		t.Fatal(err)
	}
	p, _ = profiles.Lookup("openrouter")
	if p.BaseURL != "https://project.example/v1" {
		t.Fatalf("trusted project should override user profile: %q", p.BaseURL)
	}
}

func TestResolveModelRoutesFirstMatchAndProfileDefaults(t *testing.T) {
	dir := t.TempDir()
	data := strings.Join([]string{
		"schema: 1",
		"id: routed",
		"display_name: routed",
		"protocol: openai-chat-completions",
		"base_url: https://routes.example/v1",
		"auth:",
		"  kind: bearer",
		"  header: Authorization",
		"  env_var: ROUTED_KEY",
		"default_headers:",
		"  x-profile: enabled",
		"routes:",
		"  - models: [\"anthropic-*\", \"overlap-*\"]",
		"    protocol: anthropic-messages",
		"    auth:",
		"      kind: header",
		"      header: x-api-key",
		"    default_headers:",
		"      anthropic-version: \"2023-06-01\"",
		"  - models: [\"overlap-*\"]",
		"    protocol: openai-responses",
		"catalog:",
		"  kind: none",
		"",
	}, "\n")
	if err := os.WriteFile(filepath.Join(dir, "routed.yaml"), []byte(data), 0o600); err != nil {
		t.Fatal(err)
	}
	profiles, err := Load(LoadOptions{UserDir: dir})
	if err != nil {
		t.Fatal(err)
	}

	resolved, err := profiles.ResolveModel(Instance{Name: "routed", Profile: "routed"}, "anthropic-model")
	if err != nil {
		t.Fatal(err)
	}
	if resolved.Protocol != ProtocolAnthropicMessages || resolved.Auth.Kind != AuthHeader || resolved.Auth.Header != "x-api-key" || resolved.Auth.EnvVar != "ROUTED_KEY" {
		t.Fatalf("anthropic route did not override request auth: %+v", resolved)
	}
	if resolved.BaseURL != "https://routes.example/v1" || resolved.DefaultHeaders["x-profile"] != "enabled" || resolved.DefaultHeaders["anthropic-version"] != "2023-06-01" {
		t.Fatalf("route should retain profile defaults and add headers: %+v", resolved)
	}

	resolved, err = profiles.ResolveModel(Instance{Name: "routed", Profile: "routed"}, "overlap-model")
	if err != nil {
		t.Fatal(err)
	}
	if resolved.Protocol != ProtocolAnthropicMessages {
		t.Fatalf("first matching route should win: %+v", resolved)
	}

	resolved, err = profiles.ResolveModel(Instance{Name: "routed", Profile: "routed"}, "plain-model")
	if err != nil {
		t.Fatal(err)
	}
	if resolved.Protocol != ProtocolOpenAIChatCompletions || resolved.Auth.Kind != AuthBearer || resolved.Auth.Header != "Authorization" || resolved.DefaultHeaders["anthropic-version"] != "" {
		t.Fatalf("unmatched model should use profile defaults: %+v", resolved)
	}
}

func TestProfileRouteValidationRejectsProviderFieldsAndBadPatterns(t *testing.T) {
	tests := []struct {
		name  string
		route []string
		want  string
	}{
		{name: "base url", route: []string{"  - models: [\"*\"]", "    base_url: https://other.example/v1", "    protocol: anthropic-messages"}, want: "base_url"},
		{name: "docs", route: []string{"  - models: [\"*\"]", "    docs: {}", "    protocol: anthropic-messages"}, want: "docs"},
		{name: "catalog", route: []string{"  - models: [\"*\"]", "    catalog: {}", "    protocol: anthropic-messages"}, want: "catalog"},
		{name: "environment", route: []string{"  - models: [\"*\"]", "    auth:", "      env_var: OTHER_KEY", "    protocol: anthropic-messages"}, want: "auth.env_var"},
		{name: "invalid pattern", route: []string{"  - models: [\"[\"]", "    protocol: anthropic-messages"}, want: "model pattern"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			data := profileYAML("routed", "https://routes.example/v1") + strings.Join(append([]string{"routes:"}, tt.route...), "\n") + "\n"
			if _, err := parseProfile([]byte(data), "route.yaml"); err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("route validation error should contain %q: %v", tt.want, err)
			}
		})
	}
}

func TestDuplicateProfileIDsWithinLevel(t *testing.T) {
	dir := t.TempDir()
	writeProfileFile(t, dir, "one.yaml", profileYAML("custom", "https://one.example/v1"))
	writeProfileFile(t, dir, "two.yaml", profileYAML("custom", "https://two.example/v1"))

	_, err := Load(LoadOptions{UserDir: dir})
	if err == nil || !strings.Contains(err.Error(), "duplicate profile ID \"custom\"") {
		t.Fatalf("duplicate IDs should fail clearly: %v", err)
	}
	if !strings.Contains(err.Error(), "one.yaml") || !strings.Contains(err.Error(), "two.yaml") {
		t.Fatalf("duplicate error should name both sources: %v", err)
	}
}

func TestProfileUnknownFieldsAreSourceLocated(t *testing.T) {
	source := filepath.Join(t.TempDir(), "bad.yaml")
	data := profileYAML("custom", "https://custom.example/v1") + "\nunknown_field: true\n"
	_, err := parseProfile([]byte(data), source)
	if err == nil || !strings.Contains(err.Error(), source) || !strings.Contains(err.Error(), "unknown_field") {
		t.Fatalf("unknown field should include source and field: %v", err)
	}
}

func TestProfileRejectsSecretFields(t *testing.T) {
	source := "secret-profile.yaml"
	data := profileYAML("custom", "https://custom.example/v1") + "\napi_key: should-not-be-here\n"
	_, err := parseProfile([]byte(data), source)
	if err == nil || !strings.Contains(err.Error(), source) || !strings.Contains(err.Error(), "api_key") {
		t.Fatalf("secret field should be rejected at its source: %v", err)
	}

	data = strings.Replace(profileYAML("custom", "https://custom.example/v1"),
		"default_headers: {}", "default_headers:\n  Authorization: literal-secret", 1)
	_, err = parseProfile([]byte(data), source)
	if err == nil || !strings.Contains(err.Error(), "credential header") {
		t.Fatalf("credential default header should be rejected: %v", err)
	}
}

func TestProfileURLValidation(t *testing.T) {
	tests := []struct {
		name    string
		url     string
		want    string
		wantErr string
	}{
		{name: "trailing slash normalized", url: "https://example.com/v1/", want: "https://example.com/v1"},
		{name: "loopback ipv4", url: "http://127.0.0.1:8080/v1", want: "http://127.0.0.1:8080/v1"},
		{name: "loopback ipv6", url: "http://[::1]:8080/v1", want: "http://[::1]:8080/v1"},
		{name: "loopback hostname", url: "http://localhost/v1", want: "http://localhost/v1"},
		{name: "remote http", url: "http://example.com/v1", wantErr: "loopback"},
		{name: "wrong scheme", url: "ftp://example.com/v1", wantErr: "scheme"},
		{name: "query", url: "https://example.com/v1?token=secret", wantErr: "query"},
		{name: "userinfo", url: "https://user:pass@example.com/v1", wantErr: "userinfo"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			p, err := parseProfile([]byte(profileYAML("custom", tt.url)), "test.yaml")
			if tt.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("want error containing %q, got %v", tt.wantErr, err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if p.BaseURL != tt.want {
				t.Fatalf("normalized URL = %q, want %q", p.BaseURL, tt.want)
			}
		})
	}
}

func TestResolveAnonymousLegacyProvider(t *testing.T) {
	resolved, err := (Profiles{}).Resolve(Instance{
		Name:     "legacy",
		BaseURL:  "https://legacy.example/v1/",
		Protocol: "openai-completions",
	})
	if err != nil {
		t.Fatal(err)
	}
	if resolved.Protocol != ProtocolOpenAIChatCompletions || resolved.BaseURL != "https://legacy.example/v1" {
		t.Fatalf("legacy provider was not normalized: %+v", resolved)
	}
	if resolved.Profile.ID != "anonymous" || resolved.Auth.Kind != AuthBearer || resolved.RequiresAPIKey() == false {
		t.Fatalf("anonymous profile: %+v", resolved)
	}
}

func TestResolveProfileOverridesAndAuthNone(t *testing.T) {
	profiles, err := Load(LoadOptions{UserDir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	resolved, err := profiles.Resolve(Instance{
		Name:    "local",
		Profile: "generic-openai",
		BaseURL: "http://127.0.0.1:9000/v1/",
	})
	if err != nil {
		t.Fatal(err)
	}
	if resolved.BaseURL != "http://127.0.0.1:9000/v1" || resolved.Protocol != ProtocolOpenAIChatCompletions {
		t.Fatalf("profile override: %+v", resolved)
	}

	dir := t.TempDir()
	writeProfileFile(t, dir, "local.yaml", profileYAMLWithAuth("local", "https://local.example/v1", AuthNone, ""))
	profiles, err = Load(LoadOptions{UserDir: dir})
	if err != nil {
		t.Fatal(err)
	}
	resolved, err = profiles.Resolve(Instance{Name: "local", Profile: "local"})
	if err != nil {
		t.Fatal(err)
	}
	if resolved.RequiresAPIKey() {
		t.Fatal("auth none should not require a key")
	}
}

func TestProfileAuthAndDocsMetadataValidation(t *testing.T) {
	data := strings.Replace(profileYAML("custom", "https://custom.example/v1"),
		"  header: Authorization\n", "  header: Authorization\n  env_var: CUSTOM_API_KEY\n", 1)
	data = strings.Replace(data, "default_headers: {}", "docs:\n  keys_url: https://example.com/keys\ndefault_headers: {}", 1)
	p, err := parseProfile([]byte(data), "metadata.yaml")
	if err != nil {
		t.Fatal(err)
	}
	if p.Auth.EnvVar != "CUSTOM_API_KEY" || p.Docs.KeysURL != "https://example.com/keys" {
		t.Fatalf("metadata was not parsed: %+v / %+v", p.Auth, p.Docs)
	}
	public := strings.Replace(profileYAML("public", "https://public.example/v1"),
		"catalog:\n  kind: openai-models", "catalog:\n  kind: openai-models\n  public: true", 1)
	p, err = parseProfile([]byte(public), "metadata.yaml")
	if err != nil || !p.Catalog.Public {
		t.Fatalf("public catalog metadata was not parsed: %+v / %v", p.Catalog, err)
	}

	badEnv := strings.Replace(data, "CUSTOM_API_KEY", "CUSTOM-API-KEY", 1)
	if _, err := parseProfile([]byte(badEnv), "metadata.yaml"); err == nil || !strings.Contains(err.Error(), "auth.env_var") {
		t.Fatalf("invalid env var should fail: %v", err)
	}
	badDocs := strings.Replace(data, "https://example.com/keys", "http://example.com/keys", 1)
	if _, err := parseProfile([]byte(badDocs), "metadata.yaml"); err == nil || !strings.Contains(err.Error(), "keys_url") {
		t.Fatalf("unsafe docs URL should fail: %v", err)
	}
	badNone := strings.Replace(profileYAMLWithAuth("local", "http://127.0.0.1:9000/v1", AuthNone, ""),
		"default_headers: {}", "  env_var: LOCAL_KEY\ndefault_headers: {}", 1)
	if _, err := parseProfile([]byte(badNone), "metadata.yaml"); err == nil || !strings.Contains(err.Error(), "auth.env_var") {
		t.Fatalf("auth:none env var should fail: %v", err)
	}
}

func TestResolveUnknownProfileAndProtocol(t *testing.T) {
	profiles, err := Load(LoadOptions{UserDir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := profiles.Resolve(Instance{Name: "missing", Profile: "no-such-profile"}); err == nil || !strings.Contains(err.Error(), "no-such-profile") {
		t.Fatalf("unknown profile error: %v", err)
	}
	if _, err := (Profiles{}).Resolve(Instance{Name: "bad", BaseURL: "https://bad.example/v1", Protocol: "made-up"}); err == nil || !strings.Contains(err.Error(), "unknown protocol") {
		t.Fatalf("unknown protocol error: %v", err)
	}
}

func TestResolveRemovedOpenCodeAnthropicProfileAnonymously(t *testing.T) {
	profiles, err := Load(LoadOptions{UserDir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	resolved, err := profiles.Resolve(Instance{
		Name:     "old-opencode",
		Profile:  legacyOpenCodeAnthropicProfileID,
		BaseURL:  "https://opencode.example/v1",
		Protocol: ProtocolAnthropicMessages,
	})
	if err != nil {
		t.Fatal(err)
	}
	if resolved.Profile.ID != "anonymous" || resolved.Protocol != ProtocolAnthropicMessages || resolved.Auth.Header != "x-api-key" || resolved.DefaultHeaders["anthropic-version"] != "2023-06-01" {
		t.Fatalf("removed profile should keep its old anonymous route working: %+v", resolved)
	}
}

func writeProfileFile(t *testing.T, dir, name, content string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}

func profileYAML(id, baseURL string) string {
	return profileYAMLWithAuth(id, baseURL, AuthBearer, "Authorization")
}

func profileYAMLWithAuth(id, baseURL, authKind, authHeader string) string {
	header := ""
	if authHeader != "" {
		header = fmt.Sprintf("  header: %s\n", authHeader)
	}
	return fmt.Sprintf(`schema: 1
id: %s
display_name: %s
protocol: openai-chat-completions
base_url: %s
auth:
  kind: %s
%sdefault_headers: {}
catalog:
  kind: openai-models
capabilities:
  tools: true
  vision: true
  thinking: true
  prompt_cache: provider
`, id, id, baseURL, authKind, header)
}

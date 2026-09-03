package auth

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/sacca97/ghg/internal/models"
)

func authServer(t *testing.T, expectedKey string, status int) (*httptest.Server, *int32) {
	t.Helper()
	var requests int32
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&requests, 1)
		keyOK := r.Header.Get("Authorization") == "Bearer "+expectedKey && r.Header.Get("X-Profile-Test") == "enabled"
		if r.Method == http.MethodPost && r.URL.Path == "/chat/completions" {
			if !keyOK {
				w.WriteHeader(http.StatusUnauthorized)
				_, _ = fmt.Fprintf(w, `{"error":{"type":"AuthError","message":"bad credential %s"}}`, expectedKey)
				return
			}
			w.WriteHeader(http.StatusBadRequest)
			_, _ = fmt.Fprint(w, `{"error":{"type":"ModelError","message":"invalid model"}}`)
			return
		}
		if r.URL.Path != "/models" {
			http.NotFound(w, r)
			return
		}
		if !keyOK {
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = fmt.Fprintf(w, `{"error":{"type":"AuthError","message":"bad credential %s"}}`, expectedKey)
			return
		}
		if status != http.StatusOK {
			w.WriteHeader(status)
			_, _ = fmt.Fprintf(w, `{"error":{"type":"AuthError","message":"bad credential %s"}}`, expectedKey)
			return
		}
		_, _ = fmt.Fprint(w, `{"data":[{"id":"gpt-go","context_length":128000}]}`)
	})), &requests
}

func publicCatalogServer(t *testing.T, expectedKey string) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		keyOK := r.Header.Get("Authorization") == "Bearer "+expectedKey && r.Header.Get("X-Profile-Test") == "enabled"
		if r.Method == http.MethodGet && r.URL.Path == "/models" {
			// This endpoint is intentionally public: it must not validate a key.
			_, _ = fmt.Fprint(w, `{"data":[{"id":"gpt-go","context_length":128000}]}`)
			return
		}
		if r.Method == http.MethodPost && r.URL.Path == "/chat/completions" {
			var request struct {
				Model     string `json:"model"`
				MaxTokens int    `json:"max_tokens"`
			}
			if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
				http.Error(w, "invalid probe body", http.StatusBadRequest)
				return
			}
			if request.Model != "gpt-go" || request.MaxTokens != 1 {
				http.Error(w, "probe did not use the catalog model", http.StatusBadRequest)
				return
			}
			if !keyOK {
				w.WriteHeader(http.StatusUnauthorized)
				_, _ = fmt.Fprint(w, `{"error":{"type":"AuthError","message":"invalid key"}}`)
				return
			}
			w.WriteHeader(http.StatusBadRequest)
			_, _ = fmt.Fprint(w, `{"error":{"type":"ModelError","message":"invalid model"}}`)
			return
		}
		http.NotFound(w, r)
	}))
}

func loadAuthProfile(t *testing.T, dir, id, baseURL, catalog string) models.Profiles {
	t.Helper()
	catalogKind := catalog
	public := ""
	if catalog == "public-openai-models" {
		catalogKind = models.CatalogOpenAIModels
		public = "  public: true\n"
	}
	data := fmt.Sprintf(`schema: 1
id: %s
display_name: %s
protocol: openai-chat-completions
base_url: %s
auth:
  kind: bearer
  header: Authorization
  env_var: %s_KEY
docs:
  keys_url: https://example.com/%s/keys
default_headers:
  X-Profile-Test: enabled
catalog:
  kind: %s
%scapabilities:
  tools: true
`, id, id, baseURL, strings.ToUpper(strings.ReplaceAll(id, "-", "_")), id, catalogKind, public)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, id+".yaml"), []byte(data), 0o600); err != nil {
		t.Fatal(err)
	}
	profiles, err := models.Load(models.LoadOptions{UserDir: dir})
	if err != nil {
		t.Fatal(err)
	}
	return profiles
}

func TestAuthenticateCatalogUsesOneValidatedResponse(t *testing.T) {
	server, requests := authServer(t, "sk-go", http.StatusOK)
	defer server.Close()
	profiles := loadAuthProfile(t, t.TempDir(), "opencode", server.URL, models.CatalogOpenAIModels)

	result, err := Authenticate(context.Background(), profiles, "opencode", " Bearer sk-go ", 1)
	if err != nil {
		t.Fatalf("authentication failed: %v", err)
	}
	if !result.Validated || result.NeedsConfirmation || len(result.Models) != 1 {
		t.Fatalf("unexpected authentication result: %+v", result)
	}
	if result.Profile.Auth.EnvVar != "OPENCODE_KEY" || result.Profile.Docs.KeysURL == "" {
		t.Errorf("profile metadata was not carried through: %+v", result.Profile)
	}
	if got := atomic.LoadInt32(requests); got != 1 {
		t.Errorf("catalog validation should make one request, got %d", got)
	}
}

func TestAuthenticateCatalogNoneUsesProbeWithoutModels(t *testing.T) {
	server, requests := authServer(t, "sk-probe", http.StatusOK)
	defer server.Close()
	profiles := loadAuthProfile(t, t.TempDir(), "probe-only", server.URL, models.CatalogNone)

	result, err := Authenticate(context.Background(), profiles, "probe-only", "sk-probe", 1)
	if err != nil {
		t.Fatalf("catalog-less authentication failed: %v", err)
	}
	if !result.Validated || result.NeedsConfirmation || len(result.Models) != 0 {
		t.Fatalf("probe should validate without returning a catalog: %+v", result)
	}
	if got := atomic.LoadInt32(requests); got != 1 {
		t.Errorf("probe should make one request, got %d", got)
	}
}

func TestAuthenticatePublicCatalogFetchesRealModelBeforeProbe(t *testing.T) {
	server := publicCatalogServer(t, "sk-public")
	defer server.Close()
	profiles := loadAuthProfile(t, t.TempDir(), "public-catalog", server.URL, "public-openai-models")

	result, err := Authenticate(context.Background(), profiles, "public-catalog", "garbage", 1)
	if err == nil || !strings.Contains(err.Error(), "validation failed") {
		t.Fatalf("public catalog must reject a garbage key: %v", err)
	}
	result, err = Authenticate(context.Background(), profiles, "public-catalog", "sk-public", 1)
	if err != nil || !result.Validated || len(result.Models) != 1 {
		t.Fatalf("valid key should probe then seed public catalog: %+v / %v", result, err)
	}
}

func TestAuthenticatePublicCatalogUsesModelRoute(t *testing.T) {
	var modelsCalls, messagesCalls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/models":
			modelsCalls.Add(1)
			_, _ = fmt.Fprint(w, `{"data":[{"id":"gpt-go"}]}`)
		case r.Method == http.MethodPost && r.URL.Path == "/messages":
			messagesCalls.Add(1)
			if got := r.Header.Get("x-api-key"); got != "sk-route" {
				http.Error(w, "wrong x-api-key", http.StatusUnauthorized)
				return
			}
			if got := r.Header.Get("Authorization"); got != "" {
				http.Error(w, "unexpected authorization", http.StatusBadRequest)
				return
			}
			if got := r.Header.Get("anthropic-version"); got != "2023-06-01" {
				http.Error(w, "wrong anthropic version", http.StatusBadRequest)
				return
			}
			var body struct {
				Model     string `json:"model"`
				MaxTokens int    `json:"max_tokens"`
			}
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil || body.Model != "gpt-go" || body.MaxTokens != 1 {
				http.Error(w, "wrong probe body", http.StatusBadRequest)
				return
			}
			_, _ = fmt.Fprint(w, `{}`)
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	dir := t.TempDir()
	data := fmt.Sprintf(`schema: 1
id: routed-public
display_name: Routed Public
protocol: openai-chat-completions
base_url: %s
auth:
  kind: bearer
  header: Authorization
  env_var: ROUTED_PUBLIC_KEY
default_headers: {}
routes:
  - models: ["gpt-go"]
    protocol: anthropic-messages
    auth:
      kind: header
      header: x-api-key
    default_headers:
      anthropic-version: "2023-06-01"
catalog:
  kind: openai-models
  public: true
`, server.URL)
	if err := os.WriteFile(filepath.Join(dir, "routed-public.yaml"), []byte(data), 0o600); err != nil {
		t.Fatal(err)
	}
	profiles, err := models.Load(models.LoadOptions{UserDir: dir})
	if err != nil {
		t.Fatal(err)
	}

	result, err := Authenticate(context.Background(), profiles, "routed-public", "sk-route", 1)
	if err != nil || !result.Validated {
		t.Fatalf("routed public auth failed: %+v / %v", result, err)
	}
	if modelsCalls.Load() != 1 || messagesCalls.Load() != 1 {
		t.Fatalf("requests = models:%d messages:%d, want one of each", modelsCalls.Load(), messagesCalls.Load())
	}
}

func TestAuthenticateRedactsCredentialFromValidationError(t *testing.T) {
	secret := "sk-echo-secret"
	server, _ := authServer(t, secret, http.StatusUnauthorized)
	defer server.Close()
	profiles := loadAuthProfile(t, t.TempDir(), "opencode", server.URL, models.CatalogOpenAIModels)

	_, err := Authenticate(context.Background(), profiles, "opencode", secret, 1)
	if err == nil || !strings.Contains(err.Error(), "validation failed") {
		t.Fatalf("expected a validation error: %v", err)
	}
	if strings.Contains(err.Error(), secret) {
		t.Fatalf("validation error leaked credential: %v", err)
	}
}

func TestResolveProfileReportsAvailableIDsAndAuthNoneRefusal(t *testing.T) {
	profiles := models.Profiles{}
	if _, err := ResolveProfile(profiles, "missing"); err == nil || !strings.Contains(err.Error(), "available: none") {
		t.Fatalf("unknown profile should list IDs: %v", err)
	}

	dir := t.TempDir()
	data := `schema: 1
id: local
display_name: local
protocol: openai-chat-completions
base_url: http://127.0.0.1:9999/v1
auth:
  kind: none
default_headers: {}
catalog:
  kind: none
`
	if err := os.WriteFile(filepath.Join(dir, "local.yaml"), []byte(data), 0o600); err != nil {
		t.Fatal(err)
	}
	loaded, err := models.Load(models.LoadOptions{UserDir: dir})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Authenticate(context.Background(), loaded, "local", "ignored", 1); err == nil || !strings.Contains(err.Error(), "takes no API key") {
		t.Fatalf("auth:none should refuse credentials: %v", err)
	}
}

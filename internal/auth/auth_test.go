package auth

import (
	"context"
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

func authServer(t *testing.T, expectedKey string, status int) (*authTestServer, *int32) {
	t.Helper()
	var requests int32
	return newAuthTestServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
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

func loadAuthProfile(t *testing.T, dir, id, baseURL, catalog string) models.Profiles {
	t.Helper()
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
capabilities:
  tools: true
`, id, id, baseURL, strings.ToUpper(strings.ReplaceAll(id, "-", "_")), id, catalog)
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

	result, err := authenticate(context.Background(), profiles, "opencode", " Bearer sk-go ", 1, server.Client())
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

func TestAuthenticateRedactsCredentialFromValidationError(t *testing.T) {
	secret := "sk-echo-secret"
	server, _ := authServer(t, secret, http.StatusUnauthorized)
	defer server.Close()
	profiles := loadAuthProfile(t, t.TempDir(), "opencode", server.URL, models.CatalogOpenAIModels)

	_, err := authenticate(context.Background(), profiles, "opencode", secret, 1, server.Client())
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

type authTestServer struct {
	URL    string
	client *http.Client
}

func newAuthTestServer(handler http.Handler) *authTestServer {
	return &authTestServer{
		URL:    "https://auth-test.invalid",
		client: &http.Client{Transport: authHandlerTransport{handler: handler}},
	}
}

func (s *authTestServer) Client() *http.Client { return s.client }
func (s *authTestServer) Close()               {}

type authHandlerTransport struct{ handler http.Handler }

func (h authHandlerTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	recorder := httptest.NewRecorder()
	h.handler.ServeHTTP(recorder, req)
	return recorder.Result(), nil
}

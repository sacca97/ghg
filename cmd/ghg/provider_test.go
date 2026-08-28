package main

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/sacca97/ghg/internal/config"
	"github.com/sacca97/ghg/internal/llm"
	"github.com/sacca97/ghg/internal/provider"
)

func TestNewProviderBackendUsesProfileTransportPolicy(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("x-api-key"); got != "secret" {
			t.Errorf("auth header = %q, want secret", got)
		}
		if got := r.Header.Get("x-profile"); got != "yes" {
			t.Errorf("profile header = %q, want yes", got)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"data":[{"id":"model"}]}`))
	}))
	defer srv.Close()

	profileDir := t.TempDir()
	profile := fmt.Sprintf(`schema: 1
id: lab
display_name: Lab
protocol: openai-chat-completions
base_url: %s
auth:
  kind: header
  header: x-api-key
default_headers:
  x-profile: yes
catalog:
  kind: openai-models
capabilities:
  tools: true
  vision: false
  thinking: false
  prompt_cache: none
`, srv.URL)
	if err := os.WriteFile(filepath.Join(profileDir, "lab.yaml"), []byte(profile), 0o600); err != nil {
		t.Fatal(err)
	}
	profiles, err := provider.Load(provider.LoadOptions{UserDir: profileDir})
	if err != nil {
		t.Fatal(err)
	}
	backend, err := newProviderBackend(profiles, "lab", config.Provider{Profile: "lab"}, "secret", 1, "model", "")
	if err != nil {
		t.Fatal(err)
	}
	catalog, ok := backend.(llm.CatalogBackend)
	if !ok {
		t.Fatal("profile-selected backend should expose catalog capability")
	}
	if _, err := catalog.Models(context.Background()); err != nil {
		t.Fatal(err)
	}
}

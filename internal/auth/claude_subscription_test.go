package auth

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestClaudeCredentialManagerAuthorizesStoredToken(t *testing.T) {
	dir := t.TempDir()
	data := `{"access_token":"claude-test-token","refresh_token":"refresh-token","expires_at":"` + time.Now().Add(time.Hour).UTC().Format(time.RFC3339) + `"}`
	if err := os.WriteFile(filepath.Join(dir, "claude-subscription.json"), []byte(data), 0o600); err != nil {
		t.Fatal(err)
	}

	manager := NewClaudeCredentialManager(dir)
	req := httptest.NewRequest(http.MethodGet, "https://example.test", nil)
	if err := manager.Authorize(req); err != nil {
		t.Fatal(err)
	}
	if got := req.Header.Get("Authorization"); got != "Bearer claude-test-token" {
		t.Fatalf("authorization header = %q", got)
	}
	if got := req.Header.Get("anthropic-beta"); got != claudeOAuthBeta {
		t.Fatalf("anthropic-beta = %q, want %q", got, claudeOAuthBeta)
	}
	if got := req.Header.Get("User-Agent"); !strings.Contains(got, "ghg/") {
		t.Fatalf("User-Agent = %q", got)
	}
	status, err := manager.Status(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if !status.Configured || status.Expired {
		t.Fatalf("status = %+v", status)
	}
}

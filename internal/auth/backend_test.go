package auth

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/sacca97/ghg/internal/models"
)

func TestNewBackendUsesResolvedAuthAndHeaders(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("x-api-key"); got != "secret" {
			t.Errorf("auth header = %q, want secret", got)
		}
		if got := r.Header.Get("x-profile"); got != "yes" {
			t.Errorf("profile header = %q, want yes", got)
		}
		_, _ = fmt.Fprint(w, `{"data":[{"id":"model"}]}`)
	}))
	defer server.Close()

	backend, err := NewBackend(models.Resolved{
		Name: server.URL, BaseURL: server.URL,
		Protocol:       models.ProtocolOpenAIChatCompletions,
		Auth:           models.Auth{Kind: models.AuthHeader, Header: "x-api-key"},
		DefaultHeaders: map[string]string{"x-profile": "yes"},
	}, "secret", "", 1)
	if err != nil {
		t.Fatal(err)
	}
	catalog, ok := backend.(models.CatalogBackend)
	if !ok {
		t.Fatalf("backend = %T, want catalog capability", backend)
	}
	if _, err := catalog.Models(context.Background()); err != nil {
		t.Fatal(err)
	}
}

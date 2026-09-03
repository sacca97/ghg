package config

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func modelsDevTestServer(t *testing.T, handler http.Handler) *httptest.Server {
	t.Helper()
	defer func() {
		if recovered := recover(); recovered != nil {
			if strings.Contains(fmt.Sprint(recovered), "operation not permitted") {
				t.Skip("loopback listener not permitted by sandbox environment")
			}
			panic(recovered)
		}
	}()
	return httptest.NewServer(handler)
}

func TestParseModelsDevContext(t *testing.T) {
	data := []byte(`{
		"opencode": {"models": {
			"grok-4": {"id": "grok-4", "limit": {"context": 131072}},
			"deepseek-v4-flash": {"limit": {"context": 1048576}, "reasoning_options": [
				{"type": "toggle"}, {"type": "effort", "values": ["high", "max"]}
			]},
			"qwen3.8-max": {"id": "qwen3.8-max", "limit": {"context": 1000000}, "reasoning_options": [
				{"type": "effort", "values": ["low", "medium", "high", "max"]}
			]},
			"mimo-v2.5-pro": {"id": "mimo-v2.5-pro", "limit": {"context": 262144}, "reasoning_options": [
				{"type": "effort", "values": ["none", "high"]}
			]},
			"glm-5.3": {"id": "glm-5.3", "reasoning_options": [
				{"type": "effort", "values": ["high", "max"]}
			]},
			"glm-5.3-flash": {"id": "glm-5.3-flash", "reasoning_options": [
				{"type": "effort", "values": ["low", "high"]}
			]},
			"toggle-only": {"reasoning_options": [{"type": "toggle"}]},
			"no-controls": {"reasoning_options": []},
			"missing": {"limit": {"context": 0}},
			"ignored": {"limit": {"context": 42}}
		}},
		"openai": {"models": {
			"grok-4": {"id": "grok-4", "limit": {"context": 262144}},
			"unique": {"limit": {"context": 999999}}
		}}
	}`)

	wanted := map[string]struct{}{
		"grok-4": {}, "unique": {}, "deepseek-v4-flash": {},
		"qwen3.8-max": {}, "mimo-v2.5-pro": {}, "glm-5.3": {}, "glm-5.3-flash": {},
		"toggle-only": {}, "no-controls": {},
	}
	cache, err := parseModelsDev(data, wanted)
	if err != nil {
		t.Fatal(err)
	}
	if got := cache.ContextLength("grok-4", "opencode"); got != 131072 {
		t.Fatalf("provider-specific context = %d, want 131072", got)
	}
	if got := cache.ContextLength("unique", "private-gateway"); got != 999999 {
		t.Fatalf("unique fallback context = %d, want 999999", got)
	}
	if got := cache.ContextLength("grok-4", "private-gateway"); got != 0 {
		t.Fatalf("conflicting fallback context = %d, want 0", got)
	}
	if got := cache.ContextLength("missing", "opencode"); got != 0 {
		t.Fatalf("missing context = %d, want 0", got)
	}
	if got := cache.ContextLength("ignored", "opencode"); got != 0 {
		t.Fatalf("unwanted context = %d, want 0", got)
	}
	if got, ok := cache.ReasoningFor("deepseek-v4-flash", "opencode"); !ok || !got.Toggle || len(got.Efforts) != 2 || got.Efforts[0] != "high" || got.Efforts[1] != "max" {
		t.Fatalf("deepseek reasoning = %+v, %v", got, ok)
	}
	if got := cache.ContextLength("qwen3.8-max", "opencode"); got != 1000000 {
		t.Fatalf("exact model context = %d, want 1000000", got)
	}
	if got, ok := cache.ReasoningFor("qwen3.8-max", "opencode"); !ok || strings.Join(got.Efforts, ",") != "low,medium,high,max" {
		t.Fatalf("exact model reasoning = %+v, %v", got, ok)
	}
	if got := cache.ContextLength("mimo-v2.5-pro", "opencode"); got != 262144 {
		t.Fatalf("exact model context = %d, want 262144", got)
	}
	if got, ok := cache.ReasoningFor("mimo-v2.5-pro", "opencode"); !ok || strings.Join(got.Efforts, ",") != "none,high" {
		t.Fatalf("exact model reasoning = %+v, %v", got, ok)
	}
	if got, ok := cache.ReasoningFor("glm-5.3", "opencode"); !ok || strings.Join(got.Efforts, ",") != "high,max" {
		t.Fatalf("exact GLM reasoning = %+v, %v", got, ok)
	}
	if got, ok := cache.ReasoningFor("glm-5.3-flash", "opencode"); !ok || strings.Join(got.Efforts, ",") != "low,high" {
		t.Fatalf("exact GLM flash reasoning = %+v, %v", got, ok)
	}
	if got, ok := cache.ReasoningFor("toggle-only", "opencode"); !ok || !got.Toggle || len(got.Efforts) != 0 {
		t.Fatalf("toggle-only reasoning = %+v, %v", got, ok)
	}
	if got, ok := cache.ReasoningFor("no-controls", "opencode"); !ok || got.Toggle || len(got.Efforts) != 0 {
		t.Fatalf("no-controls reasoning = %+v, %v", got, ok)
	}
	if !cache.HasModel("toggle-only") || cache.HasModel("not-wanted") {
		t.Fatal("model presence tracking")
	}
	if !cache.FetchedAt.After(time.Time{}) {
		t.Fatal("parsed cache should carry a fetch timestamp")
	}
}

func TestWantedModelIDsRequiresExactMatch(t *testing.T) {
	wanted := map[string]struct{}{"qwen3.8-max": {}}
	got := wantedModelIDs(wanted, "qwen3.8-max:thinking", "Qwen3.8-Max", "qwen3.8-max")
	if len(got) != 1 || got[0] != "qwen3.8-max" {
		t.Fatalf("exact model matching = %v", got)
	}
}

func TestModelsDevCacheRoundTrip(t *testing.T) {
	t.Setenv("GHG_HOME", t.TempDir())
	want := ModelsDevCache{
		FetchedAt: time.Now().Add(-time.Minute),
		Providers: map[string]map[string]int{
			"opencode": {"grok-4": 131072},
		},
		Reasoning: map[string]map[string]ModelsDevReasoning{
			"opencode": {"grok-4": {Toggle: true, Efforts: []string{"high", "max"}}},
		},
	}
	if err := SaveModelsDev(want); err != nil {
		t.Fatal(err)
	}
	got := LoadModelsDev()
	if got.ContextLength("grok-4", "opencode") != 131072 {
		t.Fatalf("round-trip context = %d", got.ContextLength("grok-4", "opencode"))
	}
	if info, ok := got.ReasoningFor("grok-4", "opencode"); !ok || !info.Toggle || len(info.Efforts) != 2 {
		t.Fatalf("round-trip reasoning = %+v, %v", info, ok)
	}
	if got.Stale() {
		t.Fatal("recent cache should not be stale")
	}
	if !(ModelsDevCache{}).Stale() {
		t.Fatal("empty cache should be stale")
	}
}

func TestModelsDevCacheUsesDailyTTL(t *testing.T) {
	if (ModelsDevCache{FetchedAt: time.Now().Add(-23 * time.Hour)}).Stale() {
		t.Fatal("23-hour-old cache should still be fresh")
	}
	if !(ModelsDevCache{FetchedAt: time.Now().Add(-25 * time.Hour)}).Stale() {
		t.Fatal("25-hour-old cache should be stale")
	}
}

func TestModelsDevReasoningFallbackRequiresAgreement(t *testing.T) {
	cache := ModelsDevCache{
		Reasoning: map[string]map[string]ModelsDevReasoning{
			"one":   {"model": {Efforts: []string{"low", "max"}}},
			"two":   {"model": {Efforts: []string{"low", "max"}}},
			"three": {"model": {Toggle: true}},
		},
	}
	if got, ok := cache.ReasoningFor("model", "private"); ok || got.Efforts != nil || got.Toggle {
		t.Fatalf("conflicting fallback = %+v, %v", got, ok)
	}
	if got, ok := cache.ReasoningFor("model", "two"); !ok || !sameModelsDevReasoning(got, ModelsDevReasoning{Efforts: []string{"low", "max"}}) {
		t.Fatalf("provider-specific fallback = %+v, %v", got, ok)
	}
}

func TestFetchModelsDev(t *testing.T) {
	server := modelsDevTestServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Accept") != "application/json" {
			t.Errorf("Accept = %q", r.Header.Get("Accept"))
		}
		if r.Header.Get("User-Agent") != "ghg/models-dev" {
			t.Errorf("User-Agent = %q", r.Header.Get("User-Agent"))
		}
		_, _ = w.Write([]byte(`{"opencode":{"models":{"grok-4":{"limit":{"context":131072},"reasoning_options":[{"type":"effort","values":["low",null,"max"]}]}}}}`))
	}))
	defer server.Close()

	cache, err := fetchModelsDev(context.Background(), server.Client(), server.URL, map[string]struct{}{"grok-4": {}})
	if err != nil {
		t.Fatal(err)
	}
	if got := cache.ContextLength("grok-4", "opencode"); got != 131072 {
		t.Fatalf("fetched context = %d, want 131072", got)
	}
	if got, ok := cache.ReasoningFor("grok-4", "opencode"); !ok || len(got.Efforts) != 3 || got.Efforts[1] != "none" || got.Efforts[2] != "max" {
		t.Fatalf("fetched reasoning = %+v, %v", got, ok)
	}
}

func TestFetchModelsDevRejectsBadResponses(t *testing.T) {
	server := modelsDevTestServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadGateway)
		_, _ = w.Write([]byte("upstream unavailable"))
	}))
	defer server.Close()
	if _, err := fetchModelsDev(context.Background(), server.Client(), server.URL, map[string]struct{}{"grok-4": {}}); err == nil || !strings.Contains(err.Error(), "unexpected HTTP status") {
		t.Fatalf("bad status error = %v", err)
	}

	if _, err := parseModelsDev([]byte("not json"), nil); err == nil {
		t.Fatal("malformed catalog should fail")
	}
}

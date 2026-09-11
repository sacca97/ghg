package tools

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"testing"

	"github.com/sacca97/ghg/internal/sandbox"
)

type webRoundTripFunc func(*http.Request) (*http.Response, error)

func (f webRoundTripFunc) RoundTrip(req *http.Request) (*http.Response, error) {
	return f(req)
}

func webResponse(req *http.Request, status int, contentType, body string) *http.Response {
	return &http.Response{
		StatusCode:    status,
		Status:        fmt.Sprintf("%d %s", status, http.StatusText(status)),
		Header:        http.Header{"Content-Type": []string{contentType}},
		Body:          io.NopCloser(strings.NewReader(body)),
		ContentLength: int64(len(body)),
		Request:       req,
	}
}

func publicTestLookup(_ context.Context, _ string) ([]net.IP, error) {
	return []net.IP{net.ParseIP("93.184.216.34")}, nil
}

func TestWebFetchFormatsAndRedirects(t *testing.T) {
	tests := []struct {
		name        string
		url         string
		contentType string
		body        string
		want        []string
		notWant     []string
	}{
		{
			name:        "plain text",
			url:         "https://public.example/readme.txt",
			contentType: "text/plain; charset=utf-8",
			body:        "plain result",
			want:        []string{"HTTP status: 200", "plain result"},
		},
		{
			name:        "html extraction",
			url:         "https://public.example/page",
			contentType: "text/html",
			body:        `<html><head><title>Example</title><script>ignore me</script></head><body><h1>Heading</h1><p>Hello <a href="/docs">docs</a>.</p><svg>ignore me too</svg></body></html>`,
			want:        []string{"Page title: Example", "# Heading", "Hello docs (https://public.example/docs)"},
			notWant:     []string{"ignore me", "ignore me too"},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			client := newPublicWebClient(publicTestLookup)
			client.Transport = webRoundTripFunc(func(req *http.Request) (*http.Response, error) {
				return webResponse(req, http.StatusOK, test.contentType, test.body), nil
			})
			result, err := fetchWeb(context.Background(), test.url, client, publicTestLookup)
			if err != nil {
				t.Fatal(err)
			}
			if !IsUntrusted(result) {
				t.Fatal("web fetch result was not marked untrusted")
			}
			for _, want := range test.want {
				if !strings.Contains(result.Preview, want) {
					t.Fatalf("preview=%q, missing %q", result.Preview, want)
				}
			}
			for _, notWant := range test.notWant {
				if strings.Contains(result.Preview, notWant) {
					t.Fatalf("preview=%q, unexpectedly contains %q", result.Preview, notWant)
				}
			}
		})
	}

	lookup := func(_ context.Context, host string) ([]net.IP, error) {
		if host == "private.example" {
			return []net.IP{net.ParseIP("10.0.0.1")}, nil
		}
		return []net.IP{net.ParseIP("93.184.216.34")}, nil
	}
	var calls int
	client := newPublicWebClient(lookup)
	client.Transport = webRoundTripFunc(func(req *http.Request) (*http.Response, error) {
		calls++
		if req.URL.Hostname() == "public.example" {
			return &http.Response{
				StatusCode: http.StatusFound,
				Status:     "302 Found",
				Header:     http.Header{"Location": []string{"https://private.example/secret"}},
				Body:       io.NopCloser(strings.NewReader("")),
				Request:    req,
			}, nil
		}
		return webResponse(req, http.StatusOK, "text/plain", "should not be fetched"), nil
	})
	_, err := fetchWeb(context.Background(), "https://public.example/start", client, lookup)
	if err == nil || !strings.Contains(err.Error(), "redirect rejected") {
		t.Fatalf("redirect error=%v", err)
	}
	if calls != 1 {
		t.Fatalf("redirect made %d round trips, want 1", calls)
	}
}

func TestWebSearchNormalizesResults(t *testing.T) {
	client := &http.Client{Transport: webRoundTripFunc(func(req *http.Request) (*http.Response, error) {
		if got := req.Header.Get("X-Subscription-Token"); got != "secret" {
			t.Fatalf("search key=%q", got)
		}
		return webResponse(req, http.StatusOK, "application/json", `{"web":{"results":[{"title":"First","url":"https://source.example/one","description":"A snippet"},{"title":"Second","url":"https://source.example/two","description":"Another snippet"}]}}`), nil
	})}
	result, err := searchBraveWithClient(context.Background(), webSearchArgs{Query: "ghg", Count: 2}, "secret", "https://brave.example/search", client)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"1. First", "https://source.example/one", "A snippet", "2. Second"} {
		if !strings.Contains(result.Preview, want) {
			t.Fatalf("preview=%q, missing %q", result.Preview, want)
		}
	}
	if strings.Contains(result.Preview, "secret") || result.Source != "web_search:brave" || !IsUntrusted(result) {
		t.Fatalf("search result leaked key or lost source metadata: %+v", result)
	}
}

func TestSearXNGSearchNormalizesResults(t *testing.T) {
	client := &http.Client{Transport: webRoundTripFunc(func(req *http.Request) (*http.Response, error) {
		if req.URL.Path != "/search" || req.URL.Query().Get("format") != "json" || req.URL.Query().Get("q") != "ghg" {
			t.Fatalf("SearXNG request = %s", req.URL.String())
		}
		if req.Header.Get("Authorization") != "Bearer secret" {
			t.Fatalf("SearXNG authorization = %q", req.Header.Get("Authorization"))
		}
		return webResponse(req, http.StatusOK, "application/json", `{"results":[{"title":"First","url":"https://source.example/one","content":"A snippet"},{"title":"Second","url":"https://source.example/two","content":"Another snippet"}]}`), nil
	})}
	result, err := searchSearxngWithClient(context.Background(), webSearchArgs{Query: "ghg", Count: 2}, searchBackend{Name: "personal", Endpoint: "https://search.example/search", APIKey: "secret"}, client)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"1. First", "https://source.example/one", "A snippet", "2. Second"} {
		if !strings.Contains(result.Preview, want) {
			t.Fatalf("preview=%q, missing %q", result.Preview, want)
		}
	}
	if strings.Contains(result.Preview, "secret") || result.Source != "web_search:searxng" || !IsUntrusted(result) {
		t.Fatalf("SearXNG result leaked key or lost source metadata: %+v", result)
	}
}

func TestWebSecurityAndAvailability(t *testing.T) {
	t.Setenv("GHG_HOME", t.TempDir())
	tests := []struct {
		name string
		url  string
		ips  []net.IP
	}{
		{name: "credentials", url: "https://user:pass@public.example/", ips: []net.IP{net.ParseIP("93.184.216.34")}},
		{name: "loopback", url: "https://loopback.example/", ips: []net.IP{net.ParseIP("127.0.0.1")}},
		{name: "private", url: "https://private.example/", ips: []net.IP{net.ParseIP("10.0.0.1")}},
		{name: "link local", url: "https://linklocal.example/", ips: []net.IP{net.ParseIP("169.254.169.254")}},
		{name: "IPv6 loopback", url: "https://ipv6-loopback.example/", ips: []net.IP{net.ParseIP("::1")}},
		{name: "IPv6 private", url: "https://ipv6-private.example/", ips: []net.IP{net.ParseIP("fd00::1")}},
		{name: "IPv6 link local", url: "https://ipv6-linklocal.example/", ips: []net.IP{net.ParseIP("fe80::1")}},
		{name: "mixed addresses", url: "https://mixed.example/", ips: []net.IP{net.ParseIP("93.184.216.34"), net.ParseIP("192.168.1.1")}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			lookup := func(context.Context, string) ([]net.IP, error) { return test.ips, nil }
			client := newPublicWebClient(lookup)
			client.Transport = webRoundTripFunc(func(req *http.Request) (*http.Response, error) {
				t.Fatal("request reached transport after address validation")
				return nil, nil
			})
			if _, err := fetchWeb(context.Background(), test.url, client, lookup); err == nil {
				t.Fatal("expected URL validation error")
			}
		})
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := fetchWeb(ctx, "https://public.example/", newPublicWebClient(publicTestLookup), publicTestLookup); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled fetch error=%v", err)
	}

	lookup := publicTestLookup
	client := newPublicWebClient(lookup)
	client.Transport = webRoundTripFunc(func(req *http.Request) (*http.Response, error) {
		return webResponse(req, http.StatusOK, "text/plain", ""), nil
	})
	client.Transport = webRoundTripFunc(func(req *http.Request) (*http.Response, error) {
		response := webResponse(req, http.StatusOK, "text/plain", "")
		response.ContentLength = webFetchMaxBody + 1
		return response, nil
	})
	if _, err := fetchWeb(context.Background(), "https://public.example/", client, lookup); err == nil || !strings.Contains(err.Error(), "exceeds") {
		t.Fatalf("oversized response error=%v", err)
	}

	workspace := t.TempDir()
	denied, err := sandbox.NewPolicy(sandbox.PolicyConfig{Workspace: workspace, Mode: sandbox.ModeReadOnly, Network: sandbox.NetworkDeny})
	if err != nil {
		t.Fatal(err)
	}
	host, err := sandbox.NewPolicy(sandbox.PolicyConfig{Workspace: workspace, Mode: sandbox.ModeReadOnly, Network: sandbox.NetworkHost})
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("BRAVE_SEARCH_API_KEY", "")
	filtered, notices := FilterAvailable(All(), &ToolRuntime{Policy: denied})
	if hasTool(filtered, "web_fetch") || hasTool(filtered, "web_search") || len(notices) == 0 {
		t.Fatalf("network-denied web tools=%v notices=%v", toolNames(filtered), notices)
	}
	approvedRuntime, err := NewToolRuntime(denied, ApprovalAsk, false)
	if err != nil {
		t.Fatal(err)
	}
	approvedRuntime.HumanGate = func(context.Context, GateRequest) (GateDecision, string) {
		return GateAllowOnce, ""
	}
	filtered, notices = FilterAvailable(All(), approvedRuntime)
	if !hasTool(filtered, "web_fetch") || len(notices) == 0 {
		t.Fatalf("approval-backed web tools=%v notices=%v", toolNames(filtered), notices)
	}
	granted, err := approvedRuntime.authorizeNetwork(context.Background(), "web_fetch", "https://example.com")
	if err != nil || !granted.NetworkAllowed() {
		t.Fatalf("network approval: policy=%v err=%v", granted, err)
	}
	filtered, _ = FilterAvailable(All(), &ToolRuntime{Policy: host})
	if !hasTool(filtered, "web_fetch") || hasTool(filtered, "web_search") {
		t.Fatalf("missing-key web tools=%v", toolNames(filtered))
	}
	t.Setenv("BRAVE_SEARCH_API_KEY", "configured")
	filtered, _ = FilterAvailable(All(), &ToolRuntime{Policy: host})
	if !hasTool(filtered, "web_fetch") || !hasTool(filtered, "web_search") {
		t.Fatalf("configured web tools=%v", toolNames(filtered))
	}
}

func hasTool(ts []Tool, name string) bool {
	for _, tool := range ts {
		if tool.Def.Function.Name == name {
			return true
		}
	}
	return false
}

func toolNames(ts []Tool) []string {
	names := make([]string, 0, len(ts))
	for _, tool := range ts {
		names = append(names, tool.Def.Function.Name)
	}
	return names
}

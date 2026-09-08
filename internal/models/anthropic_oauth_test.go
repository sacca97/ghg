package models

import (
	"context"
	"io"
	"net/http"
	"strings"
	"testing"
)

type testAnthropicAuthorizer struct {
	token          string
	authorizeCalls int
	refreshCalls   int
}

func (a *testAnthropicAuthorizer) Authorize(req *http.Request) error {
	a.authorizeCalls++
	req.Header.Set("Authorization", "Bearer "+a.token)
	return nil
}

func (a *testAnthropicAuthorizer) ForceRefresh(context.Context) error {
	a.refreshCalls++
	a.token = "fresh-token"
	return nil
}

type testRoundTripper func(*http.Request) (*http.Response, error)

func (f testRoundTripper) RoundTrip(req *http.Request) (*http.Response, error) {
	return f(req)
}

func TestAnthropicOAuthRefreshesOnceAfterUnauthorized(t *testing.T) {
	authorizer := &testAnthropicAuthorizer{token: "stale-token"}
	var calls int
	client, err := NewBackend(Resolved{
		BaseURL:  "https://api.example/v1",
		Protocol: ProtocolAnthropicMessages,
		Auth:     Auth{Kind: AuthNone},
	}, BackendOptions{
		HTTP: &http.Client{Transport: testRoundTripper(func(req *http.Request) (*http.Response, error) {
			calls++
			if calls == 1 {
				return &http.Response{
					StatusCode: http.StatusUnauthorized,
					Status:     "401 Unauthorized",
					Body:       io.NopCloser(strings.NewReader("expired")),
				}, nil
			}
			if got := req.Header.Get("Authorization"); got != "Bearer fresh-token" {
				t.Errorf("retry authorization = %q", got)
			}
			return &http.Response{
				StatusCode: http.StatusOK,
				Status:     "200 OK",
				Body: io.NopCloser(strings.NewReader(
					`{"content":[{"type":"text","text":"ok"}],"stop_reason":"end_turn","usage":{"input_tokens":1,"output_tokens":1}}`,
				)),
			}, nil
		}), Timeout: 0},
		MaxRetries: 1,
		Authorizer: authorizer,
	})
	if err != nil {
		t.Fatal(err)
	}

	msg, _, err := client.Complete(context.Background(), Request{
		Model:    "claude-test",
		Messages: []Message{{Role: "user", Content: "hello"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if msg.TextContent() != "ok" || calls != 2 || authorizer.authorizeCalls != 2 || authorizer.refreshCalls != 1 {
		t.Fatalf("message=%q calls=%d authorize=%d refresh=%d", msg.TextContent(), calls, authorizer.authorizeCalls, authorizer.refreshCalls)
	}
}

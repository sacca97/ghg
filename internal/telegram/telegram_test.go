package telegram

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"
)

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func telegramResponse(body string) *http.Response {
	return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader(body)), Header: make(http.Header)}
}

func TestSendCompletionMessage(t *testing.T) {
	token := "12345:secret-token"
	var gotPath string
	var got struct {
		ChatID string `json:"chat_id"`
		Text   string `json:"text"`
	}
	client := &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		gotPath = r.URL.Path
		data, err := io.ReadAll(r.Body)
		if err != nil {
			t.Errorf("read request: %v", err)
		} else if err := json.Unmarshal(data, &got); err != nil {
			t.Errorf("decode request: %v", err)
		}
		return telegramResponse(`{"ok":true}`), nil
	})}

	message := CompletionMessage("review", "ghg", "abcdefghijk", strings.Repeat("🙂", 5000))
	if got := len([]rune(message)); got != maxMessageRunes {
		t.Fatalf("message rune length = %d, want %d", got, maxMessageRunes)
	}
	if !strings.HasSuffix(message, truncatedMark) {
		t.Fatalf("truncated message missing marker: %q", message[len(message)-80:])
	}
	if err := (Client{BaseURL: "https://telegram.test", HTTPClient: client}).Send(context.TODO(), token, "chat-1", message); err != nil {
		t.Fatal(err)
	}
	if gotPath != "/bot"+token+"/sendMessage" {
		t.Fatalf("request path = %q", gotPath)
	}
	if got.ChatID != "chat-1" || got.Text != message {
		t.Fatalf("request payload = %+v", got)
	}
}

func TestSendFailureDoesNotLeakToken(t *testing.T) {
	token := "12345:secret-token"
	client := &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
		return telegramResponse(`{"ok":false,"description":"invalid token 12345:secret-token"}`), nil
	})}

	err := (Client{BaseURL: "https://telegram.test", HTTPClient: client}).Send(context.TODO(), token, "chat-1", "hello")
	if err == nil || strings.Contains(err.Error(), token) {
		t.Fatalf("error = %v, token leaked or error missing", err)
	}
}

// Package telegram sends the single outbound notification used by ghg.
package telegram

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

const (
	apiBase         = "https://api.telegram.org"
	maxMessageRunes = 4096
	requestTimeout  = 5 * time.Second
	truncatedMark   = "\n\n[truncated — open the ghg session for the full result]"
)

// Client sends Telegram Bot API requests. BaseURL and HTTPClient are
// replaceable for tests; production uses apiBase and the standard client.
type Client struct {
	BaseURL    string
	HTTPClient *http.Client
}

// Send sends one plain-text Telegram message without retries or Markdown
// parsing. The bot token is kept out of returned errors.
func (c Client) Send(ctx context.Context, token, chatID, text string) error {
	if ctx == nil {
		ctx = context.Background()
	}
	token = strings.TrimSpace(token)
	chatID = strings.TrimSpace(chatID)
	if token == "" || chatID == "" {
		return fmt.Errorf("telegram credentials are incomplete")
	}
	if strings.TrimSpace(text) == "" {
		return fmt.Errorf("telegram message is empty")
	}

	body, err := json.Marshal(struct {
		ChatID string `json:"chat_id"`
		Text   string `json:"text"`
	}{ChatID: chatID, Text: text})
	if err != nil {
		return fmt.Errorf("marshal telegram message: %w", err)
	}

	ctx, cancel := context.WithTimeout(ctx, requestTimeout)
	defer cancel()
	base := strings.TrimRight(c.BaseURL, "/")
	if base == "" {
		base = apiBase
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, base+"/bot"+token+"/sendMessage", strings.NewReader(string(body)))
	if err != nil {
		return fmt.Errorf("create telegram request: %s", redact(err.Error(), token))
	}
	req.Header.Set("Content-Type", "application/json")
	client := c.HTTPClient
	if client == nil {
		client = http.DefaultClient
	}
	response, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("telegram request failed: %s", redact(err.Error(), token))
	}
	defer response.Body.Close()

	var result struct {
		OK bool `json:"ok"`
	}
	data, readErr := io.ReadAll(io.LimitReader(response.Body, 64<<10))
	if readErr == nil {
		_ = json.Unmarshal(data, &result)
	}
	return telegramResponseError(response.StatusCode, result)
}

func telegramResponseError(status int, result struct {
	OK bool `json:"ok"`
}) error {
	if status < http.StatusOK || status >= http.StatusMultipleChoices {
		return fmt.Errorf("telegram request failed (HTTP %d)", status)
	}
	if !result.OK {
		return fmt.Errorf("telegram rejected message")
	}
	return nil
}

// CompletionMessage creates the bounded user-facing notification. The input
// Markdown is left untouched because Telegram receives plain text.
func CompletionMessage(kind, workspace, sessionID, content string) string {
	kind = strings.TrimSpace(kind)
	if kind == "" {
		kind = "completion"
	}
	workspace = strings.TrimSpace(workspace)
	if workspace == "" {
		workspace = "workspace"
	}
	shortID := strings.TrimSpace(sessionID)
	if len([]rune(shortID)) > 8 {
		shortID = string([]rune(shortID)[:8])
	}
	message := fmt.Sprintf("ghg %s · %s · session %s\n\n%s", kind, workspace, shortID, strings.TrimSpace(content))
	runes := []rune(message)
	if len(runes) <= maxMessageRunes {
		return message
	}
	keep := maxMessageRunes - len([]rune(truncatedMark))
	if keep < 0 {
		keep = 0
	}
	return string(runes[:keep]) + truncatedMark
}

// TestMessage is sent before session consent is persisted.
func TestMessage(sessionID string) string {
	return CompletionMessage("notification test", "ghg", sessionID, "Telegram notifications are ready.")
}

func redact(value, secret string) string {
	if secret != "" {
		value = strings.ReplaceAll(value, secret, "[redacted]")
	}
	return value
}

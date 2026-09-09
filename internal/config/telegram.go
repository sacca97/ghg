package config

import (
	"fmt"
	"strings"
)

// TelegramConfig contains references to the credentials used for completion
// notifications. BotToken is resolved only when a message is sent.
type TelegramConfig struct {
	BotToken string `json:"botToken"`
	ChatID   string `json:"chatId"`
}

// TelegramCredentials resolves the configured bot token without exposing its
// value in errors or operation logs.
func (c *Config) TelegramCredentials() (string, string, error) {
	if c == nil || c.Telegram == nil {
		return "", "", fmt.Errorf("telegram is not configured")
	}
	chatID := strings.TrimSpace(c.Telegram.ChatID)
	if chatID == "" {
		return "", "", fmt.Errorf("telegram chatId is empty")
	}
	tokenReference := strings.TrimSpace(c.Telegram.BotToken)
	if tokenReference == "" {
		return "", "", fmt.Errorf("telegram botToken is empty")
	}
	token, err := ResolveSecret(tokenReference)
	if err != nil {
		return "", "", fmt.Errorf("telegram botToken: %w", err)
	}
	if strings.TrimSpace(token) == "" {
		return "", "", fmt.Errorf("telegram botToken resolved to an empty value")
	}
	return token, chatID, nil
}

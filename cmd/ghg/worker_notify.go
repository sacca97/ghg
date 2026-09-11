package main

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/sacca97/ghg/internal/config"
	"github.com/sacca97/ghg/internal/session"
	"github.com/sacca97/ghg/internal/telegram"
	workerwire "github.com/sacca97/ghg/internal/worker"
)

func (w *workerProcessState) notifyCommand(ctx context.Context, request workerwire.NotifyRequest) (workerwire.NotifyResult, string, error) {
	if w.store == nil || strings.TrimSpace(w.sessionID) == "" {
		return workerwire.NotifyResult{}, "", fmt.Errorf("session store unavailable")
	}
	switch strings.ToLower(strings.TrimSpace(request.Action)) {
	case "", "status":
		enabled, err := w.store.NotifyEnabled(w.sessionID)
		if err != nil {
			return workerwire.NotifyResult{}, "", fmt.Errorf("read notification status: %w", err)
		}
		if enabled {
			return workerwire.NotifyResult{Enabled: true}, "Telegram notifications are enabled for this session", nil
		}
		return workerwire.NotifyResult{}, "Telegram notifications are disabled for this session", nil
	case "off":
		if err := w.store.SetNotify(w.sessionID, false); err != nil {
			return workerwire.NotifyResult{}, "", fmt.Errorf("disable Telegram notifications: %w", err)
		}
		return workerwire.NotifyResult{}, "Telegram notifications disabled for this session", nil
	case "on":
		cfg, err := config.Load()
		if err != nil {
			return workerwire.NotifyResult{}, "", fmt.Errorf("reload Telegram configuration: %w", err)
		}
		return w.enableTelegram(ctx, cfg, false)
	case "config":
		if strings.TrimSpace(request.BotToken) == "" || strings.TrimSpace(request.ChatID) == "" {
			return workerwire.NotifyResult{}, "", fmt.Errorf("telegram bot token and chat ID are required")
		}
		cfg, err := config.Load()
		if err != nil {
			return workerwire.NotifyResult{}, "", fmt.Errorf("load Telegram configuration: %w", err)
		}
		cfg.Telegram = &config.TelegramConfig{
			BotToken: strings.TrimSpace(request.BotToken),
			ChatID:   strings.TrimSpace(request.ChatID),
		}
		return w.enableTelegram(ctx, cfg, true)
	default:
		return workerwire.NotifyResult{}, "", fmt.Errorf("usage: /notify [config|on|off]")
	}
}

func (w *workerProcessState) enableTelegram(ctx context.Context, cfg *config.Config, saveConfig bool) (workerwire.NotifyResult, string, error) {
	token, chatID, err := cfg.TelegramCredentials()
	if err != nil {
		return workerwire.NotifyResult{}, "", err
	}
	if err := (telegram.Client{}).Send(ctx, token, chatID, telegram.TestMessage(w.sessionID)); err != nil {
		return workerwire.NotifyResult{}, "", err
	}
	if saveConfig {
		if err := cfg.Save(); err != nil {
			return workerwire.NotifyResult{}, "", fmt.Errorf("save Telegram configuration: %w", err)
		}
	}
	if err := w.store.SetNotify(w.sessionID, true); err != nil {
		return workerwire.NotifyResult{}, "", fmt.Errorf("save Telegram notification setting: %w", err)
	}
	if saveConfig {
		return workerwire.NotifyResult{Enabled: true}, "Telegram configured and enabled for this session", nil
	}
	return workerwire.NotifyResult{Enabled: true}, "Telegram notifications enabled for this session", nil
}

// notifyCompletion deliberately runs after the normal worker completion was
// published. Delivery is best effort and never changes the turn result.
func (w *workerProcessState) notifyCompletion(result workerTurnResult) {
	if result.Error != "" || result.GoalContinue || strings.TrimSpace(result.Final) == "" || w.store == nil {
		return
	}
	kind, content := "completion", result.Final
	if strings.TrimSpace(result.ReviewMarkdown) != "" {
		kind, content = "review", result.ReviewMarkdown
	} else if strings.TrimSpace(result.Plan) != "" {
		kind, content = "plan", result.Plan
	}
	workspace := "workspace"
	if wd, wdErr := os.Getwd(); wdErr == nil {
		workspace = filepath.Base(wd)
	}
	w.turns.Add(1)
	go func() {
		defer w.turns.Done()
		if err := sendTelegramCompletion(context.Background(), w.store, result.SessionID, workspace, kind, content); err != nil {
			config.LogEvent("telegram.send", "FAILED: "+err.Error())
		}
	}()
}

// sendTelegramCompletion checks the persisted opt-in before resolving the
// current global credentials. Disabled sessions therefore never touch
// Telegram configuration or the network.
func sendTelegramCompletion(ctx context.Context, store *session.Store, sessionID, workspace, kind, content string) error {
	enabled, err := store.NotifyEnabled(sessionID)
	if err != nil {
		return fmt.Errorf("read notification status: %w", err)
	}
	if !enabled {
		return nil
	}
	cfg, err := config.Load()
	if err != nil {
		return fmt.Errorf("reload Telegram configuration: %w", err)
	}
	token, chatID, err := cfg.TelegramCredentials()
	if err != nil {
		return err
	}
	return (telegram.Client{}).Send(ctx, token, chatID, telegram.CompletionMessage(kind, workspace, sessionID, content))
}

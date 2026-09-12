package tui

import (
	"strings"

	workerwire "github.com/sacca97/ghg/internal/worker"
)

// notifyConfigCommand collects credentials without putting the bot token in
// the transcript or input history. Telegram validates the pair before the
// worker saves it and enables this session.
func (m *model) notifyConfigCommand() {
	m.openNamePrompt("✈ Telegram bot token (masked, enter to continue, esc cancels):", "", func(token string) {
		token = strings.TrimSpace(token)
		if token == "" {
			m.append(dimStyle.Render("Telegram setup cancelled"))
			return
		}
		m.openNamePrompt("✈ Telegram chat ID (enter to verify, esc cancels):", "", func(chatID string) {
			chatID = strings.TrimSpace(chatID)
			if chatID == "" {
				m.append(dimStyle.Render("Telegram setup cancelled"))
				return
			}
			m.sendNotifyConfig(token, chatID)
		})
	})
	m.namePrompt.mask = true
}

func (m *model) sendNotifyConfig(token, chatID string) {
	m.sendWorkerCommand("/notify config", "notify-config", workerwire.CommandNotify, workerwire.NotifyRequest{
		Action: "config", BotToken: token, ChatID: chatID,
	})
}

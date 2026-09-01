package tui

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/sacca97/ghg/internal/config"
	"github.com/sacca97/ghg/internal/export"
	"github.com/sacca97/ghg/internal/llm"
	"github.com/sacca97/ghg/internal/session"
)

// exportResultCommand handles `/export` and `/export-result` with `[chat|plan|review|last|message] [dest] [--format json|markdown] [--force]`.
func (m *model) exportResultCommand(text string) (tea.Model, tea.Cmd) {
	trimmed := text
	for _, pfx := range []string{"/export-result", "/export"} {
		if strings.HasPrefix(trimmed, pfx) {
			trimmed = strings.TrimPrefix(trimmed, pfx)
			break
		}
	}
	args := strings.Fields(strings.TrimSpace(trimmed))
	var kind, dest, format string
	var force bool

	for i := 0; i < len(args); i++ {
		arg := args[i]
		switch {
		case arg == "--force" || arg == "-f":
			force = true
		case arg == "--format" || arg == "-format":
			if i+1 < len(args) {
				i++
				format = args[i]
			}
		case strings.HasPrefix(arg, "--format="):
			format = strings.TrimPrefix(arg, "--format=")
		case arg == "plan" || arg == "review" || arg == "last" || arg == "message" || arg == "response" || arg == "chat" || arg == "log" || arg == "transcript":
			if kind == "" {
				kind = arg
			} else if dest == "" {
				dest = arg
			}
		default:
			if dest == "" {
				dest = arg
			}
		}
	}

	if format == "" {
		if strings.HasSuffix(strings.ToLower(dest), ".json") {
			format = export.FormatJSON
		} else {
			format = export.FormatMarkdown
		}
	}

	ctx := context.Background()
	var rec session.WorkflowResultRecord
	var ok bool

	if kind == "chat" || kind == "log" || kind == "transcript" {
		msgs := m.findChatMessages()
		if len(msgs) == 0 {
			m.append(dimStyle.Render("no chat messages found in this session to export"))
			return m, nil
		}
		rawPayload, _ := json.Marshal(msgs)
		rec = session.WorkflowResultRecord{
			ResultID:  fmt.Sprintf("chat-%x", time.Now().UnixNano()&0xffffffff),
			SessionID: m.sessionID,
			Kind:      "chat",
			Version:   1,
			Payload:   string(rawPayload),
			CreatedAt: time.Now().UTC(),
		}
		ok = true
	} else if kind == "last" || kind == "message" || kind == "response" {
		lastMsg, found := m.findLastAssistantMessage()
		if !found {
			m.append(dimStyle.Render("no assistant message found in this session to export"))
			return m, nil
		}
		rawPayload, _ := json.Marshal(lastMsg.TextContent())
		rec = session.WorkflowResultRecord{
			ResultID:  fmt.Sprintf("msg-%x", time.Now().UnixNano()&0xffffffff),
			SessionID: m.sessionID,
			Kind:      "message",
			Version:   1,
			Payload:   string(rawPayload),
			CreatedAt: time.Now().UTC(),
		}
		ok = true
	} else {
		if m.store != nil && m.sessionID != "" {
			var err error
			rec, ok, err = m.store.LatestWorkflowResult(ctx, m.sessionID, kind)
			if err != nil {
				m.append(errStyle.Render("export lookup failed: " + err.Error()))
				return m, nil
			}
		}
		if !ok && (kind == "plan" || kind == "") && m.proposedPlanMD != "" {
			planJSON, _ := json.Marshal(map[string]string{"markdown": m.proposedPlanMD})
			rec = session.WorkflowResultRecord{
				ResultID:  fmt.Sprintf("plan-%x", time.Now().UnixNano()&0xffffffff),
				SessionID: m.sessionID,
				Kind:      "plan",
				Version:   2,
				Payload:   string(planJSON),
				Role:      config.RoleSmart,
				CreatedAt: time.Now().UTC(),
			}
			ok = true
		}
		if !ok && kind == "" {
			lastMsg, found := m.findLastAssistantMessage()
			if found {
				rawPayload, _ := json.Marshal(lastMsg.TextContent())
				rec = session.WorkflowResultRecord{
					ResultID:  fmt.Sprintf("msg-%x", time.Now().UnixNano()&0xffffffff),
					SessionID: m.sessionID,
					Kind:      "message",
					Version:   1,
					Payload:   string(rawPayload),
					CreatedAt: time.Now().UTC(),
				}
				ok = true
			}
		}
	}

	if !ok {
		if kind != "" {
			m.append(dimStyle.Render(fmt.Sprintf("no completed %s result found in this session to export", kind)))
		} else {
			m.append(dimStyle.Render("no completed workflow result or assistant message found to export"))
		}
		return m, nil
	}

	if dest == "" {
		dest = export.DefaultExportFilename(rec.Kind, time.Now(), format)
	}

	rendered, err := export.RenderResult(rec, format)
	if err != nil {
		m.append(errStyle.Render("export render failed: " + err.Error()))
		return m, nil
	}

	cwd, _ := os.Getwd()
	finalPath, err := export.WriteExportFile(dest, rendered, force, cwd)
	if err != nil {
		if errors.Is(err, export.ErrDestinationExists) {
			m.append(errStyle.Render(fmt.Sprintf("export target %s already exists (add --force to overwrite)", filepath.Base(finalPath))))
		} else {
			m.append(errStyle.Render("export write failed: " + err.Error()))
		}
		return m, nil
	}

	rel, err := filepath.Rel(cwd, finalPath)
	if err != nil || strings.HasPrefix(rel, "..") {
		rel = finalPath
	}
	m.append(dimStyle.Render(fmt.Sprintf("◎ exported %s (%s) to %s", rec.Kind, rec.ResultID, rel)))
	return m, nil
}

func (m *model) findLastAssistantMessage() (llm.Message, bool) {
	if m.agent != nil {
		msgs := m.agent.MessagesSnapshot()
		for i := len(msgs) - 1; i >= 0; i-- {
			if (msgs[i].Role == "assistant" || msgs[i].Role == "model") && strings.TrimSpace(msgs[i].TextContent()) != "" {
				return msgs[i], true
			}
		}
	}
	if m.store != nil && m.sessionID != "" {
		_, msgs, err := m.store.Load(m.sessionID)
		if err == nil {
			for i := len(msgs) - 1; i >= 0; i-- {
				if (msgs[i].Role == "assistant" || msgs[i].Role == "model") && strings.TrimSpace(msgs[i].TextContent()) != "" {
					return msgs[i], true
				}
			}
		}
	}
	for i := len(m.blocks) - 1; i >= 0; i-- {
		if m.blocks[i].kind == blockAssistant && strings.TrimSpace(m.blocks[i].text) != "" {
			return llm.Message{Role: "assistant", Content: m.blocks[i].text}, true
		}
	}
	if strings.TrimSpace(m.current) != "" {
		return llm.Message{Role: "assistant", Content: m.current}, true
	}
	return llm.Message{}, false
}

func (m *model) findChatMessages() []llm.Message {
	if m.agent != nil {
		msgs := m.agent.MessagesSnapshot()
		if len(msgs) > 1 || (len(msgs) == 1 && msgs[0].Role != "system") {
			return msgs
		}
	}
	if m.store != nil && m.sessionID != "" {
		_, msgs, err := m.store.Load(m.sessionID)
		if err == nil && len(msgs) > 0 {
			return msgs
		}
	}
	var out []llm.Message
	for _, b := range m.blocks {
		switch b.kind {
		case blockAssistant:
			out = append(out, llm.Message{Role: "assistant", Content: b.text})
		case blockTool:
			out = append(out, llm.Message{Role: "tool", Content: b.text})
		case blockText:
			if strings.TrimSpace(b.text) != "" {
				out = append(out, llm.Message{Role: "user", Content: b.text})
			}
		}
	}
	if len(out) == 0 && m.agent != nil {
		return m.agent.MessagesSnapshot()
	}
	return out
}

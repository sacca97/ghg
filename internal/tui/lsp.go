package tui

import (
	"fmt"
	"strings"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/sacca97/ghg/internal/lsp"
	workerwire "github.com/sacca97/ghg/internal/worker"
)

// lspCommand handles "/lsp" — a status view of language servers (sibling of
// /mcp). ponytail: no restart/toggle subcommands; add when someone needs them.
func (m *model) lspCommand(fields []string) (tea.Model, tea.Cmd) {
	if m.workerClient != nil {
		if err := m.workerClient.Send(workerwire.CommandLSPStatus, workerRequestID("lsp"), nil); err != nil {
			m.append(errStyle.Render("LSP status failed: " + err.Error()))
		}
		return m, nil
	}
	if m.lspMgr == nil {
		m.append(dimStyle.Render("no LSP servers configured — gopls is built in; add servers via the \"lsp\" block in config.json"))
		return m, nil
	}
	m.renderLSPStatuses(m.lspMgr.Statuses())
	return m, nil
}

func (m *model) renderLSPStatuses(servers []lsp.Status) {
	if len(servers) == 0 {
		m.append(dimStyle.Render("no LSP servers"))
		return
	}
	var b strings.Builder
	b.WriteString("LSP servers:\n")
	for _, s := range servers {
		icon := "○"
		detail := "idle — starts on first matching file"
		switch s.State {
		case "connected":
			icon = "●"
			detail = "connected"
			if s.Root != "" {
				detail += " (root: " + s.Root + ")"
			}
		case "failed":
			icon = "✗"
			detail = s.Err
		}
		line := fmt.Sprintf("  %s %-16s %s", icon, s.Name, detail)
		switch s.State {
		case "failed":
			b.WriteString(errStyle.Render(line) + "\n")
		case "not started":
			b.WriteString(dimStyle.Render(line) + "\n")
		default:
			b.WriteString(line + "\n")
		}
	}
	m.append(strings.TrimRight(b.String(), "\n"))
}

func workerLSPStatuses(statuses []workerwire.LSPStatus) []lsp.Status {
	converted := make([]lsp.Status, len(statuses))
	for i, status := range statuses {
		converted[i] = lsp.Status{Name: status.Name, Root: status.Root, State: status.State, Err: status.Error}
	}
	return converted
}

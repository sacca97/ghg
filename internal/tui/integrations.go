package tui

import (
	"fmt"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/sacca97/ghg/internal/config"
	"github.com/sacca97/ghg/internal/lsp"
	"github.com/sacca97/ghg/internal/mcp"
	workerwire "github.com/sacca97/ghg/internal/worker"
	"strings"
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

// mcpCommand handles "/mcp [name] [reconnect|enable|disable]".
func (m *model) mcpCommand(fields []string) (tea.Model, tea.Cmd) {
	if m.workerClient != nil {
		if len(fields) == 1 {
			if err := m.workerClient.Send(workerwire.CommandMCPStatus, workerRequestID("mcp"), nil); err != nil {
				m.append(errStyle.Render("MCP status failed: " + err.Error()))
			}
			return m, nil
		}
		name := fields[1]
		action := workerwire.CommandMCPReconnect
		if len(fields) > 2 {
			switch fields[2] {
			case "reconnect":
				action = workerwire.CommandMCPReconnect
			case "enable":
				action = workerwire.CommandMCPEnable
			case "disable":
				action = workerwire.CommandMCPDisable
			default:
				m.append(errStyle.Render("usage: /mcp [name] [reconnect|enable|disable]"))
				return m, nil
			}
		}
		if err := m.workerClient.Send(action, workerRequestID("mcp"), workerwire.MCPRequest{Name: name}); err != nil {
			m.append(errStyle.Render("MCP command failed: " + err.Error()))
		}
		return m, nil
	}
	if m.mcpMgr == nil {
		if m.workerOnly {
			m.append(m.persistedMCPStatusView())
			return m, nil
		}
		m.append(dimStyle.Render("no MCP servers configured — add one with `ghg mcp add <name> -- <cmd...>`, a .mcp.json, or ~/.codex/config.toml"))
		return m, nil
	}
	if len(fields) == 1 {
		m.append(m.mcpStatusView())
		return m, nil
	}
	name := fields[1]
	action := "reconnect"
	if len(fields) > 2 {
		action = fields[2]
	}
	switch action {
	case "reconnect":
		if !m.mcpMgr.Reconnect(name) {
			m.append(errStyle.Render("no MCP server named " + name))
			return m, nil
		}
		m.append(dimStyle.Render(fmt.Sprintf("↻ reconnecting mcp server %s…", name)))
	case "disable", "enable":
		m.mcpSetEnabled(name, action == "enable")
	default:
		m.append(errStyle.Render("usage: /mcp [name] [reconnect|enable|disable]"))
	}
	return m, nil
}

// mcpSetEnabled persists a toggle into ghg's own config and applies it
// live. For imported (claude/codex) servers the FULL definition is copied
// into ghg's config first — otherwise a bare {enabled:false} entry would
// shadow the import on next launch and lose the command/url for re-enable.
func (m *model) mcpSetEnabled(name string, enabled bool) {
	if m.mcpMgr.BlockedByPolicy(name) {
		m.append(errStyle.Render(fmt.Sprintf("mcp server %s is blocked by the mcpImport config — edit ~/.ghg/config.json (or remove the gate) to enable it", name)))
		return
	}
	live, ok := m.mcpMgr.Config(name)
	if !ok {
		m.append(errStyle.Render("no MCP server named " + name))
		return
	}
	entry := config.MCPServer{
		Command: live.Command, Env: live.Env, Cwd: live.Cwd,
		URL: live.URL, Headers: live.Headers,
		StartupTimeout: live.StartupTimeout, ToolTimeout: live.ToolTimeout,
		Enabled: &enabled,
	}
	if m.cfg.MCPServers == nil {
		m.cfg.MCPServers = map[string]config.MCPServer{}
	}
	m.cfg.MCPServers[name] = entry
	if err := m.cfg.Save(); err != nil {
		m.append(errStyle.Render("config save failed: " + err.Error()))
		return
	}
	if enabled {
		m.mcpMgr.Enable(name)
	} else {
		m.mcpMgr.Disable(name)
	}
	state := "disabled"
	if enabled {
		state = "enabled"
	}
	m.append(dimStyle.Render(fmt.Sprintf("mcp server %s: %s (persisted)", name, state)))
	m.append(m.mcpStatusView())
}

// mcpStatusView renders the /mcp table: one row per server with status,
// tool count, and failure detail.
func (m *model) mcpStatusView() string {
	if m.mcpMgr == nil {
		return dimStyle.Render("no MCP servers")
	}
	return renderMCPStatuses(append(m.mcpMgr.Statuses(), m.mcpMgr.Blocked()...))
}

func renderMCPStatuses(servers []mcp.Server) string {
	if len(servers) == 0 {
		return dimStyle.Render("no MCP servers")
	}
	var b strings.Builder
	b.WriteString("MCP servers:\n")
	for _, s := range servers {
		icon := "◌"
		detail := ""
		switch s.Status {
		case mcp.StatusReady:
			icon = "●"
			detail = fmt.Sprintf("%d tools", s.Tools)
		case mcp.StatusFailed:
			icon = "✗"
			detail = s.Err
			if s.Source != "" {
				detail += " (" + s.Source + ")"
			}
		case mcp.StatusDisabled:
			icon = "○"
			detail = "disabled"
			if s.Note != "" {
				detail = "disabled — " + s.Note
			}
		case mcp.StatusConnecting:
			icon = "◌"
			detail = "connecting…"
		}
		line := fmt.Sprintf("  %s %-20s %s", icon, s.Name, detail)
		switch s.Status {
		case mcp.StatusReady:
			b.WriteString(line + "\n")
		case mcp.StatusFailed:
			b.WriteString(errStyle.Render(line) + "\n")
		default:
			b.WriteString(dimStyle.Render(line) + "\n")
		}
	}
	return strings.TrimRight(b.String(), "\n")
}

func renderWorkerMCPStatuses(statuses []workerwire.MCPStatus) string {
	servers := make([]mcp.Server, len(statuses))
	for i, status := range statuses {
		servers[i] = mcp.Server{Name: status.Name, Note: status.Note, Err: status.Error, Tools: status.Tools, Source: status.Source}
		switch status.State {
		case "ready":
			servers[i].Status = mcp.StatusReady
		case "failed":
			servers[i].Status = mcp.StatusFailed
		case "disabled":
			servers[i].Status = mcp.StatusDisabled
		default:
			servers[i].Status = mcp.StatusConnecting
		}
	}
	return renderMCPStatuses(servers)
}

func (m *model) persistedMCPStatusView() string {
	if len(m.cfg.MCPServers) == 0 {
		return dimStyle.Render("no MCP servers configured")
	}
	servers := make([]mcp.Server, 0, len(m.cfg.MCPServers))
	for name, cfg := range m.cfg.MCPServers {
		status := mcp.StatusConnecting
		note := "configured — worker starts on first turn"
		if cfg.Enabled != nil && !*cfg.Enabled {
			status = mcp.StatusDisabled
			note = "disabled"
		}
		servers = append(servers, mcp.Server{Name: name, Status: status, Note: note})
	}
	return renderMCPStatuses(servers)
}

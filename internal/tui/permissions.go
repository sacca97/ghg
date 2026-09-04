package tui

import (
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/x/ansi"
	"github.com/sacca97/ghg/internal/tools"
	workerwire "github.com/sacca97/ghg/internal/worker"
	"strings"
)

// permDialog is the UI-thread modal state while a request is open.
type permDialog struct {
	req       tools.GateRequest
	workerID  string
	sel       int  // 0=allow once, 1=allow always, 2=reject
	rejecting bool // typing the redirect message
	rejectIn  string
}

// permKey handles keys while the dialog is open. Returns (handled).
func (m *model) permKey(msg tea.KeyMsg) bool {
	d := m.permDialog
	if d == nil {
		return false
	}
	answer := func(decision tools.GateDecision, redirect string) {
		if d.workerID != "" {
			decisionName := "reject"
			switch decision {
			case tools.GateAllowOnce:
				decisionName = "allow_once"
			case tools.GateAllowAlways:
				decisionName = "allow_always"
			}
			if m.workerClient != nil {
				if err := m.workerClient.Send(workerwire.CommandApprove, workerRequestID("approve"), workerwire.ApprovalAnswer{
					ID: d.workerID, Decision: decisionName, Redirect: redirect,
				}); err != nil {
					m.append(errStyle.Render("approval failed: " + err.Error()))
				}
			}
		}
		m.permDialog = nil
	}
	if d.rejecting {
		switch msg.Type {
		case tea.KeyEnter:
			answer(tools.GateReject, strings.TrimSpace(d.rejectIn))
		case tea.KeyEsc:
			d.rejecting, d.rejectIn = false, "" // back to the buttons
		case tea.KeyBackspace:
			if len(d.rejectIn) > 0 {
				d.rejectIn = d.rejectIn[:len(d.rejectIn)-1]
			}
		case tea.KeyRunes, tea.KeySpace:
			d.rejectIn += string(msg.Runes)
			if msg.Type == tea.KeySpace {
				d.rejectIn += " "
			}
		}
		return true
	}
	switch msg.Type {
	case tea.KeyLeft, tea.KeyUp:
		d.sel = (d.sel + 2) % 3
	case tea.KeyRight, tea.KeyDown:
		d.sel = (d.sel + 1) % 3
	case tea.KeyEnter:
		switch d.sel {
		case 0:
			answer(tools.GateAllowOnce, "")
		case 1:
			answer(tools.GateAllowAlways, "")
		case 2:
			d.rejecting = true // take the redirect text
		}
	case tea.KeyRunes:
		switch string(msg.Runes) {
		case "a":
			answer(tools.GateAllowOnce, "")
		case "A":
			answer(tools.GateAllowAlways, "")
		case "r":
			d.rejecting = true
		}
	case tea.KeyEsc:
		answer(tools.GateReject, "rejected without a reason")
	}
	return true
}

// permView renders the modal: the action, the rule "always" would install,
// and the three options (or the redirect prompt).
func (m *model) permView() string {
	d := m.permDialog
	if d == nil {
		return ""
	}
	var b strings.Builder
	title := "Allow " + d.req.Tool + "?"
	if d.req.Tool == "bash" {
		title = "Run this command?"
	}
	b.WriteString(youStyle.Render("⚠ " + title))
	b.WriteString("\n  " + ansi.Truncate(d.req.Command, m.width-4, "…"))
	rule := d.req.Rule
	if d.req.Tool != "bash" {
		rule = d.req.Command
	}
	b.WriteString(dimStyle.Render("\n  always allows: " + d.req.Tool + ":" + rule))
	if d.rejecting {
		b.WriteString("\n" + youStyle.Render("  reject with message: ") + d.rejectIn + "█")
		b.WriteString(dimStyle.Render("\n  enter sends · esc back"))
		return b.String()
	}
	opts := []string{"allow once (a)", "allow always (A)", "reject (r)"}
	b.WriteString("\n  ")
	for i, o := range opts {
		if i == d.sel {
			b.WriteString(youStyle.Render("❯ " + o + "  "))
		} else {
			b.WriteString(dimStyle.Render("  " + o + "  "))
		}
	}
	return b.String()
}

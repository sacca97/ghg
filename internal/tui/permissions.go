package tui

import (
	"fmt"
	"strings"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/x/ansi"
	"github.com/sacca97/ghg/internal/tools"
	workerwire "github.com/sacca97/ghg/internal/worker"
)

// permDialog is the UI-thread modal state while a request is open.
type permDialog struct {
	req       tools.GateRequest
	workerID  string
	sel       int  // 0=allow once, 1=allow always, 2=reject
	rejecting bool // typing the redirect message
	rejectIn  string
}

type questionDialog struct {
	request workerwire.QuestionRequest
	index   int
	sel     int
	other   bool
	otherIn string
	answers []workerwire.QuestionAnswer
}

func (m *model) questionKey(msg tea.KeyMsg) {
	d := m.questionDialog
	if d == nil || d.index >= len(d.request.Questions) {
		return
	}
	question := d.request.Questions[d.index]
	if d.other {
		switch msg.Type {
		case tea.KeyEnter:
			if strings.TrimSpace(d.otherIn) != "" {
				m.acceptQuestion(strings.TrimSpace(d.otherIn))
			}
		case tea.KeyEsc:
			d.other, d.otherIn = false, ""
		default:
			handleLineEdit(msg, &d.otherIn, 4096)
		}
		return
	}
	last := len(question.Options)
	switch msg.Type {
	case tea.KeyUp, tea.KeyLeft:
		d.sel = (d.sel + last) % (last + 1)
	case tea.KeyDown, tea.KeyRight:
		d.sel = (d.sel + 1) % (last + 1)
	case tea.KeyEnter:
		if d.sel == last {
			d.other = true
			d.otherIn = ""
		} else {
			m.acceptQuestion(question.Options[d.sel].Label)
		}
	case tea.KeyEsc:
		m.sendQuestionAnswer(true)
	}
}

func (m *model) acceptQuestion(value string) {
	d := m.questionDialog
	if d == nil {
		return
	}
	d.answers = append(d.answers, workerwire.QuestionAnswer{ID: d.request.Questions[d.index].ID, Value: value})
	if d.index+1 < len(d.request.Questions) {
		d.index++
		d.sel = 0
		d.other = false
		d.otherIn = ""
		return
	}
	m.sendQuestionAnswer(false)
}

func (m *model) sendQuestionAnswer(cancelled bool) {
	d := m.questionDialog
	if d == nil || m.workerClient == nil {
		return
	}
	request := workerwire.QuestionAnswerRequest{ID: d.request.ID, Answers: d.answers, Cancelled: cancelled}
	if err := m.workerClient.Send(workerwire.CommandAnswerQuestion, workerRequestID("question"), request); err != nil {
		m.append(errStyle.Render("question failed: " + err.Error()))
		return
	}
	if cancelled {
		_ = m.workerClient.Send(workerwire.CommandCancel, workerRequestID("question-cancel"), nil)
	}
	m.questionDialog = nil
}

func (m *model) questionView() string {
	d := m.questionDialog
	if d == nil || d.index >= len(d.request.Questions) {
		return ""
	}
	question := d.request.Questions[d.index]
	var b strings.Builder
	label := fmt.Sprintf("? %d/%d", d.index+1, len(d.request.Questions))
	b.WriteString(youStyle.Render(label + " " + question.Question))
	if d.other {
		b.WriteString("\n  ")
		b.WriteString(d.otherIn)
		b.WriteString("█")
		b.WriteString(dimStyle.Render("\n  enter sends · esc back"))
		return b.String()
	}
	for i, option := range question.Options {
		b.WriteString("\n  ")
		if i == d.sel {
			b.WriteString(youStyle.Render("❯ " + option.Label))
		} else {
			b.WriteString(dimStyle.Render("  " + option.Label))
		}
		if option.Description != "" {
			b.WriteString(dimStyle.Render(" — " + option.Description))
		}
	}
	b.WriteString("\n  ")
	if d.sel == len(question.Options) {
		b.WriteString(youStyle.Render("❯ Other"))
	} else {
		b.WriteString(dimStyle.Render("  Other"))
	}
	return b.String()
}

// permKey handles keys while the dialog is open. Returns (handled).
func (m *model) permKey(msg tea.KeyMsg) bool {
	d := m.permDialog
	if d == nil {
		return false
	}
	answer := func(decision tools.GateDecision, redirect string) {
		if d.workerID != "" {
			if m.workerClient == nil {
				m.append(errStyle.Render("approval is still pending: worker connection lost; reconnecting"))
				return
			}
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
					return
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
		default:
			handleLineEdit(msg, &d.rejectIn, 0)
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
	b.WriteString("\n  ")
	b.WriteString(ansi.Truncate(d.req.Command, m.width-4, "…"))
	rule := d.req.Rule
	if d.req.Tool != "bash" {
		rule = d.req.Command
	}
	b.WriteString(dimStyle.Render("\n  always allows: " + d.req.Tool + ":" + rule))
	if d.rejecting {
		b.WriteString("\n")
		b.WriteString(youStyle.Render("  reject with message: "))
		b.WriteString(d.rejectIn)
		b.WriteString("█")
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

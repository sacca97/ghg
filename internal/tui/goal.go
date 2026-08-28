package tui

import (
	"fmt"
	"os"
	"strings"

	"github.com/sacca97/ghg/internal/agent"
	"github.com/sacca97/ghg/internal/config"
	goalstate "github.com/sacca97/ghg/internal/goal"
	"github.com/sacca97/ghg/internal/llm"
)

// goalMaxRounds resolves the goal-loop round cap: per-project override
// (~/.ghg/projects.json, keyed by cwd) beats the global default
// (goalMaxRounds in ~/.ghg/config.json), which falls back to
// config.DefaultGoalMaxRounds. Set either with /goal rounds.
func (m *model) goalMaxRounds() int {
	if wd, err := os.Getwd(); err == nil {
		if n := config.ProjectGoalMaxRounds(wd); n > 0 {
			return n
		}
	}
	if m.cfg != nil && m.cfg.GoalMaxRounds > 0 {
		return m.cfg.GoalMaxRounds
	}
	return config.DefaultGoalMaxRounds
}

// goalContinuePrompt is sent after each completed turn while a goal is active.
// Completion is a structured update_goal call; this message only asks the
// model to inspect remaining work and continue when it has not checkpointed a
// terminal state.
func goalContinuePrompt(goal string) string {
	return fmt.Sprintf(`[goal continuation] Continue working on the active objective: %s

Inspect and verify the remaining work. Use the request-scoped goal context as the source of truth. Call update_goal with status active and a concise progress note when you have made meaningful progress; call it with status blocked only for a genuine blocker; call it with status complete only after verification. A prose claim alone never completes the goal.`, goal)
}

// currentGoalRecord returns the authoritative in-memory goal. The legacy
// string fields remain as a compatibility seam for older headless tests and
// callers that construct a model directly; production state always has the
// structured record populated by setGoal/resume.
func (m *model) currentGoalRecord() (goalstate.Record, bool) {
	if m.goalRecord == nil {
		if strings.TrimSpace(m.goal) == "" {
			return goalstate.Record{}, false
		}
		record := goalstate.New(m.goal)
		record.ID = "legacy-" + goalstate.NewID()
		record.Rounds = max(m.goalRounds, 0)
		m.goalRecord = &record
		return record, true
	}
	record := *m.goalRecord
	// Direct model construction in older tests writes m.goal. Treat a
	// non-empty mismatch as that test/caller's latest objective while keeping
	// the structured record authoritative in normal TUI operation.
	if strings.TrimSpace(m.goal) != "" && strings.TrimSpace(m.goal) != record.Objective {
		record.Objective = strings.TrimSpace(m.goal)
		record.Status = goalstate.StatusActive
		record.Blocker = ""
	}
	if m.goalRounds > record.Rounds {
		record.Rounds = m.goalRounds
	}
	return record, true
}

func (m *model) applyGoalRecord(record goalstate.Record) {
	m.goalRecord = &record
	m.goalRounds = record.Rounds
	if record.Status == goalstate.StatusActive {
		m.goal = record.Objective
	} else {
		m.goal = ""
	}
}

func (m *model) persistGoal(record goalstate.Record, checkpoint bool) {
	if m.store == nil || m.sessionID == "" {
		return
	}
	var err error
	if checkpoint {
		err = m.store.CheckpointGoal(m.sessionID, record)
	} else {
		err = m.store.SaveGoal(m.sessionID, record)
	}
	if err != nil {
		config.LogEvent("session.goal", "save failed: "+err.Error())
		m.append(errStyle.Render("goal save failed: " + err.Error()))
	}
}

func (m *model) goalRecordForSession() (goalstate.Record, bool) {
	record, ok := m.currentGoalRecord()
	if !ok {
		return goalstate.Record{}, false
	}
	if record.CreatedAt.IsZero() {
		record.CreatedAt = m.nowFn()
	}
	if record.UpdatedAt.IsZero() {
		record.UpdatedAt = record.CreatedAt
	}
	return record, true
}

func (m *model) saveGoalCheckpoint(record goalstate.Record) {
	record.UpdatedAt = m.nowFn().UTC()
	m.applyGoalRecord(record)
	m.persistGoal(record, true)
}

func addGoalUsage(record *goalstate.Record, usage llm.Usage) {
	record.PromptTokens += usage.PromptTokens
	record.CompletionTokens += usage.CompletionTokens
	record.CachedTokens += usage.Cached()
}

func goalBlocker(err error) string {
	if err == nil {
		return ""
	}
	return truncateGoalNote(err.Error())
}

func truncateGoalNote(value string) string {
	value = strings.TrimSpace(value)
	if len(value) <= goalstate.MaxNoteBytes {
		return value
	}
	return value[:goalstate.MaxNoteBytes]
}

func goalStatusForError(err error) goalstate.Status {
	if err == nil {
		return goalstate.StatusActive
	}
	message := strings.ToLower(err.Error())
	for _, marker := range []string{"rate limit", "quota", "usage limit", "credit", "billing", "too many requests", "daily limit"} {
		if strings.Contains(message, marker) {
			return goalstate.StatusUsageLimited
		}
	}
	return goalstate.StatusPaused
}

// applyGoalUpdate persists a model checkpoint as soon as it arrives from the
// live turn. The update is accepted only while the same goal ID is active;
// clearing a goal while a request is in flight therefore wins over a late
// model callback.
func (m *model) applyGoalUpdate(update agent.GoalUpdate) bool {
	record, ok := m.goalRecordForSession()
	if !ok || record.Status != goalstate.StatusActive {
		return false
	}
	if err := update.Validate(record.ID); err != nil {
		m.append(errStyle.Render("invalid goal update: " + err.Error()))
		return false
	}
	if record.Status == update.Status && record.Progress == update.Progress && record.Blocker == update.Blocker {
		return true
	}
	record.Status = update.Status
	record.Progress = truncateGoalNote(update.Progress)
	record.Blocker = truncateGoalNote(update.Blocker)
	record.UpdatedAt = m.nowFn().UTC()
	m.applyGoalRecord(record)
	m.persistGoal(record, true)
	return true
}

func (m *model) goalTurnFinished(msg turnDoneMsg, canceled bool) bool {
	record, ok := m.goalRecordForSession()
	if !ok {
		return false
	}
	if record.Status != goalstate.StatusActive {
		if msg.err == nil && (record.Status == goalstate.StatusBlocked || record.Status == goalstate.StatusComplete) {
			addGoalUsage(&record, msg.goalUsage)
			record.Rounds++
			record.UpdatedAt = m.nowFn().UTC()
			m.saveGoalCheckpoint(record)
			if record.Status == goalstate.StatusComplete {
				m.append(dimStyle.Render(fmt.Sprintf("◎ goal %s complete after %d round(s)", record.ID, record.Rounds)))
			} else {
				m.append(errStyle.Render("◎ goal blocked: " + record.Blocker + " — /goal resume to continue"))
			}
		}
		return false
	}
	for _, update := range msg.goalUpdates {
		if !m.applyGoalUpdate(update) {
			continue
		}
		record, _ = m.goalRecordForSession()
		if record.Status == goalstate.StatusBlocked || record.Status == goalstate.StatusComplete {
			break
		}
	}
	addGoalUsage(&record, msg.goalUsage)
	record.Rounds++
	record.UpdatedAt = m.nowFn().UTC()
	if msg.err != nil {
		record.Status = goalStatusForError(msg.err)
		if canceled {
			record.Status = goalstate.StatusPaused
			record.Blocker = "interrupted by user"
		} else {
			record.Blocker = goalBlocker(msg.err)
		}
		m.saveGoalCheckpoint(record)
		return false
	}
	if record.Status == goalstate.StatusBlocked || record.Status == goalstate.StatusComplete {
		m.saveGoalCheckpoint(record)
		if record.Status == goalstate.StatusComplete {
			m.append(dimStyle.Render(fmt.Sprintf("◎ goal %s complete after %d round(s)", record.ID, record.Rounds)))
		} else {
			m.append(errStyle.Render("◎ goal blocked: " + record.Blocker + " — /goal resume to continue"))
		}
		return false
	}
	if record.Rounds >= m.goalMaxRounds() {
		record.Status = goalstate.StatusBudgetLimited
		record.Blocker = fmt.Sprintf("goal round circuit breaker reached (%d rounds)", record.Rounds)
		m.saveGoalCheckpoint(record)
		m.append(errStyle.Render(fmt.Sprintf("◎ goal paused after %d rounds — /goal resume to continue, /goal clear to drop", record.Rounds)))
		return false
	}
	m.applyGoalRecord(record)
	m.persistGoal(record, false)
	return true
}

// goalRoundsCommand implements /goal rounds: bare reports the effective cap
// and where it comes from, a number sets the per-project override (--global
// sets the config default instead), and "default" clears the override.
func (m *model) goalRoundsCommand(args []string) {
	global := false
	var num string
	for _, a := range args {
		if a == "--global" || a == "-g" {
			global = true
		} else if num == "" {
			num = a
		} else {
			m.append(errStyle.Render("usage: /goal rounds [n|default] [--global]"))
			return
		}
	}
	wd, _ := os.Getwd()
	proj := config.ProjectGoalMaxRounds(wd)
	cfgN := 0
	if m.cfg != nil {
		cfgN = m.cfg.GoalMaxRounds
	}

	switch num {
	case "":
		src := fmt.Sprintf("built-in default (%d)", config.DefaultGoalMaxRounds)
		if proj > 0 {
			src = "project override"
		} else if cfgN > 0 {
			src = "global config"
		}
		m.append(dimStyle.Render(fmt.Sprintf("◎ goal rounds: %d (%s) — /goal rounds <n>|default [--global]", m.goalMaxRounds(), src)))
		return
	case "default":
		// clear
	default:
		n := 0
		if _, err := fmt.Sscan(num, &n); err != nil || n <= 0 {
			m.append(errStyle.Render("rounds must be a positive number (or \"default\")"))
			return
		}
		if global {
			m.cfg.GoalMaxRounds = n
			if err := m.cfg.Save(); err != nil {
				m.append(errStyle.Render("couldn't save config: " + err.Error()))
				return
			}
			m.append(dimStyle.Render(fmt.Sprintf("◎ global goal rounds: %d%s", n, overriddenNote(proj))))
			return
		}
		if err := config.SetProjectGoalMaxRounds(wd, n); err != nil {
			m.append(errStyle.Render("couldn't save project override: " + err.Error()))
			return
		}
		m.append(dimStyle.Render(fmt.Sprintf("◎ goal rounds for this project: %d", n)))
		return
	}

	// "default": clear the override at the chosen scope
	if global {
		m.cfg.GoalMaxRounds = 0
		if err := m.cfg.Save(); err != nil {
			m.append(errStyle.Render("couldn't save config: " + err.Error()))
			return
		}
		m.append(dimStyle.Render(fmt.Sprintf("◎ global goal rounds reset to %d%s", config.DefaultGoalMaxRounds, overriddenNote(proj))))
		return
	}
	if err := config.SetProjectGoalMaxRounds(wd, 0); err != nil {
		m.append(errStyle.Render("couldn't save project override: " + err.Error()))
		return
	}
	m.append(dimStyle.Render(fmt.Sprintf("◎ project goal rounds cleared — using %d", m.goalMaxRounds())))
}

// overriddenNote flags when a project override still wins over a global change.
func overriddenNote(proj int) string {
	if proj > 0 {
		return fmt.Sprintf(" (this project overrides it with %d)", proj)
	}
	return ""
}

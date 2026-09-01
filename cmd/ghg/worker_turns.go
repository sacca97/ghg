package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/sacca97/ghg/internal/agent"
	"github.com/sacca97/ghg/internal/config"
	goalstate "github.com/sacca97/ghg/internal/goal"
	"github.com/sacca97/ghg/internal/llm"
	"github.com/sacca97/ghg/internal/session"
	workerwire "github.com/sacca97/ghg/internal/worker"
)

func (w *workerProcessState) startCompact() bool {
	return w.startOperation("compaction", func(ctx context.Context) { w.runCompact(ctx) })
}

func (w *workerProcessState) startTurn(input workerInput) bool {
	return w.startOperation("turn", func(ctx context.Context) { w.runTurn(ctx, input) })
}

func (w *workerProcessState) startOperation(detail string, run func(context.Context)) bool {
	var ctx context.Context
	var cancel context.CancelFunc
	ok := w.transition(func() (workerwire.State, bool, string, bool) {
		if w.activeCancel != nil || w.stopRequested {
			return w.state, w.detached, "", false
		}
		ctx, cancel = context.WithCancel(context.Background())
		w.activeCancel = cancel
		w.turns.Add(1)
		return workerwire.StateRunning, w.detached, detail + " started", true
	})
	if !ok {
		return false
	}
	w.clearLive()
	go func() {
		defer w.turns.Done()
		run(ctx)
		w.finishOperation(detail)
	}()
	return true
}

// finishOperation is the one completion path for turns and auxiliary
// operations: reset the active bookkeeping, publish idle, and — when the
// worker is detached by then — arm the idle-exit grace. Detached planning and
// compaction previously skipped the idle scheduling that turn completion had,
// leaving a detached worker alive indefinitely.
func (w *workerProcessState) finishOperation(detail string) {
	var detached bool
	w.transition(func() (workerwire.State, bool, string, bool) {
		w.activeCancel = nil
		w.activeTool = ""
		detached = w.detached
		if w.stopRequested {
			return w.state, w.detached, "", false
		}
		return workerwire.StateIdle, w.detached, detail + " finished", true
	})
	w.clearLive()
	if detached && !w.hasLiveWork() {
		w.scheduleIdleExit()
	}
}

func (w *workerProcessState) runCompact(ctx context.Context) {
	before := len(w.ag.MessagesSnapshot())
	var summary string
	var cutoff int
	err := w.ag.ManualCompact(ctx, agent.Events{
		OnCompactionReady: func(messages []llm.Message, summary string, cutoff int) error {
			w.mu.Lock()
			saved, modelName, providerName := w.saved, w.modelName, w.provider
			w.mu.Unlock()
			return w.store.PersistCompaction(w.sessionID, saved, messages, modelName, providerName, summary, cutoff)
		},
		OnCompacted: func(value string, at int) { summary, cutoff = value, at },
		OnUsage:     func(usage llm.Usage) { w.publish("usage", usage, true) },
		OnRetry:     func(retry llm.RetryEvent) { w.publish("retry", retry, true) },
	})
	if err == nil {
		kept := len(w.ag.MessagesSnapshot())
		w.mu.Lock()
		w.saved = kept
		w.mu.Unlock()
		w.publish("compact", map[string]any{
			"summary": summary, "cutoff": cutoff,
			"took": before - kept, "kept": kept,
		}, true)
	}
	w.persist()
	result := workerCompactResult{Usage: w.ag.Usage(), Messages: boundedWorkerMessages(w.ag.MessagesSnapshot())}
	if err != nil {
		result.Error = err.Error()
	}
	w.publish("compact_done", result, true)
}

func (w *workerProcessState) runTurn(ctx context.Context, input workerInput) {
	if input.SystemPrompt != "" {
		w.ag.SetSystemPrompt(input.SystemPrompt)
	}
	if input.PlanMode || w.mode == "plan" {
		w.ag.PlanMode = true
	} else {
		w.ag.PlanMode = false
	}
	var turnUsage llm.Usage
	addUsage := func(u llm.Usage) {
		turnUsage.PromptTokens += u.PromptTokens
		turnUsage.CompletionTokens += u.CompletionTokens
		turnUsage.CacheCreationTokens += u.CacheCreationTokens
		if cached := u.Cached(); cached > 0 {
			if turnUsage.PromptTokensDetails == nil {
				turnUsage.PromptTokensDetails = &struct {
					CachedTokens int `json:"cached_tokens"`
				}{}
			}
			turnUsage.PromptTokensDetails.CachedTokens += cached
		}
	}
	ev := agent.Events{
		OnText: func(s string) {
			w.appendLive("text", s)
			w.publish("text", s, false)
		},
		OnThink: func(s string) {
			w.appendLive("think", s)
			w.publish("think", s, false)
		},
		OnPlanDelta: func(s string) {
			w.appendLive("plan", s)
			w.publish(workerwire.EventPlanDelta, s, false)
		},
		OnToolStart: func(id, name, args string) {
			w.mu.Lock()
			w.activeTool = name
			w.mu.Unlock()
			w.publish("tool_start", map[string]string{"id": id, "name": name, "args": args}, true)
		},
		OnToolOutput: func(id, output string) {
			w.appendLive("tool_output", output)
			w.publish("tool_output", map[string]string{"id": id, "output": output}, false)
		},
		OnToolEnd: func(id, name, result string) {
			w.mu.Lock()
			w.activeTool = ""
			w.mu.Unlock()
			w.publish("tool_end", map[string]string{"id": id, "name": name, "result": result}, true)
		},
		OnSteer: func(s string) { w.publish("steer", s, true) },
		OnUsage: func(u llm.Usage) { addUsage(u); w.publish("usage", u, true) },
		OnRetry: func(ev llm.RetryEvent) { w.publish("retry", ev, true) },
		OnGoalUpdate: func(update agent.GoalUpdate) {
			w.persistGoalUpdate(update)
			w.publish("goal_update", update, true)
		},
		OnCompactionReady: func(messages []llm.Message, summary string, cutoff int) error {
			w.mu.Lock()
			saved, modelName, providerName := w.saved, w.modelName, w.provider
			w.mu.Unlock()
			return w.store.PersistCompaction(w.sessionID, saved, messages, modelName, providerName, summary, cutoff)
		},
		OnCompacted: func(summary string, cutoff int) {
			w.mu.Lock()
			w.saved = len(w.ag.MessagesSnapshot())
			w.mu.Unlock()
			w.publish("compact", map[string]any{"summary": summary, "cutoff": cutoff}, true)
		},
	}
	var final string
	var err error
	switch {
	case len(input.Parts) > 0 && input.Goal != nil:
		final, err = w.ag.TurnWithImagesAndGoal(ctx, input.Input, input.Parts, *input.Goal, ev)
	case len(input.Parts) > 0:
		final, err = w.ag.TurnWithImages(ctx, input.Input, input.Parts, ev)
	case input.Goal != nil:
		final, err = w.ag.TurnWithGoal(ctx, input.Input, *input.Goal, ev)
	case input.Authored:
		final, err = w.ag.TurnAuthored(ctx, input.Input, ev)
	default:
		final, err = w.ag.Turn(ctx, input.Input, ev)
	}
	w.persist()
	w.persistGoalTurn(input.Goal, turnUsage, err)
	var planMD string
	if w.ag.PlanMode && final != "" {
		if extracted, ok := agent.ExtractProposedPlan(final); ok {
			planMD = extracted
			if w.store != nil && w.sessionID != "" {
				planJSON, _ := json.Marshal(map[string]string{"markdown": planMD})
				msgSeq := len(w.ag.MessagesSnapshot())
				_ = w.store.SaveWorkflowResult(context.Background(), session.WorkflowResultRecord{
					ResultID:   fmt.Sprintf("plan-%x", time.Now().UnixNano()&0xffffffff),
					SessionID:  w.sessionID,
					Kind:       "plan",
					Version:    2,
					Payload:    string(planJSON),
					Role:       config.RoleSmart,
					MessageSeq: msgSeq,
					CreatedAt:  time.Now().UTC(),
				})
			}
		}
	}
	result := workerTurnResult{
		Final: final, Usage: turnUsage, At: input.At, Snap: input.Snap,
		Messages: boundedWorkerMessages(w.ag.MessagesSnapshot()),
		Plan:     planMD,
	}
	if err != nil {
		result.Error = err.Error()
	}
	w.publish("turn_done", result, true)
}

func (w *workerProcessState) persist() {
	if w.store == nil || w.ag == nil {
		return
	}
	msgs := w.ag.MessagesSnapshot()
	w.mu.Lock()
	saved, modelName, providerName, effort := w.saved, w.modelName, w.provider, w.ag.Effort
	w.mu.Unlock()
	_ = w.store.SetEffort(w.sessionID, effort)
	_ = w.store.SetTodos(w.sessionID, w.ag.TodosJSON())
	usage := w.ag.Usage()
	_ = w.store.SetUsage(w.sessionID, usage.PromptTokens, usage.Cached(), usage.CompletionTokens)
	if len(msgs) <= saved {
		return
	}
	if err := w.store.Save(w.sessionID, saved, msgs, modelName, providerName); err == nil {
		w.mu.Lock()
		w.saved = len(msgs)
		w.mu.Unlock()
	}
}

func (w *workerProcessState) persistGoalUpdate(update agent.GoalUpdate) {
	if w.store == nil || w.sessionID == "" {
		return
	}
	record, ok, err := w.store.LoadGoal(w.sessionID)
	if err != nil || !ok || record.Status != goalstate.StatusActive {
		return
	}
	if err := update.Validate(record.ID); err != nil {
		return
	}
	record.Status = update.Status
	record.Progress = truncateWorkerGoalNote(update.Progress)
	record.Blocker = truncateWorkerGoalNote(update.Blocker)
	record.UpdatedAt = time.Now().UTC()
	_ = w.store.CheckpointGoal(w.sessionID, record)
}

func (w *workerProcessState) persistGoalTurn(input *goalstate.Record, usage llm.Usage, turnErr error) {
	if w.store == nil || w.sessionID == "" || input == nil {
		return
	}
	record, ok, err := w.store.LoadGoal(w.sessionID)
	if err != nil || !ok || record.ID != input.ID {
		return
	}
	// An explicit clear/pause made while the turn was running wins over the
	// stale request-scoped goal supplied at turn start.
	if record.Status != goalstate.StatusActive && record.Status != goalstate.StatusBlocked && record.Status != goalstate.StatusComplete {
		return
	}
	record.PromptTokens += usage.PromptTokens
	record.CachedTokens += usage.Cached()
	record.CompletionTokens += usage.CompletionTokens
	record.Rounds++
	if turnErr != nil && record.Status == goalstate.StatusActive {
		record.Status = goalstate.StatusPaused
		record.Blocker = truncateWorkerGoalNote(turnErr.Error())
	}
	if record.Status == goalstate.StatusActive && record.Rounds >= w.goalMaxRounds() {
		record.Status = goalstate.StatusBudgetLimited
		record.Blocker = fmt.Sprintf("goal round circuit breaker reached (%d rounds)", record.Rounds)
	}
	record.UpdatedAt = time.Now().UTC()
	if record.Status == goalstate.StatusActive {
		_ = w.store.SaveGoal(w.sessionID, record)
	} else {
		_ = w.store.CheckpointGoal(w.sessionID, record)
	}
}

func (w *workerProcessState) goalMaxRounds() int {
	if wd, err := os.Getwd(); err == nil {
		if n := config.ProjectGoalMaxRounds(wd); n > 0 {
			return n
		}
	}
	if w.cfg != nil && w.cfg.GoalMaxRounds > 0 {
		return w.cfg.GoalMaxRounds
	}
	return config.DefaultGoalMaxRounds
}

// appendContent lands local context (a `!` shell escape's output) on the
// worker-owned conversation: mid-turn it steers so the turn goroutine keeps
// ownership of Messages; idle it appends directly and persists, because
// nothing drains a queued steer until the next turn starts.
func (w *workerProcessState) appendContent(content string) {
	w.mu.Lock()
	busy := w.activeCancel != nil
	w.mu.Unlock()
	if busy {
		w.ag.Steer(content)
		return
	}
	w.ag.AppendUser(content)
	w.persist()
}

func truncateWorkerGoalNote(value string) string {
	value = strings.TrimSpace(value)
	if len(value) <= goalstate.MaxNoteBytes {
		return value
	}
	return value[:goalstate.MaxNoteBytes]
}

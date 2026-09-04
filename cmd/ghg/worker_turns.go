package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"slices"
	"strings"
	"time"

	"github.com/sacca97/ghg/internal/agent"
	"github.com/sacca97/ghg/internal/config"
	"github.com/sacca97/ghg/internal/export"
	"github.com/sacca97/ghg/internal/memory"
	"github.com/sacca97/ghg/internal/models"
	"github.com/sacca97/ghg/internal/session"
	"github.com/sacca97/ghg/internal/skills"
	"github.com/sacca97/ghg/internal/tools"
	workerwire "github.com/sacca97/ghg/internal/worker"
)

func (w *workerProcessState) startCompact() bool {
	return w.startOperation("compaction", func(ctx context.Context) { w.runCompact(ctx) })
}

func (w *workerProcessState) startTurn(input workerInput) bool {
	return w.startOperation("turn", func(ctx context.Context) { w.runTurn(ctx, input) })
}

func (w *workerProcessState) startGoalFromContext(window int) bool {
	return w.startOperation("goal formulation", func(ctx context.Context) { w.runGoalFromContext(ctx, window) })
}

func (w *workerProcessState) startShell(command string) bool {
	w.mu.Lock()
	if w.stopRequested || w.shellCancel != nil {
		w.mu.Unlock()
		return false
	}
	ctx, cancel := context.WithCancel(context.Background())
	w.shellCancel = cancel
	w.turns.Add(1)
	w.mu.Unlock()

	go func() {
		defer w.turns.Done()
		defer func() {
			w.mu.Lock()
			w.shellCancel = nil
			w.mu.Unlock()
		}()

		args, err := json.Marshal(workerwire.ShellRequest{Command: command})
		if err != nil {
			return
		}
		result := tools.ExecuteResult(tools.WithRuntime(ctx, w.runtime), w.ag.AllTools(), "bash", args)
		if ctx.Err() != nil {
			return
		}
		w.publish(workerwire.EventShellDone, workerwire.ShellResult{Command: command, Output: result.Preview}, true)
	}()
	return true
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

func (w *workerProcessState) updateGoal(request workerwire.GoalRequest) (agent.GoalRecord, error) {
	if err := w.requireIdleHistory(); err != nil {
		return agent.GoalRecord{}, err
	}
	if w.store == nil || w.sessionID == "" {
		return agent.GoalRecord{}, errors.New("session store unavailable")
	}
	if request.Action == "clear" {
		if record, ok, err := w.store.LoadGoal(w.sessionID); err != nil {
			return agent.GoalRecord{}, err
		} else if ok {
			record.Status = agent.GoalStatusPaused
			record.Progress = ""
			record.Blocker = "cleared by user"
			record.UpdatedAt = time.Now().UTC()
			if err := w.store.CheckpointGoal(w.sessionID, record); err != nil {
				return agent.GoalRecord{}, err
			}
			w.publish("goal", record, true)
			return record, nil
		}
		return agent.GoalRecord{}, nil
	}
	if request.Record == nil {
		return agent.GoalRecord{}, errors.New("goal record is required")
	}
	record := *request.Record
	if request.Action == "resume" {
		record.Status = agent.GoalStatusActive
		record.Blocker = ""
		record.UpdatedAt = time.Now().UTC()
	}
	if err := record.Validate(); err != nil {
		return agent.GoalRecord{}, err
	}
	if err := w.store.CheckpointGoal(w.sessionID, record); err != nil {
		return agent.GoalRecord{}, err
	}
	w.publish("goal", record, true)
	return record, nil
}

func (w *workerProcessState) runGoalFromContext(ctx context.Context, window int) {
	tail, err := agent.GoalFromContextMessages(w.ag.MessagesSnapshot(), window)
	if err == nil {
		reasoningEffort, reasoningEnabled := w.ag.ReasoningRequest()
		message, usage, callErr := w.ag.CompleteWithRoutePurpose(ctx, w.ag.Backend, w.ag.Role, w.ag.Provider, w.ag.Protocol, "goal-formulation", models.Request{
			Model: w.ag.Model, MaxTokens: 8192,
			Messages:        []models.Message{{Role: "user", Content: agent.BuildGoalFromContextPrompt(tail)}},
			ReasoningEffort: reasoningEffort, ReasoningEnabled: reasoningEnabled,
		}, agent.Events{})
		w.ag.AddUsage(usage)
		w.publish("usage", usage, true)
		err = callErr
		if err == nil {
			objective := strings.TrimSpace(message.TextContent())
			if objective == "" {
				err = errors.New("model returned an empty goal")
			} else {
				record := agent.NewGoal(objective)
				if saveErr := w.store.CheckpointGoal(w.sessionID, record); saveErr != nil {
					err = saveErr
				} else {
					w.publish("goal", record, true)
					w.publish("goal_from_context", workerwire.GoalFromContextResult{Goal: &record, Usage: usage}, true)
					w.persist()
					return
				}
			}
		}
	}
	w.publish("goal_from_context", workerwire.GoalFromContextResult{Usage: models.Usage{}, Error: err.Error()}, true)
}

func (w *workerProcessState) runCompact(ctx context.Context) {
	before := len(w.ag.MessagesSnapshot())
	var summary string
	var cutoff int
	err := w.ag.ManualCompact(ctx, agent.Events{
		OnCompactionReady: func(messages []models.Message, summary string, cutoff int) error {
			w.mu.Lock()
			saved, modelName, providerName := w.saved, w.modelName, w.provider
			w.mu.Unlock()
			return w.store.PersistCompaction(w.sessionID, saved, messages, modelName, providerName, summary, cutoff)
		},
		OnCompacted: func(value string, at int) { summary, cutoff = value, at },
		OnUsage:     func(usage models.Usage) { w.publish("usage", usage, true) },
		OnRetry:     func(retry models.RetryEvent) { w.publish("retry", retry, true) },
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

func (w *workerProcessState) compactRetry() (workerwire.HistoryResult, error) {
	if err := w.requireIdleHistory(); err != nil {
		return workerwire.HistoryResult{}, err
	}
	if w.store == nil || w.sessionID == "" {
		return workerwire.HistoryResult{}, errors.New("no session to retry a compaction in")
	}
	events := w.store.Compactions(w.sessionID)
	if len(events) == 0 {
		return workerwire.HistoryResult{}, errors.New("no compaction to retry")
	}
	last := events[len(events)-1]
	if err := w.store.DeleteCompaction(w.sessionID, last.Seq); err != nil {
		return workerwire.HistoryResult{}, err
	}
	_, messages, err := w.store.Load(w.sessionID)
	if err != nil {
		return workerwire.HistoryResult{}, err
	}
	messages = w.withSystemPrompt(messages)
	w.ag.Messages = messages
	w.ag.RebuildTouched(w.ag.MessagesSnapshot())
	w.mu.Lock()
	w.saved = len(messages)
	w.mu.Unlock()
	return w.historyResult(), nil
}

func (w *workerProcessState) rewind(request workerwire.RewindRequest) (workerwire.HistoryResult, error) {
	if err := w.requireIdleHistory(); err != nil {
		return workerwire.HistoryResult{}, err
	}
	if w.store == nil || w.sessionID == "" {
		return workerwire.HistoryResult{}, errors.New("session store unavailable")
	}
	if len(request.Messages) == 0 || request.Messages[0].Role != "system" {
		return workerwire.HistoryResult{}, errors.New("rewind history must include the system prompt")
	}
	current := w.ag.MessagesSnapshot()
	restored := 0
	cut := max(request.Cut, 1)
	if cut > len(current) && cut > len(request.Messages) {
		return workerwire.HistoryResult{}, errors.New("rewind boundary is invalid")
	}

	if cut < len(current) {
		best, bestIdx := "", -1
		for idx, ref := range w.store.Snapshots(w.sessionID) {
			if idx >= cut && (bestIdx < 0 || idx < bestIdx) {
				best, bestIdx = ref, idx
			}
		}
		if best != "" {
			wd, err := os.Getwd()
			if err != nil {
				return workerwire.HistoryResult{}, err
			}
			restored, err = session.RestoreWorkspace(wd, best)
			if err != nil {
				return workerwire.HistoryResult{}, err
			}
		}
	}
	if err := w.store.DeleteFrom(w.sessionID, cut, request.Messages); err != nil {
		return workerwire.HistoryResult{}, err
	}
	messages := slices.Clone(request.Messages)
	w.ag.Messages = messages
	w.ag.RebuildTouched(w.ag.MessagesSnapshot())
	w.mu.Lock()
	saved := min(w.saved, cut)
	modelName, providerName := w.modelName, w.provider
	w.mu.Unlock()
	if len(messages) > saved {
		if err := w.store.Save(w.sessionID, saved, messages, modelName, providerName); err != nil {
			return workerwire.HistoryResult{}, err
		}
	}
	w.mu.Lock()
	w.saved = len(messages)
	w.mu.Unlock()
	result := w.historyResult()
	result.Restored = restored
	return result, nil
}

func (w *workerProcessState) requireIdleHistory() error {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.activeCancel != nil || w.stopRequested || w.state == workerwire.StateStopping {
		return errors.New("worker is busy or stopping")
	}
	return nil
}

func (w *workerProcessState) withSystemPrompt(messages []models.Message) []models.Message {
	if len(messages) > 0 && messages[0].Role == "system" {
		return messages
	}
	system := ""
	if current := w.ag.MessagesSnapshot(); len(current) > 0 && current[0].Role == "system" {
		system = current[0].Content
	}
	return append([]models.Message{{Role: "system", Content: system}}, messages...)
}

func (w *workerProcessState) historyResult() workerwire.HistoryResult {
	return workerwire.HistoryResult{
		SessionID: w.sessionID, Messages: boundedWorkerMessages(w.ag.MessagesSnapshot()),
		Usage: w.ag.Usage(), ContextTokens: w.ag.ContextTokens(),
	}
}

func (w *workerProcessState) runTurn(ctx context.Context, input workerInput) {
	turnAt := len(w.ag.MessagesSnapshot())
	snap := input.Snap
	if snap == "" {
		if wd, err := os.Getwd(); err == nil {
			snap = session.SnapshotWorkspace(wd)
		}
	}
	if snap != "" && w.store != nil {
		_ = w.store.SetSnapshot(w.sessionID, turnAt, snap)
	}
	systemPrompt := input.SystemPrompt
	if systemPrompt == "" {
		if current := w.ag.MessagesSnapshot(); len(current) > 0 {
			systemPrompt = current[0].Content
		}
	}
	var additions []string
	if wd, wdErr := os.Getwd(); wdErr == nil {
		project := config.ProjectInstructions(wd, config.Trusted(wd))
		if strings.Contains(systemPrompt, "<project_instructions>") {
			systemPrompt = replaceProjectInstructions(systemPrompt, project)
		} else if project != "" {
			additions = append(additions, project)
		}
	}
	additions = append(additions,
		skills.PromptBlock(skills.Scan(skills.DefaultDirs()...)),
		memory.PromptBlock(memory.Installation(), memory.Session(w.sessionID)),
	)
	if w.mcp != nil {
		additions = append(additions, w.mcp.InstructionsBlock())
	}
	w.ag.SetSystemPrompt(agent.CompileSystemPrompt(systemPrompt, additions...))
	if input.AskMode {
		w.ag.AskMode = true
		w.ag.ReviewMode = false
		w.ag.PlanMode = false
	} else if input.ReviewMode {
		w.ag.ReviewMode = true
		w.ag.PlanMode = false
		w.ag.AskMode = false
	} else if input.PlanMode || w.mode == "plan" {
		w.ag.PlanMode = true
		w.ag.ReviewMode = false
		w.ag.AskMode = false
	} else {
		w.ag.PlanMode = false
		w.ag.ReviewMode = false
		w.ag.AskMode = false
	}
	var turnUsage models.Usage
	addUsage := func(u models.Usage) {
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
		OnThink: func(s string) {
			w.appendLive("think", s)
			w.publish("think", s, false)
		},
		OnSteer: func(s string) { w.publish("steer", s, true) },
		OnUsage: func(u models.Usage) { addUsage(u); w.publish("usage", u, true) },
		OnRetry: func(ev models.RetryEvent) { w.publish("retry", ev, true) },
		OnGoalUpdate: func(update agent.GoalUpdate) {
			w.persistGoalUpdate(update)
			w.publish("goal_update", update, true)
		},
		OnCompactionReady: func(messages []models.Message, summary string, cutoff int) error {
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
	setupWireEvents(&ev, w.emitWireEvent)
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
					ResultID:   fmt.Sprintf("plan-%x", time.Now().UnixNano()),
					SessionID:  w.sessionID,
					Kind:       "plan",
					Version:    2,
					Payload:    string(planJSON),
					Role:       w.ag.Role,
					MessageSeq: msgSeq,
					CreatedAt:  time.Now().UTC(),
				})
			}
		}
	}
	var reviewPayload string
	var reviewMarkdown string
	if w.ag.ReviewMode && final != "" && err == nil {
		if review, parseErr := agent.ParseReview(final); parseErr == nil {
			reviewPayload = final
			reviewMarkdown = export.RenderReviewMarkdown(review)
			if w.store != nil && w.sessionID != "" {
				msgSeq := len(w.ag.MessagesSnapshot())
				_ = w.store.SaveWorkflowResult(context.Background(), session.WorkflowResultRecord{
					ResultID:   fmt.Sprintf("review-%x", time.Now().UnixNano()),
					SessionID:  w.sessionID,
					Kind:       "review",
					Version:    1,
					Payload:    reviewPayload,
					Role:       w.ag.Role,
					MessageSeq: msgSeq,
					CreatedAt:  time.Now().UTC(),
				})
			}
		}
	}
	var goalRecord *agent.GoalRecord
	goalContinue := false
	if input.Goal != nil && w.store != nil {
		if record, ok, loadErr := w.store.LoadGoal(w.sessionID); loadErr == nil && ok {
			goalRecord = &record
			goalContinue = record.Status == agent.GoalStatusActive && err == nil
		}
	}
	w.ag.ReviewMode = false
	w.ag.PlanMode = (w.mode == "plan")
	w.ag.AskMode = false

	w.mu.Lock()
	modelID, modelName, provider, role, protocol, effort := w.ag.Model, w.modelName, w.provider, w.ag.Role, w.ag.Protocol, w.ag.Effort
	contextLimit := w.ag.ContextLimit
	w.mu.Unlock()
	clean := true
	if wd, err := os.Getwd(); err == nil {
		clean = session.WorkspaceClean(wd)
		if clean && snap != "" {
			session.DropSnapshot(wd, snap)
			if w.store != nil {
				_ = w.store.SetSnapshot(w.sessionID, turnAt, "")
			}
		}
	}
	result := workerTurnResult{
		SessionID: w.sessionID, Final: final, Usage: w.ag.Usage(),
		ContextTokens: w.ag.ContextTokens(), ContextLimit: contextLimit,
		Model: modelID, ModelName: modelName, Provider: provider,
		Role: role, Protocol: protocol, Effort: effort,
		At: turnAt, Snap: snap, Clean: clean,
		Messages: boundedWorkerMessages(w.ag.MessagesSnapshot()),
		Plan:     planMD, Review: reviewPayload, ReviewMarkdown: reviewMarkdown, Goal: goalRecord, GoalContinue: goalContinue,
	}
	if err != nil {
		result.Error = err.Error()
	}
	w.publish("turn_done", result, true)
}

func replaceProjectInstructions(prompt, project string) string {
	start := strings.Index(prompt, "<project_instructions>")
	if start < 0 {
		return prompt
	}
	rest := prompt[start:]
	end := strings.Index(rest, "</project_instructions>")
	if end < 0 {
		return prompt
	}
	end += start + len("</project_instructions>")
	return strings.Trim(strings.TrimSpace(prompt[:start])+"\n\n"+strings.TrimSpace(project)+"\n\n"+strings.TrimSpace(prompt[end:]), "\n")
}

func (w *workerProcessState) emitWireEvent(value any) {
	event, ok := value.(map[string]any)
	if !ok {
		return
	}
	kind, _ := event["type"].(string)
	if kind == "" {
		return
	}
	data := make(map[string]any, len(event)-1)
	for key, value := range event {
		if key != "type" {
			data[key] = value
		}
	}
	switch kind {
	case "text":
		if value, ok := data["delta"].(string); ok {
			w.appendLive("text", value)
			w.publish(kind, value, false)
		}
	case "plan_delta":
		if value, ok := data["delta"].(string); ok {
			w.appendLive("plan", value)
			w.publish(workerwire.EventPlanDelta, value, false)
		}
	case "tool_start":
		if name, ok := data["name"].(string); ok {
			w.mu.Lock()
			w.activeTool = name
			w.mu.Unlock()
		}
		w.publish(kind, data, false)
	case "tool_output":
		if output, ok := data["output"].(string); ok {
			w.appendLive("tool_output", output)
		}
		w.publish(kind, data, false)
	case "tool_end":
		w.mu.Lock()
		w.activeTool = ""
		w.mu.Unlock()
		w.publish(kind, data, false)
	default:
		w.publish(kind, data, false)
	}
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
	if err != nil || !ok || record.Status != agent.GoalStatusActive {
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

func (w *workerProcessState) persistGoalTurn(input *agent.GoalRecord, usage models.Usage, turnErr error) {
	if w.store == nil || w.sessionID == "" || input == nil {
		return
	}
	record, ok, err := w.store.LoadGoal(w.sessionID)
	if err != nil || !ok || record.ID != input.ID {
		return
	}
	// An explicit clear/pause made while the turn was running wins over the
	// stale request-scoped goal supplied at turn start.
	if record.Status != agent.GoalStatusActive && record.Status != agent.GoalStatusBlocked && record.Status != agent.GoalStatusComplete {
		return
	}
	record.PromptTokens += usage.PromptTokens
	record.CachedTokens += usage.Cached()
	record.CompletionTokens += usage.CompletionTokens
	record.Rounds++
	if turnErr != nil && record.Status == agent.GoalStatusActive {
		record.Status = agent.GoalStatusPaused
		record.Blocker = truncateWorkerGoalNote(turnErr.Error())
	}
	if record.Status == agent.GoalStatusActive && record.Rounds >= w.goalMaxRounds() {
		record.Status = agent.GoalStatusBudgetLimited
		record.Blocker = fmt.Sprintf("goal round circuit breaker reached (%d rounds)", record.Rounds)
	}
	record.UpdatedAt = time.Now().UTC()
	if record.Status == agent.GoalStatusActive {
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
	if len(value) <= agent.MaxNoteBytes {
		return value
	}
	return value[:agent.MaxNoteBytes]
}

func (w *workerProcessState) fork(request workerwire.ForkRequest) (workerwire.ForkResult, error) {
	if err := w.requireIdleHistory(); err != nil {
		return workerwire.ForkResult{}, err
	}
	if w.store == nil || w.sessionID == "" {
		return workerwire.ForkResult{}, errors.New("session store unavailable")
	}
	title := strings.TrimSpace(request.Title)
	if title == "" {
		return workerwire.ForkResult{}, errors.New("fork needs a title")
	}
	w.persist()
	if w.ag == nil {
		return workerwire.ForkResult{}, errors.New("no active agent to fork")
	}
	msgs := w.ag.MessagesSnapshot()
	if len(msgs) <= 1 {
		return workerwire.ForkResult{}, errors.New("nothing to fork yet")
	}
	cut := min(max(request.Cut, 0), len(msgs)-1)
	oldID := w.sessionID
	oldTitle := oldID
	if meta, _, err := w.store.Load(oldID); err == nil && meta.Title != "" {
		oldTitle = meta.Title
	}
	newID, err := w.store.Fork(oldID, cut, title)
	if err != nil {
		return workerwire.ForkResult{}, fmt.Errorf("fork: %w", err)
	}
	return workerwire.ForkResult{
		NewSessionID: newID,
		OldSessionID: oldID,
		Title:        title,
		OldTitle:     oldTitle,
	}, nil
}

func (w *workerProcessState) rename(request workerwire.RenameRequest) (workerwire.RenameResult, error) {
	if w.store == nil || w.sessionID == "" {
		return workerwire.RenameResult{}, errors.New("session store unavailable")
	}
	title := strings.TrimSpace(request.Title)
	if title == "" {
		return workerwire.RenameResult{}, errors.New("title is required")
	}
	if err := w.store.SetTitle(w.sessionID, title); err != nil {
		return workerwire.RenameResult{}, fmt.Errorf("rename: %w", err)
	}
	return workerwire.RenameResult{
		SessionID: w.sessionID,
		Title:     title,
	}, nil
}

package agent

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"

	"github.com/sacca97/ghg/internal/artifact"
	"github.com/sacca97/ghg/internal/llm"
)

// compactKeepBack counts assistant turns (and any tool results they pulled in)
// preserved verbatim at the tail of the history. Keeping recent context means
// any in-flight task the model is working on keeps its tool results in view,
// and we never leave an orphaned tool_call whose result the summary dropped.
const compactKeepBack = 6

// defaultCompactThreshold is the fraction of the provider-advertised context
// window at which Turn compacts proactively when CompactThreshold is unset.
// 50% keeps compaction deterministic instead of letting the context bloat.
const defaultCompactThreshold = 0.5

// threshold is the proactive-compaction fraction of ContextLimit.
func (a *Agent) threshold() float64 {
	if a.CompactThreshold > 0 {
		return a.CompactThreshold
	}
	return defaultCompactThreshold
}

// budget returns the maximum active token count before proactive compaction triggers.
func (a *Agent) budget() int {
	if a.ContextLimit <= 0 {
		return 0
	}
	percentBudget := int(a.threshold() * float64(a.ContextLimit))
	reserve := a.OutputReserve
	if reserve <= 0 {
		reserve = max(a.MaxTokens, 16384)
	}
	if reserve > 0 && a.ContextLimit > reserve {
		reserveBudget := a.ContextLimit - reserve
		if reserveBudget > 0 && reserveBudget < percentBudget {
			return reserveBudget
		}
	}
	return percentBudget
}

// maybeCompact folds old turns into a summary once the active token
// pressure crosses the preflight budget. Before the first successful response
// that size is estimated from local messages. It no-ops when the provider didn't
// advertise a limit (ContextLimit == 0) — the reactive context-limit retry in
// Turn still covers that case.
func (a *Agent) maybeCompact(ctx context.Context, ev Events) error {
	if !a.Checkpointing || a.ContextLimit == 0 || a.ActiveTokens() < a.budget() {
		return nil
	}
	took := len(a.Messages)
	sum, cutoff, err := a.compactWithEvents(ctx, ev)
	if err != nil {
		if err.Error() == "not enough history to compact" {
			return nil // too little history to fold; rely on the reactive retry
		}
		return err
	}
	if ev.OnCompact != nil {
		ev.OnCompact(took-len(a.Messages), len(a.Messages))
	}
	if ev.OnCompacted != nil {
		ev.OnCompacted(sum, cutoff)
	}
	return nil
}

// EstimateTokens approximates message sizes for compaction's bounded-tail
// selection and context diagnostics. The status display and proactive
// compaction trigger use Agent.ContextTokens, which is provider-reported.
// No real tokenizer is wired in, so this uses the common ~4 chars/token
// heuristic for message content and tool-call arguments, plus a small
// per-message overhead for roles and tool-call framing.
func EstimateTokens(msgs []llm.Message) int {
	total := 0
	for _, m := range msgs {
		total += 4 + (len(m.TextContent())+3)/4 + 1200*len(m.Parts) // ~tokens for an image
		for _, tc := range m.ToolCalls {
			total += 8 + (len(tc.Function.Name)+len(tc.Function.Arguments)+3)/4
		}
	}
	return total
}

// compact replaces old turns with an LLM-generated summary, keeping the
// system prompt and a token-budgeted recent tail so recent tool results and
// any in-flight assistant action stay intact. It runs a single
// non-streaming completion — on CompactBackend/CompactModel when set, else
// on the conversation's own backend and model — and stores the summary as a
// system-role message (it must carry no tool_call IDs that the kept tail
// would orphan).
func (a *Agent) compactWithEvents(ctx context.Context, ev Events) (summary string, cutoff int, err error) {
	if len(a.Messages) < 3 { // system + ≥1 user + one later message
		return "", 0, errors.New("not enough history to compact")
	}
	const sysIdx = 0
	sysPrompt := a.Messages[sysIdx]
	tailStart, tail := compactTail(a.Messages, a.ContextLimit)
	if tailStart <= sysIdx+1 {
		return "", 0, errors.New("not enough history to compact")
	}
	history := a.Messages[sysIdx+1 : tailStart]
	summaryPrompt := buildSummaryPrompt(history)
	backend, mdl := a.CompactBackend, a.CompactModel
	if backend == nil {
		backend = a.Backend
	}
	if mdl == "" {
		mdl = a.Model
	}
	role, provider, protocol := a.Role, a.Provider, a.Protocol
	if backend != a.Backend || mdl != a.Model {
		role = "tiny"
		provider = a.CompactProvider
		protocol = a.CompactProtocol
	}
	sum, usage, cerr := a.CompleteWithRoute(ctx, backend, role, provider, protocol, llm.Request{
		Model:     mdl,
		MaxTokens: 1024,
		Messages: []llm.Message{
			sysPrompt,
			{Role: "user", Content: summaryPrompt},
		},
	}, ev)
	a.AddUsage(usage) // the summary call is session spend too
	if ev.OnUsage != nil {
		ev.OnUsage(usage)
	}
	if cerr != nil {
		return "", 0, fmt.Errorf("compaction summary failed: %w", cerr)
	}
	summary = strings.TrimSpace(sum.TextContent())
	if summary == "" {
		return "", 0, errors.New("continuation checkpoint was empty")
	}
	if ev.OnCompactionReady != nil {
		if err := ev.OnCompactionReady(append([]llm.Message(nil), a.Messages...), summary, tailStart); err != nil {
			return "", 0, fmt.Errorf("persist raw history before compaction: %w", err)
		}
	}
	kept := append([]llm.Message(nil), tail...)
	manifest := buildArtifactManifest(summary, kept, a.Messages)
	view := []llm.Message{sysPrompt,
		{Role: "system", Content: "Summary of the conversation so far:\n\n" + summary},
	}
	if manifest != "" {
		view = append(view, llm.Message{Role: "system", Content: manifest})
	}
	view = append(view, kept...)
	a.msgsMu.Lock()
	a.Messages = view
	a.msgsMu.Unlock()
	return summary, tailStart, nil
}

// emergencyCutover is the deterministic last resort after a real context
// rejection and a failed semantic checkpoint. It is available only when the
// raw history has a durable session boundary, so the omitted range remains
// recoverable through history tools.
func (a *Agent) emergencyCutover(ctx context.Context, ev Events) (string, int, error) {
	if a == nil || a.HistoryCatalog == nil || a.currentSessionID() == "" {
		return "", 0, errors.New("emergency cutover requires a durable session")
	}
	if err := ctx.Err(); err != nil {
		return "", 0, err
	}
	tailStart, tail := compactTail(a.Messages, a.ContextLimit)
	if tailStart <= 1 || len(tail) == 0 {
		return "", 0, errors.New("no complete history tail fits an emergency cutover")
	}
	checkpoint := latestCheckpoint(a.Messages)
	marker := fmt.Sprintf("Emergency context cutover: raw messages before sequence %d were omitted without semantic summarization; continuation state may be incomplete. Use history_search/history_read to recover the omitted session history.", tailStart)
	summary := marker
	if checkpoint != nil {
		checkpointText := strings.TrimPrefix(checkpoint.Content, "Summary of the conversation so far:\n\n")
		if checkpointText != "" {
			summary += "\n\nLatest successful continuation checkpoint:\n" + checkpointText
		}
	}
	if ev.OnCompactionReady != nil {
		if err := ev.OnCompactionReady(append([]llm.Message(nil), a.Messages...), summary, tailStart); err != nil {
			return "", 0, fmt.Errorf("persist raw history before emergency cutover: %w", err)
		}
	}
	manifest := buildArtifactManifest("", tail, a.Messages)
	view := []llm.Message{a.Messages[0], {Role: "system", Content: "Summary of the conversation so far:\n\n" + summary}}
	if manifest != "" {
		view = append(view, llm.Message{Role: "system", Content: manifest})
	}
	view = append(view, tail...)
	a.msgsMu.Lock()
	a.Messages = view
	a.msgsMu.Unlock()
	return summary, tailStart, nil
}

func latestCheckpoint(msgs []llm.Message) *llm.Message {
	for i := len(msgs) - 1; i > 0; i-- {
		if msgs[i].Role != "system" || !strings.HasPrefix(msgs[i].Content, "Summary of the conversation so far:\n\n") {
			continue
		}
		copy := msgs[i]
		return &copy
	}
	return nil
}

const defaultCompactTailTokens = 32

// compactTail selects complete recent tool-call groups by estimated token
// budget. A context window uses a quarter for the verbatim tail; manual
// compaction without an advertised window uses a small deterministic floor.
func compactTail(msgs []llm.Message, contextLimit int) (int, []llm.Message) {
	budget := contextLimit / 4
	if budget < defaultCompactTailTokens {
		budget = defaultCompactTailTokens
	}
	start := len(msgs)
	used := 0
	for start > 1 {
		groupStart := compactGroupStart(msgs, start-1)
		cost := EstimateTokens(msgs[groupStart:start])
		if start < len(msgs) && used+cost > budget {
			break
		}
		start = groupStart
		used += cost
	}
	if start <= 1 {
		return start, nil
	}
	tail := append([]llm.Message(nil), msgs[start:]...)
	return start, shrinkCompactionTail(tail, budget)
}

func compactGroupStart(msgs []llm.Message, i int) int {
	if i < 1 || msgs[i].Role != "tool" {
		return i
	}
	for j := i - 1; j >= 1; j-- {
		if msgs[j].Role == "user" {
			break
		}
		if msgs[j].Role != "assistant" {
			continue
		}
		for _, tc := range msgs[j].ToolCalls {
			if tc.ID == msgs[i].ToolCallID {
				return j
			}
		}
	}
	return i
}

// shrinkCompactionTail keeps artifact references while shrinking an oversized
// recent batch. The source messages are copied, so the raw in-memory history
// and its persisted audit log remain untouched.
func shrinkCompactionTail(tail []llm.Message, budget int) []llm.Message {
	if EstimateTokens(tail) <= budget {
		return tail
	}
	// Keep enough room for the stable artifact reference itself. A tiny token
	// budget may not fit a full id plus marker and a head/tail slice, but
	// losing the id would make the retained evidence unreachable.
	maxBytes := max(budget*4, 256)
	for {
		out := append([]llm.Message(nil), tail...)
		for i := range out {
			if out[i].Role == "tool" {
				out[i].Content = shrinkCompactionContent(out[i].Content, maxBytes)
			}
		}
		if EstimateTokens(out) <= budget || maxBytes <= 256 {
			return out
		}
		maxBytes /= 2
	}
}

func shrinkCompactionContent(content string, maxBytes int) string {
	if len(content) <= maxBytes {
		return content
	}
	suffix := ""
	if i := strings.Index(content, "\n[artifact "); i >= 0 {
		suffix = content[i:]
	}
	marker := "\n… [preview shrunk during compaction]"
	available := maxBytes - len(marker) - len(suffix)
	if available < 2 {
		suffix = ""
		available = maxBytes - len(marker)
	}
	if available < 2 {
		return content[:maxBytes]
	}
	head := available / 2
	tail := available - head
	return content[:head] + marker + content[len(content)-tail-len(suffix):len(content)-len(suffix)] + suffix
}

// buildArtifactManifest keeps metadata for references the new prompt still
// names. References in the compacted tail are always retained; older ones are
// retained only when the generated summary cites their id or hash. This is a
// prompt aid, not a second source of truth—the session catalog remains the
// complete discovery surface.
func buildArtifactManifest(summary string, tail, all []llm.Message) string {
	refs := map[string]artifact.Ref{}
	for _, msg := range tail {
		if msg.Artifact != nil {
			refs[msg.Artifact.ID] = *msg.Artifact
		}
	}
	for _, msg := range all {
		if msg.Artifact == nil {
			continue
		}
		ref := *msg.Artifact
		if strings.Contains(summary, ref.ID) || (ref.Hash != "" && strings.Contains(summary, ref.Hash)) {
			refs[ref.ID] = ref
		}
	}
	if len(refs) == 0 {
		return ""
	}
	ids := make([]string, 0, len(refs))
	for id := range refs {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	var b strings.Builder
	b.WriteString("Artifact manifest (metadata only; use artifact_read for retained bytes):\n")
	for _, id := range ids {
		ref := refs[id]
		state := "complete"
		if !ref.Complete {
			state = "head/tail retained; middle omitted"
		}
		fmt.Fprintf(&b, "- %s hash=%s original_bytes=%d stored_bytes=%d %s\n",
			ref.ID, ref.Hash, ref.OriginalBytes, ref.StoredBytes, state)
	}
	return strings.TrimRight(b.String(), "\n")
}

// buildSummaryPrompt renders the unsummarized turns as a transcript the model
// folds into an actionable continuation checkpoint. Tool results are
// truncated so a giant file read doesn't push the summary request over the
// window we just overflowed.
func buildSummaryPrompt(msgs []llm.Message) string {
	var b strings.Builder
	b.WriteString("Create a continuation checkpoint for an agent that will continue this exact task.\n")
	b.WriteString("Preserve:\n")
	b.WriteString("- current objective and explicit user constraints\n")
	b.WriteString("- established facts and important discoveries\n")
	b.WriteString("- decisions and their rationale\n")
	b.WriteString("- files modified and relevant symbols or locations\n")
	b.WriteString("- failed approaches and why they failed\n")
	b.WriteString("- verification performed and its result\n")
	b.WriteString("- unresolved problems and blockers\n")
	b.WriteString("- immediate next actions\n\n")
	b.WriteString("Exclude routine exploration unless it established a material fact. Preserve artifact IDs and incomplete-retention warnings. Never imply omitted artifact bytes were read.\n\n")
	b.WriteString("---\n\n")
	writeTranscript(&b, msgs)
	b.WriteString("\n---\n\nWrite the continuation checkpoint now.")
	return b.String()
}

// ManualCompact lets the TUI's /compact command compact on demand. It calls
// OnCompact and reports whether compaction ran (false when there's too
// little history). It is safe to call while a turn is not in flight.
func (a *Agent) ManualCompact(ctx context.Context, ev Events) error {
	if !a.Checkpointing {
		return errors.New("continuation checkpoints are disabled")
	}
	sum, cutoff, err := a.compactWithEvents(ctx, ev)
	if err != nil {
		return err
	}
	if ev.OnCompact != nil {
		ev.OnCompact(0, len(a.Messages))
	}
	if ev.OnCompacted != nil {
		ev.OnCompacted(sum, cutoff)
	}
	return nil
}

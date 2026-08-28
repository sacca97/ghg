// Package agent runs the LLM tool-use loop.
package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/sacca97/ghg/internal/artifact"
	"github.com/sacca97/ghg/internal/llm"
	"github.com/sacca97/ghg/internal/tools"
)

// Events receives streaming callbacks during a turn. All fields are optional.
type Events struct {
	OnText           func(delta string)               // assistant text as it streams
	OnThink          func(delta string)               // reasoning/thinking tokens as they stream
	OnToolStart      func(id, name, args string)      // a tool call is about to run
	OnToolOutput     func(id, output string)          // accumulated output while a tool call runs
	OnToolEnd        func(id, name, result string)    // a tool call finished
	OnSteer          func(text string)                // a steered message was injected
	OnCompact        func(took, kept int)             // context was auto-compacted (messages removed/kept)
	OnCompacted      func(summary string, cutoff int) // a compaction ran; record it (raw log survives)
	OnUsage          func(u llm.Usage)                // a request reported its token usage
	OnRetry          func(ev llm.RetryEvent)          // a transient request failure is being retried
	OnModelCallStart func(ModelCallStart)
	OnModelCallEnd   func(ModelCallEnd)
}

// ModelCallStart describes the route selected for one provider request.
// It is deliberately independent of agent-definition loading so callers can
// use the same telemetry for ordinary turns, planning, and compaction.
type ModelCallStart struct {
	Role     string `json:"role,omitempty"`
	Provider string `json:"provider,omitempty"`
	Model    string `json:"model"`
	Protocol string `json:"protocol,omitempty"`
}

// ModelCallEnd completes a ModelCallStart with request timing and provider
// metadata. Usage is included even when all its fields are zero: some
// providers omit accounting, and consumers should not infer that a call did
// not happen from a zero value.
type ModelCallEnd struct {
	ModelCallStart
	LatencyMS    int64     `json:"latency_ms"`
	FinishReason string    `json:"finish_reason,omitempty"`
	Usage        llm.Usage `json:"usage"`
	Error        string    `json:"error,omitempty"`
}

// ArtifactCatalog is the session-scoped metadata index used by the
// artifact_list and artifact_read tools. Implementations must enforce the
// supplied session id; the agent never accepts a path from the model.
type ArtifactCatalog interface {
	LookupArtifact(ctx context.Context, sessionID, id string) (artifact.Metadata, error)
	ListArtifacts(ctx context.Context, sessionID string, filter artifact.Filter, limit int) ([]artifact.Metadata, error)
}

// SubagentFactory builds a fresh agent for a delegated task. role is one of
// the config role names; the task tool currently supplies "tiny". Keeping the
// factory at this boundary lets the TUI and headless runner select a different
// provider/model without making the agent package depend on either UI.
type SubagentFactory func(ctx context.Context, role, systemPrompt string) (*Agent, error)

// Agent holds one conversation.
type Agent struct {
	Backend   llm.Backend
	Model     string // model id sent to the API
	ModelName string // config model name (may differ from Model via id mapping)
	Provider  string // config provider name
	Protocol  string // compiled adapter protocol, for model-call telemetry
	Role      string // selected model role (default, smart, fast, or tiny)
	MaxTokens int
	Effort    string // reasoning effort: "" = parameter omitted from requests
	// ReasoningToggle indicates that the selected model has a separate
	// on/off reasoning control from models.dev. Graded efforts still travel in
	// Effort; the neutral request carries the enable/disable bit separately.
	ReasoningToggle bool
	// SubagentFactory is optional. When set, delegated foreground and
	// background tasks use it to build their role-specific agent; nil preserves
	// the legacy behavior of cloning the parent backend.
	SubagentFactory SubagentFactory
	Tools           []tools.Tool
	Messages        []llm.Message
	// ArtifactWriter receives retained tool output before the model-facing
	// preview is shortened. Nil means no artifact persistence is configured.
	ArtifactWriter artifact.Writer
	// ArtifactCatalog resolves references for the read-only artifact tools.
	// It is deliberately separate from the payload writer so session scoping
	// remains an explicit boundary.
	ArtifactCatalog ArtifactCatalog
	ArtifactStore   *artifact.Store
	// ArtifactsDisabled distinguishes an intentional config opt-out from a
	// session store that is simply unavailable.
	ArtifactsDisabled bool

	// ContextLimit is the model's context window in tokens, as advertised by
	// the provider's GET /models (0 when unadvertised — proactive compaction
	// is then disabled and only the reactive context-limit retry applies).
	ContextLimit int
	// CompactBackend and CompactModel run the compaction summary; nil/"" uses
	// the conversation's own backend and model.
	CompactBackend llm.Backend
	CompactModel   string
	// CompactThreshold is the fraction of ContextLimit at which Turn compacts
	// proactively; 0 uses defaultCompactThreshold.
	CompactThreshold float64

	// MaxTurns caps the tool-call loop (rounds of model→tools→model) so a
	// scripted run can't run away. 0 = uncapped (the TUI default).
	MaxTurns int

	mu        sync.Mutex
	pending   []pendingSteer // steered user messages awaiting injection
	compacted bool           // a compaction already happened this turn — don't retry-loop

	// msgsMu guards Messages for concurrent READERS: the turn goroutine
	// mutates Messages freely, but a test/UI reader taking msgsMu sees a
	// consistent slice. Mutations hold it only for the append.
	msgsMu sync.Mutex

	files *fileLocks // per-path mutation locks for parallel tool calls
	bg    *taskRegistry

	// Todos is the todowrite plan, rewritten in full by the model and
	// injected per round. Like Messages, it is only mutated by the turn
	// goroutine; the TUI reads it between turns via TodosJSON.
	Todos []Todo

	sessionID atomic.Pointer[string] // scopes memory and artifact tools

	// toolsMu guards mcpTools: the MCP manager's OnChange can fire (server
	// settled) while a Turn is streaming, and Turn reads the tool set per
	// request.
	toolsMu  sync.Mutex
	mcpTools []tools.Tool

	usageMu sync.Mutex
	usage   llm.Usage // session totals across every API call (PromptTokens = input)
}

// Steer queues a user message for injection at the next loop boundary of the
// running turn — after the in-flight response and its tool calls complete,
// never mid-generation.
func (a *Agent) Steer(text string) {
	a.mu.Lock()
	a.pending = append(a.pending, pendingSteer{text: text})
	a.mu.Unlock()
}

// pendingSteer is a queued steered message, optionally carrying images.
type pendingSteer struct {
	text  string
	parts []llm.ContentPart
}

// SteerImages is Steer with image parts — the model receives text and
// images together as a multimodal user message at the loop boundary.
func (a *Agent) SteerImages(text string, parts []llm.ContentPart) {
	a.mu.Lock()
	a.pending = append(a.pending, pendingSteer{text: text, parts: parts})
	a.mu.Unlock()
}

// AppendUser adds a non-authored user message to the conversation outside a
// turn — the `!` shell escape shares its output with the model this way. It
// must only be called while no turn is running (the TUI routes mid-turn
// output through Steer instead); the mutex exists so a raced caller trips
// -race on the same word rather than silently tearing the slice.
func (a *Agent) AppendUser(content string) {
	a.mu.Lock()
	a.msgsMu.Lock()
	a.Messages = append(a.Messages, llm.Message{Role: "user", Content: content})
	a.msgsMu.Unlock()
	a.mu.Unlock()
}

func (a *Agent) drainPending() []pendingSteer {
	a.mu.Lock()
	defer a.mu.Unlock()
	p := a.pending
	a.pending = nil
	return p
}

// AddUsage folds one request's usage into the session totals.
func (a *Agent) AddUsage(u llm.Usage) {
	a.usageMu.Lock()
	a.usage.PromptTokens += u.PromptTokens
	a.usage.CompletionTokens += u.CompletionTokens
	if u.PromptTokensDetails != nil {
		if a.usage.PromptTokensDetails == nil {
			a.usage.PromptTokensDetails = &struct {
				CachedTokens int `json:"cached_tokens"`
			}{}
		}
		a.usage.PromptTokensDetails.CachedTokens += u.PromptTokensDetails.CachedTokens
	}
	a.usageMu.Unlock()
}

// SetUsage seeds the session totals with stored values — a resumed session
// keeps counting from where it was saved, not from zero.
func (a *Agent) SetUsage(u llm.Usage) {
	a.usageMu.Lock()
	a.usage = u
	a.usageMu.Unlock()
}

// ResetUsage zeroes the session totals — /clear starts the spend counter
// over along with the conversation.
func (a *Agent) ResetUsage() {
	a.usageMu.Lock()
	a.usage = llm.Usage{}
	a.usageMu.Unlock()
}

// Usage returns the session's cumulative token usage: input, output, and
// cached-input tokens across every streamed call (plus compaction and
// subagent calls on this agent).
func (a *Agent) Usage() llm.Usage {
	a.usageMu.Lock()
	defer a.usageMu.Unlock()
	u := a.usage
	if a.usage.PromptTokensDetails != nil {
		d := *a.usage.PromptTokensDetails
		u.PromptTokensDetails = &d
	}
	return u
}

// ContextTokens returns the token size reported for the latest successful
// conversation request. PromptTokens already includes any provider-reported
// prompt-cache input, so the context size is simply prompt plus completion.
// It is zero before a successful assistant response is recorded.
func (a *Agent) ContextTokens() int {
	a.msgsMu.Lock()
	defer a.msgsMu.Unlock()
	for i := len(a.Messages) - 1; i >= 0; i-- {
		msg := a.Messages[i]
		if msg.Role != "assistant" || msg.Usage == nil {
			continue
		}
		return max(msg.Usage.PromptTokens+msg.Usage.CompletionTokens, 0)
	}
	return 0
}

func New(backend llm.Backend, model string, maxTokens int, systemPrompt string) *Agent {
	a := &Agent{
		Backend:   backend,
		Model:     model,
		MaxTokens: maxTokens,
		Messages:  []llm.Message{{Role: "system", Content: systemPrompt}},
	}
	if p, ok := backend.(llm.ProtocolBackend); ok {
		a.Protocol = string(p.AdapterProtocol())
	}
	a.Tools = tools.All()
	a.Tools = append(a.Tools, taskTool(a))
	a.Tools = append(a.Tools, todoTool(a))
	a.Tools = append(a.Tools, memoryTools(a)...)
	a.Tools = append(a.Tools, artifactTools(a)...)
	a.files = newFileLocks()
	a.bg = newTaskRegistry()
	return a
}

// newSubagent creates the fresh, non-recursive agent used by task calls and
// copies the parent-owned session resources onto it. The factory, when
// installed, controls only the route; task isolation and accounting remain
// the agent package's responsibility.
func (a *Agent) newSubagent(ctx context.Context, role string) (*Agent, error) {
	var sub *Agent
	var err error
	if a.SubagentFactory != nil {
		sub, err = a.SubagentFactory(ctx, role, subagentPrompt())
	} else {
		sub = New(a.Backend, a.Model, a.MaxTokens, subagentPrompt())
	}
	if err != nil {
		return nil, err
	}
	if sub == nil {
		return nil, fmt.Errorf("subagent factory returned no agent for role %q", role)
	}
	sub.Effort = a.Effort
	if a.SubagentFactory == nil {
		sub.ReasoningToggle = a.ReasoningToggle
	} else if strings.EqualFold(sub.Effort, "on") && !sub.ReasoningToggle {
		// A role-specific model may not expose the parent's toggle surface.
		// Do not send the UI sentinel as a provider reasoning_effort value.
		sub.Effort = ""
	}
	if sub.ContextLimit == 0 {
		sub.ContextLimit = a.ContextLimit
	}
	// The task tool is deliberately non-recursive: replace any factory-built
	// default tool set with the ordinary built-ins, then restore artifact tools.
	sub.Tools = tools.All()
	sub.ArtifactWriter = a.ArtifactWriter
	sub.ArtifactCatalog = a.ArtifactCatalog
	sub.ArtifactStore = a.ArtifactStore
	sub.ArtifactsDisabled = a.ArtifactsDisabled
	sub.SetSessionID(a.currentSessionID())
	sub.Tools = append(sub.Tools, artifactTools(sub)...)
	return sub, nil
}

// MessagesSnapshot returns a copy of the conversation safe to read while a
// turn runs on another goroutine. Direct field access (a.Messages) is only
// safe for the goroutine driving the turn.
func (a *Agent) MessagesSnapshot() []llm.Message {
	a.msgsMu.Lock()
	defer a.msgsMu.Unlock()
	return append([]llm.Message(nil), a.Messages...)
}

// SetMCPTools swaps in the current MCP tool set (called by the MCP manager's
// OnChange whenever a server settles). MCP tools live separately from
// a.Tools so a settle mid-turn never mutates the slice a Turn is reading.
// A Suggester is installed on first use so a stale/typo'd mcp__ call gets a
// "did you mean?" nudge instead of a dead end.
func (a *Agent) SetMCPTools(ts []tools.Tool) {
	a.toolsMu.Lock()
	a.mcpTools = ts
	a.toolsMu.Unlock()
	if tools.Suggester == nil {
		tools.Suggester = func(name string) []string { return a.suggest(name) }
	}
}

// suggest lists candidate names for tools.Suggester: built-ins + live MCP
// tools, filtered by the mcp package's edit-distance logic.
func (a *Agent) suggest(name string) []string {
	a.toolsMu.Lock()
	all := append(append([]tools.Tool(nil), a.Tools...), a.mcpTools...)
	a.toolsMu.Unlock()
	names := make([]string, len(all))
	for i, t := range all {
		names[i] = t.Def.Function.Name
	}
	return tools.SuggestTool(name, names)
}

// AllTools returns built-ins + the current MCP set.
func (a *Agent) AllTools() []tools.Tool {
	a.toolsMu.Lock()
	defer a.toolsMu.Unlock()
	return append(append([]tools.Tool(nil), a.Tools...), a.mcpTools...)
}

// Stream performs one streaming model call and emits the start/end telemetry
// around the complete backend operation, including any adapter retry delay.
// Usage accounting remains with the caller because different call paths fold
// it into different agent/session owners.
func (a *Agent) Stream(ctx context.Context, req llm.Request, sink llm.EventSink, ev Events) (llm.Message, llm.Usage, error) {
	return a.callStream(ctx, a.Backend, a.Role, a.Provider, a.Protocol, req, sink, ev)
}

// Complete performs one non-streaming model call with the same telemetry
// boundary as Stream. It does not mutate the agent's usage totals.
func (a *Agent) Complete(ctx context.Context, req llm.Request, ev Events) (llm.Message, llm.Usage, error) {
	return a.callComplete(ctx, a.Backend, a.Role, a.Provider, a.Protocol, req, ev)
}

func (a *Agent) callInfo(backend llm.Backend, role, provider, protocol, model string) ModelCallStart {
	if p, ok := backend.(llm.ProtocolBackend); ok {
		protocol = string(p.AdapterProtocol())
	}
	if protocol == "" {
		protocol = a.Protocol
	}
	return ModelCallStart{Role: role, Provider: provider, Model: model, Protocol: protocol}
}

func (a *Agent) callStream(ctx context.Context, backend llm.Backend, role, provider, protocol string, req llm.Request, sink llm.EventSink, ev Events) (llm.Message, llm.Usage, error) {
	start := time.Now()
	call := a.callInfo(backend, role, provider, protocol, req.Model)
	if ev.OnModelCallStart != nil {
		ev.OnModelCallStart(call)
	}
	if backend == nil {
		err := errors.New("agent: nil backend")
		a.emitCallEnd(ev, call, start, llm.Message{}, llm.Usage{}, err)
		return llm.Message{}, llm.Usage{}, err
	}
	msg, usage, err := backend.Stream(ctx, req, sink)
	a.emitCallEnd(ev, call, start, msg, usage, err)
	return msg, usage, err
}

func (a *Agent) callComplete(ctx context.Context, backend llm.Backend, role, provider, protocol string, req llm.Request, ev Events) (llm.Message, llm.Usage, error) {
	start := time.Now()
	call := a.callInfo(backend, role, provider, protocol, req.Model)
	if ev.OnModelCallStart != nil {
		ev.OnModelCallStart(call)
	}
	if backend == nil {
		err := errors.New("agent: nil backend")
		a.emitCallEnd(ev, call, start, llm.Message{}, llm.Usage{}, err)
		return llm.Message{}, llm.Usage{}, err
	}
	msg, usage, err := backend.Complete(ctx, req)
	a.emitCallEnd(ev, call, start, msg, usage, err)
	return msg, usage, err
}

func (a *Agent) emitCallEnd(ev Events, call ModelCallStart, start time.Time, msg llm.Message, usage llm.Usage, err error) {
	if ev.OnModelCallEnd == nil {
		return
	}
	end := ModelCallEnd{
		ModelCallStart: call,
		LatencyMS:      time.Since(start).Milliseconds(),
		FinishReason:   msg.StopReason,
		Usage:          usage,
	}
	if err != nil {
		end.Error = err.Error()
	}
	ev.OnModelCallEnd(end)
}

// Turn sends user input and loops until the model stops calling tools.
// It returns the final assistant text. When the latest successful request's
// reported context size crosses CompactThreshold (default 50%) of the
// provider-advertised context limit, Turn compacts proactively before the next
// request; if the provider still rejects the request because the conversation
// exceeded its context window, Turn auto-compacts (summarizing old turns) and
// retries once before surfacing the error to the caller.
func (a *Agent) Turn(ctx context.Context, input string, ev Events) (string, error) {
	return a.turn(ctx, input, nil, false, ev)
}

// TurnAuthored is Turn for a message the human actually typed and submitted
// (vs. a steered background-task result or goal-continuation ghg injects).
// The message is marked Authored so input-history recall cycles only real
// submissions.
func (a *Agent) TurnAuthored(ctx context.Context, input string, ev Events) (string, error) {
	return a.turn(ctx, input, nil, true, ev)
}

// TurnWithImages is TurnAuthored for a submission that attaches images. Each
// part is a vision ContentPart (see llm.ImagePart); the model receives the
// text and the images together as a multimodal content array.
func (a *Agent) TurnWithImages(ctx context.Context, input string, parts []llm.ContentPart, ev Events) (string, error) {
	return a.turn(ctx, input, parts, true, ev)
}

func (a *Agent) turn(ctx context.Context, input string, parts []llm.ContentPart, authored bool, ev Events) (string, error) {
	msg := llm.Message{Role: "user", Content: input, Parts: parts, Authored: authored}
	if authored {
		now := time.Now()
		msg.SentAt = &now
	}
	a.msgsMu.Lock()
	a.Messages = append(a.Messages, msg)
	a.msgsMu.Unlock()
	rounds := 0
	for {
		if a.MaxTurns > 0 && rounds >= a.MaxTurns {
			return "", fmt.Errorf("max turns (%d) reached — the model kept calling tools; re-run with a higher -max-turns or a more specific prompt", a.MaxTurns)
		}
		rounds++
		if err := a.maybeCompact(ctx, ev); err != nil {
			return "", err
		}
		msgs := a.Messages
		if block := a.todoBlock(); block != "" {
			// Open plan items ride along as an ephemeral system message each
			// round: a.Messages stays clean, and the plan survives long tool
			// loops and compaction because it is re-derived, not stored.
			msgs = append(append([]llm.Message(nil), a.Messages...),
				llm.Message{Role: "system", Content: block})
		}
		// Surface transient-request retries through the event hook so the UI
		// shows "retrying" instead of looking hung. The sink is request-local;
		// the backend remains safe to share with foreground and background turns.
		reasoningEffort, reasoningEnabled := a.ReasoningRequest()
		msg, usage, err := a.Stream(ctx, llm.Request{
			Model:            a.Model,
			Messages:         msgs,
			Tools:            tools.Defs(a.AllTools()),
			ReasoningEffort:  reasoningEffort,
			ReasoningEnabled: reasoningEnabled,
		}, llm.EventSink{
			OnText:  ev.OnText,
			OnThink: ev.OnThink,
			OnRetry: ev.OnRetry,
		}, ev)
		a.AddUsage(usage)
		if ev.OnUsage != nil {
			ev.OnUsage(usage)
		}
		if err != nil {
			if !a.compacted && llm.IsContextLimit(err) && ctx.Err() == nil {
				a.compacted = true
				took := len(a.Messages)
				sum, cutoff, cerr := a.compactWithEvents(ctx, ev)
				if cerr != nil {
					// restore the guard on hard errors so a manual /compact
					// can still attempt a compaction for the next turn
					a.compacted = false
					return "", cerr
				}
				if ev.OnCompact != nil {
					ev.OnCompact(took-len(a.Messages), len(a.Messages))
				}
				if ev.OnCompacted != nil {
					ev.OnCompacted(sum, cutoff)
				}
				continue // retry the (now-smaller) request
			}
			return "", err
		}
		msg.Usage = &usage
		msg.Model = a.Model + " @ " + a.Provider
		a.msgsMu.Lock()
		a.Messages = append(a.Messages, msg)
		a.msgsMu.Unlock()
		if len(msg.ToolCalls) > 0 {
			results := a.runToolResults(ctx, msg.ToolCalls, ev)
			a.msgsMu.Lock()
			for i, tc := range msg.ToolCalls {
				a.Messages = append(a.Messages, llm.Message{
					Role:       "tool",
					Content:    tools.ModelText(results[i]),
					ToolCallID: tc.ID,
					Name:       tc.Function.Name,
					Artifact:   results[i].Artifact,
					ExitCode:   results[i].ExitCode,
					Source:     results[i].Source,
				})
			}
			a.msgsMu.Unlock()
			if ctx.Err() != nil {
				return "", ctx.Err()
			}
		}
		steered := a.drainPending()
		if len(steered) > 0 {
			a.msgsMu.Lock()
		}
		for _, s := range steered {
			if ev.OnSteer != nil {
				ev.OnSteer(s.text)
			}
			a.Messages = append(a.Messages, llm.Message{Role: "user", Content: s.text, Parts: s.parts})
		}
		if len(steered) > 0 {
			a.msgsMu.Unlock()
		}
		if len(msg.ToolCalls) == 0 && len(steered) == 0 {
			a.compacted = false // reset for the next Turn
			return msg.Content, nil
		}
	}
}

// ReasoningRequest returns the provider-neutral reasoning fields for the
// selected model. Toggle-only models use an explicit enable bit; for a model
// with both a toggle and graded efforts, the bit follows whether an effort is
// selected. Models without a toggle keep the legacy effort-only request.
func (a *Agent) ReasoningRequest() (string, *bool) {
	effort := strings.TrimSpace(a.Effort)
	if !a.ReasoningToggle {
		if strings.EqualFold(effort, "on") {
			return "", nil
		}
		return effort, nil
	}
	enabled := effort != "" && !strings.EqualFold(effort, "off") && !strings.EqualFold(effort, "none")
	if !enabled || strings.EqualFold(effort, "on") {
		effort = ""
	}
	return effort, &enabled
}

// runTools executes a batch of tool calls concurrently, returning one result
// per call in the original order (the API matches tool results to call IDs, so
// order must be preserved even though execution is parallel). This is the
// channel-native version of pi's executeToolCallsParallel + withFileMutationQueue:
//
//   - Each call runs in its own goroutine; a buffered results channel collects
//     (index, output) pairs, and a final pass lays them back out in order.
//   - Mutations to the same file serialize through a per-path channel
//     semaphore (fileLocks), so two edits to foo.go can't interleave; edits to
//     different files run truly in parallel.
//   - bash takes a global lock: its side effects aren't attributable to a path.
//   - OnToolStart/OnToolEnd fire per call so the UI shows each tool as it
//     begins and lands, not in a burst at the end.
func (a *Agent) runTools(ctx context.Context, calls []llm.ToolCall, ev Events) []string {
	detailed := a.runToolResults(ctx, calls, ev)
	results := make([]string, len(detailed))
	for i := range detailed {
		results[i] = detailed[i].Preview
	}
	return results
}

// runToolResults is the structured implementation behind runTools. The
// compatibility wrapper keeps tests and integrations that only need strings
// unchanged while the agent turn retains artifact references and execution
// metadata on the persisted tool message.
func (a *Agent) runToolResults(ctx context.Context, calls []llm.ToolCall, ev Events) []tools.ToolResult {
	return a.runToolResultsWithTools(ctx, calls, ev, a.AllTools())
}

func (a *Agent) runToolResultsWithTools(ctx context.Context, calls []llm.ToolCall, ev Events, available []tools.Tool) []tools.ToolResult {
	results := make([]tools.ToolResult, len(calls))
	type outcome struct {
		i      int
		result tools.ToolResult
		ms     int64 // wall-clock run time, stored on the ToolCall for /tools perf
	}
	outCh := make(chan outcome, len(calls)) // buffered: never blocks the workers

	var wg sync.WaitGroup
	for i, tc := range calls {
		wg.Add(1)
		go func(i int, tc llm.ToolCall) {
			defer wg.Done()
			name, args := tc.Function.Name, tc.Function.Arguments

			// Serialize against other mutations before starting. Acquiring here
			// (before OnToolStart) keeps "running" rows honest: a tool only
			// shows as running once it actually holds its lock.
			var release func()
			if a.files != nil {
				if path, ok := toolMutationPath(name, args); ok {
					release = a.files.acquirePath(path)
				} else if name == "bash" {
					release = a.files.acquireGlobal()
				}
			}
			if release != nil {
				defer release()
			}

			if ev.OnToolStart != nil {
				ev.OnToolStart(tc.ID, name, args)
			}
			start := time.Now()
			toolCtx := ctx
			if ev.OnToolOutput != nil {
				toolCtx = tools.WithOnUpdate(ctx, func(output string) {
					ev.OnToolOutput(tc.ID, output)
				})
			}
			result := tools.ExecuteResult(toolCtx, available, name, json.RawMessage(args))
			result = a.attachArtifact(ctx, result)
			ms := time.Since(start).Milliseconds()
			if ev.OnToolEnd != nil {
				ev.OnToolEnd(tc.ID, name, result.Preview)
			}
			outCh <- outcome{i, result, ms}
		}(i, tc)
	}

	// Close the channel when all workers finish so the range loop terminates.
	go func() {
		wg.Wait()
		close(outCh)
	}()
	for oc := range outCh {
		results[oc.i] = oc.result
		calls[oc.i].DurationMs = oc.ms
		calls[oc.i].ExitCode = oc.result.ExitCode
	}
	return results
}

// attachArtifact persists retained evidence before the completion event is
// delivered. Artifact failures never fail a tool call: the bounded preview is
// still useful and remains the model-facing result.
func (a *Agent) attachArtifact(ctx context.Context, result tools.ToolResult) tools.ToolResult {
	if result.Artifact != nil || result.Retained == "" ||
		(result.Complete && result.OriginalBytes <= int64(len(result.Preview))) {
		return result
	}
	if a.ArtifactWriter == nil {
		if a.ArtifactsDisabled {
			result.Preview += "\n[artifact persistence disabled; omitted output is unrecoverable]"
		}
		return result
	}
	ref, err := a.ArtifactWriter.Put(ctx, artifact.PutRequest{
		Data:          []byte(result.Retained),
		OriginalBytes: result.OriginalBytes,
		Complete:      result.Complete,
		MediaType:     "text/plain",
		Metadata:      result.Metadata,
	})
	if err != nil {
		result.Preview += "\n[artifact unavailable; the omitted output cannot be recovered]"
		return result
	}
	result.Artifact = &ref
	result.Preview += tools.ArtifactReference(ref)
	return result
}

// toolExitCode infers an exit status from a tool's output. Tools signal errors
// by prefixing their output; 0 means success, 1 means the tool reported a
// failure. Best-effort: the exact status lives in the tool, not the output.
func toolExitCode(out string) int {
	if strings.HasPrefix(out, "error") || strings.HasPrefix(out, "Error") {
		return 1
	}
	return 0
}

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

// maybeCompact folds old turns into a summary once the latest successful
// request's reported context size crosses the threshold fraction of
// ContextLimit. Before the first successful response that size is zero. It
// no-ops when the provider didn't advertise a limit (ContextLimit == 0) — the
// reactive context-limit retry in Turn still covers that case.
func (a *Agent) maybeCompact(ctx context.Context, ev Events) error {
	if a.ContextLimit == 0 || a.ContextTokens() < int(a.threshold()*float64(a.ContextLimit)) {
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
//
// It returns the summary text and the cutoff (the index in the pre-compaction
// Messages the summary replaces, i.e. where the kept tail began). The caller
// records those as a compaction event so the raw log survives on disk.
func (a *Agent) compact(ctx context.Context) (summary string, cutoff int, err error) {
	return a.compactWithEvents(ctx, Events{})
}

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
	}
	sum, usage, cerr := a.callComplete(ctx, backend, role, provider, protocol, llm.Request{
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
// folds into a concise digest. Tool results are truncated so a giant file
// read doesn't push the summary request over the window we just overflowed.
func buildSummaryPrompt(msgs []llm.Message) string {
	var b strings.Builder
	b.WriteString("Summarize the following conversation between the user and the assistant. ")
	b.WriteString("Capture the user's intent, decisions made, work completed, files touched, ")
	b.WriteString("and any open task the assistant is mid-way through. ")
	b.WriteString("Be concise (a few short paragraphs at most); use bullet points for code/files. ")
	b.WriteString("Preserve artifact ids, hashes, sizes, and incomplete-retention warnings from the tool ledger. Never imply that omitted middle bytes were inspected. ")
	b.WriteString("Do not include verbatim tool output. End with a single line: ")
	b.WriteString("\"Open task: <what the assistant was doing last, or none>\".\n\n")
	b.WriteString("---\n\n")
	writeTranscript(&b, msgs)
	b.WriteString("\n---\n\nWrite the summary now.")
	return b.String()
}

// writeTranscript renders messages as a role-tagged transcript for a
// meta-prompt (compaction summary, goal formulation). Tool results are
// truncated so a giant file read doesn't blow up the request.
func writeTranscript(b *strings.Builder, msgs []llm.Message) {
	for _, m := range msgs {
		switch m.Role {
		case "user":
			fmt.Fprintf(b, "user: %s\n", truncateField(m.TextContent(), 2000))
		case "assistant":
			if c := strings.TrimSpace(m.TextContent()); c != "" {
				fmt.Fprintf(b, "assistant: %s\n", truncateField(c, 2000))
			}
			for _, tc := range m.ToolCalls {
				// Keep the established `tool(args)` prefix: goal formulation and
				// existing transcript consumers use it as a compact call shape.
				// The id and execution fields remain part of the ledger after it.
				fmt.Fprintf(b, "assistant called %s(%s) id=%s", tc.Function.Name, truncateField(tc.Function.Arguments, 500), tc.ID)
				if tc.DurationMs > 0 || tc.ExitCode != 0 {
					fmt.Fprintf(b, " [duration_ms=%d exit_code=%d]", tc.DurationMs, tc.ExitCode)
				}
				b.WriteByte('\n')
			}
		case "tool":
			source := m.Source
			if source == "" {
				source = m.Name
			}
			fmt.Fprintf(b, "tool result source=%s exit_code=%d: %s", source, m.ExitCode, truncateField(m.Content, 500))
			if m.Artifact != nil {
				ref := m.Artifact
				fmt.Fprintf(b, " [artifact id=%s hash=%s original_bytes=%d stored_bytes=%d complete=%t]",
					ref.ID, ref.Hash, ref.OriginalBytes, ref.StoredBytes, ref.Complete)
			}
			b.WriteByte('\n')
		}
	}
}

// GoalFromContextDefaultWindow is how many tail messages /goal-from-context
// distills when the user doesn't pass a count.
const GoalFromContextDefaultWindow = 8

// GoalFromContextMessages returns the last n conversation messages (the
// window /goal-from-context distills), skipping the system prompt. n <= 0
// means GoalFromContextDefaultWindow. Fewer than two messages in the window
// means there isn't enough context to formulate a goal.
func GoalFromContextMessages(msgs []llm.Message, n int) ([]llm.Message, error) {
	if n <= 0 {
		n = GoalFromContextDefaultWindow
	}
	if len(msgs) == 0 {
		return nil, errors.New("not enough context to formulate a goal — chat a bit first")
	}
	conv := msgs[1:]
	if len(conv) < 2 {
		return nil, errors.New("not enough context to formulate a goal — chat a bit first")
	}
	if n > len(conv) {
		n = len(conv)
	}
	return conv[len(conv)-n:], nil
}

// BuildGoalFromContextPrompt asks the model to distill the given tail
// messages into a concrete, verifiable goal statement suitable for /goal.
// The reply must be the bare goal text — the TUI sets it verbatim.
func BuildGoalFromContextPrompt(tail []llm.Message) string {
	var b strings.Builder
	b.WriteString("Distill the end of this conversation into a detailed goal the assistant should keep working on until it is verifiably done.\n\n")
	b.WriteString("Reply with ONLY the goal: a first line stating the concrete outcome, then a short bullet list of the specific, checkable completion criteria ")
	b.WriteString("(files to change, commands that must pass, behavior to confirm). Include the key constraints, decisions, and identifiers (file paths, function names, ")
	b.WriteString("error messages) from the conversation so the goal stands alone. No preamble, no quotes, no explanation.\n\n---\n\n")
	writeTranscript(&b, tail)
	b.WriteString("\n---\n\nWrite the goal now.")
	return b.String()
}

func truncateField(s string, n int) string {
	s = strings.ReplaceAll(s, "\n", " ")
	s = strings.TrimSpace(s)
	if len(s) > n {
		return s[:n-1] + "…"
	}
	return s
}

// ManualCompact lets the TUI's /compact command compact on demand. It calls
// OnCompact and reports whether compaction ran (false when there's too
// little history). It is safe to call while a turn is not in flight.
func (a *Agent) ManualCompact(ctx context.Context, ev Events) error {
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

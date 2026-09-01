// Package agent runs the LLM tool-use loop.
package agent

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/sacca97/ghg/internal/artifact"
	goalstate "github.com/sacca97/ghg/internal/goal"
	"github.com/sacca97/ghg/internal/llm"
	"github.com/sacca97/ghg/internal/observation"
	"github.com/sacca97/ghg/internal/search"
	"github.com/sacca97/ghg/internal/session"
	"github.com/sacca97/ghg/internal/tools"
)

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
	// Runtime is the shared execution policy for native and subprocess tools.
	// Delegated agents inherit a child view in newSubagent.
	Runtime  *tools.ToolRuntime
	Tools    []tools.Tool
	Messages []llm.Message
	// ArtifactWriter receives retained tool output before the model-facing
	// preview is shortened. Nil means no artifact persistence is configured.
	ArtifactWriter artifact.Writer
	// ArtifactCatalog resolves references for the read-only artifact tools.
	// It is deliberately separate from the payload writer so session scoping
	// remains an explicit boundary.
	ArtifactCatalog ArtifactCatalog
	ArtifactStore   *artifact.Store
	// HistoryCatalog is the durable session boundary for bounded history
	// recall. It is optional so no-session runs never advertise recall tools.
	HistoryCatalog session.HistoryStore
	// ArtifactsDisabled distinguishes an intentional config opt-out from a
	// session store that is simply unavailable.
	ArtifactsDisabled bool
	// SubagentsDisabled suppresses the task tool so the model cannot launch subagents.
	SubagentsDisabled bool

	// ContextLimit is the model's context window in tokens, as advertised by
	// the provider's GET /models (0 when unadvertised — proactive compaction
	// is then disabled and only the reactive context-limit retry applies).
	ContextLimit int
	// Checkpointing and HistoryRecall are independent switches. New agents
	// enable both; callers can disable either without changing the other.
	Checkpointing bool
	HistoryRecall bool
	// CompactBackend and CompactModel run the compaction summary; nil/"" uses
	// the conversation's own backend and model. The provider/protocol fields
	// keep route telemetry correct when the summary uses the tiny role.
	CompactBackend  llm.Backend
	CompactModel    string
	CompactProvider string
	CompactProtocol string
	// CompactThreshold is the fraction of ContextLimit at which Turn compacts
	// proactively; 0 uses defaultCompactThreshold.
	CompactThreshold float64
	// OutputReserve is the token headroom reserved for model generation and
	// safety margin before proactive compaction triggers. 0 uses max(MaxTokens, 16384).
	OutputReserve int

	// MaxTurns caps the tool-call loop (rounds of model→tools→model) so a
	// scripted run can't run away. 0 = uncapped (the TUI default).
	MaxTurns int

	// PlanMode restricts the agent to a read-only tool allowlist and injects a
	// planning prompt. It is a collaboration mode on the same agent, not a
	// separate definition: the conversation and its message history carry over
	// between planning and execution.
	PlanMode bool

	mu        sync.Mutex
	pending   []pendingSteer // steered user messages awaiting injection
	compacted bool           // a compaction already happened this turn — don't retry-loop

	// msgsMu guards Messages for concurrent READERS: the turn goroutine
	// mutates Messages freely, but a test/UI reader taking msgsMu sees a
	// consistent slice. Mutations hold it only for the append.
	msgsMu sync.Mutex

	// stateMu guards the live observation/search registries and their durable
	// adapters. A TUI command can clear or replace an agent while a background
	// task is still constructing its subagent, so pointer hand-off must be
	// synchronized just like message snapshots.
	stateMu sync.RWMutex

	files *fileLocks // per-path mutation locks for parallel tool calls
	bg    *taskRegistry

	// These registries are shared with delegated agents so a read made by a
	// subagent and a later edit in the parent still use one session boundary.
	observations     *observation.Registry
	searchState      *search.Registry
	observationStore observation.Store
	searchStore      search.Store

	touchedMu sync.Mutex
	touched   map[string]struct{}

	operationMu   sync.Mutex
	seenOperation map[string]int

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

// ContextTokens returns the projected active token pressure of the conversation:
// the latest provider-reported base count plus estimated tokens of any
// messages/tool results appended since that report (or the full estimate if
// no report exists).
func (a *Agent) ContextTokens() int {
	return a.ActiveTokens()
}

// ActiveTokens returns the projected active token pressure of the conversation:
// the latest provider-reported base count plus estimated tokens of any
// messages/tool results appended since that report (or the full estimate if
// no report exists).
func (a *Agent) ActiveTokens() int {
	a.msgsMu.Lock()
	defer a.msgsMu.Unlock()
	for i := len(a.Messages) - 1; i >= 0; i-- {
		msg := a.Messages[i]
		if msg.Role == "assistant" && msg.Usage != nil && (msg.Usage.PromptTokens > 0 || msg.Usage.CompletionTokens > 0) {
			base := max(msg.Usage.PromptTokens+msg.Usage.CompletionTokens, 0)
			unreported := EstimateTokens(a.Messages[i+1:])
			return base + unreported
		}
	}
	return EstimateTokens(a.Messages)
}

func New(backend llm.Backend, model string, maxTokens int, systemPrompt string) *Agent {
	a := &Agent{
		Backend:       backend,
		Model:         model,
		MaxTokens:     maxTokens,
		Messages:      []llm.Message{{Role: "system", Content: systemPrompt}},
		Checkpointing: true,
		HistoryRecall: true,
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
	a.observations = observation.NewRegistry()
	a.searchState = search.NewRegistry()
	a.touched = make(map[string]struct{})
	a.seenOperation = make(map[string]int)
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
	observations, searchState, observationStore, searchStore := a.stateSnapshot()
	sub.stateMu.Lock()
	sub.observations = observations
	sub.searchState = searchState
	sub.observationStore = observationStore
	sub.searchStore = searchStore
	sub.stateMu.Unlock()
	sub.Checkpointing = a.Checkpointing
	sub.HistoryRecall = a.HistoryRecall
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
	sub.Runtime = a.Runtime.Child()
	sub.files = a.files
	sub.ArtifactWriter = a.ArtifactWriter
	sub.ArtifactCatalog = a.ArtifactCatalog
	sub.ArtifactStore = a.ArtifactStore
	sub.HistoryCatalog = a.HistoryCatalog
	sub.ArtifactsDisabled = a.ArtifactsDisabled
	sub.SubagentsDisabled = a.SubagentsDisabled
	sub.SetSessionID(a.currentSessionID())
	sub.Tools = append(sub.Tools, artifactTools(sub)...)
	return sub, nil
}

// SetObservationStore installs the durable observation mirror. The live
// registry remains owned by the agent and is safe to share with subagents.
func (a *Agent) SetObservationStore(store observation.Store) {
	if a == nil {
		return
	}
	a.stateMu.Lock()
	registry := a.observations
	a.observationStore = store
	if registry != nil {
		registry.SetPersistent(store)
	}
	a.stateMu.Unlock()
}

// SetSearchStore installs the durable search-snapshot mirror.
func (a *Agent) SetSearchStore(store search.Store) {
	if a == nil {
		return
	}
	a.stateMu.Lock()
	registry := a.searchState
	a.searchStore = store
	if registry != nil {
		registry.SetPersistent(store)
	}
	a.stateMu.Unlock()
}

// ResetState discards live observations, search cursors, and ranking hints
// when the conversation is cleared. Durable state remains available for the
// next session through the stores previously installed by the caller.
func (a *Agent) ResetState() {
	if a == nil {
		return
	}
	a.stateMu.Lock()
	observations := observation.NewRegistry()
	observations.SetPersistent(a.observationStore)
	searchState := search.NewRegistry()
	searchState.SetPersistent(a.searchStore)
	a.observations = observations
	a.searchState = searchState
	a.stateMu.Unlock()
	a.touchedMu.Lock()
	a.touched = make(map[string]struct{})
	a.touchedMu.Unlock()
}

// ShareState carries live observations, search snapshots, and ranking hints
// to a replacement agent during a model/provider switch in the same session.
func (a *Agent) ShareState(other *Agent) {
	if a == nil || other == nil {
		return
	}
	observations, searchState, observationStore, searchStore := a.stateSnapshot()
	a.touchedMu.Lock()
	touched := make(map[string]struct{}, len(a.touched))
	for path := range a.touched {
		touched[path] = struct{}{}
	}
	a.touchedMu.Unlock()
	other.stateMu.Lock()
	other.observations = observations
	other.searchState = searchState
	other.observationStore = observationStore
	other.searchStore = searchStore
	other.stateMu.Unlock()
	other.touchedMu.Lock()
	other.touched = touched
	other.touchedMu.Unlock()
}

// BindState persists observations and search snapshots collected before the
// session id existed. The caller owns the context and can bound database work.
func (a *Agent) BindState(ctx context.Context) error {
	if a == nil {
		return nil
	}
	observations, searchState, _, _ := a.stateSnapshot()
	id := a.currentSessionID()
	if err := observations.BindSession(ctx, id); err != nil {
		return err
	}
	return searchState.BindSession(ctx, id)
}

func (a *Agent) stateSnapshot() (*observation.Registry, *search.Registry, observation.Store, search.Store) {
	if a == nil {
		return nil, nil, nil, nil
	}
	a.stateMu.RLock()
	observations, searchState := a.observations, a.searchState
	observationStore, searchStore := a.observationStore, a.searchStore
	a.stateMu.RUnlock()
	return observations, searchState, observationStore, searchStore
}

// operationFingerprint is observation-only telemetry. It hashes canonical,
// redacted tool arguments so duplicate measurements never carry a secret or
// raw argument payload into an event stream.
func (a *Agent) operationFingerprint(name, args string) string {
	canonical := strings.TrimSpace(args)
	var value any
	if json.Unmarshal([]byte(canonical), &value) == nil {
		value = redactOperationValue(value, a.Runtime)
		if data, err := json.Marshal(value); err == nil {
			canonical = string(data)
		}
	} else if a.Runtime != nil {
		canonical = a.Runtime.RedactText(canonical)
	}
	sum := sha256.Sum256([]byte(name + "\x00" + canonical))
	return "sha256:" + hex.EncodeToString(sum[:])[:24]
}

func redactOperationValue(value any, runtime *tools.ToolRuntime) any {
	switch value := value.(type) {
	case map[string]any:
		for key, item := range value {
			if sensitiveOperationKey(key) {
				value[key] = "<redacted>"
				continue
			}
			value[key] = redactOperationValue(item, runtime)
		}
	case []any:
		for i, item := range value {
			value[i] = redactOperationValue(item, runtime)
		}
	case string:
		if runtime != nil {
			return runtime.RedactText(value)
		}
	}
	return value
}

func sensitiveOperationKey(key string) bool {
	key = strings.ToLower(strings.TrimSpace(key))
	for _, marker := range []string{"key", "token", "auth", "password", "secret", "credential", "cookie", "private"} {
		if strings.Contains(key, marker) {
			return true
		}
	}
	return false
}

func (a *Agent) seenOperationCount(fingerprint string) int {
	a.operationMu.Lock()
	defer a.operationMu.Unlock()
	if a.seenOperation == nil {
		a.seenOperation = make(map[string]int)
	}
	a.seenOperation[fingerprint]++
	return a.seenOperation[fingerprint]
}

// MessagesSnapshot returns a copy of the conversation safe to read while a
// turn runs on another goroutine. Direct field access (a.Messages) is only
// safe for the goroutine driving the turn.
func (a *Agent) MessagesSnapshot() []llm.Message {
	a.msgsMu.Lock()
	defer a.msgsMu.Unlock()
	return append([]llm.Message(nil), a.Messages...)
}

// SetSystemPrompt replaces the session's system prompt between turns. The
// worker attach path refreshes it with the current skills and project
// instructions without exposing the message slice to another goroutine.
func (a *Agent) SetSystemPrompt(prompt string) {
	if a == nil {
		return
	}
	a.msgsMu.Lock()
	if len(a.Messages) == 0 {
		a.Messages = []llm.Message{{Role: "system", Content: prompt}}
	} else {
		a.Messages[0].Role = "system"
		a.Messages[0].Content = prompt
	}
	a.msgsMu.Unlock()
}

// SetMCPTools swaps in the current MCP tool set (called by the MCP manager's
// OnChange whenever a server settles). MCP tools live separately from
// a.Tools so a settle mid-turn never mutates the slice a Turn is reading.
func (a *Agent) SetMCPTools(ts []tools.Tool) {
	a.toolsMu.Lock()
	a.mcpTools = ts
	a.toolsMu.Unlock()
}

// AllTools returns built-ins + the current MCP set.
func (a *Agent) AllTools() []tools.Tool {
	a.toolsMu.Lock()
	defer a.toolsMu.Unlock()
	all := append(append([]tools.Tool(nil), a.Tools...), a.mcpTools...)
	if a.HistoryRecall && a.HistoryCatalog != nil && a.currentSessionID() != "" {
		all = append(all, historyTools(a)...)
	}
	if a.SubagentsDisabled {
		filtered := make([]tools.Tool, 0, len(all))
		for _, t := range all {
			if t.Def.Function.Name != "task" {
				filtered = append(filtered, t)
			}
		}
		return filtered
	}
	return all
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
	return a.CompleteWithRoute(ctx, a.Backend, a.Role, a.Provider, a.Protocol, req, ev)
}

// CompleteWithRoute runs one non-streaming call through the shared telemetry
// wrapper using the route that actually owns backend. It is used by direct
// one-shot callers such as title generation and goal formulation, which do
// not run through Turn but must still report their provider and adapter.
func (a *Agent) CompleteWithRoute(ctx context.Context, backend llm.Backend, role, provider, protocol string, req llm.Request, ev Events) (llm.Message, llm.Usage, error) {
	return a.callCompletePurpose(ctx, backend, role, provider, protocol, "", req, ev)
}

// CompleteWithRoutePurpose is the telemetry-aware variant used by bounded
// internal model calls such as the optional approval reviewer. Purpose keeps
// that spend distinguishable from an ordinary tiny subagent turn.
func (a *Agent) CompleteWithRoutePurpose(ctx context.Context, backend llm.Backend, role, provider, protocol, purpose string, req llm.Request, ev Events) (llm.Message, llm.Usage, error) {
	return a.callCompletePurpose(ctx, backend, role, provider, protocol, purpose, req, ev)
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
	a.emitPromptView(ev, call, req)
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

func (a *Agent) callCompletePurpose(ctx context.Context, backend llm.Backend, role, provider, protocol, purpose string, req llm.Request, ev Events) (llm.Message, llm.Usage, error) {
	start := time.Now()
	call := a.callInfo(backend, role, provider, protocol, req.Model)
	call.Purpose = purpose
	a.emitPromptView(ev, call, req)
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

func (a *Agent) emitPromptView(ev Events, call ModelCallStart, req llm.Request) {
	if ev.OnPromptView == nil {
		return
	}
	data, _ := json.Marshal(req)
	ev.OnPromptView(PromptView{
		ModelCallStart:  call,
		MessageCount:    len(req.Messages),
		EstimatedTokens: EstimateTokens(req.Messages),
		SerializedBytes: len(data),
		ContextLimit:    a.ContextLimit,
	})
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
	return a.turn(ctx, input, nil, false, nil, ev)
}

// TurnAuthored is Turn for a message the human actually typed and submitted
// (vs. a steered background-task result or goal-continuation ghg injects).
// The message is marked Authored so input-history recall cycles only real
// submissions.
func (a *Agent) TurnAuthored(ctx context.Context, input string, ev Events) (string, error) {
	return a.turn(ctx, input, nil, true, nil, ev)
}

// TurnWithImages is TurnAuthored for a submission that attaches images. Each
// part is a vision ContentPart (see llm.ImagePart); the model receives the
// text and the images together as a multimodal content array.
func (a *Agent) TurnWithImages(ctx context.Context, input string, parts []llm.ContentPart, ev Events) (string, error) {
	return a.turn(ctx, input, parts, true, nil, ev)
}

// TurnWithGoal runs a normal turn with a request-scoped active goal context.
// The goal record is copied so model updates cannot mutate caller state; the
// caller receives each validated update through Events.OnGoalUpdate.
func (a *Agent) TurnWithGoal(ctx context.Context, input string, goal goalstate.Record, ev Events) (string, error) {
	return a.turn(ctx, input, nil, false, &goal, ev)
}

// TurnAuthoredWithGoal is TurnWithGoal for a human-authored submission.
func (a *Agent) TurnAuthoredWithGoal(ctx context.Context, input string, goal goalstate.Record, ev Events) (string, error) {
	return a.turn(ctx, input, nil, true, &goal, ev)
}

// TurnWithImagesAndGoal combines an authored multimodal turn with a
// request-scoped active goal context.
func (a *Agent) TurnWithImagesAndGoal(ctx context.Context, input string, parts []llm.ContentPart, goal goalstate.Record, ev Events) (string, error) {
	return a.turn(ctx, input, parts, true, &goal, ev)
}

// assembleRequestMessages constructs the model request messages while keeping
// a byte-stable prefix across turns and tool rounds:
//   1. Base system prompt (history[0])
//   2. Stable collaboration-mode prompt (planModePrompt, if PlanMode)
//   3. Budget reminder (if non-empty)
//   4. Conversation history (history[1:])
//   5. Trailing transient blocks (todoContent, goalContent)
func (a *Agent) assembleRequestMessages(history []llm.Message, todoContent, goalContent, budgetReminder string) []llm.Message {
	if len(history) == 0 {
		return history
	}
	prefixCount := 1
	if a.PlanMode {
		prefixCount++
		if budgetReminder != "" {
			prefixCount++
		}
	}
	suffixCount := 0
	if todoContent != "" {
		suffixCount++
	}
	if goalContent != "" {
		suffixCount++
	}
	reqMsgs := make([]llm.Message, 0, prefixCount+len(history)-1+suffixCount)
	reqMsgs = append(reqMsgs, history[0])
	if a.PlanMode {
		reqMsgs = append(reqMsgs, llm.Message{Role: "system", Content: planModePrompt})
		if budgetReminder != "" {
			reqMsgs = append(reqMsgs, llm.Message{Role: "system", Content: budgetReminder})
		}
	}
	if len(history) > 1 {
		reqMsgs = append(reqMsgs, history[1:]...)
	}
	if todoContent != "" {
		reqMsgs = append(reqMsgs, llm.Message{Role: "system", Content: todoContent})
	}
	if goalContent != "" {
		reqMsgs = append(reqMsgs, llm.Message{Role: "system", Content: goalContent})
	}
	return reqMsgs
}

func (a *Agent) turn(ctx context.Context, input string, parts []llm.ContentPart, authored bool, goalCtx *goalstate.Record, ev Events) (string, error) {
	var activeGoal *goalstate.Record
	if goalCtx != nil {
		goal := *goalCtx
		if err := goal.Validate(); err != nil {
			return "", fmt.Errorf("invalid goal context: %w", err)
		}
		if goal.Status == goalstate.StatusActive {
			activeGoal = &goal
		}
	}
	msg := llm.Message{Role: "user", Content: input, Parts: parts, Authored: authored}
	if authored {
		now := time.Now()
		msg.SentAt = &now
	}
	a.msgsMu.Lock()
	if len(a.Messages) > 0 && a.Messages[len(a.Messages)-1].Role == "user" && len(parts) == 0 && (strings.TrimSpace(input) == "continue" || strings.TrimSpace(input) == "Continue") {
		// User is explicitly asking to continue the prior unanswered prompt.
		prev := a.Messages[len(a.Messages)-1]
		msg.Content = prev.Content
		a.Messages[len(a.Messages)-1] = msg
	} else {
		a.Messages = append(a.Messages, msg)
	}
	a.msgsMu.Unlock()

	var planBudget *rolloutBudget
	if a.PlanMode && authored {
		planBudget = newPlanRolloutBudget()
	}

	rounds := 0
	for {
		if a.MaxTurns > 0 && rounds >= a.MaxTurns {
			return "", fmt.Errorf("max turns (%d) reached — the model kept calling tools; re-run with a higher -max-turns or a more specific prompt", a.MaxTurns)
		}
		rounds++
		if err := a.maybeCompact(ctx, ev); err != nil {
			return "", err
		}

		var budgetReminder string
		if planBudget != nil {
			budgetReminder = planBudget.ReminderBlock()
		}
		todoContent := a.todoBlock()
		var goalContent string
		if activeGoal != nil {
			goalContent = goalContextBlock(*activeGoal)
		}
		msgs := a.assembleRequestMessages(a.Messages, todoContent, goalContent, budgetReminder)

		available := a.AllTools()
		if a.PlanMode {
			if planBudget != nil && planBudget.IsReserveCrossed() {
				available = nil // Reserve crossed: disable tools for final synthesis request
			} else {
				available = a.planTools()
			}
		}
		if activeGoal != nil {
			// The goal tool is request-local. Ordinary conversations, Plan mode,
			// declarative definitions, and subagents never receive this control surface.
			available = append(available, goalUpdateTool(*activeGoal))
		}
		// Surface transient-request retries through the event hook so the UI
		// shows "retrying" instead of looking hung. The sink is request-local;
		// the backend remains safe to share with foreground and background turns.
		reasoningEffort, reasoningEnabled := a.ReasoningRequest()
		var parser *planStreamParser
		sink := llm.EventSink{
			OnThink: ev.OnThink,
			OnRetry: ev.OnRetry,
		}
		if a.PlanMode {
			// Plan mode routes streamed text through a block parser so the
			// proposed-plan artifact is surfaced via OnPlanDelta while the
			// surrounding conversational text still streams via OnText.
			parser = &planStreamParser{}
			parser.visible = ev.OnText
			parser.onPlan = ev.OnPlanDelta
			sink.OnText = parser.feed
		} else {
			sink.OnText = ev.OnText
		}
		msg, usage, err := a.Stream(ctx, llm.Request{
			Model:            a.Model,
			Messages:         msgs,
			Tools:            tools.Defs(available),
			ReasoningEffort:  reasoningEffort,
			ReasoningEnabled: reasoningEnabled,
			SessionID:        a.currentSessionID(),
		}, sink, ev)
		if a.PlanMode && parser != nil {
			parser.close()
		}
		a.AddUsage(usage)
		if ev.OnUsage != nil {
			ev.OnUsage(usage)
		}
		if planBudget != nil {
			planBudget.RecordUsage(usage, EstimateTokens(msgs))
		}
		if err != nil {
			if errors.Is(err, context.Canceled) || ctx.Err() != nil {
				if msg.Content != "" || len(msg.ToolCalls) > 0 {
					msg.Usage = &usage
					msg.Model = a.Model + " @ " + a.Provider
					msg.StopReason = "interrupted"
					a.msgsMu.Lock()
					a.Messages = append(a.Messages, msg)
					for _, tc := range msg.ToolCalls {
						a.Messages = append(a.Messages, llm.Message{
							Role:       "tool",
							Content:    "Error: tool call interrupted — the turn was canceled by user before execution completed",
							ToolCallID: tc.ID,
							Name:       tc.Function.Name,
						})
					}
					a.msgsMu.Unlock()
				}
			}
			if a.Checkpointing && !a.compacted && llm.IsContextLimit(err) && ctx.Err() == nil {
				a.compacted = true
				took := len(a.Messages)
				sum, cutoff, cerr := a.compactWithEvents(ctx, ev)
				if cerr != nil {
					if emergency, emergencyCutoff, emergencyErr := a.emergencyCutover(ctx, ev); emergencyErr == nil {
						if ev.OnCompact != nil {
							ev.OnCompact(took-len(a.Messages), len(a.Messages))
						}
						if ev.OnCompacted != nil {
							ev.OnCompacted(emergency, emergencyCutoff)
						}
						continue
					}
					// restore the guard on hard errors so a manual /compact
					// can still attempt a compaction for the next turn
					a.compacted = false
					return "", fmt.Errorf("context limit exceeded and continuation checkpoint failed: %w", cerr)
				}
				if ev.OnCompact != nil {
					ev.OnCompact(took-len(a.Messages), len(a.Messages))
				}
				if ev.OnCompacted != nil {
					ev.OnCompacted(sum, cutoff)
				}
				continue // retry the (now-smaller) request
			}
			if planBudget != nil && planBudget.Remaining() <= 0 {
				return "", &ErrPlanBudgetExhausted{
					Calls:        planBudget.calls,
					UsedUnits:    planBudget.usedUnits,
					FreshInput:   planBudget.freshInput,
					CachedInput:  planBudget.cachedInput,
					OutputTokens: planBudget.outputTokens,
				}
			}
			return "", err
		}
		msg.Usage = &usage
		msg.Model = a.Model + " @ " + a.Provider
		a.msgsMu.Lock()
		a.Messages = append(a.Messages, msg)
		a.msgsMu.Unlock()
		if len(msg.ToolCalls) > 0 {
			results := a.runToolResultsWithTools(ctx, msg.ToolCalls, ev, available)
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
			if activeGoal != nil {
				terminal := false
				for i := range results {
					update, ok := goalUpdateFromResult(results[i])
					if !ok {
						continue
					}
					if err := update.Validate(activeGoal.ID); err != nil {
						// The tool validates before producing metadata. Keep this
						// guard at the agent boundary in case a future result
						// producer supplies goal metadata directly.
						continue
					}
					activeGoal.Status = update.Status
					activeGoal.Progress = update.Progress
					activeGoal.Blocker = update.Blocker
					if ev.OnGoalUpdate != nil {
						ev.OnGoalUpdate(update)
					}
					if update.Status == goalstate.StatusBlocked || update.Status == goalstate.StatusComplete {
						terminal = true
					}
				}
				if terminal {
					a.compacted = false
					return msg.Content, nil
				}
			}
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

// runToolResultsWithTools executes a batch of tool calls concurrently, returning
// one structured result per call in the original order.

func (a *Agent) runToolResultsWithTools(ctx context.Context, calls []llm.ToolCall, ev Events, available []tools.Tool) []tools.ToolResult {
	for _, tc := range calls {
		a.recordTouched(tc.Function.Name, tc.Function.Arguments)
	}
	hints := a.searchHints()

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
			fingerprint := a.operationFingerprint(name, args)
			duplicate := a.seenOperationCount(fingerprint) > 1

			// Serialize against other mutations before starting. Acquiring here
			// (before OnToolStart) keeps "running" rows honest: a tool only
			// shows as running once it actually holds its lock.
			var release func()
			if a.files != nil {
				if paths := toolMutationPaths(name, args); len(paths) > 0 {
					release = a.files.acquirePaths(paths)
				} else if toolRequiresGlobalMutation(name, args) {
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
			observations, searchState, _, _ := a.stateSnapshot()
			toolCtx = tools.WithObservationStore(toolCtx, a.currentSessionID(), observations)
			toolCtx = tools.WithSearchStore(toolCtx, a.currentSessionID(), searchState)
			toolCtx = tools.WithSearchHints(toolCtx, hints)
			toolCtx = tools.WithSessionID(toolCtx, a.currentSessionID())
			toolCtx = tools.WithRuntime(toolCtx, a.Runtime)
			result := tools.ExecuteResult(toolCtx, available, name, json.RawMessage(args))
			if duplicate && (name == "read" || name == "grep" || name == "glob" || name == "find_files") {
				if result.ExitCode == 0 && len(result.Preview) > 0 {
					note := "\n[Notice: Exact duplicate query already executed earlier in this session. Use the evidence already gathered; proceed to synthesize or take action.]"
					result.Preview += note
					result.Retained += note
				}
			}
			result = a.attachArtifact(ctx, result)
			ms := time.Since(start).Milliseconds()
			if ev.OnToolTelemetry != nil {
				metadata := make(map[string]string, len(result.Metadata))
				for key, value := range result.Metadata {
					metadata[key] = value
				}
				ev.OnToolTelemetry(ToolTelemetry{
					ID:            tc.ID,
					Name:          name,
					PreviewBytes:  len(result.Preview),
					RetainedBytes: len(result.Retained),
					OriginalBytes: result.OriginalBytes,
					Truncated:     !result.Complete || result.OriginalBytes > int64(len(result.Preview)),
					BashRedirect:  result.Metadata["bash_redirect"] == "true",
					Fingerprint:   fingerprint,
					Duplicate:     duplicate,
					Metadata:      metadata,
				})
			}
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
			result.Preview = tools.TruncateWithSuffix(result.Preview, "\n[artifact persistence disabled; omitted output is unrecoverable]")
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
		result.Preview = tools.TruncateWithSuffix(result.Preview, "\n[artifact unavailable; the omitted output cannot be recovered]")
		return result
	}
	result.Artifact = &ref
	result.Preview = tools.TruncateWithSuffix(result.Preview, tools.ArtifactReference(ref))
	return result
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

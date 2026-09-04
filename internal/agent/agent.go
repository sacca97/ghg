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

	"github.com/sacca97/ghg/internal/memory"
	"github.com/sacca97/ghg/internal/models"
	"github.com/sacca97/ghg/internal/observation"
	"github.com/sacca97/ghg/internal/search"
	"github.com/sacca97/ghg/internal/session"
	"github.com/sacca97/ghg/internal/tools"
)

const (
	maxToolCallsPerResponse = 64
	maxConcurrentTools      = 4

	planFinalizationToolError   = "Error: exploration is complete for this plan. Tools are disabled; emit the final <proposed_plan> block now."
	reviewFinalizationToolError = "Error: exploration is complete for this review. Only submit_review is available; submit the best evidence-backed result now."
	malformedToolCallError      = "Error: tool call arguments were malformed (invalid JSON or exceeded the per-call size limit) and were omitted. Reissue the call with valid JSON arguments."
	oversizedToolBatchError     = "Error: the tool-call batch exceeded the aggregate argument size limit. Split the calls into smaller batches and reissue them with valid JSON arguments."

	maxToolCallArgBytes  = 256 * 1024 // 256 KiB
	maxToolBatchArgBytes = 512 * 1024 // 512 KiB
	// ponytail: bound duplicate telemetry memory; retain an LRU only if older
	// duplicate history becomes a product requirement.
	maxSeenOperations = 4096

	explorationCheckpointOne   = 10
	explorationCheckpointTwo   = 20
	explorationCheckpointFinal = 30
)

// HistoryCatalog is the durable session boundary for bounded history recall.
type HistoryCatalog interface {
	SearchHistory(context.Context, string, string, string, *int, int) ([]session.HistoryHit, error)
	ReadHistory(context.Context, string, int, int, *int, int) ([]session.HistoryMessage, []string, error)
}

type HistoryHit = session.HistoryHit
type HistoryMessage = session.HistoryMessage

// SubagentFactory builds a fresh agent for a delegated task. role is one of
// the config role names; the task tool currently supplies "tiny". Keeping the
// factory at this boundary lets the TUI and headless runner select a different
// provider/model without making the agent package depend on either UI.
type SubagentFactory func(ctx context.Context, role, systemPrompt string) (*Agent, error)

// Agent holds one conversation.
type Agent struct {
	Backend   models.Backend
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
	Messages []models.Message
	// Outputs receives retained tool output before the model-facing preview is
	// shortened. Nil disables output persistence.
	Outputs       *session.OutputStore
	OutputCatalog session.OutputCatalog
	// HistoryCatalog is the durable session boundary for bounded history
	// recall. It is optional so no-session runs never advertise recall tools.
	HistoryCatalog HistoryCatalog
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
	CompactBackend      models.Backend
	CompactModel        string
	CompactProvider     string
	CompactProtocol     string
	CompactContextLimit int
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

	// ReviewMode restricts the agent to a read-only tool allowlist plus
	// submit_review and injects a review prompt. It is a one-shot collaboration
	// mode that terminates upon successful review submission.
	ReviewMode bool

	// AskMode restricts one turn to read-only tools and injects a direct
	// question-answering prompt.
	AskMode bool

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

	sessionID atomic.Pointer[string] // scopes memory and output tools

	// toolsMu guards mcpTools: the MCP manager's OnChange can fire (server
	// settled) while a Turn is streaming, and Turn reads the tool set per
	// request.
	toolsMu  sync.Mutex
	mcpTools []tools.Tool

	usageMu sync.Mutex
	usage   models.Usage // session totals across every API call (PromptTokens = input)
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
	parts []models.ContentPart
}

// SteerImages is Steer with image parts — the model receives text and
// images together as a multimodal user message at the loop boundary.
func (a *Agent) SteerImages(text string, parts []models.ContentPart) {
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
	a.Messages = append(a.Messages, models.Message{Role: "user", Content: content})
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
func (a *Agent) AddUsage(u models.Usage) {
	a.usageMu.Lock()
	a.usage.Add(u)
	a.usageMu.Unlock()
}

// SetUsage seeds the session totals with stored values — a resumed session
// keeps counting from where it was saved, not from zero.
func (a *Agent) SetUsage(u models.Usage) {
	a.usageMu.Lock()
	a.usage = u
	a.usageMu.Unlock()
}

// ResetUsage zeroes the session totals — /clear starts the spend counter
// over along with the conversation.
func (a *Agent) ResetUsage() {
	a.usageMu.Lock()
	a.usage = models.Usage{}
	a.usageMu.Unlock()
}

// Usage returns the session's cumulative token usage: input, output, and
// cached-input tokens across every streamed call (plus compaction and
// subagent calls on this agent).
func (a *Agent) Usage() models.Usage {
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

func New(backend models.Backend, model string, maxTokens int, systemPrompt string) *Agent {
	a := &Agent{
		Backend:       backend,
		Model:         model,
		MaxTokens:     maxTokens,
		Messages:      []models.Message{{Role: "system", Content: systemPrompt}},
		Checkpointing: true,
		HistoryRecall: true,
	}
	if p, ok := backend.(models.ProtocolBackend); ok {
		a.Protocol = string(p.AdapterProtocol())
	}
	a.Tools = tools.All()
	a.Tools = append(a.Tools, taskTool(a))
	a.Tools = append(a.Tools, todoTool(a))
	a.Tools = append(a.Tools, memory.Tools(a.currentSessionID)...)
	a.Tools = append(a.Tools, tools.OutputTools(tools.OutputToolConfig{
		SessionID: a.currentSessionID,
		Catalog:   func() session.OutputCatalog { return a.OutputCatalog },
		Store:     func() *session.OutputStore { return a.Outputs },
		Messages:  a.MessagesSnapshot,
	})...)
	a.files = newFileLocks()
	a.bg = newTaskRegistry()
	a.observations = observation.NewRegistry()
	a.searchState = search.NewRegistry()
	a.touched = make(map[string]struct{})
	a.seenOperation = make(map[string]int)
	return a
}

// SetSessionID scopes session-backed tools.
func (a *Agent) SetSessionID(id string) {
	if id == "" {
		a.sessionID.Store(nil)
		return
	}
	a.sessionID.Store(&id)
}

func (a *Agent) currentSessionID() string {
	if p := a.sessionID.Load(); p != nil {
		return *p
	}
	return ""
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
	// default tool set with the ordinary built-ins, then restore output tools.
	sub.Tools = tools.All()
	sub.Runtime = a.Runtime.Child()
	sub.files = a.files
	sub.Outputs = a.Outputs
	sub.OutputCatalog = a.OutputCatalog
	sub.HistoryCatalog = a.HistoryCatalog
	sub.SubagentsDisabled = a.SubagentsDisabled
	sub.SetSessionID(a.currentSessionID())
	sub.Tools = append(sub.Tools, tools.OutputTools(tools.OutputToolConfig{
		SessionID: sub.currentSessionID,
		Catalog:   func() session.OutputCatalog { return sub.OutputCatalog },
		Store:     func() *session.OutputStore { return sub.Outputs },
		Messages:  sub.MessagesSnapshot,
	})...)
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
	if _, exists := a.seenOperation[fingerprint]; !exists && len(a.seenOperation) >= maxSeenOperations {
		a.seenOperation = make(map[string]int, maxSeenOperations)
	}
	a.seenOperation[fingerprint]++
	return a.seenOperation[fingerprint]
}

func (a *Agent) resetSeenOperations() {
	if a == nil {
		return
	}
	a.operationMu.Lock()
	a.seenOperation = make(map[string]int)
	a.operationMu.Unlock()
}

// validateToolBatch rejects pathological tool batches before anything executes.
// It enforces fixed bounds on batch size, duplicate calls within a single
// response, and tool ID/name integrity.
func (a *Agent) validateToolBatch(calls []models.ToolCall) error {
	if len(calls) > maxToolCallsPerResponse {
		return fmt.Errorf("model returned an unsafe tool batch: %d calls exceeds limit %d", len(calls), maxToolCallsPerResponse)
	}
	seenIDs := make(map[string]bool, len(calls))
	seenFingerprints := make(map[string]struct{}, len(calls))
	duplicateCounts := make(map[string]int)
	firstDuplicateTool := ""

	for _, tc := range calls {
		if tc.ID == "" {
			return errors.New("model returned a tool call with empty id")
		}
		if seenIDs[tc.ID] {
			return fmt.Errorf("model returned duplicate tool call id %q", tc.ID)
		}
		seenIDs[tc.ID] = true

		if tc.Function.Name == "" {
			return errors.New("model returned a tool call with empty name")
		}

		fp := a.operationFingerprint(tc.Function.Name, tc.Function.Arguments)
		if _, exists := seenFingerprints[fp]; exists {
			duplicateCounts[tc.Function.Name]++
			if firstDuplicateTool == "" {
				firstDuplicateTool = tc.Function.Name
			}
		} else {
			seenFingerprints[fp] = struct{}{}
		}
	}

	if firstDuplicateTool != "" {
		return fmt.Errorf("model returned duplicate tool calls: %s repeated %d times", firstDuplicateTool, duplicateCounts[firstDuplicateTool]+1)
	}
	return nil
}

// findMalformedToolCalls returns indices of tool calls whose arguments are invalid
// JSON, exceed the per-call size limit, or lack required identifiers. The second
// result reports an otherwise-valid batch that exceeds the aggregate size limit.
func findMalformedToolCalls(calls []models.ToolCall) ([]int, bool) {
	var malformed []int
	totalBytes := 0
	for i, tc := range calls {
		totalBytes += len(tc.Function.Arguments)
		if tc.ID == "" || tc.Function.Name == "" || len(tc.Function.Arguments) > maxToolCallArgBytes || !json.Valid([]byte(tc.Function.Arguments)) {
			malformed = append(malformed, i)
		}
	}
	return malformed, totalBytes > maxToolBatchArgBytes
}

func toolResultError(res tools.ToolResult) (string, bool) {
	if res.ExitCode == 0 {
		return "", false
	}
	text := res.Preview
	if strings.HasPrefix(text, "Error:") {
		text = strings.TrimPrefix(text, "Error:")
	}
	text = strings.TrimSpace(text)
	if text == "" {
		text = "tool execution failed"
	}
	return text, true
}

func toolDiagnosticName(call models.ToolCall) string {
	if call.Function.Name != "bash" {
		return call.Function.Name
	}
	var args struct {
		Command string `json:"command"`
	}
	if json.Unmarshal([]byte(call.Function.Arguments), &args) == nil && strings.TrimSpace(args.Command) != "" {
		return truncateField(args.Command, 200)
	}
	return call.Function.Name
}

// MessagesSnapshot returns a copy of the conversation safe to read while a
// turn runs on another goroutine. Direct field access (a.Messages) is only
// safe for the goroutine driving the turn.
func (a *Agent) MessagesSnapshot() []models.Message {
	a.msgsMu.Lock()
	defer a.msgsMu.Unlock()
	return append([]models.Message(nil), a.Messages...)
}

// MessageCount returns the current conversation length without copying it.
func (a *Agent) MessageCount() int {
	a.msgsMu.Lock()
	defer a.msgsMu.Unlock()
	return len(a.Messages)
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
		a.Messages = []models.Message{{Role: "system", Content: prompt}}
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
		all = append(all, HistoryTools(a.HistoryCatalog, a.currentSessionID, a.searchState)...)
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
func (a *Agent) Stream(ctx context.Context, req models.Request, sink models.EventSink, ev Events) (models.Message, models.Usage, error) {
	return a.callStream(ctx, a.Backend, a.Role, a.Provider, a.Protocol, req, sink, ev, 0)
}

func (a *Agent) streamForTurn(ctx context.Context, req models.Request, sink models.EventSink, ev Events, checkpointLevel int) (models.Message, models.Usage, error) {
	return a.callStream(ctx, a.Backend, a.Role, a.Provider, a.Protocol, req, sink, ev, checkpointLevel)
}

// Complete performs one non-streaming model call with the same telemetry
// boundary as Stream. It does not mutate the agent's usage totals.
func (a *Agent) Complete(ctx context.Context, req models.Request, ev Events) (models.Message, models.Usage, error) {
	return a.CompleteWithRoute(ctx, a.Backend, a.Role, a.Provider, a.Protocol, req, ev)
}

// CompleteWithRoute runs one non-streaming call through the shared telemetry
// wrapper using the route that actually owns backend. It is used by direct
// one-shot callers such as title generation and goal formulation, which do
// not run through Turn but must still report their provider and adapter.
func (a *Agent) CompleteWithRoute(ctx context.Context, backend models.Backend, role, provider, protocol string, req models.Request, ev Events) (models.Message, models.Usage, error) {
	return a.callCompletePurpose(ctx, backend, role, provider, protocol, "", req, ev)
}

// CompleteWithRoutePurpose is the telemetry-aware variant used by bounded
// internal model calls such as the optional approval reviewer. Purpose keeps
// that spend distinguishable from an ordinary tiny subagent turn.
func (a *Agent) CompleteWithRoutePurpose(ctx context.Context, backend models.Backend, role, provider, protocol, purpose string, req models.Request, ev Events) (models.Message, models.Usage, error) {
	return a.callCompletePurpose(ctx, backend, role, provider, protocol, purpose, req, ev)
}

func (a *Agent) callInfo(backend models.Backend, role, provider, protocol, model string) ModelCallStart {
	if p, ok := backend.(models.ProtocolBackend); ok {
		protocol = string(p.AdapterProtocol())
	}
	if protocol == "" {
		protocol = a.Protocol
	}
	return ModelCallStart{Role: role, Provider: provider, Model: model, Protocol: protocol}
}

func (a *Agent) callStream(ctx context.Context, backend models.Backend, role, provider, protocol string, req models.Request, sink models.EventSink, ev Events, checkpointLevel int) (models.Message, models.Usage, error) {
	if req.SessionID == "" {
		req.SessionID = a.currentSessionID()
	}
	start := time.Now()
	call := a.callInfo(backend, role, provider, protocol, req.Model)
	a.emitPromptView(ev, call, req)
	if ev.OnModelCallStart != nil {
		ev.OnModelCallStart(call)
	}
	if backend == nil {
		err := errors.New("agent: nil backend")
		a.emitCallEnd(ev, call, start, models.Message{}, models.Usage{}, err, checkpointLevel)
		return models.Message{}, models.Usage{}, err
	}
	msg, usage, err := backend.Stream(ctx, req, sink)
	a.emitCallEnd(ev, call, start, msg, usage, err, checkpointLevel)
	return msg, usage, err
}

func (a *Agent) callCompletePurpose(ctx context.Context, backend models.Backend, role, provider, protocol, purpose string, req models.Request, ev Events) (models.Message, models.Usage, error) {
	if req.SessionID == "" {
		req.SessionID = a.currentSessionID()
	}
	start := time.Now()
	call := a.callInfo(backend, role, provider, protocol, req.Model)
	call.Purpose = purpose
	a.emitPromptView(ev, call, req)
	if ev.OnModelCallStart != nil {
		ev.OnModelCallStart(call)
	}
	if backend == nil {
		err := errors.New("agent: nil backend")
		a.emitCallEnd(ev, call, start, models.Message{}, models.Usage{}, err, 0)
		return models.Message{}, models.Usage{}, err
	}
	msg, usage, err := backend.Complete(ctx, req)
	a.emitCallEnd(ev, call, start, msg, usage, err, 0)
	return msg, usage, err
}

func (a *Agent) emitPromptView(ev Events, call ModelCallStart, req models.Request) {
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

func (a *Agent) emitCallEnd(ev Events, call ModelCallStart, start time.Time, msg models.Message, usage models.Usage, err error, checkpointLevel int) {
	if ev.OnModelCallEnd == nil {
		return
	}
	end := ModelCallEnd{
		ModelCallStart:           call,
		LatencyMS:                time.Since(start).Milliseconds(),
		FinishReason:             msg.StopReason,
		Usage:                    usage,
		CheckpointLevel:          checkpointLevel,
		ContinuedAfterCheckpoint: checkpointLevel > 0 && len(msg.ToolCalls) > 0,
	}
	if err != nil {
		end.Error = err.Error()
	}
	ev.OnModelCallEnd(end)
}

func isRepositoryNavigationTool(name string) bool {
	switch name {
	case "read", "grep", "structural_search", "glob", "find_files", "lsp",
		"output_list", "output_read", "artifact_list", "artifact_read", "history_search", "history_read":
		return true
	default:
		return false
	}
}

func explorationBatch(calls []models.ToolCall) (hasNavigation, hasMutation bool) {
	for _, call := range calls {
		hasNavigation = hasNavigation || isRepositoryNavigationTool(call.Function.Name)
		hasMutation = hasMutation || potentiallyMutatingReadGuardTool(call.Function.Name, call.Function.Arguments)
	}
	return hasNavigation, hasMutation
}

func explorationCheckpointLevel(round int) int {
	switch round {
	case explorationCheckpointOne:
		return 1
	case explorationCheckpointTwo:
		return 2
	case explorationCheckpointFinal:
		return 3
	default:
		return 0
	}
}

func explorationCheckpointReminder(level, rounds int) string {
	prompt := "Before another repository-navigation call, state the specific unresolved question and why the call can materially change the result. Tools remain available."
	switch level {
	case 2:
		prompt = "Reassess whether more exploration is necessary. If you continue, state the specific unresolved question and why the next call can materially change the result. Tools remain available."
	case 3:
		prompt = "This is the final exploration checkpoint. Continue only for a concrete unresolved question, state why the next call can materially change the result, then synthesize or implement. Tools remain available."
	}
	return fmt.Sprintf("<exploration_checkpoint level=\"%d\">\nYou have completed %d repository-exploration rounds in this turn.\n%s\n</exploration_checkpoint>", level, rounds, prompt)
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
// part is a vision ContentPart (see models.ImagePart); the model receives the
// text and the images together as a multimodal content array.
func (a *Agent) TurnWithImages(ctx context.Context, input string, parts []models.ContentPart, ev Events) (string, error) {
	return a.turn(ctx, input, parts, true, nil, ev)
}

// TurnWithGoal runs a normal turn with a request-scoped active goal context.
// The goal record is copied so model updates cannot mutate caller state; the
// caller receives each validated update through Events.OnGoalUpdate.
func (a *Agent) TurnWithGoal(ctx context.Context, input string, goal GoalRecord, ev Events) (string, error) {
	return a.turn(ctx, input, nil, false, &goal, ev)
}

// TurnAuthoredWithGoal is TurnWithGoal for a human-authored submission.
func (a *Agent) TurnAuthoredWithGoal(ctx context.Context, input string, goal GoalRecord, ev Events) (string, error) {
	return a.turn(ctx, input, nil, true, &goal, ev)
}

// TurnWithImagesAndGoal combines an authored multimodal turn with a
// request-scoped active goal context.
func (a *Agent) TurnWithImagesAndGoal(ctx context.Context, input string, parts []models.ContentPart, goal GoalRecord, ev Events) (string, error) {
	return a.turn(ctx, input, parts, true, &goal, ev)
}

func (a *Agent) readOnlyCollaborationMode() bool {
	return a.PlanMode || a.ReviewMode || a.AskMode
}

func (a *Agent) collaborationPrompt() string {
	if a.ReviewMode {
		return reviewModePrompt
	}
	if a.PlanMode {
		return planModePrompt
	}
	if a.AskMode {
		return askModePrompt
	}
	return ""
}

// currentToolGuidance is transient request context. It describes only the
// tools that survived deterministic preflight for this turn.
func currentToolGuidance(ts []tools.Tool, notices []string) string {
	names := make([]string, 0, len(ts))
	hasBash, hasLSP := false, false
	for _, tool := range ts {
		name := tool.Def.Function.Name
		names = append(names, name)
		switch name {
		case "bash":
			hasBash = true
		case "lsp":
			hasLSP = true
		}
	}
	var b strings.Builder
	b.WriteString("Current tool availability for this request (authoritative): ")
	if len(names) == 0 {
		b.WriteString("none")
	} else {
		b.WriteString(strings.Join(names, ", "))
	}
	for _, notice := range notices {
		b.WriteString("\n- ")
		b.WriteString(notice)
	}
	if hasBash {
		b.WriteString("\n- Reserve bash for builds, tests, git, and operations the dedicated tools cannot express.")
	}
	if hasLSP {
		b.WriteString("\n- Use lsp for semantic identity, references, and symbol context.")
	}
	return b.String()
}

// assembleRequestMessages constructs the model request messages while keeping
// a byte-stable prefix across turns and tool rounds:
//  1. Base system prompt (history[0])
//  2. Stable collaboration-mode prompt (planModePrompt / reviewModePrompt)
//  3. Current capability guidance (if non-empty)
//  4. Exploration checkpoint reminder (if non-empty)
//  5. Budget reminder (if non-empty)
//  6. Conversation history (history[1:])
//  7. Trailing transient blocks (todoContent, goalContent)
func (a *Agent) assembleRequestMessages(history []models.Message, todoContent, goalContent, budgetReminder, capabilityGuidance, explorationReminder string) []models.Message {
	if len(history) == 0 {
		return history
	}
	if !a.readOnlyCollaborationMode() && todoContent == "" && goalContent == "" && budgetReminder == "" && capabilityGuidance == "" && explorationReminder == "" {
		return history
	}
	prefixCount := 1
	if a.readOnlyCollaborationMode() {
		prefixCount++
	}
	if capabilityGuidance != "" {
		prefixCount++
	}
	if explorationReminder != "" {
		prefixCount++
	}
	if a.readOnlyCollaborationMode() && budgetReminder != "" {
		prefixCount++
	}
	suffixCount := 0
	if todoContent != "" {
		suffixCount++
	}
	if goalContent != "" {
		suffixCount++
	}
	reqMsgs := make([]models.Message, 0, prefixCount+len(history)-1+suffixCount)
	reqMsgs = append(reqMsgs, history[0])
	if a.readOnlyCollaborationMode() {
		reqMsgs = append(reqMsgs, models.Message{Role: "system", Content: a.collaborationPrompt()})
	}
	if capabilityGuidance != "" {
		reqMsgs = append(reqMsgs, models.Message{Role: "system", Content: capabilityGuidance})
	}
	if explorationReminder != "" {
		reqMsgs = append(reqMsgs, models.Message{Role: "system", Content: explorationReminder})
	}
	if a.readOnlyCollaborationMode() {
		if budgetReminder != "" {
			reqMsgs = append(reqMsgs, models.Message{Role: "system", Content: budgetReminder})
		}
	}
	if len(history) > 1 {
		reqMsgs = append(reqMsgs, history[1:]...)
	}
	if todoContent != "" {
		reqMsgs = append(reqMsgs, models.Message{Role: "system", Content: todoContent})
	}
	if goalContent != "" {
		reqMsgs = append(reqMsgs, models.Message{Role: "system", Content: goalContent})
	}
	return reqMsgs
}

func (a *Agent) turn(ctx context.Context, input string, parts []models.ContentPart, authored bool, goalCtx *GoalRecord, ev Events) (string, error) {
	a.compacted = false // compaction retry state is scoped to this turn
	var activeGoal *GoalRecord
	if goalCtx != nil {
		goal := *goalCtx
		if err := goal.Validate(); err != nil {
			return "", fmt.Errorf("invalid goal context: %w", err)
		}
		if goal.Status == GoalStatusActive {
			activeGoal = &goal
		}
	}
	msg := models.Message{Role: "user", Content: input, Parts: parts, Authored: authored}
	if authored {
		now := time.Now()
		msg.SentAt = &now
	}
	a.msgsMu.Lock()
	if len(a.Messages) > 0 && a.Messages[len(a.Messages)-1].Role == "user" && a.Messages[len(a.Messages)-1].Authored && len(parts) == 0 && (strings.TrimSpace(input) == "continue" || strings.TrimSpace(input) == "Continue") {
		// User is explicitly asking to continue the prior unanswered prompt.
		prev := a.Messages[len(a.Messages)-1]
		msg.Content = prev.Content
		msg.Parts = append([]models.ContentPart(nil), prev.Parts...)
		a.Messages[len(a.Messages)-1] = msg
	} else {
		a.Messages = append(a.Messages, msg)
	}
	a.msgsMu.Unlock()
	reviewTarget := msg.Content

	var planBudget *rolloutBudget
	if (a.PlanMode || a.ReviewMode) && authored {
		planBudget = newPlanRolloutBudget()
	}
	readGuard := newReadCoverageTracker()
	compactionEvents := ev
	compactionEvents.OnCompacted = func(summary string, cutoff int) {
		readGuard.clear()
		if ev.OnCompacted != nil {
			ev.OnCompacted(summary, cutoff)
		}
	}

	// Freeze tools and precompute definitions once for the entire turn.
	turnTools := a.AllTools()
	if a.readOnlyCollaborationMode() {
		turnTools = filterPlanTools(turnTools)
		if a.ReviewMode {
			turnTools = append(turnTools, submitReviewTool())
		}
	}
	if activeGoal != nil && !a.readOnlyCollaborationMode() {
		turnTools = append(turnTools, GoalTool(*activeGoal))
	}
	var capabilityGuidance string
	if a.Runtime != nil {
		var capabilityNotices []string
		turnTools, capabilityNotices = tools.FilterAvailable(turnTools, a.Runtime)
		capabilityGuidance = currentToolGuidance(turnTools, capabilityNotices)
	}
	turnDefs := tools.Defs(turnTools)
	var synthesisDefs []models.Tool

	turnErrors := make(map[string]int)
	malformedRoundsInTurn := 0
	rounds := 0
	explorationRounds := 0
	explorationReminder := ""
	checkpointLevel := 0
	postEditVerification := false
	for {
		if a.MaxTurns > 0 && rounds >= a.MaxTurns {
			return "", fmt.Errorf("max turns (%d) reached — the model kept calling tools; re-run with a higher -max-turns or a more specific prompt", a.MaxTurns)
		}
		rounds++
		if err := a.maybeCompact(ctx, compactionEvents); err != nil {
			return "", err
		}
		requestCheckpointLevel := checkpointLevel
		checkpointLevel = 0
		requestExplorationReminder := explorationReminder
		explorationReminder = ""

		finalizing := planBudget != nil && planBudget.IsReserveCrossed()
		var budgetReminder string
		if planBudget != nil {
			budgetReminder = planBudget.reminderBlock(finalizing, a.ReviewMode)
		}
		todoContent := a.todoBlock()
		var goalContent string
		if activeGoal != nil {
			goalContent = GoalContextBlock(*activeGoal)
		}
		msgs := a.assembleRequestMessages(a.Messages, todoContent, goalContent, budgetReminder, capabilityGuidance, requestExplorationReminder)

		reqDefs := turnDefs
		available := turnTools
		if a.readOnlyCollaborationMode() && finalizing {
			if a.ReviewMode {
				available = []tools.Tool{submitReviewTool()}
				reqDefs = tools.Defs(available)
			} else {
				reqDefs = synthesisDefs
				available = nil // Reserve crossed: disable tools for final synthesis request
			}
		}

		// Surface transient-request retries through the event hook so the UI
		// shows "retrying" instead of looking hung. The sink is request-local;
		// the backend remains safe to share with foreground and background turns.
		reasoningEffort, reasoningEnabled := a.ReasoningRequest()
		var parser *planStreamParser
		sink := models.EventSink{
			OnThink: ev.OnThink,
			OnRetry: ev.OnRetry,
		}
		if a.PlanMode {
			// Plan mode routes streamed text through a block parser so the
			// proposed-plan output is surfaced via OnPlanDelta while the
			// surrounding conversational text still streams via OnText.
			parser = &planStreamParser{}
			parser.visible = ev.OnText
			parser.onPlan = ev.OnPlanDelta
			sink.OnText = parser.feed
		} else {
			sink.OnText = ev.OnText
		}
		msg, usage, err := a.streamForTurn(ctx, models.Request{
			Model:            a.Model,
			Messages:         msgs,
			Tools:            reqDefs,
			ReasoningEffort:  reasoningEffort,
			ReasoningEnabled: reasoningEnabled,
			SessionID:        a.currentSessionID(),
		}, sink, ev, requestCheckpointLevel)
		if a.PlanMode && parser != nil {
			parser.close()
		}
		a.AddUsage(usage)
		if ev.OnUsage != nil {
			ev.OnUsage(usage)
		}
		if planBudget != nil {
			var fallbackTokens int
			if usage.PromptTokens == 0 && usage.InputTokens == 0 && usage.CompletionTokens == 0 && usage.OutputTokens == 0 {
				fallbackTokens = EstimateTokens(msgs)
			}
			planBudget.RecordUsage(usage, fallbackTokens)
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
						a.Messages = append(a.Messages, models.Message{
							Role:       "tool",
							Content:    "Error: tool call interrupted — the turn was canceled by user before execution completed",
							ToolCallID: tc.ID,
							Name:       tc.Function.Name,
						})
					}
					a.msgsMu.Unlock()
				}
			}
			if a.Checkpointing && !a.compacted && models.IsContextLimit(err) && ctx.Err() == nil {
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
						readGuard.clear()
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
				readGuard.clear()
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
		hasNavigation, hasMutation := explorationBatch(msg.ToolCalls)
		if hasMutation {
			postEditVerification = true
		} else if hasNavigation {
			if postEditVerification {
				// ponytail: skip only the first navigation-only response after a
				// mutation; distinguishing later exploration from verification
				// would require intent/path tracking.
				postEditVerification = false
			} else {
				explorationRounds++
				if level := explorationCheckpointLevel(explorationRounds); level > 0 {
					checkpointLevel = level
					explorationReminder = explorationCheckpointReminder(level, explorationRounds)
				}
			}
		}
		if len(msg.ToolCalls) > 0 {
			malformedIndices, batchTooLarge := findMalformedToolCalls(msg.ToolCalls)
			if len(malformedIndices) > 0 || batchTooLarge {
				if malformedRoundsInTurn > 0 {
					if batchTooLarge {
						return "", errors.New("model tool batch remained oversized")
					}
					return "", errors.New("model tool channel remained malformed")
				}
				malformedRoundsInTurn++
				if !batchTooLarge {
					malformedSet := make(map[int]bool, len(malformedIndices))
					for _, idx := range malformedIndices {
						malformedSet[idx] = true
					}
					for i := range msg.ToolCalls {
						if malformedSet[i] {
							msg.ToolCalls[i].Function.Arguments = "{}"
							if msg.ToolCalls[i].ID == "" {
								msg.ToolCalls[i].ID = fmt.Sprintf("malformed_call_%d", i)
							}
						}
					}
				}
				msg.Usage = &usage
				msg.Model = a.Model + " @ " + a.Provider
				a.msgsMu.Lock()
				a.Messages = append(a.Messages, msg)
				errorMessage := malformedToolCallError
				if batchTooLarge {
					errorMessage = oversizedToolBatchError
				}
				for _, tc := range msg.ToolCalls {
					a.Messages = append(a.Messages, models.Message{
						Role:       "tool",
						Content:    errorMessage,
						ToolCallID: tc.ID,
						Name:       tc.Function.Name,
					})
				}
				a.msgsMu.Unlock()
				continue
			}

			if err := a.validateToolBatch(msg.ToolCalls); err != nil {
				return "", err
			}
		}
		msg.Usage = &usage
		msg.Model = a.Model + " @ " + a.Provider
		a.msgsMu.Lock()
		a.Messages = append(a.Messages, msg)
		a.msgsMu.Unlock()
		if len(msg.ToolCalls) > 0 {
			finalizationError := ""
			if finalizing {
				finalizationError = planFinalizationToolError
				if a.ReviewMode {
					finalizationError = reviewFinalizationToolError
				}
			}
			results := a.runToolResultsWithPolicy(ctx, msg.ToolCalls, ev, available, turnTools, finalizationError, readGuard)
			a.msgsMu.Lock()
			for i, tc := range msg.ToolCalls {
				a.Messages = append(a.Messages, models.Message{
					Role:       "tool",
					Content:    tools.ModelText(results[i]),
					ToolCallID: tc.ID,
					Name:       tc.Function.Name,
					Output:     results[i].Output,
					ExitCode:   results[i].ExitCode,
					Source:     results[i].Source,
				})
			}
			a.msgsMu.Unlock()
			for i, res := range results {
				if res.Metadata != nil && res.Metadata["failure_kind"] == "sandbox_network_denied" {
					cmdName := toolDiagnosticName(msg.ToolCalls[i])
					return "", fmt.Errorf("sandbox capability failure (%s): local network listener denied by sandbox policy", cmdName)
				}
			}
			for i, tc := range msg.ToolCalls {
				if errText, ok := toolResultError(results[i]); ok {
					fp := a.operationFingerprint(tc.Function.Name+":error", errText)
					turnErrors[fp]++
					if turnErrors[fp] >= 3 {
						snippet := truncateField(strings.ReplaceAll(errText, "\n", " "), 200)
						return "", fmt.Errorf("tool %s failed repeatedly with the same error: %s", tc.Function.Name, snippet)
					}
				}
			}
			if a.ReviewMode {
				if reviewArgs, ok := a.reviewTerminal(reviewTarget, msg, results, ev); ok {
					return reviewArgs, nil
				}
			}
			if activeGoal != nil && a.goalTerminal(activeGoal, results, ev) {
				a.compacted = false
				return msg.Content, nil
			}
			if ctx.Err() != nil {
				return "", ctx.Err()
			}
		}
		steered := a.drainPending()
		if len(steered) > 0 {
			a.msgsMu.Lock()
			for _, s := range steered {
				a.Messages = append(a.Messages, models.Message{Role: "user", Content: s.text, Parts: s.parts})
			}
			a.msgsMu.Unlock()
			if ev.OnSteer != nil {
				for _, s := range steered {
					ev.OnSteer(s.text)
				}
			}
		}
		if len(msg.ToolCalls) == 0 && len(steered) == 0 {
			a.compacted = false // reset for the next Turn
			return msg.Content, nil
		}
	}
}

func (a *Agent) reviewTerminal(target string, msg models.Message, results []tools.ToolResult, ev Events) (string, bool) {
	for i, call := range msg.ToolCalls {
		if _, isErr := toolResultError(results[i]); isErr || call.Function.Name != "submit_review" {
			continue
		}
		reviewArgs := string(call.Function.Arguments)
		if rev, err := ParseReview(reviewArgs); err == nil && a.HistoryCatalog != nil && a.currentSessionID() != "" && ev.OnCompactionReady != nil {
			checkpoint := buildReviewCheckpoint(target, rev)
			took := len(a.Messages)
			cutoff := took
			if err := ev.OnCompactionReady(append([]models.Message(nil), a.Messages...), checkpoint, cutoff); err == nil {
				a.msgsMu.Lock()
				a.Messages = []models.Message{
					a.Messages[0],
					{Role: "system", Content: "Summary of the conversation so far:\n\n" + checkpoint},
				}
				a.msgsMu.Unlock()
				if ev.OnCompact != nil {
					ev.OnCompact(took-len(a.Messages), len(a.Messages))
				}
				if ev.OnCompacted != nil {
					ev.OnCompacted(checkpoint, cutoff)
				}
				a.resetSeenOperations()
				a.compacted = true
				return reviewArgs, true
			}
		}
		a.compacted = false
		return reviewArgs, true
	}
	return "", false
}

func (a *Agent) goalTerminal(goal *GoalRecord, results []tools.ToolResult, ev Events) bool {
	terminal := false
	for _, result := range results {
		update, ok := GoalUpdateFromResult(result)
		if !ok {
			continue
		}
		if err := update.Validate(goal.ID); err != nil {
			continue
		}
		goal.Status = update.Status
		goal.Progress = update.Progress
		goal.Blocker = update.Blocker
		if ev.OnGoalUpdate != nil {
			ev.OnGoalUpdate(update)
		}
		if update.Status == GoalStatusBlocked || update.Status == GoalStatusComplete {
			terminal = true
		}
	}
	return terminal
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
func (a *Agent) runToolResultsWithTools(ctx context.Context, calls []models.ToolCall, ev Events, available []tools.Tool) []tools.ToolResult {
	return a.runToolResultsWithPolicy(ctx, calls, ev, available, nil, "", nil)
}

// runToolResultsWithPolicy is the same executor with an optional policy result
// for known tools that are temporarily withdrawn from the available set.
func (a *Agent) runToolResultsWithPolicy(ctx context.Context, calls []models.ToolCall, ev Events, available, known []tools.Tool, unavailableMessage string, readGuard *readCoverageTracker) []tools.ToolResult {
	for _, tc := range calls {
		a.recordTouched(tc.Function.Name, tc.Function.Arguments)
	}
	hints := a.searchHints()
	var unavailable map[string]struct{}
	if unavailableMessage != "" {
		availableNames := make(map[string]struct{}, len(available))
		for _, tool := range available {
			availableNames[tool.Def.Function.Name] = struct{}{}
		}
		unavailable = make(map[string]struct{}, len(known))
		for _, tool := range known {
			name := tool.Def.Function.Name
			if _, ok := availableNames[name]; !ok {
				unavailable[name] = struct{}{}
			}
		}
	}
	decisions := []readDecision(nil)
	if readGuard != nil {
		decisions = readGuard.prepare(calls, unavailable)
	}

	results := make([]tools.ToolResult, len(calls))
	type outcome struct {
		i      int
		result tools.ToolResult
		ms     int64 // wall-clock run time, stored on the ToolCall for /tools perf
	}
	outCh := make(chan outcome, len(calls)) // buffered: never blocks the workers

	sem := make(chan struct{}, maxConcurrentTools)
	var wg sync.WaitGroup
	// The operation fingerprint and duplicate count feed only the tool
	// telemetry event, and each costs a sha256 hash plus a redaction walk over
	// the arguments (and a map counter). Skip both when no consumer is
	// subscribed; the duplicate tool-call guard is applied earlier by
	// validateToolBatch with its own fingerprint pass.
	observeTelemetry := ev.OnToolTelemetry != nil
	batchSize := len(calls)
	sameToolCounts := make(map[string]int)
	if observeTelemetry {
		for _, call := range calls {
			sameToolCounts[call.Function.Name]++
		}
	}
	for i, tc := range calls {
		if i < len(decisions) && decisions[i].suppressed {
			continue
		}
		wg.Add(1)
		go func(i int, tc models.ToolCall) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()

			name, args := tc.Function.Name, tc.Function.Arguments
			var fingerprint string
			duplicate := false
			if observeTelemetry {
				fingerprint = a.operationFingerprint(name, args)
				duplicate = a.seenOperationCount(fingerprint) > 1
			}

			// Serialize against other mutations before starting. Acquiring here
			// (before OnToolStart) keeps "running" rows honest: a tool only
			// shows as running once it actually holds its lock.
			var release func()
			_, policyUnavailable := unavailable[name]
			if a.files != nil && !policyUnavailable {
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
			var result tools.ToolResult
			if policyUnavailable {
				result = tools.ToolResult{Preview: unavailableMessage, ExitCode: 1, Source: name}
			} else {
				result = tools.ExecuteResult(toolCtx, available, name, json.RawMessage(args))
			}
			result = a.attachOutput(ctx, result)
			ms := time.Since(start).Milliseconds()
			if ev.OnToolTelemetry != nil {
				metadata := make(map[string]string, len(result.Metadata))
				for key, value := range result.Metadata {
					metadata[key] = value
				}
				ev.OnToolTelemetry(ToolTelemetry{
					ID:            tc.ID,
					Name:          name,
					BatchSize:     batchSize,
					SameToolCount: sameToolCounts[name],
					DurationMS:    ms,
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
	if readGuard != nil {
		readGuard.apply(a, ev, calls, results, decisions)
	}
	return results
}

// attachOutput persists retained evidence before the completion event is
// delivered. Output failures never fail a tool call: the bounded preview is
// still useful and remains the model-facing result.
func (a *Agent) attachOutput(ctx context.Context, result tools.ToolResult) tools.ToolResult {
	if result.Output != nil || result.Retained == "" ||
		(result.Complete && result.OriginalBytes <= int64(len(result.Preview))) {
		return result
	}
	if a.Outputs == nil {
		return result
	}
	ref, err := a.Outputs.Put(ctx, []byte(result.Retained), result.OriginalBytes, result.Complete, "text/plain")
	if err != nil {
		result.Preview = tools.TruncateWithSuffix(result.Preview, "\n[output unavailable; the omitted output cannot be recovered]")
		return result
	}
	ref.Metadata = result.Metadata
	result.Output = &ref
	result.Preview = tools.TruncateWithSuffix(result.Preview, tools.OutputReference(ref))
	return result
}

// writeTranscript renders messages as a role-tagged transcript for a
// meta-prompt (compaction summary, goal formulation). Tool results are
// truncated so a giant file read doesn't blow up the request.

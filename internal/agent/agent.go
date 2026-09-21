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
	maxToolCallsPerResponse      = 64
	maxConcurrentTools           = 4
	reviewFinalEvidenceToolLimit = 4

	planFinalizationToolError   = "Error: exploration is complete for this plan. Exploration tools are disabled; set_next_reasoning_effort remains available for the next call. Emit the final <proposed_plan> block now."
	reviewFinalizationToolError = "Error: exploration is complete for this review. Only submit_review and set_next_reasoning_effort are available; submit the best evidence-backed result now."
	askFinalizationToolError    = "Error: exploration is complete for this answer. Tools are disabled; answer directly using the evidence gathered."
	malformedToolCallError      = "Error: tool call arguments were malformed (invalid JSON or exceeded the per-call size limit) and were omitted. Reissue the call with valid JSON arguments."
	oversizedToolBatchError     = "Error: the tool-call batch exceeded the aggregate argument size limit. Split the calls into smaller batches and reissue them with valid JSON arguments."

	maxToolCallArgBytes  = 256 * 1024 // 256 KiB
	maxToolBatchArgBytes = 512 * 1024 // 512 KiB
	// ponytail: bound duplicate telemetry memory; retain an LRU only if older
	// duplicate history becomes a product requirement.
	maxSeenOperations = 4096

	explorationCheckpointOne        = 15
	explorationCheckpointTwo        = 30
	explorationCheckpointFinal      = 45
	explorationCheckpointFinalLevel = 3
	executionExplorationCheckpoint  = 8
	goalAttentionCheckpoint         = 50
	finalizationRequestTimeout      = 10 * time.Minute
)

// HistoryCatalog is the durable session boundary for bounded history recall.
type HistoryCatalog interface {
	SearchHistory(context.Context, string, string, string, *int, int) ([]session.HistoryHit, error)
	ReadHistory(context.Context, string, int, int, *int, int) ([]session.HistoryMessage, []string, error)
}

type HistoryHit = session.HistoryHit
type HistoryMessage = session.HistoryMessage

// SubagentFactory creates an agent for a delegated task.
type SubagentFactory func(ctx context.Context, role, systemPrompt string) (*Agent, error)

type Agent struct {
	Backend   models.Backend
	Model     string // model identifier sent to API
	ModelName string // configured model name
	Provider  string // configured provider name
	Protocol  string // adapter protocol
	Role      string // model role (default, smart, fast, tiny)
	MaxTokens int
	Effort    string // reasoning effort parameter ("" = omitted)
	// ReasoningToggle indicates that the selected model has a separate
	// on/off reasoning control from models.dev. Graded efforts still travel in
	// Effort; the neutral request carries the enable/disable bit separately.
	ReasoningToggle bool
	// ReasoningEfforts contains the provider-advertised graded values. It is
	// capability metadata only; Effort remains the configured baseline.
	ReasoningEfforts []string
	// Subagents and internal model calls never expose the foreground-only
	// one-shot effort selector.
	reasoningSelectorDisabled bool
	// DynamicReasoning is nil/on by default. A false value disables the model's
	// one-call effort selector while preserving the configured Effort.
	DynamicReasoning *bool
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
	// CompactCandidates are configured routes tried in compaction order. Each
	// candidate carries its own backend and model capabilities.
	CompactCandidates []*Agent
	// CompactThreshold is the fraction of ContextLimit at which Turn compacts
	// proactively; 0 uses the adaptive 80% default.
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
	ReviewMode         bool
	reviewContinuation *reviewContinuation
	planContinuation   *planContinuation

	// AskMode restricts one turn to read-only tools and injects a direct
	// question-answering prompt.
	AskMode bool

	mu             sync.Mutex
	pending        []pendingSteer // steered user messages awaiting injection
	reviewFinalize bool           // an explicit live steering request closes review exploration
	compacted      bool           // a compaction already happened this turn — don't retry-loop

	// msgsMu guards Messages for concurrent READERS: the turn goroutine
	// mutates Messages freely, but a test/UI reader taking msgsMu sees a
	// consistent slice. Mutations hold it only for the append.
	msgsMu             sync.Mutex
	tokenEstimate      int
	tokenEstimateCount int
	tokenEstimateValid bool

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

	turnRuntimeMu    sync.RWMutex
	turnRuntimeValue *tools.ToolRuntime

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
	if reviewFinalizeInstruction(text) {
		a.reviewFinalize = true
	}
	a.mu.Unlock()
}

// steerNotice queues an internal result without treating its model-generated
// text as an operator request to finalize review exploration.
func (a *Agent) steerNotice(text string) {
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
	if reviewFinalizeInstruction(text) {
		a.reviewFinalize = true
	}
	a.mu.Unlock()
}

func (a *Agent) consumeReviewFinalize() bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	requested := a.reviewFinalize
	a.reviewFinalize = false
	return requested
}

func reviewFinalizeInstruction(text string) bool {
	text = strings.ToLower(strings.TrimSpace(text))
	if text == "" || strings.Contains(text, "do not provide") || strings.Contains(text, "don't provide") || strings.Contains(text, "do not give") || strings.Contains(text, "don't give") {
		return false
	}
	for _, phrase := range []string{"provide the report", "provide a report", "give me the report", "stop exploring", "synthesize the review"} {
		if strings.Contains(text, phrase) {
			return true
		}
	}
	return false
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
	a.tokenEstimateValid = false
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
	if !a.tokenEstimateValid || len(a.Messages) < a.tokenEstimateCount {
		a.tokenEstimate = EstimateTokens(a.Messages)
		a.tokenEstimateCount = len(a.Messages)
		a.tokenEstimateValid = true
	} else if len(a.Messages) > a.tokenEstimateCount {
		a.tokenEstimate += EstimateTokens(a.Messages[a.tokenEstimateCount:])
		a.tokenEstimateCount = len(a.Messages)
	}
	return a.tokenEstimate
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
	sub.reasoningSelectorDisabled = true
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
	sub.ReasoningEfforts = append([]string(nil), a.ReasoningEfforts...)
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
	a.mu.Lock()
	a.reviewFinalize = false
	a.mu.Unlock()
	a.reviewContinuation = nil
	a.planContinuation = nil
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
	other.reviewContinuation = a.reviewContinuation
	other.planContinuation = a.planContinuation
}

func (a *Agent) setTurnRuntime(runtime *tools.ToolRuntime) {
	if a == nil {
		return
	}
	a.turnRuntimeMu.Lock()
	a.turnRuntimeValue = runtime
	a.turnRuntimeMu.Unlock()
}

func (a *Agent) turnRuntime() *tools.ToolRuntime {
	if a == nil {
		return nil
	}
	a.turnRuntimeMu.RLock()
	runtime := a.turnRuntimeValue
	a.turnRuntimeMu.RUnlock()
	if runtime != nil {
		return runtime
	}
	return a.Runtime
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
	seenCalls := make(map[string]struct{}, len(calls))
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

		identity := tc.Function.Name + "\x00" + strings.TrimSpace(tc.Function.Arguments)
		if _, exists := seenCalls[identity]; exists {
			duplicateCounts[tc.Function.Name]++
			if firstDuplicateTool == "" {
				firstDuplicateTool = tc.Function.Name
			}
		} else {
			seenCalls[identity] = struct{}{}
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
	text := strings.TrimPrefix(res.Preview, "Error:")
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
	messages := append([]models.Message(nil), a.Messages...)
	for i := range messages {
		if len(messages[i].ToolCalls) > 0 {
			messages[i].ToolCalls = append([]models.ToolCall(nil), messages[i].ToolCalls...)
		}
	}
	return messages
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
	a.tokenEstimateValid = false
	a.msgsMu.Unlock()
}

// SetMessages replaces the conversation between turns.
func (a *Agent) SetMessages(messages []models.Message) {
	if a == nil {
		return
	}
	a.msgsMu.Lock()
	a.Messages = append([]models.Message(nil), messages...)
	a.tokenEstimateValid = false
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
	_, searchState, _, _ := a.stateSnapshot()
	a.toolsMu.Lock()
	defer a.toolsMu.Unlock()
	all := append(append([]tools.Tool(nil), a.Tools...), a.mcpTools...)
	if a.HistoryRecall && a.HistoryCatalog != nil && a.currentSessionID() != "" {
		all = append(all, HistoryTools(a.HistoryCatalog, a.currentSessionID, searchState)...)
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
	call.ReasoningEffort, call.ReasoningEnabled = requestReasoningTelemetry(req)
	call.ConfiguredEffort = req.ConfiguredReasoningEffort
	call.DynamicReasoning = req.DynamicReasoning
	call.EffortRequested = req.ReasoningEffort
	call.EffortApplied = req.ReasoningEffort
	call.SelectionReason = req.ReasoningSelectionReason
	call.ReasoningSelectorExposed, call.ReasoningSelectorEfforts = a.reasoningSelectorTelemetry(req.Tools)
	var requestDiagnostics *models.RequestDiagnostics
	previousDiagnostics := req.OnRequestDiagnostics
	req.OnRequestDiagnostics = func(value models.RequestDiagnostics) {
		if previousDiagnostics != nil {
			previousDiagnostics(value)
		}
		copy := value
		requestDiagnostics = &copy
	}
	requestShape := requestShapeInfo{}
	if ev.OnModelCallEnd != nil {
		requestShape = requestShapeTelemetry(req)
	}
	a.emitPromptView(ev, call, req, requestShape)
	if ev.OnModelCallStart != nil {
		ev.OnModelCallStart(call)
	}
	if backend == nil {
		err := errors.New("agent: nil backend")
		a.emitCallEnd(ev, call, start, req, models.Message{}, models.Usage{}, err, requestDiagnostics, checkpointLevel, requestShape)
		return models.Message{}, models.Usage{}, err
	}
	msg, usage, err := backend.Stream(ctx, req, sink)
	a.emitCallEnd(ev, call, start, req, msg, usage, err, requestDiagnostics, checkpointLevel, requestShape)
	return msg, usage, err
}

func (a *Agent) callCompletePurpose(ctx context.Context, backend models.Backend, role, provider, protocol, purpose string, req models.Request, ev Events) (models.Message, models.Usage, error) {
	if req.SessionID == "" {
		req.SessionID = a.currentSessionID()
	}
	start := time.Now()
	call := a.callInfo(backend, role, provider, protocol, req.Model)
	call.Purpose = purpose
	call.ReasoningEffort, call.ReasoningEnabled = requestReasoningTelemetry(req)
	call.ConfiguredEffort = req.ConfiguredReasoningEffort
	call.DynamicReasoning = req.DynamicReasoning
	call.EffortRequested = req.ReasoningEffort
	call.EffortApplied = req.ReasoningEffort
	call.SelectionReason = req.ReasoningSelectionReason
	call.ReasoningSelectorExposed, call.ReasoningSelectorEfforts = a.reasoningSelectorTelemetry(req.Tools)
	var requestDiagnostics *models.RequestDiagnostics
	previousDiagnostics := req.OnRequestDiagnostics
	req.OnRequestDiagnostics = func(value models.RequestDiagnostics) {
		if previousDiagnostics != nil {
			previousDiagnostics(value)
		}
		copy := value
		requestDiagnostics = &copy
	}
	requestShape := requestShapeInfo{}
	if ev.OnModelCallEnd != nil {
		requestShape = requestShapeTelemetry(req)
	}
	a.emitPromptView(ev, call, req, requestShape)
	if ev.OnModelCallStart != nil {
		ev.OnModelCallStart(call)
	}
	if backend == nil {
		err := errors.New("agent: nil backend")
		a.emitCallEnd(ev, call, start, req, models.Message{}, models.Usage{}, err, requestDiagnostics, 0, requestShape)
		return models.Message{}, models.Usage{}, err
	}
	msg, usage, err := backend.Complete(ctx, req)
	a.emitCallEnd(ev, call, start, req, msg, usage, err, requestDiagnostics, 0, requestShape)
	return msg, usage, err
}

func requestReasoningTelemetry(req models.Request) (string, *bool) {
	if req.ReasoningEnabled == nil {
		return req.ReasoningEffort, nil
	}
	enabled := *req.ReasoningEnabled
	return req.ReasoningEffort, &enabled
}

func (a *Agent) reasoningSelectorTelemetry(defs []models.Tool) (bool, []string) {
	for _, def := range defs {
		if def.Function.Name == nextReasoningEffortToolName {
			return true, advertisedReasoningEfforts(a.ReasoningEfforts)
		}
	}
	return false, nil
}

func (a *Agent) emitPromptView(ev Events, call ModelCallStart, req models.Request, shape requestShapeInfo) {
	if ev.OnPromptView == nil {
		return
	}
	serializedBytes := shape.Bytes
	if serializedBytes == 0 {
		data, _ := json.Marshal(req)
		serializedBytes = len(data)
	}
	ev.OnPromptView(PromptView{
		ModelCallStart:  call,
		MessageCount:    len(req.Messages),
		EstimatedTokens: EstimateTokens(req.Messages),
		SerializedBytes: serializedBytes,
		ContextLimit:    a.ContextLimit,
	})
}

func (a *Agent) emitCallEnd(ev Events, call ModelCallStart, start time.Time, req models.Request, msg models.Message, usage models.Usage, err error, requestDiagnostics *models.RequestDiagnostics, checkpointLevel int, requestShape requestShapeInfo) {
	if ev.OnModelCallEnd == nil {
		return
	}
	end := ModelCallEnd{
		ModelCallStart:           call,
		LatencyMS:                time.Since(start).Milliseconds(),
		FinishReason:             msg.StopReason,
		Usage:                    usage,
		RequestDiagnostics:       requestDiagnostics,
		RequestSHA256:            requestShape.Hash,
		RequestPrefixSHA256:      requestShape.PrefixHash,
		RequestBytes:             requestShape.Bytes,
		RequestPrefixBytes:       requestShape.PrefixBytes,
		RequestPrefixMessages:    requestShape.PrefixMessages,
		CheckpointLevel:          checkpointLevel,
		ContinuedAfterCheckpoint: checkpointLevel > 0 && len(msg.ToolCalls) > 0,
	}
	if err != nil {
		end.Error = err.Error()
	}
	ev.OnModelCallEnd(end)
}

const requestTelemetryPrefixMessages = 16

type requestTelemetryMessage struct {
	Role           string                 `json:"role"`
	Content        string                 `json:"content"`
	Parts          []models.ContentPart   `json:"parts,omitempty"`
	ToolCalls      []requestTelemetryCall `json:"tool_calls,omitempty"`
	ToolCallID     string                 `json:"tool_call_id,omitempty"`
	ProviderBlocks []json.RawMessage      `json:"provider_blocks,omitempty"`
	Name           string                 `json:"name,omitempty"`
}

type requestTelemetryCall struct {
	ID       string `json:"id"`
	Type     string `json:"type"`
	Function struct {
		Name      string `json:"name"`
		Arguments string `json:"arguments"`
	} `json:"function"`
}

type requestShape struct {
	Model           string                    `json:"model"`
	Messages        []requestTelemetryMessage `json:"messages"`
	Tools           []models.Tool             `json:"tools,omitempty"`
	ReasoningEffort string                    `json:"reasoning_effort,omitempty"`
}

type requestShapeInfo struct {
	Hash           string
	PrefixHash     string
	Bytes          int
	PrefixBytes    int
	PrefixMessages int
}

func requestShapeTelemetry(req models.Request) requestShapeInfo {
	messages := make([]requestTelemetryMessage, len(req.Messages))
	for i, msg := range req.Messages {
		messages[i] = requestTelemetryMessage{
			Role:           msg.Role,
			Content:        msg.Content,
			Parts:          msg.Parts,
			ToolCallID:     msg.ToolCallID,
			ProviderBlocks: msg.ProviderBlocks,
			Name:           msg.Name,
		}
		if len(msg.ToolCalls) > 0 {
			messages[i].ToolCalls = make([]requestTelemetryCall, len(msg.ToolCalls))
			for j, call := range msg.ToolCalls {
				messages[i].ToolCalls[j].ID = call.ID
				messages[i].ToolCalls[j].Type = call.Type
				messages[i].ToolCalls[j].Function.Name = call.Function.Name
				messages[i].ToolCalls[j].Function.Arguments = call.Function.Arguments
			}
		}
	}
	shape := requestShape{
		Model:           req.Model,
		Messages:        messages,
		Tools:           req.Tools,
		ReasoningEffort: req.ReasoningEffort,
	}
	full, err := json.Marshal(shape)
	if err != nil {
		return requestShapeInfo{}
	}
	prefix := shape
	if len(prefix.Messages) > requestTelemetryPrefixMessages {
		prefix.Messages = prefix.Messages[:requestTelemetryPrefixMessages]
	}
	prefixJSON, err := json.Marshal(prefix)
	if err != nil {
		return requestShapeInfo{}
	}
	return requestShapeInfo{
		Hash:           requestHash(full),
		PrefixHash:     requestHash(prefixJSON),
		Bytes:          len(full),
		PrefixBytes:    len(prefixJSON),
		PrefixMessages: len(prefix.Messages),
	}
}

func requestHash(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

func isRepositoryNavigationTool(name string) bool {
	switch name {
	case "read", "grep", "glob", "find_files", "lsp",
		"output_list", "output_read", "artifact_list", "artifact_read", "history_search", "history_read", "web_fetch", "web_search":
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
	prompt := "Reassess whether more exploration is necessary, state the specific unresolved question and why the next call can materially change the result, then continue with the next necessary call or synthesize."
	switch level {
	case 2:
		prompt = "This is the second exploration checkpoint. Reassess whether more exploration is necessary, state the specific unresolved question and why the next call can materially change the result, then continue with the next necessary call or synthesize."
	case explorationCheckpointFinalLevel:
		prompt = "This is the third and strongest exploration checkpoint warning. Reassess whether more exploration is necessary, state the specific unresolved question and why the next call can materially change the result, then continue with the next necessary call or synthesize."
	}
	return fmt.Sprintf("<exploration_checkpoint level=\"%d\">\nYou have completed %d repository-exploration rounds in this turn.\n%s\n</exploration_checkpoint>", level, rounds, prompt)
}

func executionExplorationCheckpointReminder(rounds int) string {
	return fmt.Sprintf("<execution_checkpoint>\nYou have completed %d repository-exploration rounds without a change. Choose the next concrete action: make the needed change, run targeted verification, or state the blocker. Continue broad exploration only when it resolves a specific unanswered question.\n</execution_checkpoint>", rounds)
}

func goalAttentionCheckpointReminder(rounds int) string {
	return fmt.Sprintf("<goal_attention_checkpoint>\nYou have completed %d goal model/tool rounds. Pause and assess the active goal: what concrete progress has been made since the last checkpoint? Confirm that the current work still advances the goal. If it has drifted, realign before continuing. Record a concise progress update with update_goal, then continue with the next necessary action. Do not stop merely because this checkpoint appeared.\n</goal_attention_checkpoint>", rounds)
}

// Turn executes agent interaction rounds until tool execution finishes.
// Triggers automatic context compaction when usage exceeds the configured threshold.
func (a *Agent) Turn(ctx context.Context, input string, ev Events) (string, error) {
	return a.turn(ctx, input, nil, false, false, nil, ev)
}

// TurnAuthored executes Turn for directly entered user messages (Authored=true).
func (a *Agent) TurnAuthored(ctx context.Context, input string, ev Events) (string, error) {
	return a.turn(ctx, input, nil, true, false, nil, ev)
}

// Continue resumes the interrupted turn through an explicit command intent.
func (a *Agent) Continue(ctx context.Context, ev Events) (string, error) {
	return a.continueTurn(ctx, "", ev)
}

// ContinueWithInstruction resumes the interrupted turn with an optional
// user-supplied instruction.
func (a *Agent) ContinueWithInstruction(ctx context.Context, instruction string, ev Events) (string, error) {
	return a.continueTurn(ctx, instruction, ev)
}

func (a *Agent) continueTurn(ctx context.Context, instruction string, ev Events) (string, error) {
	if strings.TrimSpace(instruction) == "" {
		instruction = "continue"
	}
	return a.turn(ctx, instruction, nil, true, true, nil, ev)
}

// TurnWithImages is TurnAuthored for a submission that attaches images. Each
// part is a vision ContentPart (see models.ImagePart); the model receives the
// text and the images together as a multimodal content array.
func (a *Agent) TurnWithImages(ctx context.Context, input string, parts []models.ContentPart, ev Events) (string, error) {
	return a.turn(ctx, input, parts, true, false, nil, ev)
}

// TurnWithGoal runs a normal turn with a request-scoped active goal context.
// The goal record is copied so model updates cannot mutate caller state; the
// caller receives each validated update through Events.OnGoalUpdate.
func (a *Agent) TurnWithGoal(ctx context.Context, input string, goal GoalRecord, ev Events) (string, error) {
	return a.turn(ctx, input, nil, false, false, &goal, ev)
}

// TurnAuthoredWithGoal is TurnWithGoal for a human-authored submission.
func (a *Agent) TurnAuthoredWithGoal(ctx context.Context, input string, goal GoalRecord, ev Events) (string, error) {
	return a.turn(ctx, input, nil, true, false, &goal, ev)
}

// TurnWithImagesAndGoal combines an authored multimodal turn with a
// request-scoped active goal context.
func (a *Agent) TurnWithImagesAndGoal(ctx context.Context, input string, parts []models.ContentPart, goal GoalRecord, ev Events) (string, error) {
	return a.turn(ctx, input, parts, true, false, &goal, ev)
}

// ReviewPending reports whether a failed review turn can be resumed.
func (a *Agent) ReviewPending() bool {
	return a != nil && a.reviewContinuation != nil
}

// PlanPending reports whether a failed plan turn can be resumed.
func (a *Agent) PlanPending() bool {
	return a != nil && a.planContinuation != nil
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
	discoveryTools := make([]string, 0, 2)
	for _, tool := range ts {
		name := tool.Def.Function.Name
		names = append(names, name)
		switch name {
		case "bash":
			hasBash = true
		case "lsp":
			hasLSP = true
		case "glob", "find_files":
			discoveryTools = append(discoveryTools, name)
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
	if len(discoveryTools) > 0 {
		b.WriteString("\n- For paths not established by evidence, use ")
		b.WriteString(strings.Join(discoveryTools, " or "))
		b.WriteString(" before read or grep; do not guess directory names or platform-specific paths.")
	}
	return b.String()
}

// assembleRequestMessages builds request messages with a byte-stable prefix.
// Sequences base prompts, history, and transient reminder blocks.
func (a *Agent) assembleRequestMessages(history []models.Message, todoContent, goalContent, budgetReminder, capabilityGuidance, explorationReminder, reviewPreflight string) []models.Message {
	if len(history) == 0 {
		return history
	}
	if !a.readOnlyCollaborationMode() && todoContent == "" && goalContent == "" && budgetReminder == "" && capabilityGuidance == "" && explorationReminder == "" && reviewPreflight == "" {
		return history
	}
	reqMsgs := make([]models.Message, 0, len(history)+7)
	reqMsgs = append(reqMsgs, history[0])
	if a.readOnlyCollaborationMode() {
		reqMsgs = append(reqMsgs, models.Message{Role: "system", Content: a.collaborationPrompt()})
	}
	if reviewPreflight != "" {
		reqMsgs = append(reqMsgs, models.Message{Role: "system", Content: reviewPreflight})
	}
	if len(history) > 1 {
		reqMsgs = append(reqMsgs, history[1:]...)
	}
	if capabilityGuidance != "" {
		reqMsgs = append(reqMsgs, models.Message{Role: "system", Content: capabilityGuidance, Transient: true})
	}
	if explorationReminder != "" {
		reqMsgs = append(reqMsgs, models.Message{Role: "system", Content: explorationReminder, Transient: true})
	}
	if budgetReminder != "" {
		reqMsgs = append(reqMsgs, models.Message{Role: "system", Content: budgetReminder, Transient: true})
	}
	if todoContent != "" {
		reqMsgs = append(reqMsgs, models.Message{Role: "system", Content: todoContent, Transient: true})
	}
	if goalContent != "" {
		reqMsgs = append(reqMsgs, models.Message{Role: "system", Content: goalContent, Transient: true})
	}
	return reqMsgs
}

func (a *Agent) turn(ctx context.Context, input string, parts []models.ContentPart, authored, continuation bool, goalCtx *GoalRecord, ev Events) (string, error) {
	a.compacted = false // compaction retry state is scoped to this turn
	// A live steering flag belongs to the turn that was already running. Clear
	// leftovers before starting a new turn; steering during this turn sets it
	// again at the next loop boundary.
	a.consumeReviewFinalize()
	resumeReview := a.ReviewMode && authored && continuation && a.reviewContinuation != nil
	resumePlan := a.PlanMode && authored && continuation && a.planContinuation != nil
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
	if continuation {
		// User is explicitly asking to continue the prior unanswered prompt.
		// A canceled tool turn persists an interrupted assistant/tool tail, so
		// remove that tail before replaying the authored prompt.
		promptAt := -1
		for i := len(a.Messages) - 1; i >= 0; i-- {
			if a.Messages[i].Role == "user" && a.Messages[i].Authored {
				promptAt = i
				break
			}
		}
		if promptAt >= 0 && (promptAt == len(a.Messages)-1 || interruptedTail(a.Messages[promptAt+1:])) {
			prev := a.Messages[promptAt]
			a.Messages = a.Messages[:promptAt+1]
			msg.Content = continuedPrompt(prev.Content, input)
			msg.Parts = append([]models.ContentPart(nil), prev.Parts...)
			a.Messages[promptAt] = msg
		} else {
			a.Messages = append(a.Messages, msg)
		}
	} else {
		a.Messages = append(a.Messages, msg)
	}
	a.tokenEstimateValid = false
	a.msgsMu.Unlock()
	reviewTarget := msg.Content

	var planBudget *rolloutBudget
	if a.PlanMode && authored {
		if a.PlanMode && resumePlan {
			reviewTarget = a.planContinuation.target
			planBudget = a.planContinuation.budget
		} else {
			planBudget = newPlanRolloutBudget()
		}
	}
	var reviewBudget *ReviewBudget
	if a.ReviewMode && authored {
		if resumeReview {
			reviewTarget = a.reviewContinuation.target
			reviewBudget = a.reviewContinuation.budget
		} else {
			// Scope sizing is a preflight for the real review request. It has no
			// repository tools and falls back deterministically when the optional
			// tiny assessor is not configured.
			reviewBudget = a.newReviewBudget(ctx, reviewTarget, ev)
			a.reviewContinuation = &reviewContinuation{target: reviewTarget, budget: reviewBudget}
		}
	}
	if a.PlanMode && authored && !resumePlan {
		a.planContinuation = &planContinuation{target: reviewTarget, budget: planBudget}
	}
	if authored {
		a.setTurnRuntime(a.Runtime)
		if a.Runtime != nil && a.Runtime.Policy != nil {
			if roots := taggedReadRoots(a, reviewTarget); len(roots) > 0 {
				if granted, err := a.Runtime.Policy.Grant(roots, nil, false); err == nil {
					a.setTurnRuntime(a.Runtime.WithPolicy(granted))
				}
			}
		}
		defer a.setTurnRuntime(a.Runtime)
	}
	readGuard := newReadCoverageTracker()
	compactionEvents := ev
	compactionEvents.OnCompacted = func(summary string, cutoff int) {
		readGuard.clear()
		if ev.OnCompacted != nil {
			ev.OnCompacted(summary, cutoff)
		}
	}

	// Freeze tools and precompute definitions once for the entire turn. Keep the
	// unfiltered set so stale calls can receive the mode-specific unavailable
	// result instead of an opaque unknown-tool error.
	knownTools := a.AllTools()
	turnTools := append([]tools.Tool(nil), knownTools...)
	if a.readOnlyCollaborationMode() {
		turnTools = filterPlanTools(turnTools)
		if a.ReviewMode {
			extension := requestReviewExtensionTool()
			knownTools = append(knownTools, extension)
			turnTools = append(turnTools, submitReviewTool(), extension)
		}
	}
	var askUserMu sync.Mutex
	askUserUsed := false
	if a.PlanMode && authored && a.Runtime != nil && !a.Runtime.Headless && ev.OnQuestion != nil {
		askTool := askUserTool(ev.OnQuestion, func() bool {
			askUserMu.Lock()
			defer askUserMu.Unlock()
			if askUserUsed {
				return false
			}
			askUserUsed = true
			return true
		})
		knownTools = append(knownTools, askTool)
		turnTools = append(turnTools, askTool)
	}
	if activeGoal != nil && !a.readOnlyCollaborationMode() {
		turnTools = append(turnTools, GoalTool(*activeGoal))
	}
	var capabilityNotices []string
	if a.Runtime != nil {
		turnTools, capabilityNotices = tools.FilterAvailable(turnTools, a.Runtime)
	}
	var reasoningSelector tools.Tool
	if selector, ok := a.reasoningSelectorTool(); ok {
		reasoningSelector = selector
		knownTools = append(knownTools, selector)
		turnTools = append(turnTools, selector)
	}
	turnDefs := tools.Defs(turnTools)

	turnErrors := make(map[string]int)
	malformedRoundsInTurn := 0
	rounds := 0
	explorationRounds := 0
	explorationReminder := ""
	goalAttentionReminder := ""
	baselineEffort := a.Effort
	nextEffort := ""
	checkpointLevel := 0
	postEditVerification := false
	reviewFinalEvidenceRetryUsed := false
	reviewFinalizationRetryUsed := false
	reviewCheckpointPending := false
	reviewClosed := false
	scopePreflight := ""
	planState := a.planContinuation
	if a.PlanMode && authored && planState != nil {
		explorationRounds = planState.explorationRounds
		checkpointLevel = planState.checkpointLevel
		if planState.checkpointPending {
			explorationReminder = explorationCheckpointReminder(checkpointLevel, explorationRounds)
		}
		defer func() {
			if a.planContinuation == planState {
				planState.explorationRounds = explorationRounds
				planState.checkpointPending = planState.checkpointPending || explorationReminder != ""
				if checkpointLevel > 0 {
					planState.checkpointLevel = checkpointLevel
				}
			}
		}()
	}
	if reviewBudget != nil {
		state := a.reviewContinuation
		if state != nil {
			reviewCheckpointPending = state.checkpointPending
			reviewClosed = state.closed
			if resumeReview {
				a.emitReviewProgress(ev, reviewBudget, "exploration", "resumed")
			}
			defer func() {
				if a.reviewContinuation == state {
					state.checkpointPending = reviewCheckpointPending
					state.closed = reviewClosed
				}
			}()
		}
		scopePreflight = reviewPreflightPrompt(reviewBudget)
	} else if a.PlanMode {
		scopePreflight = planTaggedScopePrompt(a, reviewTarget)
	}
	closeReviewExploration := func() {
		if reviewBudget == nil {
			return
		}
		reviewCheckpointPending = false
		reviewBudget.checkpointOpen = false
		if reviewClosed {
			return
		}
		reviewClosed = true
		a.emitReviewProgress(ev, reviewBudget, "exploration_closed", "")
	}
	for {
		if activeGoal == nil && a.MaxTurns > 0 && rounds >= a.MaxTurns {
			return "", fmt.Errorf("max turns (%d) reached — the model kept calling tools; re-run with a higher -max-turns or a more specific prompt", a.MaxTurns)
		}
		rounds++
		goalAttentionReminder = ""
		if activeGoal != nil && rounds%goalAttentionCheckpoint == 0 {
			goalAttentionReminder = goalAttentionCheckpointReminder(rounds)
		}
		if err := a.maybeCompact(ctx, compactionEvents); err != nil {
			return "", err
		}
		if reviewClosed {
			// A closed review is terminal. Clear stale state defensively so a
			// malformed post-finalization response cannot reopen its boundary.
			reviewCheckpointPending = false
			reviewBudget.checkpointOpen = false
		} else if reviewBudget != nil && a.consumeReviewFinalize() {
			closeReviewExploration()
		}
		requestCheckpointLevel := checkpointLevel
		checkpointLevel = 0
		requestExplorationReminder := explorationReminder
		explorationReminder = ""
		if a.PlanMode && planState != nil && planState.checkpointPending {
			requestCheckpointLevel = planState.checkpointLevel
			requestExplorationReminder = explorationCheckpointReminder(requestCheckpointLevel, explorationRounds)
		}
		reviewCheckpointRequest := reviewBudget != nil && reviewCheckpointPending && !reviewClosed
		if reviewCheckpointRequest {
			requestExplorationReminder = reviewBudgetCheckpointReminder(reviewBudget)
		}
		budgetFinalizing := planBudget != nil && planBudget.IsReserveCrossed()

		finalizing := budgetFinalizing
		var budgetReminder string
		if planBudget != nil {
			budgetReminder = planBudget.reminderBlock(budgetFinalizing, a.ReviewMode)
		}
		todoContent := a.todoBlock()
		var goalContent string
		if activeGoal != nil {
			goalContent = GoalContextBlock(*activeGoal)
			if goalAttentionReminder != "" {
				goalContent += "\n\n" + goalAttentionReminder
			}
		}
		// Checkpoint reminders only add transient context. Review keeps the
		// request schema stable; phase restrictions are enforced below.
		reqDefs := turnDefs
		available := turnTools
		if reviewCheckpointRequest && !budgetFinalizing {
			available = append([]tools.Tool(nil), turnTools...)
			if reviewBudget.Allocation >= reviewBudget.HardLimit {
				available = withoutReviewExtension(available)
			}
		} else if reviewBudget != nil && reviewClosed {
			available = []tools.Tool{submitReviewTool()}
			if reasoningSelector.Def.Function.Name != "" {
				available = append(available, reasoningSelector)
			}
		} else if a.readOnlyCollaborationMode() && finalizing {
			if a.ReviewMode {
				available = []tools.Tool{submitReviewTool()}
				if reasoningSelector.Def.Function.Name != "" {
					available = append(available, reasoningSelector)
				}
			} else {
				reqDefs = nil
				available = nil // Reserve crossed: disable exploration tools for final synthesis request
				if reasoningSelector.Def.Function.Name != "" {
					available = []tools.Tool{reasoningSelector}
					reqDefs = tools.Defs(available)
				}
			}
		} else if a.ReviewMode {
			available = withoutReviewExtension(available)
		}
		requestCapabilityGuidance := ""
		if a.Runtime != nil {
			requestCapabilityGuidance = currentToolGuidance(available, capabilityNotices)
		}
		msgs := a.assembleRequestMessages(a.Messages, todoContent, goalContent, budgetReminder, requestCapabilityGuidance, requestExplorationReminder, scopePreflight)
		if reviewClosed && reviewFinalizationRetryUsed {
			msgs = append(msgs, models.Message{Role: "system", Content: "<review_finalization_retry> Only submit_review and set_next_reasoning_effort are available. Submit the complete review now; do not describe what you would do next. </review_finalization_retry>", Transient: true})
		}
		// Surface transient-request retries through the event hook so the UI
		// shows "retrying" instead of looking hung. The sink is request-local;
		// the backend remains safe to share with foreground and background turns.
		reasoningEffort, reasoningReason := baselineEffort, "configured"
		if nextEffort != "" {
			reasoningEffort, reasoningReason = nextEffort, "model_requested"
		}
		reasoningEffort, reasoningEnabled := a.reasoningRequest(reasoningEffort)
		toolChoice := ""
		if a.ReviewMode && reviewClosed {
			toolChoice = "submit_review"
		}
		requestTimeout := time.Duration(0)
		if finalizing || (reviewBudget != nil && reviewClosed) {
			requestTimeout = finalizationRequestTimeout
		}
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
			Model:                     a.Model,
			Messages:                  msgs,
			Tools:                     reqDefs,
			ToolChoice:                toolChoice,
			ReasoningEffort:           reasoningEffort,
			ReasoningEnabled:          reasoningEnabled,
			ConfiguredReasoningEffort: baselineEffort,
			DynamicReasoning:          a.DynamicReasoning == nil || *a.DynamicReasoning,
			ReasoningSelectionReason:  reasoningReason,
			SessionID:                 a.currentSessionID(),
			RequestTimeout:            requestTimeout,
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
			if reviewClosed && ctx.Err() == nil {
				if !reviewFinalizationRetryUsed {
					reviewFinalizationRetryUsed = true
					continue
				}
				return "", fmt.Errorf("review evidence was retained but final submission failed: %w", err)
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
		if a.PlanMode && planState != nil && planState.checkpointPending {
			planState.checkpointPending = false
			planState.checkpointLevel = 0
		}
		hasNavigation, hasMutation := explorationBatch(msg.ToolCalls)
		reviewNavigationBatch := reviewBudget != nil && !reviewClosed && !reviewCheckpointRequest && hasNavigation && !hasMutation
		reviewFinalEvidenceBatch := false
		var reviewExtension *reviewExtensionRequest
		reviewCheckpointError := ""
		if reviewCheckpointRequest {
			extensionCount, submitCount := 0, 0
			for _, call := range msg.ToolCalls {
				switch call.Function.Name {
				case "request_review_extension":
					extensionCount++
				case "submit_review":
					submitCount++
				}
			}
			switch {
			case extensionCount > 0:
				if reviewBudget.Allocation >= reviewBudget.HardLimit {
					reviewCheckpointError = "review exploration has reached its absolute limit; submit_review or use final evidence"
				} else if extensionCount != 1 || len(msg.ToolCalls) != 1 {
					reviewCheckpointError = "request_review_extension must be the only tool call at a review checkpoint"
				} else {
					request, err := parseReviewExtension(json.RawMessage(msg.ToolCalls[0].Function.Arguments))
					if err != nil {
						reviewCheckpointError = err.Error()
					} else {
						reviewExtension = &request
					}
				}
			case hasNavigation:
				if submitCount > 0 {
					reviewCheckpointError = "final evidence navigation cannot be combined with submit_review"
				} else if len(msg.ToolCalls) > reviewFinalEvidenceToolLimit {
					reviewCheckpointError = fmt.Sprintf("final evidence is limited to %d tool calls", reviewFinalEvidenceToolLimit)
				} else {
					for _, call := range msg.ToolCalls {
						if !isRepositoryNavigationTool(call.Function.Name) {
							reviewCheckpointError = "final evidence may contain only repository-navigation tools"
							break
						}
					}
					if reviewCheckpointError == "" {
						reviewFinalEvidenceBatch = true
					}
				}
			case submitCount > 0 && len(msg.ToolCalls) != 1:
				reviewCheckpointError = "submit_review must be the only tool call at a review checkpoint"
			}
		}
		if reviewCheckpointError != "" {
			msg.Usage = &usage
			msg.Model = a.Model + " @ " + a.Provider
			a.msgsMu.Lock()
			a.Messages = append(a.Messages, msg)
			for _, tc := range msg.ToolCalls {
				a.Messages = append(a.Messages, models.Message{
					Role: "tool", Content: "Error: " + reviewCheckpointError,
					ToolCallID: tc.ID, Name: tc.Function.Name,
				})
			}
			a.msgsMu.Unlock()
			continue
		}
		evidenceBatch := reviewFinalEvidenceBatch
		checkpointTriggered := false
		if hasMutation {
			postEditVerification = true
		} else if hasNavigation && reviewBudget == nil && activeGoal == nil {
			if postEditVerification {
				// ponytail: skip only the first navigation-only response after a
				// mutation; distinguishing later exploration from verification
				// would require intent/path tracking.
				postEditVerification = false
			} else {
				explorationRounds++
				if explorationRounds == executionExplorationCheckpoint && !a.readOnlyCollaborationMode() {
					explorationReminder = executionExplorationCheckpointReminder(explorationRounds)
					checkpointTriggered = true
				} else if level := explorationCheckpointLevel(explorationRounds); level > 0 {
					checkpointLevel = level
					explorationReminder = explorationCheckpointReminder(level, explorationRounds)
					if a.PlanMode && planState != nil {
						planState.checkpointPending = true
						planState.checkpointLevel = level
					}
					checkpointTriggered = true
				}
			}
		}
		if checkpointTriggered {
			if ev.OnNotice != nil {
				label := "exploration"
				if explorationRounds == executionExplorationCheckpoint && !a.readOnlyCollaborationMode() {
					label = "execution"
				}
				ev.OnNotice(fmt.Sprintf("◎ %s checkpoint · round %d", label, explorationRounds))
			}
			if !a.readOnlyCollaborationMode() {
				continue
			}
		}
		if len(msg.ToolCalls) > 0 {
			malformedIndices, batchTooLarge := findMalformedToolCalls(msg.ToolCalls)
			if len(malformedIndices) > 0 || batchTooLarge {
				if evidenceBatch {
					if reviewFinalEvidenceRetryUsed {
						closeReviewExploration()
					} else {
						reviewFinalEvidenceRetryUsed = true
					}
				} else if malformedRoundsInTurn > 0 {
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
				if !evidenceBatch {
					return "", err
				}
				if reviewFinalEvidenceRetryUsed {
					closeReviewExploration()
				} else {
					reviewFinalEvidenceRetryUsed = true
				}
				msg.Usage = &usage
				msg.Model = a.Model + " @ " + a.Provider
				a.msgsMu.Lock()
				a.Messages = append(a.Messages, msg)
				for _, tc := range msg.ToolCalls {
					a.Messages = append(a.Messages, models.Message{
						Role:       "tool",
						Content:    "Error: final evidence batch was rejected before execution: " + err.Error(),
						ToolCallID: tc.ID,
						Name:       tc.Function.Name,
					})
				}
				a.msgsMu.Unlock()
				continue
			}
		}
		msg.Usage = &usage
		msg.Model = a.Model + " @ " + a.Provider
		a.msgsMu.Lock()
		a.Messages = append(a.Messages, msg)
		a.msgsMu.Unlock()
		if len(msg.ToolCalls) > 0 {
			finalizationError := ""
			if finalizing || (a.ReviewMode && reviewClosed) {
				finalizationError = planFinalizationToolError
				if a.ReviewMode {
					finalizationError = reviewFinalizationToolError
				} else if a.AskMode {
					finalizationError = askFinalizationToolError
				}
			}
			if a.ReviewMode && finalizationError == "" {
				finalizationError = "Error: this tool is unavailable in the current review phase. Use the tools currently allowed by the review checkpoint."
			}
			results := a.runToolResultsWithPolicy(ctx, msg.ToolCalls, ev, available, knownTools, finalizationError, readGuard)
			nextEffort = ""
			selected, requestedEffort, selectionErr := a.selectedReasoningEffort(msg.ToolCalls, results)
			selectionReason := ""
			if selectionErr != nil {
				selectionReason = "invalid_multiple_requests"
				for i, call := range msg.ToolCalls {
					if call.Function.Name == nextReasoningEffortToolName {
						results[i] = tools.ToolResult{Preview: "Error: " + selectionErr.Error(), ExitCode: 1, Source: call.Function.Name}
					}
				}
			} else if selected != "" {
				nextEffort = selected
				selectionReason = "applied_next_foreground_call"
			} else if requestedEffort != "" {
				selectionReason = "unsupported_or_failed_request"
			}
			if requestedEffort != "" && ev.OnReasoningSelection != nil {
				ev.OnReasoningSelection(ReasoningSelection{
					Current: baselineEffort, Requested: requestedEffort, Next: nextEffort,
					Applied: selected != "", Reason: selectionReason,
				})
			}
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
			if reviewExtension != nil && len(results) == 1 {
				if _, failed := toolResultError(results[0]); !failed {
					from := reviewBudget.Allocation
					grant := reviewExtensionAllocation(from, reviewBudget.HardLimit, reviewBudget.Baseline)
					if grant > 0 {
						reviewBudget.Allocation += grant
						reviewBudget.checkpointOpen = false
						reviewCheckpointPending = false
						a.emitReviewExtensionProgress(ev, reviewBudget, from, reviewBudget.Allocation, reviewExtension.displayReason())
					}
				}
			}
			if reviewFinalEvidenceBatch {
				reviewCheckpointPending = false
				a.emitReviewProgress(ev, reviewBudget, "final_evidence", "")
				closeReviewExploration()
			} else if reviewNavigationBatch {
				reviewBudget.CurrentRound++
				a.emitReviewProgress(ev, reviewBudget, "exploration", "")
				if reviewBudget.CurrentRound >= reviewBudget.Allocation {
					reviewBudget.checkpointOpen = true
					reviewCheckpointPending = true
					a.emitReviewProgress(ev, reviewBudget, "budget_boundary", "")
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
				if reviewArgs, ok := a.reviewTerminal(msg, results); ok {
					a.emitReviewProgress(ev, reviewBudget, "completed", "")
					a.reviewContinuation = nil
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
			if reviewBudget != nil && reviewClosed {
				if reviewFinalizationRetryUsed {
					return "", errors.New("review did not submit_review after the finalization correction")
				}
				reviewFinalizationRetryUsed = true
				continue
			}
			if reviewCheckpointRequest {
				continue
			}
			a.compacted = false // reset for the next Turn
			if a.PlanMode {
				a.planContinuation = nil
			}
			return msg.Content, nil
		}
	}
}

func interruptedTail(messages []models.Message) bool {
	if len(messages) == 0 {
		return false
	}
	sawInterrupted := false
	for _, message := range messages {
		if message.Role == "assistant" && strings.EqualFold(message.StopReason, "interrupted") {
			sawInterrupted = true
			continue
		}
		if sawInterrupted && message.Role == "tool" && strings.HasPrefix(message.Content, "Error: tool call interrupted") {
			continue
		}
		return false
	}
	return sawInterrupted
}

func continuedPrompt(previous, instruction string) string {
	instruction = strings.TrimSpace(instruction)
	if instruction == "" || instruction == "continue" {
		return previous
	}
	return previous + "\n\nAdditional instruction for this continuation:\n" + instruction
}

func (a *Agent) reviewTerminal(msg models.Message, results []tools.ToolResult) (string, bool) {
	for i, call := range msg.ToolCalls {
		if _, isErr := toolResultError(results[i]); isErr || call.Function.Name != "submit_review" {
			continue
		}
		reviewArgs := string(call.Function.Arguments)
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
	return a.reasoningRequest(a.Effort)
}

func (a *Agent) reasoningRequest(selected string) (string, *bool) {
	effort := strings.TrimSpace(selected)
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
				} else if toolRequiresGlobalMutation(name) {
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
			toolCtx = tools.WithRuntime(toolCtx, a.turnRuntime())
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
		a.msgsMu.Lock()
		calls[oc.i].DurationMs = oc.ms
		calls[oc.i].ExitCode = oc.result.ExitCode
		a.msgsMu.Unlock()
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

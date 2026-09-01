package agent

import "github.com/sacca97/ghg/internal/llm"

// Events receives streaming callbacks during a turn. All fields are optional.
type Events struct {
	OnText           func(delta string)               // assistant text as it streams
	OnThink          func(delta string)               // reasoning/thinking tokens as they stream
	OnToolStart      func(id, name, args string)      // a tool call is about to run
	OnToolOutput     func(id, output string)          // accumulated output while a tool call runs
	OnToolEnd        func(id, name, result string)    // a tool call finished
	OnSteer          func(text string)                // a steered message was injected
	OnCompact        func(took, kept int)             // context was auto-compacted (messages removed/kept)
	OnCompacted      func(summary string, cutoff int) // a durable compaction completed
	OnUsage          func(u llm.Usage)                // a request reported its token usage
	OnRetry          func(ev llm.RetryEvent)          // a transient request failure is being retried
	OnGoalUpdate     func(GoalUpdate)                 // structured active-goal checkpoint
	OnToolTelemetry  func(ToolTelemetry)              // bounded-output accounting for one tool call
	OnModelCallStart func(ModelCallStart)
	OnPromptView     func(PromptView)
	OnModelCallEnd   func(ModelCallEnd)
	// OnPlanDelta receives the streamed body of the <proposed_plan> block while
	// the agent is in Plan mode, as it is generated. The surrounding normal text
	// continues to stream through OnText.
	OnPlanDelta func(delta string)
	// OnCompactionReady receives the exact pre-cutover view after checkpoint
	// generation succeeds, but before Agent.Messages is replaced. Persistence
	// adapters must save the raw messages and compaction event or return an error.
	OnCompactionReady func(messages []llm.Message, summary string, cutoff int) error
}

// ToolTelemetry records the distinction between what a tool produced, what
// was retained for recovery, and what the model actually saw. It is emitted
// after artifact attachment so the artifact path is included in the same
// lifecycle boundary as the tool result.
type ToolTelemetry struct {
	ID            string            `json:"id,omitempty"`
	Name          string            `json:"name"`
	PreviewBytes  int               `json:"preview_bytes"`
	RetainedBytes int               `json:"retained_bytes"`
	OriginalBytes int64             `json:"original_bytes"`
	Truncated     bool              `json:"truncated"`
	BashRedirect  bool              `json:"bash_redirect"`
	Fingerprint   string            `json:"fingerprint,omitempty"`
	Duplicate     bool              `json:"duplicate,omitempty"`
	Metadata      map[string]string `json:"metadata,omitempty"`
}

// ModelCallStart describes the route selected for one provider request.
// It is deliberately independent of agent-definition loading so callers can
// use the same telemetry for ordinary turns, planning, and compaction.
type ModelCallStart struct {
	Role     string `json:"role,omitempty"`
	Provider string `json:"provider,omitempty"`
	Model    string `json:"model"`
	Protocol string `json:"protocol,omitempty"`
	Purpose  string `json:"purpose,omitempty"`
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

// PromptView records bounded request-shape telemetry without retaining the
// prompt itself. It lets paired evaluations compare derived views while the
// raw session remains outside the event payload.
type PromptView struct {
	ModelCallStart
	MessageCount    int `json:"message_count"`
	EstimatedTokens int `json:"estimated_tokens"`
	SerializedBytes int `json:"serialized_bytes"`
	ContextLimit    int `json:"context_limit"`
}

// FanIn multiplexes several Events values into one: every fired callback is
// invoked on each source that implements it. A background worker runs its
// Turn with FanIn(registry.emitter(id), Events{OnUsage: …}) so the TUI's
// per-task subscribers and the parent's usage accounting both see the live
// stream.
func FanIn(evs ...Events) Events {
	return Events{
		OnText: func(s string) {
			for _, e := range evs {
				if e.OnText != nil {
					e.OnText(s)
				}
			}
		},
		OnThink: func(s string) {
			for _, e := range evs {
				if e.OnThink != nil {
					e.OnThink(s)
				}
			}
		},
		OnToolStart: func(id, name, args string) {
			for _, e := range evs {
				if e.OnToolStart != nil {
					e.OnToolStart(id, name, args)
				}
			}
		},
		OnToolOutput: func(id, output string) {
			for _, e := range evs {
				if e.OnToolOutput != nil {
					e.OnToolOutput(id, output)
				}
			}
		},
		OnToolEnd: func(id, name, result string) {
			for _, e := range evs {
				if e.OnToolEnd != nil {
					e.OnToolEnd(id, name, result)
				}
			}
		},
		OnToolTelemetry: func(telemetry ToolTelemetry) {
			for _, e := range evs {
				if e.OnToolTelemetry != nil {
					e.OnToolTelemetry(telemetry)
				}
			}
		},
		OnSteer: func(text string) {
			for _, e := range evs {
				if e.OnSteer != nil {
					e.OnSteer(text)
				}
			}
		},
		OnCompact: func(took, kept int) {
			for _, e := range evs {
				if e.OnCompact != nil {
					e.OnCompact(took, kept)
				}
			}
		},
		OnCompacted: func(summary string, cutoff int) {
			for _, e := range evs {
				if e.OnCompacted != nil {
					e.OnCompacted(summary, cutoff)
				}
			}
		},
		OnCompactionReady: func(messages []llm.Message, summary string, cutoff int) error {
			for _, e := range evs {
				if e.OnCompactionReady != nil {
					if err := e.OnCompactionReady(messages, summary, cutoff); err != nil {
						return err
					}
				}
			}
			return nil
		},
		OnUsage: func(u llm.Usage) {
			for _, e := range evs {
				if e.OnUsage != nil {
					e.OnUsage(u)
				}
			}
		},
		OnGoalUpdate: func(update GoalUpdate) {
			for _, e := range evs {
				if e.OnGoalUpdate != nil {
					e.OnGoalUpdate(update)
				}
			}
		},
		OnModelCallStart: func(call ModelCallStart) {
			for _, e := range evs {
				if e.OnModelCallStart != nil {
					e.OnModelCallStart(call)
				}
			}
		},
		OnPromptView: func(view PromptView) {
			for _, e := range evs {
				if e.OnPromptView != nil {
					e.OnPromptView(view)
				}
			}
		},
		OnModelCallEnd: func(call ModelCallEnd) {
			for _, e := range evs {
				if e.OnModelCallEnd != nil {
					e.OnModelCallEnd(call)
				}
			}
		},
		OnPlanDelta: func(delta string) {
			for _, e := range evs {
				if e.OnPlanDelta != nil {
					e.OnPlanDelta(delta)
				}
			}
		},
	}
}

package agent

import "github.com/sacca97/ghg/internal/models"

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
	OnUsage          func(u models.Usage)             // a request reported its token usage
	OnRetry          func(ev models.RetryEvent)       // a transient request failure is being retried
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
	OnCompactionReady func(messages []models.Message, summary string, cutoff int) error
}

// ToolTelemetry records the distinction between what a tool produced, what
// was retained for recovery, and what the model actually saw. It is emitted
// after output attachment so the output reference is included in the same
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
	LatencyMS    int64        `json:"latency_ms"`
	FinishReason string       `json:"finish_reason,omitempty"`
	Usage        models.Usage `json:"usage"`
	Error        string       `json:"error,omitempty"`
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
// FanIn multiplexes several Events values into one: every fired callback is
// invoked on each source that implements it. Callbacks absent from all inputs
// remain nil so background agents do not calculate unneeded telemetry.
func FanIn(evs ...Events) Events {
	var out Events
	var hasText, hasThink, hasToolStart, hasToolOutput, hasToolEnd, hasTelemetry, hasSteer bool
	var hasCompact, hasCompacted, hasCompactionReady, hasUsage, hasGoalUpdate bool
	var hasModelCallStart, hasPromptView, hasModelCallEnd, hasPlanDelta, hasRetry bool

	for _, e := range evs {
		if e.OnText != nil {
			hasText = true
		}
		if e.OnThink != nil {
			hasThink = true
		}
		if e.OnToolStart != nil {
			hasToolStart = true
		}
		if e.OnToolOutput != nil {
			hasToolOutput = true
		}
		if e.OnToolEnd != nil {
			hasToolEnd = true
		}
		if e.OnToolTelemetry != nil {
			hasTelemetry = true
		}
		if e.OnSteer != nil {
			hasSteer = true
		}
		if e.OnCompact != nil {
			hasCompact = true
		}
		if e.OnCompacted != nil {
			hasCompacted = true
		}
		if e.OnCompactionReady != nil {
			hasCompactionReady = true
		}
		if e.OnUsage != nil {
			hasUsage = true
		}
		if e.OnRetry != nil {
			hasRetry = true
		}
		if e.OnGoalUpdate != nil {
			hasGoalUpdate = true
		}
		if e.OnModelCallStart != nil {
			hasModelCallStart = true
		}
		if e.OnPromptView != nil {
			hasPromptView = true
		}
		if e.OnModelCallEnd != nil {
			hasModelCallEnd = true
		}
		if e.OnPlanDelta != nil {
			hasPlanDelta = true
		}
	}

	if hasText {
		out.OnText = func(s string) {
			for _, e := range evs {
				if e.OnText != nil {
					e.OnText(s)
				}
			}
		}
	}
	if hasThink {
		out.OnThink = func(s string) {
			for _, e := range evs {
				if e.OnThink != nil {
					e.OnThink(s)
				}
			}
		}
	}
	if hasToolStart {
		out.OnToolStart = func(id, name, args string) {
			for _, e := range evs {
				if e.OnToolStart != nil {
					e.OnToolStart(id, name, args)
				}
			}
		}
	}
	if hasToolOutput {
		out.OnToolOutput = func(id, output string) {
			for _, e := range evs {
				if e.OnToolOutput != nil {
					e.OnToolOutput(id, output)
				}
			}
		}
	}
	if hasToolEnd {
		out.OnToolEnd = func(id, name, result string) {
			for _, e := range evs {
				if e.OnToolEnd != nil {
					e.OnToolEnd(id, name, result)
				}
			}
		}
	}
	if hasTelemetry {
		out.OnToolTelemetry = func(telemetry ToolTelemetry) {
			for _, e := range evs {
				if e.OnToolTelemetry != nil {
					e.OnToolTelemetry(telemetry)
				}
			}
		}
	}
	if hasSteer {
		out.OnSteer = func(text string) {
			for _, e := range evs {
				if e.OnSteer != nil {
					e.OnSteer(text)
				}
			}
		}
	}
	if hasCompact {
		out.OnCompact = func(took, kept int) {
			for _, e := range evs {
				if e.OnCompact != nil {
					e.OnCompact(took, kept)
				}
			}
		}
	}
	if hasCompacted {
		out.OnCompacted = func(summary string, cutoff int) {
			for _, e := range evs {
				if e.OnCompacted != nil {
					e.OnCompacted(summary, cutoff)
				}
			}
		}
	}
	if hasCompactionReady {
		out.OnCompactionReady = func(messages []models.Message, summary string, cutoff int) error {
			for _, e := range evs {
				if e.OnCompactionReady != nil {
					if err := e.OnCompactionReady(messages, summary, cutoff); err != nil {
						return err
					}
				}
			}
			return nil
		}
	}
	if hasUsage {
		out.OnUsage = func(u models.Usage) {
			for _, e := range evs {
				if e.OnUsage != nil {
					e.OnUsage(u)
				}
			}
		}
	}
	if hasRetry {
		out.OnRetry = func(ev models.RetryEvent) {
			for _, e := range evs {
				if e.OnRetry != nil {
					e.OnRetry(ev)
				}
			}
		}
	}
	if hasGoalUpdate {
		out.OnGoalUpdate = func(update GoalUpdate) {
			for _, e := range evs {
				if e.OnGoalUpdate != nil {
					e.OnGoalUpdate(update)
				}
			}
		}
	}
	if hasModelCallStart {
		out.OnModelCallStart = func(call ModelCallStart) {
			for _, e := range evs {
				if e.OnModelCallStart != nil {
					e.OnModelCallStart(call)
				}
			}
		}
	}
	if hasPromptView {
		out.OnPromptView = func(view PromptView) {
			for _, e := range evs {
				if e.OnPromptView != nil {
					e.OnPromptView(view)
				}
			}
		}
	}
	if hasModelCallEnd {
		out.OnModelCallEnd = func(call ModelCallEnd) {
			for _, e := range evs {
				if e.OnModelCallEnd != nil {
					e.OnModelCallEnd(call)
				}
			}
		}
	}
	if hasPlanDelta {
		out.OnPlanDelta = func(delta string) {
			for _, e := range evs {
				if e.OnPlanDelta != nil {
					e.OnPlanDelta(delta)
				}
			}
		}
	}
	return out
}

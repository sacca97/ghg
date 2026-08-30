package agent

import "github.com/sacca97/ghg/internal/llm"

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
	}
}

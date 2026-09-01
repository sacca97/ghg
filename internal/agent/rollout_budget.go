package agent

import (
	"fmt"

	"github.com/sacca97/ghg/internal/llm"
)

const (
	defaultPlanBudgetLimit   = 100_000.0
	defaultPlanBudgetReserve = 10_000.0
	weightFreshInput         = 0.1
	weightCompletion         = 1.0
)

// ErrPlanBudgetExhausted indicates that the planning turn exhausted its
// allowed rollout budget.
type ErrPlanBudgetExhausted struct {
	Calls        int
	UsedUnits    float64
	FreshInput   int
	CachedInput  int
	OutputTokens int
}

func (e *ErrPlanBudgetExhausted) Error() string {
	return fmt.Sprintf("Plan rollout budget exhausted after %d calls (used %.1f weighted units; %d fresh input, %d cached input, %d output)",
		e.Calls, e.UsedUnits, e.FreshInput, e.CachedInput, e.OutputTokens)
}

// rolloutBudget tracks turn-local token expenditure weighted by cache effectiveness.
type rolloutBudget struct {
	limit        float64
	reserve      float64
	usedUnits    float64
	freshInput   int
	cachedInput  int
	outputTokens int
	calls        int
	finalized    bool
}

func newPlanRolloutBudget() *rolloutBudget {
	return &rolloutBudget{
		limit:   defaultPlanBudgetLimit,
		reserve: defaultPlanBudgetReserve,
	}
}

// RecordUsage accounts for one successful model request.
func (b *rolloutBudget) RecordUsage(u llm.Usage, fallbackTokens int) {
	if b == nil {
		return
	}
	b.calls++
	prompt := u.PromptTokens
	if prompt == 0 && u.InputTokens > 0 {
		prompt = u.InputTokens
	}
	cached := u.Cached()
	comp := u.CompletionTokens
	if comp == 0 && u.OutputTokens > 0 {
		comp = u.OutputTokens
	}
	if prompt == 0 && comp == 0 && fallbackTokens > 0 {
		prompt = fallbackTokens
	}
	fresh := prompt - cached
	if fresh < 0 {
		fresh = 0
	}
	b.freshInput += fresh
	b.cachedInput += cached
	b.outputTokens += comp

	units := float64(fresh)*weightFreshInput + float64(comp)*weightCompletion
	b.usedUnits += units
}

// Remaining returns the unused weighted units in the budget.
func (b *rolloutBudget) Remaining() float64 {
	if b == nil {
		return 0
	}
	rem := b.limit - b.usedUnits
	if rem < 0 {
		return 0
	}
	return rem
}

// IsReserveCrossed reports whether the remaining budget has dropped to or below the reserve.
func (b *rolloutBudget) IsReserveCrossed() bool {
	if b == nil {
		return false
	}
	return b.Remaining() <= b.reserve
}

// ReminderBlock returns the system prompt reminder corresponding to the current
// remaining threshold. Crossing a threshold updates the reminder, while staying
// within the band produces an identical string to preserve prefix caching.
func (b *rolloutBudget) ReminderBlock() string {
	if b == nil {
		return ""
	}
	rem := b.Remaining()
	if rem <= b.reserve {
		return `<rollout_budget>
You have reached the planning budget reserve. Tools are now disabled. Synthesize the best available implementation plan from your gathered evidence and emit the <proposed_plan> block now.
</rollout_budget>`
	}
	var threshold int
	switch {
	case rem <= 25_000:
		threshold = 25_000
	case rem <= 50_000:
		threshold = 50_000
	default:
		return ""
	}
	return fmt.Sprintf(`<rollout_budget>
You have %d weighted tokens remaining in this planning turn.
Converge on unresolved implementation decisions and avoid repeating
exploration already completed.
</rollout_budget>`, threshold)
}

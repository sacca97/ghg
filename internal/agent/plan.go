package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/sacca97/ghg/internal/models"
	"github.com/sacca97/ghg/internal/sandbox"
	"github.com/sacca97/ghg/internal/tools"
)

// QuestionRequest is one bounded clarification batch for an interactive plan.
type QuestionRequest struct {
	Questions []Question `json:"questions"`
}

type Question struct {
	ID       string           `json:"id"`
	Question string           `json:"question"`
	Options  []QuestionOption `json:"options"`
}

type QuestionOption struct {
	Label       string `json:"label"`
	Description string `json:"description,omitempty"`
}

type QuestionAnswer struct {
	ID    string `json:"id"`
	Value string `json:"value"`
}

type QuestionResult struct {
	Answers []QuestionAnswer `json:"answers"`
}

func (r QuestionRequest) Validate() error {
	if len(r.Questions) == 0 || len(r.Questions) > 3 {
		return fmt.Errorf("ask_user requires 1 to 3 questions")
	}
	seen := make(map[string]struct{}, len(r.Questions))
	for i, question := range r.Questions {
		id := strings.TrimSpace(question.ID)
		if id == "" || len(id) > 64 {
			return fmt.Errorf("question %d has an invalid id", i+1)
		}
		if _, ok := seen[id]; ok {
			return fmt.Errorf("question id %q is repeated", id)
		}
		seen[id] = struct{}{}
		if strings.TrimSpace(question.Question) == "" || len(question.Question) > 240 {
			return fmt.Errorf("question %d is empty or too long", i+1)
		}
		if len(question.Options) < 2 || len(question.Options) > 3 {
			return fmt.Errorf("question %d requires 2 or 3 options", i+1)
		}
		for j, option := range question.Options {
			if strings.TrimSpace(option.Label) == "" || len(option.Label) > 100 {
				return fmt.Errorf("question %d option %d is empty or too long", i+1, j+1)
			}
			if len(option.Description) > 200 {
				return fmt.Errorf("question %d option %d description is too long", i+1, j+1)
			}
		}
	}
	return nil
}

func askUserTool(ask func(context.Context, QuestionRequest) (QuestionResult, error), claim func() bool) tools.Tool {
	return tools.Tool{
		Def: models.NewTool("ask_user",
			"Ask only when the answer would materially change the plan and cannot be safely inferred. Otherwise make a reasonable assumption. Combine all necessary questions into one call.",
			`{"type":"object","properties":{"questions":{"type":"array","minItems":1,"maxItems":3,"items":{"type":"object","properties":{"id":{"type":"string"},"question":{"type":"string"},"options":{"type":"array","minItems":2,"maxItems":3,"items":{"type":"object","properties":{"label":{"type":"string"},"description":{"type":"string"}},"required":["label"],"additionalProperties":false}}},"required":["id","question","options"],"additionalProperties":false}}},"required":["questions"],"additionalProperties":false}`),
		RunResult: func(ctx context.Context, args json.RawMessage) (tools.ToolResult, error) {
			if !claim() {
				return tools.ToolResult{}, fmt.Errorf("ask_user may be called only once per planning turn")
			}
			var request QuestionRequest
			if err := json.Unmarshal(args, &request); err != nil {
				return tools.ToolResult{}, fmt.Errorf("invalid ask_user request: %w", err)
			}
			if err := request.Validate(); err != nil {
				return tools.ToolResult{}, err
			}
			result, err := ask(ctx, request)
			if err != nil {
				return tools.ToolResult{}, err
			}
			data, err := json.Marshal(result)
			if err != nil {
				return tools.ToolResult{}, err
			}
			return tools.TextResult(string(data), string(data)), nil
		},
	}
}

// Plan is the structured result produced by the planning workflow. Steps are
// deliberately plain text: todowrite owns execution state, while acceptance
// checks remain part of the acting prompt.
type Plan struct {
	Goal             string   `json:"goal"`
	Assumptions      []string `json:"assumptions,omitempty"`
	Steps            []string `json:"steps"`
	AcceptanceChecks []string `json:"acceptance_checks"`
	Risks            []string `json:"risks,omitempty"`
}

// Validate checks the limits shared with the conversation's todowrite plan.
func (p Plan) Validate() error {
	if strings.TrimSpace(p.Goal) == "" {
		return fmt.Errorf("plan has no goal")
	}
	if len(p.Steps) == 0 {
		return fmt.Errorf("plan has no steps")
	}
	if len(p.Steps) > maxTodos {
		return fmt.Errorf("plan has more than %d steps", maxTodos)
	}
	for i, step := range p.Steps {
		if step = strings.TrimSpace(step); step == "" {
			return fmt.Errorf("plan step %d is empty", i+1)
		} else if len(step) > maxTodoContent {
			return fmt.Errorf("plan step %d exceeds %d characters", i+1, maxTodoContent)
		}
	}
	if len(p.AcceptanceChecks) == 0 {
		return fmt.Errorf("plan has no acceptance checks")
	}
	for i, check := range p.AcceptanceChecks {
		if check = strings.TrimSpace(check); check == "" {
			return fmt.Errorf("acceptance check %d is empty", i+1)
		} else if len(check) > maxTodoContent {
			return fmt.Errorf("acceptance check %d exceeds %d characters", i+1, maxTodoContent)
		}
	}
	for i, a := range p.Assumptions {
		if strings.TrimSpace(a) == "" {
			return fmt.Errorf("assumption %d is empty", i+1)
		}
	}
	for i, r := range p.Risks {
		if strings.TrimSpace(r) == "" {
			return fmt.Errorf("risk %d is empty", i+1)
		}
	}
	return nil
}

// ParsePlan accepts the JSON object returned by the planner.
func ParsePlan(response string) (Plan, error) {
	response = strings.TrimSpace(response)
	var p Plan
	if err := json.Unmarshal([]byte(response), &p); err != nil {
		return Plan{}, fmt.Errorf("planner returned invalid JSON: %w", err)
	}
	if err := p.Validate(); err != nil {
		return Plan{}, err
	}
	p.Goal = strings.TrimSpace(p.Goal)
	for i := range p.Steps {
		p.Steps[i] = strings.TrimSpace(p.Steps[i])
	}
	for i := range p.AcceptanceChecks {
		p.AcceptanceChecks[i] = strings.TrimSpace(p.AcceptanceChecks[i])
	}
	for i := range p.Assumptions {
		p.Assumptions[i] = strings.TrimSpace(p.Assumptions[i])
	}
	for i := range p.Risks {
		p.Risks[i] = strings.TrimSpace(p.Risks[i])
	}
	return p, nil
}

// Todos converts a validated plan into the existing execution checklist.
func (p Plan) Todos() []Todo {
	items := make([]Todo, len(p.Steps))
	for i, step := range p.Steps {
		status := "pending"
		if i == 0 {
			status = "in_progress"
		}
		items[i] = Todo{ID: fmt.Sprintf("t%d", i+1), Content: step, Status: status}
	}
	return items
}

// SetTodos validates and replaces the current conversation plan. It is the
// public boundary used by the TUI when a planner proposal becomes executable.
func (a *Agent) SetTodos(items []Todo) error {
	_, err := a.setTodos(append([]Todo(nil), items...))
	return err
}

// planModePrompt is injected as a transient system message on every model
// round while the agent is in Plan mode. It is kept as a plain constant here
// (alongside the mode implementation) rather than routed through prompt
// loading — Plan mode is a collaboration mode, not an agent definition.
const planModePrompt = `You are planning in a read-only collaboration mode. Use only the read-only tools currently exposed in this request. You cannot write, edit, run shell commands, spawn tasks, or mutate memory — treat them as unavailable.

Use grep for text, lsp for semantic symbol questions, and read only for the exact source needed. Prefer composite lsp operations when they eliminate an otherwise deterministic locate→navigate→read sequence.

Inspect only the code necessary to understand requirements, locate relevant components, and resolve ambiguity. Reuse evidence already gathered and do not reread unchanged source. Once the remaining uncertainties cannot materially change the implementation decisions, stop exploring and produce the plan.

If clarification is unavailable, make reasonable assumptions and state them in the plan. The set_next_reasoning_effort tool is available when the model advertises multiple efforts; use it when a different advertised effort would be useful for the next model call, otherwise keep the configured effort. Select only an advertised effort.

When the user message contains a tagged-path note, treat its paths as the authoritative scope inventory. Inspect those paths directly; do not use glob or find_files to rediscover them, and do not call both for the same target.

End your response with a Markdown implementation plan in a single, exact block:

<proposed_plan>
...Markdown plan...
</proposed_plan>

A response without that block is valid only when actively gathering necessary initial evidence or asking clarifying questions. Only emit <proposed_plan> once, as your final answer.`

const taggedPathNote = "[note: the user tagged "

// taggedPathsFromTarget returns existing, canonical paths from the mention note.
func taggedPathsFromTarget(target string) []string {
	start := strings.Index(target, taggedPathNote)
	if start < 0 {
		return nil
	}
	value := target[start+len(taggedPathNote):]
	if end := strings.IndexByte(value, ']'); end >= 0 {
		value = value[:end]
	}
	value = strings.TrimSpace(value)
	value = strings.TrimSuffix(value, " — contents are not inlined…")
	value = strings.TrimSuffix(value, " — contents are not inlined")
	var paths []string
	seen := make(map[string]struct{})
	for _, raw := range strings.Split(value, ";") {
		path := strings.TrimSpace(raw)
		if path == "" {
			continue
		}
		path = stripTaggedLineRange(path)
		canonical, err := sandbox.CanonicalPath(path, false)
		if err != nil {
			continue
		}
		info, err := os.Stat(canonical)
		if err != nil || (!info.IsDir() && !info.Mode().IsRegular()) {
			continue
		}
		if _, ok := seen[canonical]; ok {
			continue
		}
		seen[canonical] = struct{}{}
		paths = append(paths, canonical)
	}
	return paths
}

func stripTaggedLineRange(path string) string {
	const prefix = " (lines "
	if !strings.HasSuffix(path, ")") {
		return path
	}
	start := strings.LastIndex(path, prefix)
	if start < 0 || !validTaggedLineRange(path[start+len(prefix):len(path)-1]) {
		return path
	}
	return strings.TrimSpace(path[:start])
}

func validTaggedLineRange(value string) bool {
	parts := strings.Split(value, "-")
	if len(parts) > 2 || len(parts) == 0 {
		return false
	}
	for _, part := range parts {
		if part == "" {
			return false
		}
		for _, r := range part {
			if r < '0' || r > '9' {
				return false
			}
		}
	}
	return true
}

func taggedReadRoots(a *Agent, target string) []string {
	paths := taggedPathsFromTarget(target)
	if a == nil || a.Runtime == nil || a.Runtime.Policy == nil {
		return paths
	}
	protected := a.Runtime.Policy.ProtectedRoots()
	filtered := paths[:0]
	for _, path := range paths {
		blocked := false
		for _, root := range protected {
			if reviewWithin(root, path) {
				blocked = true
				break
			}
		}
		if !blocked {
			filtered = append(filtered, path)
		}
	}
	return filtered
}

func taggedScopeName(workspace, path string) string {
	if workspace != "" {
		if rel, err := filepath.Rel(workspace, path); err == nil && reviewWithin(workspace, path) {
			return filepath.ToSlash(rel)
		}
	}
	return filepath.ToSlash(path)
}

func planTaggedScopePrompt(a *Agent, target string) string {
	if !strings.Contains(target, taggedPathNote) {
		return ""
	}
	workspace := reviewWorkspace(a)
	inventory := reviewInventoryAt(workspace, target)
	files := make([]string, 0, len(inventory.Files)+len(taggedPathsFromTarget(target)))
	seen := make(map[string]struct{}, len(files))
	for _, path := range taggedReadRoots(a, target) {
		name := taggedScopeName(workspace, path)
		if _, ok := seen[name]; ok {
			continue
		}
		seen[name] = struct{}{}
		files = append(files, name)
	}
	for _, file := range inventory.Files {
		if _, ok := seen[file]; ok {
			continue
		}
		seen[file] = struct{}{}
		files = append(files, file)
	}
	if len(files) == 0 {
		return ""
	}
	var b strings.Builder
	b.WriteString("<tagged_scope>\n")
	b.WriteString("The following deterministic inventory comes from the user's tagged paths. Inspect these paths directly; do not use glob or find_files to rediscover listed paths.\nfiles:\n")
	for _, file := range files {
		b.WriteString("- ")
		b.WriteString(file)
		b.WriteByte('\n')
	}
	if inventory.Partial {
		b.WriteString("inventory: partial; use a scoped discovery call only if needed to complete the tagged directory.\n")
	}
	b.WriteString("</tagged_scope>")
	return b.String()
}

const askModePrompt = `You are answering the user's question in a read-only mode. Answer the question directly; it may be about the repository or a general subject, and infer which from the question.

If the question concerns the repository, inspect only the files and evidence needed to answer accurately. Use only the read-only tools currently exposed in this request. You cannot write or edit files, run shell commands, spawn tasks, update goals, or otherwise mutate anything. The set_next_reasoning_effort tool is available when the model advertises multiple efforts; use it when a different advertised effort would be useful for the next model call, otherwise keep the configured effort. Select only an advertised effort. Do not propose an implementation plan unless the user explicitly asks for one.`

// planSafeTools is the read-only allowlist shared by Plan and Ask modes. Enforcement
// happens when building "available", not only through prompting, so mutating
// and side-effecting tools are structurally unreachable in Plan mode. MCP tools
// are intentionally excluded because ghg cannot prove they are read-only.
var planSafeTools = map[string]bool{
	"read":           true,
	"grep":           true,
	"glob":           true,
	"lsp":            true,
	"find_files":     true,
	"output_list":    true,
	"output_read":    true,
	"history_search": true,
	"history_read":   true,
	"web_fetch":      true,
	"web_search":     true,
}

// planTools returns the read-only tool allowlist. It intentionally
// does not include the MCP set (unproven read-only) or any mutating built-in.
func (a *Agent) planTools() []tools.Tool {
	a.toolsMu.Lock()
	defer a.toolsMu.Unlock()
	return filterPlanTools(a.Tools)
}

func filterPlanTools(all []tools.Tool) []tools.Tool {
	var out []tools.Tool
	for _, t := range all {
		if planSafeTools[t.Def.Function.Name] {
			out = append(out, t)
		}
	}
	return out
}

// planStreamParser is a small stateful parser for the exact, line-delimited
// proposed-plan block, tolerant of tags divided across provider chunks. Normal
// text is forwarded to visible; the block body is forwarded to onPlan; wrapper
// tags are dropped from both.
type planStreamParser struct {
	visible func(string)
	onPlan  func(string)
	mode    int
	buf     string
}

const (
	ppModeText = iota
	ppModePlan
)

const (
	proposedPlanOpen  = "<proposed_plan>"
	proposedPlanClose = "</proposed_plan>"
)

// feed consumes one streaming delta, forwarding visible text and plan body
// fragments to the respective callbacks.
func (p *planStreamParser) feed(delta string) {
	p.buf += delta
	p.process()
}

func (p *planStreamParser) process() {
	for {
		var tag string
		if p.mode == ppModeText {
			tag = proposedPlanOpen
		} else {
			tag = proposedPlanClose
		}
		idx := strings.Index(p.buf, tag)
		if idx >= 0 {
			body, rest := p.buf[:idx], p.buf[idx+len(tag):]
			if body != "" {
				p.emit(body)
			}
			p.buf = rest
			if p.mode == ppModeText {
				p.mode = ppModePlan
			} else {
				p.mode = ppModeText
			}
			continue
		}
		// No complete tag yet. Hold back any trailing bytes that could start a
		// tag (so a split tag resolves across chunks) and flush the rest.
		keep := longestPrefixSuffix(p.buf, tag)
		flush := p.buf[:len(p.buf)-keep]
		if flush != "" {
			p.emit(flush)
		}
		p.buf = p.buf[len(p.buf)-keep:]
		return
	}
}

func (p *planStreamParser) emit(s string) {
	if p.mode == ppModeText {
		if p.visible != nil {
			p.visible(s)
		}
		return
	}
	if p.onPlan != nil {
		p.onPlan(s)
	}
}

// close flushes any trailing buffered content. An unfinished plan block at the
// end of the stream is treated as closed: its body is emitted as plan content.
func (p *planStreamParser) close() {
	if p.buf != "" {
		p.emit(p.buf)
		p.buf = ""
	}
}

// longestPrefixSuffix returns the length of the longest suffix of s that equals
// a prefix of tag. It is bounded by len(s).
func longestPrefixSuffix(s, tag string) int {
	max := len(tag) - 1
	if len(s) < max {
		max = len(s)
	}
	for n := max; n > 0; n-- {
		if strings.HasSuffix(s, tag[:n]) {
			return n
		}
	}
	return 0
}

// ExtractProposedPlan returns the body of the last complete
// <proposed_plan>...</proposed_plan> block in text. If a provider violates the
// one-block instruction, the last block wins.
func ExtractProposedPlan(text string) (string, bool) {
	var last string
	rest := text
	for {
		open := strings.Index(rest, proposedPlanOpen)
		if open < 0 {
			break
		}
		body := rest[open+len(proposedPlanOpen):]
		closeIdx := strings.Index(body, proposedPlanClose)
		if closeIdx < 0 {
			break
		}
		last = strings.TrimSpace(body[:closeIdx])
		rest = body[closeIdx+len(proposedPlanClose):]
	}
	return last, last != ""
}

const (
	defaultPlanBudgetLimit   = 200_000.0
	defaultPlanBudgetReserve = 40_000.0
	maxPlanCalls             = 128
	maxPlanCallsReserve      = 8
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
}

type planContinuation struct {
	target            string
	budget            *rolloutBudget
	explorationRounds int
	checkpointPending bool
	checkpointLevel   int
}

func newPlanRolloutBudget() *rolloutBudget {
	return &rolloutBudget{
		limit:   defaultPlanBudgetLimit,
		reserve: defaultPlanBudgetReserve,
	}
}

// RecordUsage accounts for one successful model request.
func (b *rolloutBudget) RecordUsage(u models.Usage, fallbackTokens int) {
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

// IsReserveCrossed reports whether the remaining budget has dropped to or below the reserve
// or the model-call ceiling has entered its reserve.
func (b *rolloutBudget) IsReserveCrossed() bool {
	if b == nil {
		return false
	}
	if b.calls >= (maxPlanCalls - maxPlanCallsReserve) {
		return true
	}
	return b.Remaining() <= b.reserve
}

// ReminderBlock returns the system prompt reminder corresponding to the current
// remaining threshold. Crossing a threshold updates the reminder, while staying
// within the band produces an identical string to preserve prefix caching.
func (b *rolloutBudget) ReminderBlock(reviewMode ...bool) string {
	isReview := len(reviewMode) > 0 && reviewMode[0]
	return b.reminderBlock(b.IsReserveCrossed(), isReview)
}

func (b *rolloutBudget) reminderBlock(finalizing, isReview bool) string {
	if b == nil {
		return ""
	}
	rem := b.Remaining()
	if finalizing {
		if isReview {
			return `<rollout_budget>
You have reached the review budget reserve. Tools are now disabled except submit_review. Submit the best available evidence-backed review using submit_review now.
</rollout_budget>`
		}
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
	modeName := "planning"
	if isReview {
		modeName = "review"
	}
	return fmt.Sprintf(`<rollout_budget>
You have %d weighted tokens remaining in this %s turn.
Converge on unresolved implementation decisions and avoid repeating
exploration already completed.
</rollout_budget>`, threshold, modeName)
}

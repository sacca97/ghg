package agent

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"

	"github.com/sacca97/ghg/internal/llm"
	"github.com/sacca97/ghg/internal/tools"
	"gopkg.in/yaml.v3"
)

// Definition is the declarative contract for a named agent. The Markdown body
// is the prompt; the frontmatter only selects the route, tools, and round
// budget. Path is empty for the built-in planner.
type Definition struct {
	Name        string
	Description string
	Role        string
	Tools       []string
	MaxRounds   int
	Prompt      string
	Path        string
}

// DefinitionLoadOptions controls the two discovery roots. ProjectDir and
// UserDir are the direct agent-definition directories, not their parents. A
// trusted project is loaded before the user directory, so project definitions
// take precedence over same-named user definitions. Zero values use
// .agents/ in the current directory and ~/.ghg/agents/ respectively.
type DefinitionLoadOptions struct {
	ProjectDir     string
	UserDir        string
	ProjectTrusted bool
}

const (
	builtInPlannerName  = "planner"
	builtInReviewerName = "reviewer"
	maxDefinitionRounds = 32
	maxDefinitionName   = 64
)

var definitionNameRE = regexp.MustCompile(`^[a-z0-9][a-z0-9-]*$`)

var knownDefinitionTools = map[string]struct{}{
	"bash": {}, "read": {}, "write": {}, "edit": {}, "grep": {}, "glob": {}, "find_files": {}, "lsp": {}, "lsp_rename": {},
	"task": {}, "todowrite": {}, "remember": {}, "forget": {},
	"artifact_list": {}, "artifact_read": {}, "submit_plan": {}, "submit_review": {},
}

// BuiltInPlannerDefinition is the first agent definition shipped by ghg. It
// is intentionally a value rather than a special execution branch: the same
// validation, tool resolution, and bounded runner used by loaded definitions
// runs it. User files cannot shadow the reserved planner definition.
func BuiltInPlannerDefinition() Definition {
	return Definition{
		Name:        builtInPlannerName,
		Description: "Read-only implementation planner",
		Role:        "smart",
		Tools:       []string{"read", "grep", "glob", "lsp", "submit_plan"},
		MaxRounds:   maxDefinitionRounds,
		Prompt: `You are ghg's planning agent. Produce an implementation plan for the user's goal.

Inspect the repository when useful with read, grep, glob, and bounded lsp navigation. These tools are read-only. Do not use bash, write, edit, lsp_rename, task, MCP, or any other tool. When you have enough information, call submit_plan exactly once with a concrete goal, ordered imperative steps, and independently verifiable acceptance checks. Do not finish with a prose plan: submit_plan is the terminal result.`,
	}
}

// BuiltInReviewerDefinition is the code review agent definition shipped by ghg.
func BuiltInReviewerDefinition() Definition {
	return Definition{
		Name:        builtInReviewerName,
		Description: "Read-only code review agent",
		Role:        "smart",
		Tools:       []string{"read", "grep", "glob", "lsp", "submit_review"},
		MaxRounds:   maxDefinitionRounds,
		Prompt: `You are ghg's code review agent. Produce a structured review for the requested changes or code.

Inspect the repository when useful with read, grep, glob, and bounded lsp navigation. These tools are read-only. Do not use bash, write, edit, lsp_rename, task, MCP, or any other tool. When you have enough information, call submit_review exactly once with summary, verdict (approve, request_changes, or comment), structured findings (title, severity, file, optional line, evidence, recommendation), and checks_performed. Do not finish with a prose review: submit_review is the terminal result.`,
	}
}

// LoadAgentDefinitions discovers project and user Markdown definitions and
// always includes the built-in planner and reviewer. A malformed definition or
// unknown tool is an error; an absent discovery directory is normal.
func LoadAgentDefinitions(opts DefinitionLoadOptions) (map[string]Definition, error) {
	defs := map[string]Definition{
		builtInPlannerName:  BuiltInPlannerDefinition(),
		builtInReviewerName: BuiltInReviewerDefinition(),
	}
	projectDir, userDir := opts.ProjectDir, opts.UserDir
	if projectDir == "" {
		if wd, err := os.Getwd(); err == nil {
			projectDir = filepath.Join(wd, ".agents")
		}
	}
	if userDir == "" {
		if home, err := os.UserHomeDir(); err == nil {
			userDir = filepath.Join(home, ".ghg", "agents")
		}
	}

	dirs := make([]string, 0, 2)
	if opts.ProjectTrusted && projectDir != "" {
		dirs = append(dirs, projectDir)
	}
	if userDir != "" {
		dirs = append(dirs, userDir)
	}
	for _, dir := range dirs {
		entries, err := os.ReadDir(dir)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return nil, fmt.Errorf("agent definitions: read %s: %w", dir, err)
		}
		for _, entry := range entries {
			if entry.IsDir() || strings.ToLower(filepath.Ext(entry.Name())) != ".md" {
				continue
			}
			path := filepath.Join(dir, entry.Name())
			if info, statErr := os.Stat(path); statErr != nil {
				return nil, fmt.Errorf("agent definition %s: %w", path, statErr)
			} else if !info.Mode().IsRegular() {
				continue
			}
			def, parseErr := parseDefinition(path)
			if parseErr != nil {
				return nil, parseErr
			}
			if def.Name == builtInPlannerName || def.Name == builtInReviewerName {
				return nil, fmt.Errorf("agent definition %s: name %q is reserved", path, def.Name)
			}
			if _, exists := defs[def.Name]; exists {
				// Directories are visited in precedence order and the first
				// definition wins, matching the project-before-user discovery
				// shape used by ghg's skills and provider layers.
				continue
			}
			defs[def.Name] = def
		}
	}
	return defs, nil
}

type definitionFrontmatter struct {
	Name        string   `yaml:"name"`
	Description string   `yaml:"description"`
	Role        string   `yaml:"role"`
	Tools       []string `yaml:"tools"`
	MaxRounds   int      `yaml:"max_rounds"`
}

func parseDefinition(path string) (Definition, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return Definition{}, fmt.Errorf("agent definition %s: %w", path, err)
	}
	lines := strings.Split(string(data), "\n")
	if len(lines) == 0 || strings.TrimSpace(strings.TrimSuffix(lines[0], "\r")) != "---" {
		return Definition{}, fmt.Errorf("agent definition %s: no frontmatter", path)
	}
	end := -1
	for i := 1; i < len(lines); i++ {
		if strings.TrimSpace(strings.TrimSuffix(lines[i], "\r")) == "---" {
			end = i
			break
		}
	}
	if end < 0 {
		return Definition{}, fmt.Errorf("agent definition %s: unterminated frontmatter", path)
	}

	var fm definitionFrontmatter
	decoder := yaml.NewDecoder(bytes.NewBufferString(strings.Join(lines[1:end], "\n")))
	decoder.KnownFields(true)
	if err := decoder.Decode(&fm); err != nil {
		return Definition{}, fmt.Errorf("agent definition %s: invalid frontmatter: %w", path, err)
	}
	var extra any
	if err := decoder.Decode(&extra); err != io.EOF {
		if err == nil {
			return Definition{}, fmt.Errorf("agent definition %s: frontmatter has multiple documents", path)
		}
		return Definition{}, fmt.Errorf("agent definition %s: invalid frontmatter: %w", path, err)
	}
	def := Definition{
		Name:        strings.TrimSpace(fm.Name),
		Description: strings.TrimSpace(fm.Description),
		Role:        strings.TrimSpace(fm.Role),
		Tools:       cleanDefinitionTools(fm.Tools),
		MaxRounds:   fm.MaxRounds,
		Prompt:      strings.TrimSpace(strings.Join(lines[end+1:], "\n")),
		Path:        path,
	}
	if err := def.Validate(); err != nil {
		return Definition{}, fmt.Errorf("agent definition %s: %w", path, err)
	}
	return def, nil
}

func cleanDefinitionTools(names []string) []string {
	clean := make([]string, 0, len(names))
	seen := make(map[string]struct{}, len(names))
	for _, name := range names {
		name = strings.TrimSpace(name)
		if name == "" {
			clean = append(clean, name)
			continue
		}
		if _, ok := seen[name]; ok {
			clean = append(clean, name)
			continue
		}
		seen[name] = struct{}{}
		clean = append(clean, name)
	}
	return clean
}

// Validate checks both the declarative shape and the static tool allowlist.
// Dynamic MCP tools are deliberately not accepted here: definitions must be
// reviewable from their Markdown files and the built-in planner must never
// inherit arbitrary MCP capabilities.
func (d Definition) Validate() error {
	if strings.TrimSpace(d.Name) == "" {
		return errors.New("name is required")
	}
	if len(d.Name) > maxDefinitionName || !definitionNameRE.MatchString(d.Name) || strings.Contains(d.Name, "--") {
		return fmt.Errorf("name %q is invalid (use lowercase letters, numbers, and single hyphens; max %d characters)", d.Name, maxDefinitionName)
	}
	if strings.TrimSpace(d.Description) == "" {
		return errors.New("description is required")
	}
	switch d.Role {
	case "default", "smart", "fast", "tiny":
	default:
		return fmt.Errorf("role %q is invalid (want default, smart, fast, or tiny)", d.Role)
	}
	if d.MaxRounds < 1 || d.MaxRounds > maxDefinitionRounds {
		return fmt.Errorf("max_rounds must be between 1 and %d", maxDefinitionRounds)
	}
	if strings.TrimSpace(d.Prompt) == "" {
		return errors.New("Markdown body is required")
	}
	seen := make(map[string]struct{}, len(d.Tools))
	for _, name := range d.Tools {
		name = strings.TrimSpace(name)
		if _, ok := knownDefinitionTools[name]; !ok {
			return fmt.Errorf("unknown tool %q", name)
		}
		if _, ok := seen[name]; ok {
			return fmt.Errorf("tool %q is listed more than once", name)
		}
		seen[name] = struct{}{}
	}
	return nil
}

// DefinitionNames returns loaded names in stable display order.
func DefinitionNames(defs map[string]Definition) []string {
	return slices.Sorted(maps.Keys(defs))
}

// DefinitionResult is the output of one declarative agent run. A terminal
// tool call is preserved as raw JSON so its caller can validate the structured
// payload without parsing model prose.
type DefinitionResult struct {
	Text         string
	TerminalName string
	TerminalArgs json.RawMessage
}

// RunDefinition executes a definition with only the tools named by its
// frontmatter. Definitions that advertise submit_plan stop after a successful
// submit_plan call; all other definitions return the model's final text.
func (a *Agent) RunDefinition(ctx context.Context, input string, def Definition, ev Events) (DefinitionResult, error) {
	if err := def.Validate(); err != nil {
		return DefinitionResult{}, err
	}
	if a == nil || a.Backend == nil {
		return DefinitionResult{}, errors.New("agent definition: no backend")
	}
	available, err := a.definitionTools(def.Name, def.Tools)
	if err != nil {
		return DefinitionResult{}, err
	}
	messages := a.MessagesSnapshot()
	if len(messages) > 0 && messages[0].Role == "system" {
		messages[0].Content = appendDefinitionPrompt(messages[0].Content, def)
	} else {
		messages = append([]llm.Message{{Role: "system", Content: definitionPrompt(def)}}, messages...)
	}
	messages = append(messages, llm.Message{Role: "user", Content: input})
	terminalToolName := ""
	if slices.Contains(def.Tools, "submit_plan") {
		terminalToolName = "submit_plan"
	} else if slices.Contains(def.Tools, "submit_review") {
		terminalToolName = "submit_review"
	}
	requiresTerminal := terminalToolName != ""
	terminalTools, err := a.definitionTools(def.Name, []string{terminalToolName})
	if requiresTerminal && err != nil {
		return DefinitionResult{}, err
	}
	for round := 0; round < def.MaxRounds; round++ {
		roundTools := available
		if requiresTerminal && round == def.MaxRounds-1 {
			roundTools = terminalTools
			messages = append(messages, llm.Message{
				Role:    "user",
				Content: "Exploration rounds exhausted. Submit your final result now using " + terminalToolName + ".",
			})
		}
		reasoningEffort, reasoningEnabled := a.ReasoningRequest()
		msg, usage, callErr := a.Complete(ctx, llm.Request{
			Model:            a.Model,
			Messages:         messages,
			Tools:            tools.Defs(roundTools),
			MaxTokens:        a.MaxTokens,
			ReasoningEffort:  reasoningEffort,
			ReasoningEnabled: reasoningEnabled,
		}, ev)
		a.AddUsage(usage)
		if ev.OnUsage != nil {
			ev.OnUsage(usage)
		}
		if callErr != nil {
			return DefinitionResult{}, callErr
		}
		if len(msg.ToolCalls) == 0 {
			return DefinitionResult{Text: msg.TextContent()}, nil
		}
		messages = append(messages, msg)
		results := a.runToolResultsWithTools(ctx, msg.ToolCalls, ev, roundTools)
		for i, call := range msg.ToolCalls {
			messages = append(messages, llm.Message{
				Role:       "tool",
				Content:    tools.ModelText(results[i]),
				ToolCallID: call.ID,
				Name:       call.Function.Name,
				Artifact:   results[i].Artifact,
				ExitCode:   results[i].ExitCode,
				Source:     results[i].Source,
			})
		}
		if ctx.Err() != nil {
			return DefinitionResult{}, ctx.Err()
		}
		if requiresTerminal {
			for i, call := range msg.ToolCalls {
				if call.Function.Name == terminalToolName && results[i].ExitCode == 0 {
					return DefinitionResult{
						TerminalName: call.Function.Name,
						TerminalArgs: json.RawMessage(append([]byte(nil), call.Function.Arguments...)),
					}, nil
				}
			}
		}
	}
	if requiresTerminal {
		return DefinitionResult{}, fmt.Errorf("agent definition %q reached its max_rounds (%d) without a valid terminal tool call", def.Name, def.MaxRounds)
	}
	return DefinitionResult{}, fmt.Errorf("agent definition %q reached its max_rounds (%d)", def.Name, def.MaxRounds)
}

func (a *Agent) definitionTools(definitionName string, names []string) ([]tools.Tool, error) {
	base := a.Tools
	if len(base) == 0 {
		base = tools.All()
	}
	byName := make(map[string]tools.Tool, len(base)+1)
	for _, tool := range base {
		byName[tool.Def.Function.Name] = tool
	}
	available := make([]tools.Tool, 0, len(names))
	for _, name := range names {
		if name == "submit_plan" {
			available = append(available, submitPlanTool())
			continue
		}
		if name == "submit_review" {
			available = append(available, submitReviewTool())
			continue
		}
		tool, ok := byName[name]
		if !ok {
			return nil, fmt.Errorf("agent definition %q requested unavailable tool %q", definitionName, name)
		}
		available = append(available, tool)
	}
	return available, nil
}

func definitionPrompt(def Definition) string {
	return "Agent definition " + def.Name + ":\n\n" + def.Prompt
}

func appendDefinitionPrompt(system string, def Definition) string {
	if strings.TrimSpace(system) == "" {
		return definitionPrompt(def)
	}
	return system + "\n\n" + definitionPrompt(def)
}

func submitPlanTool() tools.Tool {
	return tools.Tool{
		Def: llm.NewTool("submit_plan",
			"Submit the validated implementation plan. This is the planner's terminal tool; call it once when repository inspection is complete.",
			`{"type":"object","properties":{"goal":{"type":"string","description":"Concrete outcome of the work"},"assumptions":{"type":"array","items":{"type":"string"}},"steps":{"type":"array","description":"Ordered imperative implementation steps","items":{"type":"string"}},"acceptance_checks":{"type":"array","description":"Independently verifiable checks","items":{"type":"string"}},"risks":{"type":"array","items":{"type":"string"}}},"required":["goal","steps","acceptance_checks"]}`),
		Run: func(_ context.Context, args json.RawMessage) (string, error) {
			if _, err := ParsePlan(string(args)); err != nil {
				return "", err
			}
			return "Plan accepted.", nil
		},
	}
}

func submitReviewTool() tools.Tool {
	return tools.Tool{
		Def: llm.NewTool("submit_review",
			"Submit the validated code review. This is the reviewer's terminal tool; call it once when inspection is complete.",
			`{"type":"object","properties":{"summary":{"type":"string","description":"Executive summary of the review"},"verdict":{"type":"string","enum":["approve","request_changes","comment"],"description":"Review verdict"},"findings":{"type":"array","description":"Structured findings","items":{"type":"object","properties":{"title":{"type":"string"},"severity":{"type":"string","enum":["critical","high","medium","low","info"]},"file":{"type":"string"},"line":{"type":"integer"},"evidence":{"type":"string"},"recommendation":{"type":"string"}},"required":["title","severity"]}},"checks_performed":{"type":"array","items":{"type":"string"}}},"required":["summary","verdict","findings"]}`),
		Run: func(_ context.Context, args json.RawMessage) (string, error) {
			if _, err := ParseReview(string(args)); err != nil {
				return "", err
			}
			return "Review accepted.", nil
		},
	}
}

const maxPlanAttempts = 2

// ProposePlan runs the built-in planner definition. It is the shared planning
// entry point for the TUI and headless CLI, so both surfaces enforce the same
// terminal tool, read-only allowlist, and retry budget.
func ProposePlan(ctx context.Context, planner *Agent, goal string, ev Events) (Plan, error) {
	return ProposePlanWithDefinition(ctx, planner, goal, BuiltInPlannerDefinition(), ev)
}

// ProposePlanWithDefinition is the definition-aware planning entry point. The
// caller may select a loaded definition, but the definition itself still
// controls the model-visible tools and round budget.
func ProposePlanWithDefinition(ctx context.Context, planner *Agent, goal string, def Definition, ev Events) (Plan, error) {
	var lastErr error
	for attempt := 0; attempt < maxPlanAttempts; attempt++ {
		input := plannerInput(goal, attempt > 0)
		result, err := planner.RunDefinition(ctx, input, def, ev)
		if err != nil {
			return Plan{}, err
		}
		if result.TerminalName != "submit_plan" {
			lastErr = errors.New("planner did not call submit_plan")
			continue
		}
		plan, err := ParsePlan(string(result.TerminalArgs))
		if err == nil {
			return plan, nil
		}
		lastErr = err
	}
	if lastErr == nil {
		lastErr = errors.New("planner returned no plan")
	}
	return Plan{}, fmt.Errorf("invalid planner output after %d attempts: %w", maxPlanAttempts, lastErr)
}

func plannerInput(goal string, retry bool) string {
	input := fmt.Sprintf("Plan this implementation goal:\n\n%s\n\nInspect the repository as needed, then submit the plan with submit_plan.", strings.TrimSpace(goal))
	if retry {
		input += "\n\nYour previous proposal was invalid or did not use the terminal tool. Correct it and call submit_plan now."
	}
	return input
}

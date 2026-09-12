package worker

// CommandOwner names the layer that executes a user-facing command.
type CommandOwner string

const (
	// OwnerWorker commands are validated and executed by the worker process.
	OwnerWorker CommandOwner = "worker"
	// OwnerClient commands control local UI state or collect input the worker
	// cannot (masked credential prompts, pickers, file exports).
	OwnerClient CommandOwner = "client"
	// OwnerSupervisor commands switch or inspect workers and stay outside the
	// ordinary worker command path.
	OwnerSupervisor CommandOwner = "supervisor"
)

// CommandSpec is the declarative catalogue entry for one user-facing command:
// its canonical name, accepted aliases, one-line hint, and owning layer.
// Execution stays in each adapter's ordinary switch statement; only the
// metadata is shared so help, completion, and availability cannot drift
// between the TUI, the VS Code extension, and the worker.
type CommandSpec struct {
	Name    string
	Aliases []string
	Hint    string
	Owner   CommandOwner
}

// commandCatalogue is the single source of truth for user-facing commands.
// The hint text is rendered verbatim by both clients, so keep it short and
// keep the canonical name first.
var commandCatalogue = []CommandSpec{
	{Name: "!cmd", Hint: "— run a shell command in the worker; output lands in the transcript and conversation", Owner: OwnerWorker},
	{Name: "/ask", Hint: "<question> — answer directly; repository questions may be investigated read-only", Owner: OwnerWorker},
	{Name: "/approval", Hint: "[ask|auto|never] — switch capability approval live (bare shows the current mode)", Owner: OwnerWorker},
	{Name: "/auth", Hint: "[provider] [key] — connect any profile (bare lists profiles; provider-only opens a masked prompt; also: ghg auth <provider>)", Owner: OwnerClient},
	{Name: "/cd", Hint: "[dir] — change working directory (bare prints it)", Owner: OwnerWorker},
	{Name: "/clear", Hint: "— reset conversation", Owner: OwnerClient},
	{Name: "/compact", Hint: "— compact now using tiny → fast → default → smart; retry undoes the last compaction, log lists them; compaction level: ctrl+p › Compaction level", Owner: OwnerWorker},
	{Name: "/context-doctor", Hint: "— audit what a fresh session injects (skills, MCP, tool schemas) and its token cost", Owner: OwnerWorker},
	{Name: "/continue", Hint: "— continue the interrupted turn using the current session history", Owner: OwnerWorker},
	{Name: "/detach", Hint: "— stop the worker and exit; resume later (ctrl+d)", Owner: OwnerWorker},
	{Name: "/dynamic-reasoning", Hint: "— toggle model per-call reasoning effort selection (default on)", Owner: OwnerWorker},
	{Name: "/effort", Hint: "[level] — reasoning effort: off·low·medium·high (bare opens selector)", Owner: OwnerWorker},
	{Name: "/execute", Hint: "[plan] — execute the latest proposal or supplied plan with the fast model", Owner: OwnerWorker},
	{Name: "/export", Aliases: []string{"/export-result", "/export-chat", "/export-log"}, Hint: "[chat|plan|review|last] [path] [--format json|markdown] [--force] — export chat log or structured result to a file", Owner: OwnerClient},
	{Name: "/fork", Hint: "[name] — copy the conversation into a new session (pick a point in the rewind picker with f)", Owner: OwnerWorker},
	{Name: "/goal", Hint: "<text> — keep working until the goal is met (unbounded; resume | clear)", Owner: OwnerWorker},
	{Name: "/goal-from-context", Hint: "[n] — formulate a goal from the last n messages (default 8) and work until it's met", Owner: OwnerWorker},
	{Name: "/help", Aliases: []string{"/commands"}, Hint: "— show all commands and keybindings", Owner: OwnerClient},
	{Name: "/lsp", Hint: "— show language server status", Owner: OwnerWorker},
	{Name: "/mcp", Hint: "[name] [reconnect|enable|disable] — MCP servers: status, reconnect, toggle", Owner: OwnerWorker},
	{Name: "/me", Aliases: []string{"/agents"}, Hint: "— edit your standing instructions (~/.ghg/AGENTS.md) in $EDITOR", Owner: OwnerClient},
	{Name: "/memory", Hint: "[n] [session] — saved memories: list what's injected each turn, mark entry n done", Owner: OwnerClient},
	{Name: "/model", Hint: "<name> [provider] — switch model (any provider-catalog model works; refresh pulls new announcements)", Owner: OwnerWorker},
	{Name: "/notify", Hint: "[config|on|off] — configure or toggle Telegram completion notifications", Owner: OwnerWorker},
	{Name: "/plan", Hint: "[goal] — enter read-only Plan mode or explore a goal with the smart model (run it with /execute)", Owner: OwnerWorker},
	{Name: "/pwd", Hint: "— print working directory", Owner: OwnerClient},
	{Name: "/quit", Aliases: []string{"/exit", "/q"}, Hint: "— exit", Owner: OwnerClient},
	{Name: "/rename", Hint: "[title] — retitle this session", Owner: OwnerWorker},
	{Name: "/report", Hint: "— bug-report bundle: prefilled GitHub-issue link + copy-pastable environment snippet (terminal, versions)", Owner: OwnerClient},
	{Name: "/resume", Hint: "[id] — resume a previous session", Owner: OwnerSupervisor},
	{Name: "/review", Hint: "<target> — run a one-shot read-only review with structured findings using the smart model", Owner: OwnerWorker},
	{Name: "/schedule", Hint: "@every 10m|<@at time> <prompt> — schedule a wakeup turn; list | cancel <n>", Owner: OwnerClient},
	{Name: "/search-providers", Hint: "[add|use|remove] — configure SearXNG search endpoints", Owner: OwnerWorker},
	{Name: "/tasks", Hint: "[id] — background subagents: focus the dock, or open one subagent's live view", Owner: OwnerClient},
}

// Commands returns the user-facing command catalogue.
func Commands() []CommandSpec {
	return commandCatalogue
}

// FindCommand resolves a canonical name or one of its aliases to its catalogue
// entry, or nil for an unknown command.
func FindCommand(name string) *CommandSpec {
	for i := range commandCatalogue {
		if commandCatalogue[i].Name == name {
			return &commandCatalogue[i]
		}
		for _, alias := range commandCatalogue[i].Aliases {
			if alias == name {
				return &commandCatalogue[i]
			}
		}
	}
	return nil
}

// CommandName resolves a command or alias to its canonical catalogue name,
// returning the input unchanged when it is unknown.
func CommandName(name string) string {
	if spec := FindCommand(name); spec != nil {
		return spec.Name
	}
	return name
}

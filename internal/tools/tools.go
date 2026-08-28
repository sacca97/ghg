// Package tools implements the agent's built-in tools.
package tools

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/sacca97/ghg/internal/llm"
	"github.com/sacca97/ghg/internal/tools/bashrun"
)

// Tool is a named executable tool with a JSON schema.
type Tool struct {
	Def       llm.Tool
	Run       func(ctx context.Context, args json.RawMessage) (string, error)
	RunResult func(ctx context.Context, args json.RawMessage) (ToolResult, error)
}

// InteractiveRunner runs an interactive bash command with PTY-backed live I/O.
// The TUI installs one so the agent's bash tool can hand interactive prompts
// (sudo, ssh, gpg) to the user. ctx caps the whole run; keys feeds keystrokes
// the user types; the returned string is fed back to the model as tool output.
// Implementations must be safe to call from a goroutine that is not the UI
// thread, and must not block forever when no input arrives.
type InteractiveRunner interface {
	Run(ctx context.Context, command string, timeout time.Duration, keys <-chan []byte) string
}

// InteractiveBash is the hook installed by the TUI; nil means the agent's bash
// tool runs interactive commands itself using the non-interactive fallback
// (which fast-fails sudo-style prompts instead of hanging).
var InteractiveBash InteractiveRunner

// LSP, when non-nil, feeds language-server diagnostics back to the model by
// appending a <diagnostics> block to write/edit tool output (see
// internal/lsp). Installed by the TUI at startup; nil in tests and headless
// runs. Implementations must be safe for concurrent use (parallel tool
// calls) and must honor ctx (ctrl+c cancels the wait).
var LSP interface {
	WaitDiagnostics(ctx context.Context, path string) string
}

// All returns the built-in tool set.
func All() []Tool {
	return []Tool{bashTool(), readTool(), writeTool(), editTool(), grepTool(), globTool(), findFilesTool()}
}

// Defs returns the llm.Tool definitions for a tool set.
func Defs(ts []Tool) []llm.Tool {
	defs := make([]llm.Tool, len(ts))
	for i, t := range ts {
		defs[i] = t.Def
	}
	return defs
}

// Suggester returns the closest known tool names for an unknown one —
// installed by the agent (which knows the live MCP tool set) so a stale or
// typo'd tool call nudges the model toward the right name instead of
// dead-ending the turn.
var Suggester func(name string) []string

// Execute runs the named tool. Errors are returned as strings so they can be
// fed back to the model rather than aborting the loop.
func Execute(ctx context.Context, ts []Tool, name string, args json.RawMessage) string {
	return ExecuteResult(ctx, ts, name, args).Preview
}

// ExecuteResult runs the named tool and returns its structured result. The
// string Execute wrapper above remains the compatibility surface for MCP and
// existing callers; agent turns use this form so retained evidence is still
// available after the model preview is bounded.
func ExecuteResult(ctx context.Context, ts []Tool, name string, args json.RawMessage) ToolResult {
	for _, t := range ts {
		if t.Def.Function.Name == name {
			var result ToolResult
			var err error
			if t.RunResult != nil {
				result, err = t.RunResult(ctx, args)
			} else if t.Run != nil {
				var out string
				out, err = t.Run(ctx, args)
				if err == nil {
					result = textResult(out, out, 0)
				}
			} else {
				err = errors.New("tool has no implementation")
			}
			if err != nil {
				result := errorToolResult(err)
				result.Source = name
				return result
			}
			result = normalizeResult(result)
			if result.Source == "" {
				result.Source = name
			}
			if result.Metadata == nil {
				result.Metadata = map[string]string{}
			}
			if _, ok := result.Metadata["source"]; !ok {
				result.Metadata["source"] = result.Source
			}
			return result
		}
	}
	msg := fmt.Sprintf("Error: unknown tool %q", name)
	if Suggester != nil {
		if hints := Suggester(name); len(hints) > 0 {
			msg += " — did you mean " + strings.Join(hints, " or ") + "?"
		}
	}
	result := errorToolResult(errors.New(strings.TrimPrefix(msg, "Error: ")))
	result.Source = name
	return result
}

const maxOutput = 50_000 // bytes of tool output fed back to the model

// Truncate caps tool output at maxOutput with a marker; exported for the MCP
// bridge, which flattens remote results into the same budget.
func Truncate(s string) string {
	return truncate(s)
}

func truncate(s string) string {
	if len(s) <= maxOutput {
		return s
	}
	return s[:maxOutput] + fmt.Sprintf("\n... [truncated %d bytes]", len(s)-maxOutput)
}

// lspDiagnostics appends the LSP diagnostics block for a just-written file.
// Never fails the tool: a nil hook, an uncovered file, or a slow server all
// yield "" (the wait is capped inside internal/lsp).
func lspDiagnostics(ctx context.Context, path string) string {
	if LSP == nil {
		return ""
	}
	return LSP.WaitDiagnostics(ctx, path)
}

// TruncateTail caps tool output at maxOutput bytes, keeping the tail (the end
// is usually where the error is). Exported for the TUI's `!` shell escape,
// which formats output exactly like the bash tool.
func TruncateTail(s string) string {
	if len(s) <= maxOutput {
		return s
	}
	return fmt.Sprintf("[... first %d bytes truncated]\n", len(s)-maxOutput) + s[len(s)-maxOutput:]
}

func bashTool() Tool {
	return Tool{
		Def: llm.NewTool("bash",
			"Execute a bash command in the current working directory and return its combined stdout/stderr. Use for builds, tests, git, and operations the dedicated read/search/edit tools cannot express. Prefer grep, glob, find_files, and bounded read for exploration; simple recursive inspection commands may be redirected.",
			`{"type":"object","properties":{"command":{"type":"string","description":"The bash command to execute"},"timeout":{"type":"number","description":"Timeout in seconds (default 120)"},"interactive":{"type":"boolean","description":"Run in a PTY so sudo/ssh-style password prompts work. ghg stays in control of the terminal and forwards your keystrokes; the command is killed after 15s of no input. Use only for commands that genuinely need a password."}},"required":["command"]}`),
		Run: func(ctx context.Context, args json.RawMessage) (string, error) {
			result, err := runBashResult(ctx, args)
			return result.Preview, err
		},
		RunResult: runBashResult,
	}
}

func runBashResult(ctx context.Context, args json.RawMessage) (ToolResult, error) {
	var a struct {
		Command     string  `json:"command"`
		Timeout     float64 `json:"timeout"`
		Interactive bool    `json:"interactive"`
	}
	if err := json.Unmarshal(args, &a); err != nil {
		return ToolResult{}, err
	}
	if a.Timeout <= 0 {
		a.Timeout = 120
	}
	if redirect, ok := redirectBashSearch(a.Command); ok {
		result := textResult(redirect.Message, redirect.Message, 0)
		result.Source = "bash"
		result.Metadata = map[string]string{
			"source":           "bash",
			"bash_redirect":    "true",
			"redirect_tool":    redirect.Tool,
			"redirect_command": strings.Fields(a.Command)[0],
		}
		return result, nil
	}
	if deny := checkGate("bash", a.Command); deny != "" {
		return ToolResult{}, errors.New(deny)
	}
	dur := time.Duration(a.Timeout * float64(time.Second))

	// Interactive mode hands the live terminal to the user only when the
	// TUI has wired a runner. Without it we run non-interactively, which
	// fails sudo-style prompts fast instead of hanging on ghg's tty.
	if a.Interactive && InteractiveBash != nil {
		keys := make(chan []byte, 16)
		out := InteractiveBash.Run(ctx, a.Command, dur, keys)
		return MarkUntrusted(TextResultWithSize(out, boundedTailPreview(out, bashPreviewLimit(a.Command)), int64(len(out)), true, 0), "bash"), nil
	}

	var update func(string)
	if fn := onUpdate(ctx); fn != nil {
		update = func(snapshot string) { fn(boundedTailPreview(snapshot, bashPreviewLimit(a.Command))) }
	}
	res := bashrun.Run(ctx, bashrun.Options{
		Command:  a.Command,
		Timeout:  dur,
		OnUpdate: update,
	})

	full := res.Output
	originalBytes := res.OriginalBytes
	complete := res.Complete
	if res.TimedOut {
		marker := "\n(command timed out)"
		full += marker
		originalBytes += int64(len(marker))
		complete = false
	}
	if res.Exit != "" {
		marker := fmt.Sprintf("\n(%s)", res.Exit)
		full += marker
		originalBytes += int64(len(marker))
	}
	return MarkUntrusted(
		capturedResult(full, boundedTailPreview(full, bashPreviewLimit(a.Command)), originalBytes, complete, boolToExitCode(res.Exit == "" && !res.TimedOut)),
		"bash",
	), nil
}

func boolToExitCode(success bool) int {
	if success {
		return 0
	}
	return 1
}

func readTool() Tool {
	return Tool{
		Def: llm.NewTool("read",
			"Read a bounded range of complete lines and issue an observation id for later range-authorized edits. Use offset/limit to continue.",
			`{"type":"object","properties":{"path":{"type":"string","description":"Path to the file"},"offset":{"type":"number","description":"1-based line to start from (default 1)"},"limit":{"type":"number","description":"Max complete lines to return (default 200, maximum 1000)"}},"required":["path"]}`),
		Run: func(ctx context.Context, args json.RawMessage) (string, error) {
			result, err := runReadResult(ctx, args)
			return result.Preview, err
		},
		RunResult: runReadResult,
	}
}

func runReadResult(ctx context.Context, args json.RawMessage) (ToolResult, error) {
	var a struct {
		Path   string `json:"path"`
		Offset int    `json:"offset"`
		Limit  int    `json:"limit"`
	}
	if err := json.Unmarshal(args, &a); err != nil {
		return ToolResult{}, err
	}
	return runObservedRead(ctx, a)
}

func writeTool() Tool {
	return Tool{
		Def: llm.NewTool("write",
			"Write content to a file, creating it (and parent directories) or overwriting it.",
			`{"type":"object","properties":{"path":{"type":"string","description":"Path to the file"},"content":{"type":"string","description":"Full file content"}},"required":["path","content"]}`),
		Run: func(ctx context.Context, args json.RawMessage) (string, error) {
			result, err := runWriteResult(ctx, args)
			return result.Preview, err
		},
		RunResult: runWriteResult,
	}
}

func runWriteResult(ctx context.Context, args json.RawMessage) (ToolResult, error) {
	var a struct {
		Path    string `json:"path"`
		Content string `json:"content"`
	}
	if err := json.Unmarshal(args, &a); err != nil {
		return ToolResult{}, err
	}
	if deny := checkGate("write", a.Path); deny != "" {
		return ToolResult{}, errors.New(deny)
	}
	if err := os.MkdirAll(filepath.Dir(a.Path), 0o755); err != nil {
		return ToolResult{}, err
	}
	if err := os.WriteFile(a.Path, []byte(a.Content), 0o644); err != nil {
		return ToolResult{}, err
	}
	raw := fmt.Sprintf("Wrote %d bytes to %s", len(a.Content), a.Path) + lspDiagnostics(ctx, a.Path)
	return textResult(raw, raw, 0), nil
}

func editTool() Tool {
	return Tool{
		Def: llm.NewTool("edit",
			"Apply one or more observed line-range edits atomically. Each primary edit references a read observation; use mode=exact only for temporary unique old_string compatibility.",
			`{"type":"object","properties":{"mode":{"type":"string","enum":["observed","exact"],"description":"observed is the primary range-authorized mode; exact is compatibility mode"},"edits":{"type":"array","description":"Observed operations to apply atomically across one or more files","items":{"type":"object","properties":{"observation":{"type":"string"},"path":{"type":"string"},"start_line":{"type":"integer"},"end_line":{"type":"integer"},"operation":{"type":"string","enum":["replace","delete","insert_before","insert_after"]},"content":{"type":"string"}},"required":["observation","path","start_line","end_line","operation","content"]}},"path":{"type":"string","description":"Compatibility-mode file path"},"old_string":{"type":"string","description":"Compatibility-mode exact text"},"new_string":{"type":"string","description":"Compatibility-mode replacement"},"replace_all":{"type":"boolean","description":"Compatibility-mode replace every occurrence"}},"required":["mode"]}`),
		Run: func(ctx context.Context, args json.RawMessage) (string, error) {
			result, err := runEditResult(ctx, args)
			return result.Preview, err
		},
		RunResult: runEditResult,
	}
}

func runEditResult(ctx context.Context, args json.RawMessage) (ToolResult, error) {
	return runEdit(ctx, args)
}

// editDiff renders the changed region of an edit as a compact unified-ish
// diff: one line of common context on each side of the first/last changed
// lines, "- old"/"+ new" pairs in between. "" when old and new are identical.
func editDiff(oldS, newS string) string {
	o := strings.Split(strings.TrimSuffix(oldS, "\n"), "\n")
	n := strings.Split(strings.TrimSuffix(newS, "\n"), "\n")
	p := 0
	for p < len(o) && p < len(n) && o[p] == n[p] {
		p++
	}
	s := 0
	for s < len(o)-p && s < len(n)-p && o[len(o)-1-s] == n[len(n)-1-s] {
		s++
	}
	if p == len(o) && p == len(n) {
		return ""
	}
	var b strings.Builder
	ctxLine := func(prefix, line string) {
		if len(line) > 200 {
			line = line[:200] + "…"
		}
		b.WriteString(prefix + line + "\n")
	}
	if p > 0 {
		ctxLine(" ", o[p-1])
	}
	writeCappedDiffLines(&b, "-", o[p:len(o)-s])
	writeCappedDiffLines(&b, "+", n[p:len(n)-s])
	if s > 0 {
		ctxLine(" ", o[len(o)-1])
	}
	return strings.TrimSuffix(b.String(), "\n")
}

const maxEditDiffLines = 40

func writeCappedDiffLines(b *strings.Builder, prefix string, lines []string) {
	if len(lines) <= maxEditDiffLines {
		for _, line := range lines {
			if len(line) > 200 {
				line = line[:200] + "…"
			}
			b.WriteString(prefix + line + "\n")
		}
		return
	}
	head := maxEditDiffLines / 2
	tail := maxEditDiffLines - head
	for _, line := range lines[:head] {
		if len(line) > 200 {
			line = line[:200] + "…"
		}
		b.WriteString(prefix + line + "\n")
	}
	fmt.Fprintf(b, "%s... [%d lines omitted]\n", prefix, len(lines)-maxEditDiffLines)
	for _, line := range lines[len(lines)-tail:] {
		if len(line) > 200 {
			line = line[:200] + "…"
		}
		b.WriteString(prefix + line + "\n")
	}
}

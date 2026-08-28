// Package tools implements the agent's built-in tools.
package tools

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
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
	return []Tool{bashTool(), readTool(), writeTool(), editTool(), grepTool(), globTool()}
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
			"Execute a bash command in the current working directory and return its combined stdout/stderr. Use for running programs, git, searching (grep/rg), listing files, etc.",
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
		return textResult(out, TruncateTail(out), 0), nil
	}

	var update func(string)
	if fn := onUpdate(ctx); fn != nil {
		update = func(snapshot string) { fn(TruncateTail(snapshot)) }
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
		capturedResult(full, TruncateTail(full), originalBytes, complete, boolToExitCode(res.Exit == "" && !res.TimedOut)),
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
			"Read a file and return its contents with line numbers.",
			`{"type":"object","properties":{"path":{"type":"string","description":"Path to the file"},"offset":{"type":"number","description":"1-based line to start from"},"limit":{"type":"number","description":"Max lines to return (default 2000)"}},"required":["path"]}`),
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
	if err := ctx.Err(); err != nil {
		return ToolResult{}, err
	}
	f, err := os.Open(a.Path)
	if err != nil {
		return ToolResult{}, err
	}
	defer func() { _ = f.Close() }()
	start := max(a.Offset-1, 0)
	limit := a.Limit
	if limit <= 0 {
		limit = 2000
	}
	reader := bufio.NewReaderSize(f, 64<<10)
	capture := NewTextCapture(maxArtifactBytes)
	lineNo := 0
	selected := 0
	for {
		if err := ctx.Err(); err != nil {
			return ToolResult{}, err
		}
		want := lineNo >= start && selected < limit
		first := true
		done := false
		for {
			chunk, readErr := reader.ReadSlice('\n')
			if want {
				if first {
					capture.WriteString(fmt.Sprintf("%d\t", lineNo+1))
					first = false
				}
				capture.WriteString(string(chunk))
			}
			switch readErr {
			case nil:
				// The newline terminates this line.
			case bufio.ErrBufferFull:
				// Keep consuming a pathological line in bounded chunks; the
				// capture, not the reader, owns the hard retention ceiling.
				continue
			case io.EOF:
				if len(chunk) == 0 {
					done = true // empty file or the byte after a final newline
					break
				}
				// The final unterminated fragment is still one line.
				if want {
					selected++
				}
				lineNo++
				done = true
			default:
				return ToolResult{}, fmt.Errorf("read %s: %w", a.Path, readErr)
			}
			if done || readErr == nil {
				break
			}
		}
		if done {
			break
		}
		if want {
			selected++
		}
		lineNo++
		if selected >= limit {
			break
		}
	}
	if lineNo <= start {
		return ToolResult{}, fmt.Errorf("offset %d past end of file (%d lines)", a.Offset, lineNo)
	}
	raw := capture.String()
	return MarkUntrusted(
		capturedResult(raw, truncate(raw), capture.total, !capture.truncated, 0),
		"read",
	), nil
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
			"Replace an exact string in a file. old_string must appear exactly once unless replace_all is true.",
			`{"type":"object","properties":{"path":{"type":"string","description":"Path to the file"},"old_string":{"type":"string","description":"Exact text to replace"},"new_string":{"type":"string","description":"Replacement text"},"replace_all":{"type":"boolean","description":"Replace every occurrence"}},"required":["path","old_string","new_string"]}`),
		Run: func(ctx context.Context, args json.RawMessage) (string, error) {
			result, err := runEditResult(ctx, args)
			return result.Preview, err
		},
		RunResult: runEditResult,
	}
}

func runEditResult(ctx context.Context, args json.RawMessage) (ToolResult, error) {
	var a struct {
		Path       string `json:"path"`
		OldString  string `json:"old_string"`
		NewString  string `json:"new_string"`
		ReplaceAll bool   `json:"replace_all"`
	}
	if err := json.Unmarshal(args, &a); err != nil {
		return ToolResult{}, err
	}
	if deny := checkGate("edit", a.Path); deny != "" {
		return ToolResult{}, errors.New(deny)
	}
	data, err := os.ReadFile(a.Path)
	if err != nil {
		return ToolResult{}, err
	}
	s := string(data)
	n := strings.Count(s, a.OldString)
	switch {
	case n == 0:
		return ToolResult{}, fmt.Errorf("old_string not found in %s", a.Path)
	case n > 1 && !a.ReplaceAll:
		return ToolResult{}, fmt.Errorf("old_string appears %d times in %s; make it unique or set replace_all", n, a.Path)
	}
	s = strings.ReplaceAll(s, a.OldString, a.NewString)
	if err := os.WriteFile(a.Path, []byte(s), 0o644); err != nil {
		return ToolResult{}, err
	}
	out := fmt.Sprintf("Replaced %d occurrence(s) in %s", n, a.Path)
	if d := editDiff(a.OldString, a.NewString); d != "" {
		out += "\n```diff\n" + d + "\n```"
	}
	raw := out + lspDiagnostics(ctx, a.Path)
	return textResult(raw, truncate(raw), 0), nil
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
	for _, l := range o[p : len(o)-s] {
		ctxLine("-", l)
	}
	for _, l := range n[p : len(n)-s] {
		ctxLine("+", l)
	}
	if s > 0 {
		ctxLine(" ", o[len(o)-1])
	}
	return strings.TrimSuffix(b.String(), "\n")
}

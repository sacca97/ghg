package tools

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/sacca97/ghg/internal/models"
	"github.com/sacca97/ghg/internal/sandbox"
	"github.com/sacca97/ghg/internal/tools/bashrun"
)

// InteractiveRunner runs a bash command with live PTY input.
type InteractiveRunner interface {
	Run(ctx context.Context, command string, timeout time.Duration, keys <-chan []byte) string
}

func bashTool() Tool {
	return resultTool(models.NewTool("bash",
		"Execute a bash command in the current working directory and return its combined stdout/stderr. Use for builds, tests, git, and operations the dedicated read/search/edit tools cannot express. Prefer grep, glob, find_files, and bounded read for exploration; simple recursive inspection commands may be redirected.",
		`{"type":"object","properties":{"command":{"type":"string","description":"The bash command to execute"},"timeout":{"type":"number","description":"Timeout in seconds (default 120)"},"interactive":{"type":"boolean","description":"Run in a PTY so sudo/ssh-style password prompts work. ghg stays in control of the terminal and forwards your keystrokes; the command is killed after 15s of no input. Use only for commands that genuinely need a password."}},"required":["command"]}`),
		runBashResult)
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
	if redirect, ok := redirectBashInspection(a.Command); ok {
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
	runtime := RuntimeFromContext(ctx)
	var executionPolicy *sandbox.Policy
	approvalCovered := false
	if runtime != nil {
		var err error
		executionPolicy, approvalCovered, err = runtime.authorizeCommand(ctx, "bash", a.Command, ".")
		if err != nil {
			return ToolResult{}, err
		}
	}
	if !approvalCovered {
		if deny := checkGate(ctx, "bash", a.Command); deny != "" {
			return ToolResult{}, errors.New(deny)
		}
	}
	dur := time.Duration(a.Timeout * float64(time.Second))
	commandCtx := ctx
	if runtime != nil && executionPolicy != nil {
		commandCtx = WithRuntime(ctx, runtime.WithPolicy(executionPolicy))
	}
	var runner InteractiveRunner
	if runtime != nil && runtime.InteractiveRunner != nil {
		runner = runtime.InteractiveRunner
	}
	if a.Interactive && runner != nil {
		keys := make(chan []byte, 16)
		out := runner.Run(commandCtx, a.Command, dur, keys)
		return MarkUntrusted(TextResultWithSize(out, boundedTailPreview(out, bashPreviewLimit(a.Command)), int64(len(out)), true, 0), "bash"), nil
	}

	var update func(string)
	if fn := onUpdate(commandCtx); fn != nil {
		update = func(snapshot string) { fn(boundedTailPreview(snapshot, bashPreviewLimit(a.Command))) }
	}
	opts := bashrun.Options{Command: a.Command, Timeout: dur, OnUpdate: update}
	if runtime != nil && executionPolicy != nil {
		opts.Env = runtime.ChildEnv(nil)
		opts.Sandbox = executionPolicy
	}
	res := bashrun.Run(commandCtx, opts)

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
	ret := MarkUntrusted(
		capturedResult(full, boundedTailPreview(full, bashPreviewLimit(a.Command)), originalBytes, complete, boolToExitCode(res.Exit == "" && !res.TimedOut)),
		"bash",
	)
	if opts.Sandbox != nil && (res.Exit != "" || res.TimedOut) && isSandboxNetworkDenied(full) {
		if ret.Metadata == nil {
			ret.Metadata = make(map[string]string)
		}
		ret.Metadata["failure_kind"] = "sandbox_network_denied"
	}
	return ret, nil
}

func isSandboxNetworkDenied(output string) bool {
	lower := strings.ToLower(output)
	if strings.Contains(lower, "httptest: failed to listen") {
		return true
	}
	return strings.Contains(lower, "listen tcp") && strings.Contains(lower, "operation not permitted")
}

func boolToExitCode(success bool) int {
	if success {
		return 0
	}
	return 1
}

func bashPreviewLimit(command string) int {
	if isBashExploration(command) {
		return 8 << 10
	}
	return 14 << 10
}

func isInspectionBinary(bin string) bool {
	switch filepath.Base(bin) {
	case "grep", "rg", "find", "ls", "cat", "sed", "head", "tail", "wc", "cd", "pwd", "echo", "tree", "sort", "uniq", "cut", "tr", "jq", "awk":
		return true
	default:
		return false
	}
}

func isBashExploration(command string) bool {
	command = strings.TrimSpace(command)
	if command == "" {
		return false
	}
	if tokens, ok := simpleCommandTokens(command); ok {
		return isInspectionBinary(tokens[0])
	}
	segments, err := SegmentShell(command)
	if err != nil || len(segments) == 0 {
		return false
	}
	for _, seg := range segments {
		op := strings.TrimSpace(seg.Operator)
		if op != "" && op != ";" && op != "&&" && op != "||" && op != "|" {
			return false
		}
		argv := unwrapTransparent(seg.Argv)
		if len(argv) == 0 || !isInspectionBinary(argv[0]) {
			return false
		}
	}
	return true
}

func boundedTailPreview(s string, limit int) string {
	if limit <= 0 || len(s) <= limit {
		return s
	}
	marker := []byte("[... first " + strconv.Itoa(len(s)-limit) + " bytes truncated]\n")
	if len(marker) >= limit {
		return string([]byte(s)[len(s)-limit:])
	}
	return string(marker) + s[len(s)-(limit-len(marker)):]
}

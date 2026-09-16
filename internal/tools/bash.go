package tools

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/sacca97/ghg/internal/models"
	"github.com/sacca97/ghg/internal/sandbox"
	"github.com/sacca97/ghg/internal/session"
)

func bashTool() Tool {
	return resultTool(models.NewTool("bash",
		"Execute a bash command in the current working directory and return its combined stdout/stderr. Use for builds, tests, git, and operations the dedicated read/search/edit tools cannot express. Prefer grep, glob, find_files, and bounded read for exploration; simple recursive inspection commands may be redirected.",
		`{"type":"object","properties":{"command":{"type":"string","description":"The bash command to execute"},"timeout":{"type":"number","description":"Timeout in seconds (default 120)"}},"required":["command"]}`),
		runBashResult)
}

func runBashResult(ctx context.Context, args json.RawMessage) (ToolResult, error) {
	var a struct {
		Command string  `json:"command"`
		Timeout float64 `json:"timeout"`
	}
	if err := json.Unmarshal(args, &a); err != nil {
		return ToolResult{}, err
	}
	segments, parseErr := SegmentShell(a.Command)
	if parseErr == nil {
		if redirect, ok := redirectBashInspectionSegments(a.Command, segments); ok {
			return runBashRedirect(ctx, redirect), nil
		}
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
	dur := defaultBashTimeout
	if a.Timeout > 0 {
		dur = time.Duration(a.Timeout * float64(time.Second))
	}
	commandCtx := ctx
	if runtime != nil && executionPolicy != nil {
		commandCtx = WithRuntime(ctx, runtime.WithPolicy(executionPolicy))
	}

	var update func(string)
	previewLimit := bashPreviewLimitSegments(segments)
	if fn := onUpdate(commandCtx); fn != nil {
		update = func(snapshot string) { fn(TruncateTailWithLimit(snapshot, previewLimit)) }
	}
	opts := bashOptions{Command: a.Command, Timeout: dur, OnUpdate: update}
	if runtime != nil && executionPolicy != nil {
		opts.Env = runtime.ChildEnv(nil)
		opts.Sandbox = executionPolicy
	}
	res := runBashCommand(commandCtx, opts)
	return bashToolResult(segments, res, opts.Sandbox), nil
}

func runBashRedirect(ctx context.Context, redirect bashRedirect) ToolResult {
	var (
		result ToolResult
		err    error
	)
	switch redirect.Tool {
	case "read":
		result, err = runReadResult(ctx, redirect.Args)
	case "grep":
		result, err = runGrepResult(ctx, redirect.Args)
	case "glob":
		result, err = runGlobResult(ctx, redirect.Args)
	default:
		err = fmt.Errorf("unsupported bash redirect tool %q", redirect.Tool)
	}
	if err != nil {
		result = errorToolResult(err)
	}
	result.Source = "bash"
	if result.Metadata == nil {
		result.Metadata = make(map[string]string)
	}
	result.Metadata["source"] = "bash"
	result.Metadata["bash_redirect"] = "true"
	result.Metadata["redirect_tool"] = redirect.Tool
	result.Metadata["redirect_command"] = redirect.Command
	return result
}

func bashToolResult(segments []CommandSegment, res bashResult, sandboxPolicy *sandbox.Policy) ToolResult {
	full := res.Output
	originalBytes := res.OriginalBytes
	complete := res.Complete
	noMatch := expectedSearchNoMatch(segments, res)
	exit := res.Exit
	if noMatch {
		marker := "grep: (no matches)"
		if full != "" {
			marker = "\n" + marker
		}
		full += marker
		originalBytes += int64(len(marker))
		exit = ""
	}
	if res.TimedOut {
		marker := "\n(command timed out)"
		full += marker
		originalBytes += int64(len(marker))
		complete = false
	}
	if exit != "" {
		marker := fmt.Sprintf("\n(%s)", exit)
		full += marker
		originalBytes += int64(len(marker))
	}
	exitCode := 0
	if exit != "" || res.TimedOut {
		exitCode = 1
	}
	ret := MarkUntrusted(capturedResult(full, TruncateTailWithLimit(full, bashPreviewLimitSegments(segments)), originalBytes, complete, exitCode), "bash")
	if sandboxPolicy != nil && (res.Exit != "" || res.TimedOut) && isSandboxNetworkDenied(full) {
		if ret.Metadata == nil {
			ret.Metadata = make(map[string]string)
		}
		ret.Metadata["failure_kind"] = "sandbox_network_denied"
	}
	return ret
}

func expectedSearchNoMatch(segments []CommandSegment, res bashResult) bool {
	if !res.Started || res.TimedOut || res.Killed || res.ExitCode != 1 {
		return false
	}
	foundSearch := false
	for _, segment := range segments {
		argv := unwrapTransparent(segment.Argv)
		if len(argv) == 0 || !isInspectionCommand(argv) {
			return false
		}
		base := filepath.Base(argv[0])
		if base == "grep" || base == "rg" {
			foundSearch = true
		}
	}
	return foundSearch
}

func isSandboxNetworkDenied(output string) bool {
	lower := strings.ToLower(output)
	return strings.Contains(lower, "listen tcp") && strings.Contains(lower, "operation not permitted")
}

func bashPreviewLimitSegments(segments []CommandSegment) int {
	if isBashExplorationSegments(segments) {
		return 8 << 10
	}
	return 14 << 10
}

func isInspectionCommand(argv []string) bool {
	switch filepath.Base(argv[0]) {
	case "grep", "rg", "find", "ls", "cat", "sed", "head", "tail", "wc", "cd", "pwd", "echo", "tree", "sort", "uniq", "cut", "tr", "jq", "awk":
		return true
	default:
		return false
	}
}

func isBashExplorationSegments(segments []CommandSegment) bool {
	if len(segments) == 0 {
		return false
	}
	for _, seg := range segments {
		op := strings.TrimSpace(seg.Operator)
		if op != "" && op != ";" && op != "&&" && op != "||" && op != "|" {
			return false
		}
		argv := unwrapTransparent(seg.Argv)
		if len(argv) == 0 || !isInspectionCommand(argv) {
			return false
		}
	}
	return true
}

type bashRedirect struct {
	Tool    string
	Args    json.RawMessage
	Command string
}

func redirectBashInspectionSegments(command string, segments []CommandSegment) (bashRedirect, bool) {
	if len(segments) != 1 || segments[0].Operator != "" {
		return bashRedirect{}, false
	}
	tokens := segments[0].Argv
	if len(tokens) == 0 {
		return bashRedirect{}, false
	}
	switch tokens[0] {
	case "cat":
		if len(tokens) == 2 || len(tokens) == 3 && tokens[1] == "--" {
			path := tokens[len(tokens)-1]
			if literalInspectionPath(path) {
				return bashReadRedirect(tokens[0], path, 1, 1)
			}
		}
	case "head":
		if len(tokens) == 2 && literalInspectionPath(tokens[1]) {
			return bashReadRedirect(tokens[0], tokens[1], 1, defaultReadLines)
		}
		if len(tokens) == 4 && tokens[1] == "-n" {
			if limit, ok := positiveInt(tokens[2]); ok && literalInspectionPath(tokens[3]) {
				return bashReadRedirect(tokens[0], tokens[3], 1, limit)
			}
		}
		if len(tokens) == 3 && strings.HasPrefix(tokens[1], "-") {
			if limit, ok := positiveInt(strings.TrimPrefix(tokens[1], "-")); ok && literalInspectionPath(tokens[2]) {
				return bashReadRedirect(tokens[0], tokens[2], 1, limit)
			}
		}
	case "sed":
		if len(tokens) == 4 && tokens[1] == "-n" {
			if start, end, ok := sedRange(tokens[2]); ok && literalInspectionPath(tokens[3]) {
				return bashReadRedirect(tokens[0], tokens[3], start, end-start+1)
			}
		}
	case "grep", "rg":
		if redirect, ok := searchRedirect(tokens); ok {
			return redirect, true
		}
	case "find":
		if len(tokens) == 4 && tokens[2] == "-name" && literalInspectionPath(tokens[1]) && tokens[3] != "" {
			pattern := tokens[3]
			if !strings.Contains(pattern, "/") {
				pattern = "**/" + pattern
			}
			return bashGlobRedirect(tokens[0], tokens[1], pattern)
		}
	}
	return bashRedirect{}, false
}

func bashReadRedirect(command, path string, offset, limit int) (bashRedirect, bool) {
	args, err := json.Marshal(map[string]any{"path": path, "offset": offset, "limit": limit})
	if err != nil {
		return bashRedirect{}, false
	}
	return bashRedirect{Tool: "read", Args: args, Command: command}, true
}

func bashGlobRedirect(command, path, pattern string) (bashRedirect, bool) {
	args, err := json.Marshal(map[string]string{"path": path, "pattern": pattern})
	if err != nil {
		return bashRedirect{}, false
	}
	return bashRedirect{Tool: "glob", Args: args, Command: command}, true
}

func searchRedirect(tokens []string) (bashRedirect, bool) {
	index := 1
	if index < len(tokens) && tokens[index] == "--" {
		index++
	}
	if index < len(tokens) && (tokens[index] == "-r" || tokens[index] == "-R" || tokens[index] == "-rn") {
		index++
	}
	if len(tokens)-index < 1 || len(tokens)-index > 2 || strings.HasPrefix(tokens[index], "-") {
		return bashRedirect{}, false
	}
	args := map[string]string{"pattern": tokens[index]}
	if len(tokens)-index == 2 {
		if !literalInspectionPath(tokens[index+1]) {
			return bashRedirect{}, false
		}
		args["path"] = tokens[index+1]
	}
	data, err := json.Marshal(args)
	if err != nil {
		return bashRedirect{}, false
	}
	return bashRedirect{Tool: "grep", Args: data, Command: tokens[0]}, true
}

func positiveInt(value string) (int, bool) {
	parsed, err := strconv.Atoi(value)
	return parsed, err == nil && parsed > 0
}

func literalInspectionPath(value string) bool {
	return value != "" && !strings.HasPrefix(value, "-") && !strings.ContainsAny(value, "*?[")
}

func sedRange(value string) (start, end int, ok bool) {
	if !strings.HasSuffix(value, "p") {
		return 0, 0, false
	}
	parts := strings.Split(strings.TrimSuffix(value, "p"), ",")
	if len(parts) != 2 {
		return 0, 0, false
	}
	start, ok = positiveInt(parts[0])
	if !ok {
		return 0, 0, false
	}
	end, ok = positiveInt(parts[1])
	return start, end, ok && end >= start
}

const defaultBashTimeout = 120 * time.Second

type bashOptions struct {
	Command  string
	Timeout  time.Duration
	Env      []string
	Sandbox  *sandbox.Policy
	OnUpdate func(string)
}

type bashResult struct {
	Output        string
	Exit          string
	ExitCode      int
	Started       bool
	TimedOut      bool
	Killed        bool
	OriginalBytes int64
	Complete      bool
}

func userShell() string {
	if sh := os.Getenv("SHELL"); sh != "" {
		return sh
	}
	if sh := passwdShell(); sh != "" {
		return sh
	}
	return "bash"
}

func bashAvailable() bool {
	_, err := exec.LookPath(userShell())
	return err == nil
}

func passwdShell() string {
	u, err := user.Current()
	if err != nil {
		return ""
	}
	data, err := os.ReadFile("/etc/passwd")
	if err != nil {
		return ""
	}
	for line := range strings.Lines(string(data)) {
		fields := strings.Split(strings.TrimRight(line, "\n"), ":")
		if len(fields) == 7 && fields[2] == u.Uid {
			return fields[6]
		}
	}
	return ""
}

func runBashCommand(ctx context.Context, opts bashOptions) bashResult {
	if opts.Timeout <= 0 {
		opts.Timeout = defaultBashTimeout
	}
	ctx, cancel := context.WithTimeout(ctx, opts.Timeout)
	defer cancel()

	program := userShell()
	args := []string{"-c", opts.Command}
	switch filepath.Base(program) {
	case "bash", "zsh", "ksh":
		args = []string{"-o", "pipefail", "-c", opts.Command}
	}
	var dir string
	if opts.Sandbox != nil {
		wrapped, err := opts.Sandbox.WrapCommand(sandbox.CommandSpec{
			Program: program,
			Args:    args,
			Env:     opts.Env,
		})
		if err != nil {
			return bashResult{Exit: "sandbox: " + err.Error(), ExitCode: 1}
		}
		program, args, dir = wrapped.Program, wrapped.Args, wrapped.Dir
	}
	cmd := exec.CommandContext(ctx, program, args...)
	if dir != "" {
		cmd.Dir = dir
	}
	if opts.Env != nil {
		cmd.Env = append(append([]string(nil), opts.Env...), "GHG=1")
	} else {
		cmd.Env = append(os.Environ(), "GHG=1")
	}

	return runPipedBash(ctx, cmd, opts)
}

func runPipedBash(ctx context.Context, cmd *exec.Cmd, opts bashOptions) bashResult {
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return bashResult{Exit: "pipe: " + err.Error(), ExitCode: 1}
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		return bashResult{Exit: "pipe: " + err.Error(), ExitCode: 1}
	}
	devNull, err := os.OpenFile(os.DevNull, os.O_RDWR, 0)
	if err == nil {
		cmd.Stdin = devNull
		defer devNull.Close()
	}
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}

	if err := cmd.Start(); err != nil {
		return bashResult{Exit: exitString(err), ExitCode: 1}
	}

	out := NewOutputCapture(session.DefaultMaxBytes, opts.OnUpdate != nil)
	var mu sync.Mutex
	var wg sync.WaitGroup
	wg.Add(2)
	drain := func(r io.Reader) {
		defer wg.Done()
		buf := make([]byte, 4096)
		for {
			n, rerr := r.Read(buf)
			if n > 0 {
				mu.Lock()
				_, _ = out.Write(buf[:n])
				mu.Unlock()
			}
			if rerr != nil {
				return
			}
		}
	}
	go drain(stdout)
	go drain(stderr)

	var updateWG sync.WaitGroup
	var updateDone chan struct{}
	if opts.OnUpdate != nil {
		updateDone = make(chan struct{})
		updateWG.Add(1)
		go func() {
			defer updateWG.Done()
			ticker := time.NewTicker(100 * time.Millisecond)
			defer ticker.Stop()
			last := ""
			publish := func() {
				mu.Lock()
				snapshot := out.Preview(14 << 10)
				mu.Unlock()
				if snapshot == "" || snapshot == last {
					return
				}
				last = snapshot
				opts.OnUpdate(snapshot)
			}
			for {
				select {
				case <-ticker.C:
					publish()
				case <-updateDone:
					publish()
					return
				}
			}
		}()
	}

	watchDone := make(chan struct{})
	defer close(watchDone)
	go func() {
		select {
		case <-ctx.Done():
			if cmd.Process != nil {
				_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
			}
		case <-watchDone:
		}
	}()

	waitErr := cmd.Wait()
	_ = stdout.Close()
	_ = stderr.Close()
	wg.Wait()
	if updateDone != nil {
		close(updateDone)
		updateWG.Wait()
	}

	mu.Lock()
	result := bashResult{
		Output:        out.String(),
		OriginalBytes: out.OriginalBytes(),
		Complete:      out.Complete(),
		ExitCode:      exitCode(waitErr),
		Started:       true,
	}
	mu.Unlock()
	return finalizeBashResult(ctx, result, waitErr)
}

func exitString(err error) string {
	if err == nil {
		return ""
	}
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) {
		return fmt.Sprintf("(exit: %s)", exitErr)
	}
	return fmt.Sprintf("(exit: %v)", err)
}

func exitCode(err error) int {
	if err == nil {
		return 0
	}
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) && exitErr.ExitCode() > 0 {
		return exitErr.ExitCode()
	}
	return 1
}

func finalizeBashResult(ctx context.Context, result bashResult, waitErr error) bashResult {
	if ctx.Err() == context.DeadlineExceeded {
		result.TimedOut = true
		result.Killed = true
		result.Exit = "timed out"
		return result
	}
	if ctx.Err() == context.Canceled {
		result.Killed = true
		result.Exit = "cancelled"
		return result
	}
	result.Exit = exitString(waitErr)
	if waitErr != nil {
		result.Killed = isKilledBySignal(waitErr)
	}
	return result
}

func isKilledBySignal(err error) bool {
	var exitErr *exec.ExitError
	if !errors.As(err, &exitErr) {
		return false
	}
	ws, ok := exitErr.Sys().(syscall.WaitStatus)
	return ok && ws.Signaled()
}

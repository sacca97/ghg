package tools

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/sacca97/ghg/internal/sandbox"
	"github.com/sacca97/ghg/internal/search"
)

var rgPathLookedUp bool
var rgBinaryPath string

func rgAvailable() (string, bool) {
	if !rgPathLookedUp {
		rgBinaryPath, _ = exec.LookPath("rg")
		rgPathLookedUp = true
	}
	return rgBinaryPath, rgBinaryPath != ""
}

type rgMatchEvent struct {
	Type string `json:"type"`
	Data struct {
		Path struct {
			Text string `json:"text"`
		} `json:"path"`
		Lines struct {
			Text string `json:"text"`
		} `json:"lines"`
		LineNumber int `json:"line_number"`
	} `json:"data"`
}

func grepSnapshotRG(ctx context.Context, args grepArgs, scope *searchScope, out *searchCollector) error {
	bin, ok := rgAvailable()
	if !ok {
		return errors.New("rg not available")
	}

	cmdArgs := []string{
		"--json",
		"--line-number",
		"--color=never",
		"--hidden",
		"--no-follow",
		"--no-require-git",
		"--glob", "!.git/*",
	}

	if args.CaseSensitive != nil && !*args.CaseSensitive {
		cmdArgs = append(cmdArgs, "--ignore-case")
	}
	if args.Literal {
		cmdArgs = append(cmdArgs, "--fixed-strings")
	}

	patterns := append([]string(nil), args.Patterns...)
	if len(patterns) == 0 && args.Pattern != "" {
		patterns = []string{args.Pattern}
	}
	for _, p := range patterns {
		cmdArgs = append(cmdArgs, "-e", p)
	}

	if args.Include != "" {
		includePattern := args.Include
		if !strings.HasPrefix(includePattern, "*") && !strings.Contains(includePattern, "/") {
			includePattern = "**/" + includePattern
		}
		cmdArgs = append(cmdArgs, "--glob", includePattern)
	}

	targetPath := scope.rootPath
	if scope.single {
		targetPath = filepath.Join(scope.rootPath, scope.start)
	}
	cmdArgs = append(cmdArgs, "--", targetPath)

	prog := bin
	finalArgs := cmdArgs
	dir := scope.cwdPath

	if runtime := RuntimeFromContext(ctx); runtime != nil && runtime.Policy != nil {
		wrapped, err := runtime.WrapCommand(sandbox.CommandSpec{
			Program: bin,
			Args:    cmdArgs,
			Dir:     dir,
			Env:     runtime.ChildEnv(nil),
		})
		if err != nil {
			return err
		}
		prog = wrapped.Program
		finalArgs = wrapped.Args
		dir = wrapped.Dir
	}

	cmd := exec.CommandContext(ctx, prog, finalArgs...)
	cmd.Dir = dir

	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return fmt.Errorf("rg stdout pipe: %w", err)
	}
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("rg start: %w", err)
	}

	scanner := bufio.NewScanner(stdout)
	scanner.Buffer(make([]byte, 64<<10), 1<<20)

	var scanErr error
	for scanner.Scan() {
		lineBytes := scanner.Bytes()
		if len(lineBytes) == 0 || lineBytes[0] != '{' {
			continue
		}
		var ev rgMatchEvent
		if err := json.Unmarshal(lineBytes, &ev); err != nil {
			continue
		}
		if ev.Type != "match" {
			continue
		}

		filePath := ev.Data.Path.Text
		display := filePath
		if rel, ok := relativePath(scope.cwdPath, filePath); ok {
			display = rel
		}
		display = filepath.ToSlash(display)

		text := strings.TrimSuffix(ev.Data.Lines.Text, "\n")
		text = strings.TrimSuffix(text, "\r")
		if len(text) > maxMatchLineBytes {
			text = text[:maxMatchLineBytes] + "… [line truncated]"
		}

		if addErr := out.add(ctx, search.Item{
			Path: display,
			Line: ev.Data.LineNumber,
			Text: text,
		}); addErr != nil {
			if errors.Is(addErr, errSearchLimit) {
				_ = cmd.Process.Kill()
				_ = cmd.Wait()
				return nil
			}
			scanErr = addErr
			break
		}
	}

	if err := scanner.Err(); err != nil && scanErr == nil {
		scanErr = err
	}

	waitErr := cmd.Wait()
	if scanErr != nil {
		return scanErr
	}
	if waitErr != nil {
		var exitErr *exec.ExitError
		if errors.As(waitErr, &exitErr) {
			// rg returns exit code 1 on 0 matches, 2 on syntax/error
			if exitErr.ExitCode() == 1 {
				return nil
			}
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return fmt.Errorf("rg: %w", waitErr)
	}

	return nil
}

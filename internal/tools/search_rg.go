package tools

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"

	"github.com/sacca97/ghg/internal/sandbox"
)

var rgPath = sync.OnceValue(func() string {
	path, err := exec.LookPath("rg")
	if err != nil {
		return ""
	}
	return path
})

func rgAvailable() (string, bool) {
	path := rgPath()
	return path, path != ""
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

func grepSnapshotRG(ctx context.Context, args grepArgs, scope *searchScope, matcher *grepMatcher, include *searchPattern, out *searchCollector) error {
	_, ok := rgAvailable()
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
		"--glob", "!.git",
	}

	if args.CaseSensitive != nil && !*args.CaseSensitive {
		cmdArgs = append(cmdArgs, "--ignore-case")
	}
	if args.Literal {
		cmdArgs = append(cmdArgs, "--fixed-strings")
	}

	for _, p := range matcher.patterns {
		cmdArgs = append(cmdArgs, "-e", p)
	}

	targetPath := searchTargetPath(scope)
	cmdArgs = append(cmdArgs, "--", targetPath)

	cmd, err := newRGCommand(ctx, scope, cmdArgs)
	if err != nil {
		return err
	}

	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return fmt.Errorf("rg stdout pipe: %w", err)
	}
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("rg start: %w", err)
	}
	stop := func() {
		if cmd.Process != nil {
			_ = cmd.Process.Kill()
		}
	}

	scanner := bufio.NewScanner(stdout)
	scanner.Buffer(make([]byte, 64<<10), 1<<20)

	var scanErr error
	for scanner.Scan() {
		lineBytes := scanner.Bytes()
		if !bytes.HasPrefix(lineBytes, []byte(`{"type":"match"`)) {
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
		rel, relErr := rgRelativePath(scope, filePath)
		if include != nil && (relErr != nil || !include.matches(scope.matchPath(rel))) {
			continue
		}
		display := filePath
		if rel, ok := relativePath(scope.cwdPath, filePath); ok {
			display = rel
		}
		display = filepath.ToSlash(display)

		matchText := strings.TrimSuffix(ev.Data.Lines.Text, "\n")
		matchText = strings.TrimSuffix(matchText, "\r")
		text := truncateMatchText(matchText, false)

		patterns := singlePatternMatch
		if len(matcher.regexes) > 1 {
			patterns = matcher.matches([]byte(matchText))
		}
		if addErr := appendGrepMatches(ctx, out, display, ev.Data.LineNumber, text, matcher, patterns); addErr != nil {
			stop()
			scanErr = addErr
		}
		if scanErr != nil {
			break
		}
	}

	if err := scanner.Err(); err != nil && scanErr == nil {
		scanErr = err
		stop()
	}

	waitErr := cmd.Wait()
	if scanErr != nil {
		if errors.Is(scanErr, errSearchLimit) {
			return nil
		}
		if errors.Is(scanErr, context.Canceled) || errors.Is(scanErr, context.DeadlineExceeded) {
			return scanErr
		}
		if ctxErr := ctx.Err(); ctxErr != nil {
			return ctxErr
		}
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

func listFilesRG(ctx context.Context, scope *searchScope, visit func(string) error) error {
	_, ok := rgAvailable()
	if !ok {
		return errors.New("rg not available")
	}
	args := []string{
		"--files",
		"--null",
		"--hidden",
		"--no-follow",
		"--no-require-git",
		"--glob", "!.git",
		"--sort", "path",
	}
	args = append(args, "--", searchTargetPath(scope))
	cmd, err := newRGCommand(ctx, scope, args)
	if err != nil {
		return err
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return fmt.Errorf("rg stdout pipe: %w", err)
	}
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("rg start: %w", err)
	}
	stop := func() {
		if cmd.Process != nil {
			_ = cmd.Process.Kill()
		}
	}
	reader := bufio.NewReaderSize(stdout, 64<<10)
	var visitErr error
	entries := 0
	for {
		name, readErr := reader.ReadString('\x00')
		if len(name) > 0 {
			name = strings.TrimSuffix(name, "\x00")
			if visitErr == nil {
				entries++
				if entries > maxSearchEntries {
					visitErr = errSearchLimit
				} else {
					visitErr = visit(name)
				}
				if visitErr != nil {
					stop()
				}
			}
		}
		if readErr != nil {
			if !errors.Is(readErr, io.EOF) && visitErr == nil {
				visitErr = readErr
				stop()
			}
			break
		}
		if visitErr != nil {
			break
		}
	}
	waitErr := cmd.Wait()
	if visitErr != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return ctxErr
		}
		return visitErr
	}
	if waitErr != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return fmt.Errorf("rg: %w", waitErr)
	}
	return nil
}

func newRGCommand(ctx context.Context, scope *searchScope, args []string) (*exec.Cmd, error) {
	bin, ok := rgAvailable()
	if !ok {
		return nil, errors.New("rg not available")
	}
	prog := bin
	finalArgs := args
	dir := scope.cwdPath
	var env []string
	if runtime := RuntimeFromContext(ctx); runtime != nil && runtime.Policy != nil {
		wrapped, err := runtime.WrapCommand(sandbox.CommandSpec{
			Program: bin,
			Args:    args,
			Dir:     dir,
			Env:     runtime.ChildEnv(nil),
		})
		if err != nil {
			return nil, err
		}
		prog = wrapped.Program
		finalArgs = wrapped.Args
		dir = wrapped.Dir
		env = wrapped.Env
	}
	cmd := exec.CommandContext(ctx, prog, finalArgs...)
	cmd.Dir = dir
	if env != nil {
		cmd.Env = env
	}
	return cmd, nil
}

func searchTargetPath(scope *searchScope) string {
	if scope.start == "." {
		return scope.rootPath
	}
	return filepath.Join(scope.rootPath, filepath.FromSlash(scope.start))
}

func rgRelativePath(scope *searchScope, name string) (string, error) {
	name = filepath.FromSlash(name)
	if !filepath.IsAbs(name) {
		name = filepath.Join(scope.rootPath, name)
	}
	rel, ok := relativePath(scope.rootPath, name)
	if !ok || rel == "." {
		return "", fmt.Errorf("rg returned path outside search scope: %q", name)
	}
	return filepath.ToSlash(rel), nil
}

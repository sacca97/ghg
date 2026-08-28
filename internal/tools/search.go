package tools

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/sacca97/ghg/internal/llm"
)

const (
	defaultSearchMaxResults = 1000
	maxSearchResults        = 10000
	maxSearchEntries        = 100000
	maxBinaryProbeBytes     = 8 << 10
	maxSearchLineBytes      = 1 << 20
	maxMatchLineBytes       = 4 << 10
	maxSearchPatternBytes   = 16 << 10
)

var errSearchLimit = errors.New("search limit reached")

type grepArgs struct {
	Pattern       string `json:"pattern"`
	Path          string `json:"path"`
	Include       string `json:"include"`
	MaxResults    int    `json:"max_results"`
	CaseSensitive *bool  `json:"case_sensitive"`
	Literal       bool   `json:"literal"`
}

type globArgs struct {
	Pattern    string `json:"pattern"`
	Path       string `json:"path"`
	MaxResults int    `json:"max_results"`
}

func grepTool() Tool {
	return Tool{
		Def: llm.NewTool("grep",
			"Search text files for a regular expression and return path, line number, and matching line. Respects nested .gitignore files and skips binary files and symlinks.",
			`{"type":"object","properties":{"pattern":{"type":"string","description":"Regular expression to search for"},"path":{"type":"string","description":"File or directory to search (default: current working directory)"},"include":{"type":"string","description":"Optional glob filter such as *.go"},"max_results":{"type":"integer","description":"Maximum matches to return (default 1000, maximum 10000)"},"case_sensitive":{"type":"boolean","description":"Whether the regular expression is case-sensitive (default true)"},"literal":{"type":"boolean","description":"Treat pattern as literal text instead of a regular expression"}},"required":["pattern"]}`),
		Run: func(ctx context.Context, args json.RawMessage) (string, error) {
			result, err := runGrepResult(ctx, args)
			return result.Preview, err
		},
		RunResult: runGrepResult,
	}
}

func globTool() Tool {
	return Tool{
		Def: llm.NewTool("glob",
			"Find regular files by slash-aware glob pattern. Use ** for recursive matches. Results are deterministic, respect nested .gitignore files, and never follow symlinks.",
			`{"type":"object","properties":{"pattern":{"type":"string","description":"Glob pattern relative to path, for example **/*.go"},"path":{"type":"string","description":"Directory or file to search (default: current working directory)"},"max_results":{"type":"integer","description":"Maximum paths to return (default 1000, maximum 10000)"}},"required":["pattern"]}`),
		Run: func(ctx context.Context, args json.RawMessage) (string, error) {
			result, err := runGlobResult(ctx, args)
			return result.Preview, err
		},
		RunResult: runGlobResult,
	}
}

func runGrepResult(ctx context.Context, args json.RawMessage) (ToolResult, error) {
	var a grepArgs
	if err := json.Unmarshal(args, &a); err != nil {
		return ToolResult{}, err
	}
	raw, err := runGrep(ctx, a)
	if err != nil {
		return ToolResult{}, err
	}
	return MarkUntrusted(textResult(raw, truncate(raw), 0), "grep"), nil
}

func runGlobResult(ctx context.Context, args json.RawMessage) (ToolResult, error) {
	var a globArgs
	if err := json.Unmarshal(args, &a); err != nil {
		return ToolResult{}, err
	}
	raw, err := runGlob(ctx, a)
	if err != nil {
		return ToolResult{}, err
	}
	return MarkUntrusted(textResult(raw, truncate(raw), 0), "glob"), nil
}

func runGrep(ctx context.Context, args grepArgs) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	if args.Pattern == "" {
		return "", errors.New("pattern is required")
	}
	if len(args.Pattern) > maxSearchPatternBytes {
		return "", fmt.Errorf("pattern exceeds %d-byte limit", maxSearchPatternBytes)
	}
	expression := args.Pattern
	if args.Literal {
		expression = regexp.QuoteMeta(expression)
	}
	caseSensitive := true
	if args.CaseSensitive != nil {
		caseSensitive = *args.CaseSensitive
	}
	if !caseSensitive {
		expression = "(?i:" + expression + ")"
	}
	matcher, err := regexp.Compile(expression)
	if err != nil {
		return "", fmt.Errorf("invalid pattern: %w", err)
	}
	include, err := compileInclude(args.Include)
	if err != nil {
		return "", err
	}
	scope, err := openSearchScope(args.Path)
	if err != nil {
		return "", err
	}
	defer func() { _ = scope.Close() }()

	out := newSearchOutput(args.MaxResults)
	if scope.single {
		if include != nil && !include.matches(scope.matchPath(scope.start)) {
			return out.finish(), nil
		}
		if err := grepFile(ctx, scope.fsys, scope.start, scope.displayPath(scope.start), matcher, out); err != nil && !errors.Is(err, errSearchLimit) {
			return "", err
		}
		return out.finish(), nil
	}

	walker := newSearchWalker(scope)
	err = walker.walk(ctx, func(name string, entry fs.DirEntry, ignored bool) error {
		if ignored || entry.IsDir() || !isRegularEntry(entry) {
			return nil
		}
		if include != nil && !include.matches(scope.matchPath(name)) {
			return nil
		}
		return grepFile(ctx, scope.fsys, name, scope.displayPath(name), matcher, out)
	})
	if err != nil {
		return "", err
	}
	if walker.scanLimited {
		out.stop("scan limited to 100000 entries")
	}
	return out.finish(), nil
}

func runGlob(ctx context.Context, args globArgs) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	matcher, err := compileSearchPattern(args.Pattern, false)
	if err != nil {
		return "", err
	}
	scope, err := openSearchScope(args.Path)
	if err != nil {
		return "", err
	}
	defer func() { _ = scope.Close() }()

	out := newSearchOutput(args.MaxResults)
	if scope.single {
		if matcher.matches(scope.matchPath(scope.start)) {
			_ = out.add(scope.displayPath(scope.start) + "\n")
		}
		return out.finish(), nil
	}

	walker := newSearchWalker(scope)
	err = walker.walk(ctx, func(name string, entry fs.DirEntry, ignored bool) error {
		if ignored || entry.IsDir() || !isRegularEntry(entry) || !matcher.matches(scope.matchPath(name)) {
			return nil
		}
		return out.add(scope.displayPath(name) + "\n")
	})
	if err != nil {
		return "", err
	}
	if walker.scanLimited {
		out.stop("scan limited to 100000 entries")
	}
	return out.finish(), nil
}

type searchPattern struct {
	regex    *regexp.Regexp
	basename bool
}

func (p searchPattern) matches(name string) bool {
	if p.basename {
		name = path.Base(name)
	}
	return p.regex.MatchString(name)
}

func compileInclude(pattern string) (*searchPattern, error) {
	if pattern == "" {
		return nil, nil
	}
	return compileSearchPattern(pattern, true)
}

func compileSearchPattern(pattern string, basenameWithoutSlash bool) (*searchPattern, error) {
	pattern = filepath.ToSlash(pattern)
	if pattern == "" {
		return nil, errors.New("glob pattern is required")
	}
	if len(pattern) > maxSearchPatternBytes {
		return nil, fmt.Errorf("glob pattern exceeds %d-byte limit", maxSearchPatternBytes)
	}
	if filepath.IsAbs(pattern) || strings.HasPrefix(pattern, "/") {
		return nil, errors.New("glob pattern must be relative to the search path")
	}
	pattern = strings.TrimPrefix(pattern, "./")
	for _, part := range strings.Split(pattern, "/") {
		if part == ".." {
			return nil, errors.New("glob pattern cannot contain ..")
		}
	}
	regex, err := compileGlobPattern(pattern)
	if err != nil {
		return nil, fmt.Errorf("invalid glob pattern %q: %w", pattern, err)
	}
	return &searchPattern{
		regex:    regex,
		basename: basenameWithoutSlash && !strings.Contains(pattern, "/"),
	}, nil
}

type searchOutput struct {
	b         strings.Builder
	max       int
	matches   int
	reason    string
	stopAfter bool
}

func newSearchOutput(max int) *searchOutput {
	if max <= 0 {
		max = defaultSearchMaxResults
	}
	if max > maxSearchResults {
		max = maxSearchResults
	}
	return &searchOutput{max: max}
}

func (o *searchOutput) add(line string) error {
	if o.stopAfter {
		return errSearchLimit
	}
	if o.matches >= o.max {
		o.stop("result limit reached")
		return errSearchLimit
	}
	if int64(o.b.Len()+len(line)) > maxArtifactBytes {
		o.stop(fmt.Sprintf("output limited to %d bytes", maxArtifactBytes))
		return errSearchLimit
	}
	o.b.WriteString(line)
	o.matches++
	if o.matches >= o.max {
		o.stop("result limit reached")
		return errSearchLimit
	}
	return nil
}

func (o *searchOutput) stop(reason string) {
	if o.reason == "" {
		o.reason = reason
	}
	o.stopAfter = true
}

func (o *searchOutput) finish() string {
	result := strings.TrimSuffix(o.b.String(), "\n")
	if o.reason != "" {
		marker := "... [" + o.reason + "]"
		if result == "" {
			return marker
		}
		result += "\n" + marker
	}
	if result == "" {
		return "(no matches)"
	}
	return result
}

type searchScope struct {
	root     *os.Root
	fsys     fs.FS
	rootPath string
	cwdPath  string
	start    string
	single   bool
}

func (s *searchScope) Close() error {
	if s == nil || s.root == nil {
		return nil
	}
	return s.root.Close()
}

func (s *searchScope) displayPath(name string) string {
	abs := filepath.Join(s.rootPath, filepath.FromSlash(name))
	if rel, ok := relativePath(s.cwdPath, abs); ok {
		return filepath.ToSlash(rel)
	}
	return filepath.ToSlash(abs)
}

func (s *searchScope) matchPath(name string) string {
	if s.single {
		return path.Base(name)
	}
	if s.start == "." {
		return name
	}
	if rel, ok := relativeFSPath(s.start, name); ok {
		return rel
	}
	return name
}

func openSearchScope(requested string) (*searchScope, error) {
	if strings.TrimSpace(requested) == "" {
		requested = "."
	}
	abs, err := filepath.Abs(requested)
	if err != nil {
		return nil, fmt.Errorf("resolve search path: %w", err)
	}
	info, err := os.Lstat(abs)
	if err != nil {
		return nil, fmt.Errorf("search path %q: %w", requested, err)
	}
	if info.Mode()&os.ModeSymlink != 0 {
		return nil, fmt.Errorf("search path %q is a symlink; symlinks are not followed", requested)
	}
	resolved, err := filepath.EvalSymlinks(abs)
	if err != nil {
		return nil, fmt.Errorf("resolve search path %q: %w", requested, err)
	}
	info, err = os.Stat(resolved)
	if err != nil {
		return nil, fmt.Errorf("search path %q: %w", requested, err)
	}

	cwd, err := filepath.Abs(".")
	if err != nil {
		return nil, fmt.Errorf("resolve working directory: %w", err)
	}
	cwd, err = filepath.EvalSymlinks(cwd)
	if err != nil {
		return nil, fmt.Errorf("resolve working directory: %w", err)
	}

	scope := &searchScope{cwdPath: cwd}
	if info.IsDir() {
		scope.rootPath = resolved
		scope.start = "."
		if relative, ok := relativePath(cwd, resolved); ok {
			scope.rootPath = cwd
			scope.start = filepath.ToSlash(relative)
		}
	} else if info.Mode().IsRegular() {
		scope.single = true
		scope.rootPath = filepath.Dir(resolved)
		scope.start = filepath.Base(resolved)
		if relative, ok := relativePath(cwd, resolved); ok {
			scope.rootPath = cwd
			scope.start = filepath.ToSlash(relative)
		}
	} else {
		return nil, fmt.Errorf("search path %q is not a regular file or directory", requested)
	}

	scope.root, err = os.OpenRoot(scope.rootPath)
	if err != nil {
		return nil, fmt.Errorf("open search root %q: %w", requested, err)
	}
	scope.fsys = scope.root.FS()
	scope.start = cleanFSPath(scope.start)
	return scope, nil
}

func relativePath(root, target string) (string, bool) {
	rel, err := filepath.Rel(root, target)
	if err != nil || filepath.IsAbs(rel) || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return "", false
	}
	return rel, true
}

type searchWalker struct {
	scope       *searchScope
	ignores     *ignoreTree
	entries     int
	scanLimited bool
}

func newSearchWalker(scope *searchScope) *searchWalker {
	return &searchWalker{
		scope:   scope,
		ignores: newIgnoreTree(scope.fsys),
	}
}

func (w *searchWalker) walk(ctx context.Context, visit func(name string, entry fs.DirEntry, ignored bool) error) error {
	err := fs.WalkDir(w.scope.fsys, w.scope.start, func(name string, entry fs.DirEntry, walkErr error) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		if walkErr != nil {
			return fmt.Errorf("walk %s: %w", w.scope.displayPath(name), walkErr)
		}
		if entry == nil {
			return nil
		}
		if entry.Type()&fs.ModeSymlink != 0 {
			if entry.IsDir() {
				return fs.SkipDir
			}
			return nil
		}
		if entry.IsDir() && name != w.scope.start && path.Base(name) == ".git" {
			return fs.SkipDir
		}
		ignored, err := w.ignores.ignored(name, entry.IsDir())
		if err != nil {
			return err
		}
		w.entries++
		if w.entries > maxSearchEntries {
			w.scanLimited = true
			return errSearchLimit
		}
		if entry.IsDir() && ignored {
			// A later rule could only re-include a descendant if this
			// directory were itself re-included. ignored() has already
			// applied every rule visible from its ancestors, so pruning is
			// both safe and what keeps large ignored trees bounded.
			return fs.SkipDir
		}
		if entry.IsDir() {
			if err := w.ignores.ensureDir(name); err != nil {
				return err
			}
		}
		return visit(name, entry, ignored)
	})
	if errors.Is(err, errSearchLimit) {
		return nil
	}
	return err
}

func isRegularEntry(entry fs.DirEntry) bool {
	return entry.Type().IsRegular()
}

func grepFile(ctx context.Context, fsys fs.FS, name, display string, matcher *regexp.Regexp, out *searchOutput) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	f, err := fsys.Open(name)
	if err != nil {
		return fmt.Errorf("open %s: %w", display, err)
	}
	binary, probeErr := binaryFile(f)
	closeErr := f.Close()
	if probeErr != nil {
		return fmt.Errorf("inspect %s: %w", display, probeErr)
	}
	if closeErr != nil {
		return fmt.Errorf("close %s: %w", display, closeErr)
	}
	if binary {
		return nil
	}

	f, err = fsys.Open(name)
	if err != nil {
		return fmt.Errorf("open %s: %w", display, err)
	}
	defer func() { _ = f.Close() }()
	reader := bufio.NewReaderSize(f, 64<<10)
	lineNumber := 0
	for {
		line, eof, lineTruncated, err := readSearchLine(reader)
		if err != nil {
			return fmt.Errorf("read %s: %w", display, err)
		}
		if line == nil && eof {
			return nil
		}
		lineNumber++
		if matcher.Match(line) {
			text := strings.TrimSuffix(string(line), "\r")
			if len(text) > maxMatchLineBytes {
				text = text[:maxMatchLineBytes] + "… [line truncated]"
			} else if lineTruncated {
				text += "… [line truncated]"
			}
			if err := out.add(fmt.Sprintf("%s:%d:%s\n", display, lineNumber, text)); err != nil {
				return err
			}
		}
		if eof {
			return nil
		}
		if err := ctx.Err(); err != nil {
			return err
		}
	}
}

func binaryFile(f fs.File) (bool, error) {
	buf := make([]byte, maxBinaryProbeBytes)
	n, err := f.Read(buf)
	if err != nil && !errors.Is(err, io.EOF) {
		return false, err
	}
	return bytes.IndexByte(buf[:n], 0) >= 0, nil
}

func readSearchLine(reader *bufio.Reader) (line []byte, eof, truncated bool, err error) {
	for {
		chunk, readErr := reader.ReadSlice('\n')
		if len(chunk) > 0 && len(line) < maxSearchLineBytes {
			remaining := maxSearchLineBytes - len(line)
			if len(chunk) > remaining {
				line = append(line, chunk[:remaining]...)
				truncated = true
			} else {
				line = append(line, chunk...)
			}
		}
		switch readErr {
		case nil:
			return bytes.TrimSuffix(line, []byte{'\n'}), false, truncated, nil
		case bufio.ErrBufferFull:
			if len(line) >= maxSearchLineBytes {
				truncated = true
			}
			continue
		case io.EOF:
			if len(line) == 0 {
				return nil, true, truncated, nil
			}
			return bytes.TrimSuffix(line, []byte{'\n'}), true, truncated, nil
		default:
			return nil, false, truncated, readErr
		}
	}
}

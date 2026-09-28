package tools

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os/exec"
	"path/filepath"
	"regexp"
	"regexp/syntax"
	"slices"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/sacca97/ghg/internal/models"
	"github.com/sacca97/ghg/internal/sandbox"
	"github.com/sacca97/ghg/internal/search"
)

type grepArgs struct {
	Pattern       string   `json:"pattern"`
	Patterns      []string `json:"patterns"`
	Path          string   `json:"path"`
	Include       string   `json:"include"`
	MaxResults    int      `json:"max_results"`
	CaseSensitive *bool    `json:"case_sensitive"`
	Literal       bool     `json:"literal"`
	Cursor        string   `json:"cursor"`
}

type grepMatcher struct {
	patterns []string
	regexes  []*regexp.Regexp
}

func grepTool() Tool {
	return withAvailability(resultTool(models.NewTool("grep",
		"Search text files for a regular expression. Prefer this for text; use patterns for independent searches in one traversal. Results respect nested .gitignore files, skip binaries and symlinks, are grouped by file or pattern, and paginate with an opaque cursor. Never construct or infer a cursor; pass it only when this tool explicitly returned one and copy it exactly.",
		`{"type":"object","properties":{"pattern":{"type":"string","description":"Regular expression to search for"},"patterns":{"type":"array","minItems":1,"items":{"type":"string"},"description":"Independent regular expressions; results are labeled by pattern and searched in one traversal"},"path":{"type":"string","description":"File or directory to search (default: current working directory)"},"include":{"type":"string","description":"Optional glob filter such as *.go"},"max_results":{"type":"integer","description":"Matches per page (default 25, maximum 250)"},"cursor":{"type":"string","description":"Opaque cursor returned by this same tool in an earlier result; copy it exactly and do not infer or construct one"},"case_sensitive":{"type":"boolean","description":"Whether the expression is case-sensitive (default true)"},"literal":{"type":"boolean","description":"Treat patterns as literal text instead of regular expressions"}},"anyOf":[{"required":["pattern"]},{"required":["patterns"]},{"required":["cursor"]}]}`),
		runGrepResult), searchAvailability)
}

func runGrepResult(ctx context.Context, args json.RawMessage) (ToolResult, error) {
	var a grepArgs
	if err := json.Unmarshal(args, &a); err != nil {
		return ToolResult{}, err
	}
	var collected search.Snapshot
	result, err := resumeOrCollect(ctx, searchQuery{
		Kind: grepKind, Cursor: a.Cursor, PageSize: pageSize(a.MaxResults),
	}, func() (search.Snapshot, error) {
		var err error
		collected, err = collectGrepSnapshot(ctx, a)
		return collected, err
	}, searchPageOptions{perFileCap: searchPerFileCap, grouped: true})
	if err != nil || a.Cursor != "" {
		return result, err
	}
	patternCount := len(a.Patterns)
	if patternCount == 0 {
		patternCount = 1
	}
	itemStatus := "complete"
	if !collected.Complete {
		itemStatus = "partial"
	}
	items := make([]batchItem, patternCount)
	for index := range items {
		items[index] = batchItem{Index: index + 1, Status: itemStatus}
		if itemStatus == "partial" {
			items[index].Error = collected.Reason
		}
	}
	return applyBatchReport(result, batchReport{LogicalOperations: patternCount, InternalSubcalls: 1, Items: items}), nil
}

func collectGrepSnapshot(ctx context.Context, args grepArgs) (search.Snapshot, error) {
	matcher, err := compileGrepMatcher(args)
	if err != nil {
		return search.Snapshot{}, err
	}
	include, err := compileInclude(args.Include)
	if err != nil {
		return search.Snapshot{}, err
	}
	scope, err := openSearchScope(ctx, args.Path)
	if err != nil {
		return search.Snapshot{}, err
	}
	defer func() { _ = scope.Close() }()

	collector := newSearchCollector()
	err = grepSnapshotRG(ctx, args, scope, matcher, include, collector)
	if errors.Is(err, errSearchLimit) {
		collector.stop(fmt.Sprintf("scan limited to %d entries", maxSearchEntries))
	}
	if err != nil && !errors.Is(err, errSearchLimit) {
		return search.Snapshot{}, err
	}
	var modified map[string]struct{}
	if len(collector.items) > 0 {
		modified = gitModifiedPaths(ctx, scope.rootPath)
	}
	rankSearchItems(collector.items, scope, args.Path, SearchHintsFor(ctx), modified)
	if len(matcher.patterns) > 1 {
		sort.SliceStable(collector.items, func(i, j int) bool {
			return collector.items[i].Pattern < collector.items[j].Pattern
		})
	}
	var patternLabels []string
	if len(matcher.patterns) > 1 {
		patternLabels = slices.Clone(matcher.patterns)
	}
	return finishSearchSnapshot(ctx, grepKind, collector, patternLabels)
}

func compileGrepMatcher(args grepArgs) (*grepMatcher, error) {
	patterns := append([]string(nil), args.Patterns...)
	if len(patterns) == 0 && args.Pattern != "" {
		patterns = []string{args.Pattern}
	}
	if len(patterns) == 0 {
		return nil, errors.New("pattern or patterns is required")
	}
	total := 0
	for _, pattern := range patterns {
		if pattern == "" {
			return nil, errors.New("grep patterns cannot be empty")
		}
		total += len(pattern)
		if total > maxSearchPatternBytes {
			return nil, fmt.Errorf("patterns exceed %d-byte limit", maxSearchPatternBytes)
		}
	}
	if len(patterns) == 1 {
		if !args.Literal {
			pattern := patterns[0]
			if args.CaseSensitive != nil && !*args.CaseSensitive {
				pattern = "(?i:" + pattern + ")"
			}
			if _, err := syntax.Parse(pattern, syntax.Perl); err != nil {
				return nil, fmt.Errorf("invalid pattern: %w", err)
			}
		}
		return &grepMatcher{patterns: patterns}, nil
	}
	regexes := make([]*regexp.Regexp, 0, len(patterns))
	for _, pattern := range patterns {
		if args.Literal {
			pattern = regexp.QuoteMeta(pattern)
		}
		if args.CaseSensitive != nil && !*args.CaseSensitive {
			pattern = "(?i:" + pattern + ")"
		}
		regex, err := regexp.Compile(pattern)
		if err != nil {
			return nil, fmt.Errorf("invalid pattern: %w", err)
		}
		regexes = append(regexes, regex)
	}
	return &grepMatcher{patterns: patterns, regexes: regexes}, nil
}

func (m *grepMatcher) matches(line []byte) []int {
	matches := make([]int, 0, len(m.regexes))
	for index, regex := range m.regexes {
		if regex.Match(line) {
			matches = append(matches, index)
		}
	}
	return matches
}

var singlePatternMatch = []int{0}

func truncateMatchText(text string, lineWasTruncated bool) string {
	if len(text) > maxMatchLineBytes {
		return text[:maxMatchLineBytes] + "… [line truncated]"
	}
	if lineWasTruncated {
		return text + "… [line truncated]"
	}
	return text
}

func appendGrepMatches(ctx context.Context, out *searchCollector, display string, line int, text string, matcher *grepMatcher, matched []int) error {
	for _, pattern := range matched {
		item := search.Item{Path: display, Line: line, Text: text}
		if len(matcher.patterns) > 1 {
			item.Pattern = pattern + 1
		}
		if err := out.add(ctx, item); err != nil {
			return err
		}
	}
	return nil
}

func rankSearchItems(items []search.Item, scope *searchScope, requested string, hints SearchHints, modified map[string]struct{}) {
	touched := canonicalPathSet(hints.Touched)
	ranks := make(map[string]searchRank, len(items))
	for _, item := range items {
		if _, ok := ranks[item.Path]; !ok {
			ranks[item.Path] = searchItemRank(item.Path, scope, requested, touched, modified)
		}
	}
	sort.SliceStable(items, func(i, j int) bool {
		a, b := items[i], items[j]
		ra, rb := ranks[a.Path], ranks[b.Path]
		if ra.priority != rb.priority {
			return ra.priority < rb.priority
		}
		if ra.depth != rb.depth {
			return ra.depth < rb.depth
		}
		if ra.pathLength != rb.pathLength {
			return ra.pathLength < rb.pathLength
		}
		if a.Path != b.Path {
			return a.Path < b.Path
		}
		if a.Line != b.Line {
			return a.Line < b.Line
		}
		return a.Pattern < b.Pattern
	})
}

type searchRank struct {
	priority   int
	depth      int
	pathLength int
}

func searchItemRank(display string, scope *searchScope, requested string, touched, modified map[string]struct{}) searchRank {
	abs := display
	if !filepath.IsAbs(display) {
		if candidate, err := filepath.Abs(display); err == nil {
			abs = candidate
		}
	}
	abs = canonicalPathHintForSearch(abs)
	priority := 4
	explicit := scope != nil && scope.single
	if explicit {
		priority = 1
	} else if _, ok := touched[abs]; ok {
		priority = 2
	} else if _, ok := modified[abs]; ok {
		priority = 3
	}
	depth, pathLength := strings.Count(display, "/"), len(display)
	if scope != nil {
		if rel, ok := relativePath(scope.rootPath, abs); ok {
			depth = strings.Count(filepath.ToSlash(rel), "/")
			pathLength = len(rel)
		}
	}
	return searchRank{priority: priority, depth: depth, pathLength: pathLength}
}

func canonicalPathSet(paths []string) map[string]struct{} {
	set := make(map[string]struct{}, len(paths))
	for _, name := range paths {
		if path := canonicalPathHintForSearch(name); path != "" {
			set[path] = struct{}{}
		}
	}
	return set
}

func canonicalPathHintForSearch(name string) string {
	if strings.TrimSpace(name) == "" {
		return ""
	}
	abs, err := filepath.Abs(name)
	if err != nil {
		return ""
	}
	if resolved, err := filepath.EvalSymlinks(abs); err == nil {
		abs = resolved
	}
	return filepath.Clean(abs)
}

const defaultGitStatusTTL = 1500 * time.Millisecond

type gitStatusEntry struct {
	paths     map[string]struct{}
	expiresAt time.Time
}

type gitStatusFlight struct {
	done  chan struct{}
	paths map[string]struct{}
}

type gitStatusCache struct {
	mu       sync.Mutex
	entries  map[string]gitStatusEntry
	inflight map[string]*gitStatusFlight
	ttl      time.Duration
	now      func() time.Time
	loader   func(ctx context.Context, root string) map[string]struct{}
}

func newGitStatusCache(ttl time.Duration, loader func(ctx context.Context, root string) map[string]struct{}, now func() time.Time) *gitStatusCache {
	if now == nil {
		now = time.Now
	}
	return &gitStatusCache{
		entries:  make(map[string]gitStatusEntry),
		inflight: make(map[string]*gitStatusFlight),
		ttl:      ttl,
		now:      now,
		loader:   loader,
	}
}

var defaultGitStatusCache = newGitStatusCache(defaultGitStatusTTL, uncachedGitModifiedPaths, time.Now)

func gitModifiedPaths(ctx context.Context, root string) map[string]struct{} {
	return defaultGitStatusCache.get(ctx, root)
}

func (c *gitStatusCache) get(ctx context.Context, root string) map[string]struct{} {
	if strings.TrimSpace(root) == "" {
		return make(map[string]struct{})
	}
	canonical := canonicalPathHintForSearch(root)
	if canonical == "" {
		canonical = filepath.Clean(root)
	}

	c.mu.Lock()
	now := c.now()
	for k, entry := range c.entries {
		if !now.Before(entry.expiresAt) {
			delete(c.entries, k)
		}
	}

	if entry, ok := c.entries[canonical]; ok && now.Before(entry.expiresAt) {
		c.mu.Unlock()
		return entry.paths
	}
	if flight, ok := c.inflight[canonical]; ok {
		c.mu.Unlock()
		select {
		case <-flight.done:
			return flight.paths
		case <-ctx.Done():
			return make(map[string]struct{})
		}
	}

	flight := &gitStatusFlight{done: make(chan struct{})}
	c.inflight[canonical] = flight
	c.mu.Unlock()

	var paths map[string]struct{}
	defer func() {
		c.mu.Lock()
		if paths == nil {
			paths = make(map[string]struct{})
		}
		flight.paths = paths
		close(flight.done)
		delete(c.inflight, canonical)
		c.entries[canonical] = gitStatusEntry{paths: paths, expiresAt: c.now().Add(c.ttl)}
		c.mu.Unlock()
	}()

	paths = c.loader(ctx, root)
	return paths
}

func uncachedGitModifiedPaths(ctx context.Context, root string) map[string]struct{} {
	set := make(map[string]struct{})
	gitCtx, cancel := context.WithTimeout(ctx, 250*time.Millisecond)
	defer cancel()
	gitArgs := []string{"-C", root, "status", "--porcelain=v1", "-z", "--untracked-files=all"}
	cmd := exec.CommandContext(gitCtx, "git", gitArgs...)
	if runtime := RuntimeFromContext(ctx); runtime != nil && runtime.Policy != nil {
		wrapped, err := runtime.WrapCommand(sandbox.CommandSpec{
			Program: "git",
			Args:    gitArgs,
			Dir:     root,
			Env:     runtime.ChildEnv(nil),
		})
		if err != nil {
			return set
		}
		cmd = exec.CommandContext(gitCtx, wrapped.Program, wrapped.Args...)
		cmd.Dir = wrapped.Dir
		cmd.Env = wrapped.Env
	}
	out, err := cmd.Output()
	if err != nil {
		return set
	}
	for _, record := range strings.Split(string(out), "\x00") {
		if len(record) < 4 {
			continue
		}
		name := strings.TrimSpace(record[3:])
		if strings.Contains(name, " -> ") {
			name = strings.TrimSpace(strings.Split(name, " -> ")[1])
		}
		set[canonicalPathHintForSearch(filepath.Join(root, name))] = struct{}{}
	}
	return set
}

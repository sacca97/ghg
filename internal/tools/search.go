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
	"os/exec"
	"path"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/sacca97/ghg/internal/llm"
	"github.com/sacca97/ghg/internal/sandbox"
	"github.com/sacca97/ghg/internal/search"
)

const (
	defaultSearchMaxResults = 25
	maxSearchResults        = 10000
	maxSearchEntries        = 100000
	maxBinaryProbeBytes     = 8 << 10
	maxSearchLineBytes      = 1 << 20
	maxMatchLineBytes       = 4 << 10
	maxSearchPatternBytes   = 16 << 10
	searchPreviewBytes      = 16 << 10
	searchPerFileCap        = 4
)

var errSearchLimit = errors.New("search limit reached")

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

type globArgs struct {
	Pattern    string `json:"pattern"`
	Path       string `json:"path"`
	MaxResults int    `json:"max_results"`
	Cursor     string `json:"cursor"`
}

type findFilesArgs struct {
	Query      string `json:"query"`
	Path       string `json:"path"`
	MaxResults int    `json:"max_results"`
	Cursor     string `json:"cursor"`
}

func grepTool() Tool {
	return resultTool(llm.NewTool("grep",
		"Search text files for a regular expression. Prefer this for text; use patterns for one OR search. Results respect nested .gitignore files, skip binaries and symlinks, are grouped by file, and paginate with cursor.",
		`{"type":"object","properties":{"pattern":{"type":"string","description":"Regular expression to search for; use patterns for multiple alternatives"},"patterns":{"type":"array","items":{"type":"string"},"description":"Regular expressions ORed together and searched in one traversal"},"path":{"type":"string","description":"File or directory to search (default: current working directory)"},"include":{"type":"string","description":"Optional glob filter such as *.go"},"max_results":{"type":"integer","description":"Matches per page (default 25, maximum 10000)"},"cursor":{"type":"string","description":"Cursor returned by an earlier page; reuse it with max_results to continue"},"case_sensitive":{"type":"boolean","description":"Whether the expression is case-sensitive (default true)"},"literal":{"type":"boolean","description":"Treat patterns as literal text instead of regular expressions"}},"required":[]}`),
		runGrepResult)
}

func globTool() Tool {
	return resultTool(llm.NewTool("glob",
		"Find regular files by deterministic slash-aware glob. Use ** for recursive paths. It respects nested .gitignore files, never follows symlinks, and paginates with cursor.",
		`{"type":"object","properties":{"pattern":{"type":"string","description":"Glob pattern relative to path, for example **/*.go"},"path":{"type":"string","description":"Directory or file to search (default: current working directory)"},"max_results":{"type":"integer","description":"Paths per page (default 25, maximum 10000)"},"cursor":{"type":"string","description":"Cursor returned by an earlier page"}},"required":["pattern"]}`),
		runGlobResult)
}

func findFilesTool() Tool {
	return resultTool(llm.NewTool("find_files",
		"Find files by fuzzy path or filename match. Every candidate is scored before the best results are selected; use glob for exact patterns.",
		`{"type":"object","properties":{"query":{"type":"string","description":"Filename or path text to match fuzzily"},"path":{"type":"string","description":"Directory to search (default: current working directory)"},"max_results":{"type":"integer","description":"Paths per page (default 25, maximum 10000)"},"cursor":{"type":"string","description":"Cursor returned by an earlier page"}},"required":["query"]}`),
		runFindFilesResult)
}

func runGrepResult(ctx context.Context, args json.RawMessage) (ToolResult, error) {
	var a grepArgs
	if err := json.Unmarshal(args, &a); err != nil {
		return ToolResult{}, err
	}
	if a.Cursor != "" {
		snapshot, cursor, err := loadSearchPage(ctx, "grep", a.Cursor)
		if err != nil {
			return ToolResult{}, err
		}
		return renderSearchResult(ctx, snapshot, cursor, pageSize(a.MaxResults), searchPerFileCap, true), nil
	}
	snapshot, err := collectGrepSnapshot(ctx, a)
	if err != nil {
		return ToolResult{}, err
	}
	return renderSearchResult(ctx, snapshot, searchCursor{Kind: "grep", ID: snapshot.ID}, pageSize(a.MaxResults), searchPerFileCap, true), nil
}

func runGlobResult(ctx context.Context, args json.RawMessage) (ToolResult, error) {
	var a globArgs
	if err := json.Unmarshal(args, &a); err != nil {
		return ToolResult{}, err
	}
	if a.Cursor != "" {
		snapshot, cursor, err := loadSearchPage(ctx, "glob", a.Cursor)
		if err != nil {
			return ToolResult{}, err
		}
		return renderSearchResult(ctx, snapshot, cursor, pageSize(a.MaxResults), 0, false), nil
	}
	snapshot, err := compileGlobSnapshot(ctx, a)
	if err != nil {
		return ToolResult{}, err
	}
	return renderSearchResult(ctx, snapshot, searchCursor{Kind: "glob", ID: snapshot.ID}, pageSize(a.MaxResults), 0, false), nil
}

func runFindFilesResult(ctx context.Context, args json.RawMessage) (ToolResult, error) {
	var a findFilesArgs
	if err := json.Unmarshal(args, &a); err != nil {
		return ToolResult{}, err
	}
	if a.Cursor != "" {
		snapshot, cursor, err := loadSearchPage(ctx, "find_files", a.Cursor)
		if err != nil {
			return ToolResult{}, err
		}
		return renderSearchResult(ctx, snapshot, cursor, pageSize(a.MaxResults), 0, false), nil
	}
	if strings.TrimSpace(a.Query) == "" {
		return ToolResult{}, errors.New("query is required")
	}
	if len(a.Query) > maxSearchPatternBytes {
		return ToolResult{}, fmt.Errorf("query exceeds %d-byte limit", maxSearchPatternBytes)
	}
	root := a.Path
	if strings.TrimSpace(root) == "" {
		root = "."
	}
	abs, err := filepath.Abs(root)
	if err != nil {
		return ToolResult{}, fmt.Errorf("resolve find_files path: %w", err)
	}
	authorized := abs
	if runtime := RuntimeFromContext(ctx); runtime != nil && runtime.Policy != nil {
		authorized, err = runtime.Policy.Authorize(root, sandbox.AccessRead, true)
		if err != nil {
			return ToolResult{}, err
		}
	}
	info, err := os.Lstat(abs)
	if err != nil {
		return ToolResult{}, fmt.Errorf("find_files path %q: %w", root, err)
	}
	if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return ToolResult{}, fmt.Errorf("find_files path %q is not a real directory", root)
	}
	abs = authorized
	resolved, err := filepath.EvalSymlinks(abs)
	if err != nil {
		return ToolResult{}, fmt.Errorf("resolve find_files path %q: %w", root, err)
	}
	hits := search.FuzzyFiles(resolved, a.Query, 0)
	items := make([]search.Item, 0, min(len(hits), maxSearchResults))
	for _, hit := range hits {
		if len(items) >= maxSearchResults {
			break
		}
		display := filepath.Join(resolved, filepath.FromSlash(hit))
		cwd, _ := filepath.Abs(".")
		if rel, ok := relativePath(cwd, display); ok {
			display = rel
		}
		items = append(items, search.Item{Path: filepath.ToSlash(display)})
	}
	snapshot := search.Snapshot{ID: search.NewID("find_files"), Kind: "find_files", Items: items, Complete: len(hits) <= maxSearchResults, CreatedAt: time.Now().UTC()}
	if !snapshot.Complete {
		snapshot.Reason = fmt.Sprintf("result set limited to %d paths", maxSearchResults)
	}
	if err := saveSearchSnapshot(ctx, snapshot); err != nil {
		return ToolResult{}, err
	}
	return renderSearchResult(ctx, snapshot, searchCursor{Kind: "find_files", ID: snapshot.ID}, pageSize(a.MaxResults), 0, false), nil
}

type searchCollector struct {
	items    []search.Item
	bytes    int64
	complete bool
	reason   string
}

func newSearchCollector() *searchCollector {
	return &searchCollector{items: make([]search.Item, 0, defaultSearchMaxResults), complete: true}
}

func (c *searchCollector) add(ctx context.Context, item search.Item) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if len(c.items) >= maxSearchResults {
		c.stop(fmt.Sprintf("result set limited to %d matches", maxSearchResults))
		return errSearchLimit
	}
	// This is deliberately an accounting budget, not the model-facing page
	// budget. It bounds the immutable cursor snapshot before it can become an
	// artifact while still retaining enough results for many small pages.
	itemBytes := int64(len(item.Path) + len(item.Text) + 32)
	if c.bytes+itemBytes > maxArtifactBytes {
		c.stop(fmt.Sprintf("search snapshot limited to %d bytes", maxArtifactBytes))
		return errSearchLimit
	}
	c.items = append(c.items, item)
	c.bytes += itemBytes
	return nil
}

func (c *searchCollector) stop(reason string) {
	c.complete = false
	if c.reason == "" {
		c.reason = reason
	}
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
	if scope.single {
		if include == nil || include.matches(scope.matchPath(scope.start)) {
			err = grepSnapshotFile(ctx, scope.fsys, scope.start, scope.displayPath(scope.start), matcher, collector)
		}
	} else {
		walker := newSearchWalker(scope)
		err = walker.walk(ctx, func(name string, entry fs.DirEntry, ignored bool) error {
			if ignored || entry.IsDir() || !isRegularEntry(entry) {
				return nil
			}
			if include != nil && !include.matches(scope.matchPath(name)) {
				return nil
			}
			return grepSnapshotFile(ctx, scope.fsys, name, scope.displayPath(name), matcher, collector)
		})
		if walker.scanLimited {
			collector.stop(fmt.Sprintf("scan limited to %d entries", maxSearchEntries))
		}
	}
	if err != nil && !errors.Is(err, errSearchLimit) {
		return search.Snapshot{}, err
	}
	var modified map[string]struct{}
	if len(collector.items) > 0 {
		modified = gitModifiedPaths(ctx, scope.rootPath)
	}
	rankSearchItems(collector.items, scope, args.Path, searchHintsFor(ctx), modified)
	snapshot := search.Snapshot{
		ID:        search.NewID("grep"),
		Kind:      "grep",
		Items:     collector.items,
		Complete:  collector.complete,
		Reason:    collector.reason,
		CreatedAt: time.Now().UTC(),
	}
	if err := saveSearchSnapshot(ctx, snapshot); err != nil {
		return search.Snapshot{}, err
	}
	return snapshot, nil
}

func compileGrepMatcher(args grepArgs) (*regexp.Regexp, error) {
	patterns := append([]string(nil), args.Patterns...)
	if len(patterns) == 0 && args.Pattern != "" {
		patterns = []string{args.Pattern}
	}
	if len(patterns) == 0 {
		return nil, errors.New("pattern or patterns is required")
	}
	total := 0
	parts := make([]string, 0, len(patterns))
	for _, pattern := range patterns {
		if pattern == "" {
			return nil, errors.New("grep patterns cannot be empty")
		}
		total += len(pattern)
		if total > maxSearchPatternBytes {
			return nil, fmt.Errorf("patterns exceed %d-byte limit", maxSearchPatternBytes)
		}
		if args.Literal {
			pattern = regexp.QuoteMeta(pattern)
		}
		parts = append(parts, "(?:"+pattern+")")
	}
	expression := strings.Join(parts, "|")
	if args.CaseSensitive != nil && !*args.CaseSensitive {
		expression = "(?i:" + expression + ")"
	}
	matcher, err := regexp.Compile(expression)
	if err != nil {
		return nil, fmt.Errorf("invalid pattern: %w", err)
	}
	return matcher, nil
}

func compileGlobSnapshot(ctx context.Context, args globArgs) (search.Snapshot, error) {
	matcher, err := compileSearchPattern(args.Pattern, false)
	if err != nil {
		return search.Snapshot{}, err
	}
	scope, err := openSearchScope(ctx, args.Path)
	if err != nil {
		return search.Snapshot{}, err
	}
	defer func() { _ = scope.Close() }()
	collector := newSearchCollector()
	add := func(name string) error {
		return collector.add(ctx, search.Item{Path: scope.displayPath(name)})
	}
	if scope.single {
		if matcher.matches(scope.matchPath(scope.start)) {
			err = add(scope.start)
		}
	} else {
		walker := newSearchWalker(scope)
		err = walker.walk(ctx, func(name string, entry fs.DirEntry, ignored bool) error {
			if ignored || entry.IsDir() || !isRegularEntry(entry) || !matcher.matches(scope.matchPath(name)) {
				return nil
			}
			return add(name)
		})
		if walker.scanLimited {
			collector.stop(fmt.Sprintf("scan limited to %d entries", maxSearchEntries))
		}
	}
	if err != nil && !errors.Is(err, errSearchLimit) {
		return search.Snapshot{}, err
	}
	sort.SliceStable(collector.items, func(i, j int) bool {
		return collector.items[i].Path < collector.items[j].Path
	})
	snapshot := search.Snapshot{
		ID:        search.NewID("glob"),
		Kind:      "glob",
		Items:     collector.items,
		Complete:  collector.complete,
		Reason:    collector.reason,
		CreatedAt: time.Now().UTC(),
	}
	if err := saveSearchSnapshot(ctx, snapshot); err != nil {
		return search.Snapshot{}, err
	}
	return snapshot, nil
}

func saveSearchSnapshot(ctx context.Context, snapshot search.Snapshot) error {
	sessionID, store := searchContextFor(ctx)
	if store == nil {
		return nil
	}
	return store.Save(ctx, sessionID, snapshot)
}

type searchCursor struct {
	Kind   string
	ID     string
	Offset int
}

func parseSearchCursor(raw string) (searchCursor, error) {
	parts := strings.Split(raw, "/")
	if len(parts) != 3 || parts[0] == "" || parts[1] == "" {
		return searchCursor{}, errors.New("invalid search cursor")
	}
	offset, err := strconv.Atoi(parts[2])
	if err != nil || offset < 0 {
		return searchCursor{}, errors.New("invalid search cursor offset")
	}
	return searchCursor{Kind: parts[0], ID: parts[1], Offset: offset}, nil
}

func searchCursorString(c searchCursor) string {
	return c.Kind + "/" + c.ID + "/" + strconv.Itoa(c.Offset)
}

func loadSearchPage(ctx context.Context, kind, raw string) (search.Snapshot, searchCursor, error) {
	cursor, err := parseSearchCursor(raw)
	if err != nil {
		return search.Snapshot{}, searchCursor{}, err
	}
	if cursor.Kind != kind {
		return search.Snapshot{}, searchCursor{}, fmt.Errorf("cursor belongs to %s, not %s", cursor.Kind, kind)
	}
	_, store := searchContextFor(ctx)
	if store == nil {
		return search.Snapshot{}, searchCursor{}, errors.New("search cursor requires an active agent session; run the search again")
	}
	sessionID, _ := searchContextFor(ctx)
	snapshot, err := store.Load(ctx, sessionID, cursor.ID)
	if err != nil {
		return search.Snapshot{}, searchCursor{}, fmt.Errorf("load search cursor: %w", err)
	}
	if snapshot.Kind != kind || snapshot.ID != cursor.ID {
		return search.Snapshot{}, searchCursor{}, errors.New("search cursor does not match its snapshot")
	}
	return snapshot, cursor, nil
}

func pageSize(n int) int {
	if n <= 0 {
		return defaultSearchMaxResults
	}
	if n > maxSearchResults {
		return maxSearchResults
	}
	return n
}

func renderSearchResult(ctx context.Context, snapshot search.Snapshot, cursor searchCursor, size, perFileCap int, grouped bool) ToolResult {
	if err := ctx.Err(); err != nil {
		return errorToolResult(err)
	}
	if size <= 0 {
		size = defaultSearchMaxResults
	}
	if grouped && perFileCap > size {
		perFileCap = size
	}
	chunks := searchPageChunks(snapshot.Items, perFileCap, grouped)
	if cursor.Offset < 0 || cursor.Offset > len(chunks) {
		return errorToolResult(errors.New("search cursor offset is out of range"))
	}
	_, searchStore := searchContextFor(ctx)
	page, nextOffset := selectSearchPage(snapshot, chunks, cursor.Offset, size, searchStore != nil, grouped)
	hasMore := searchStore != nil && nextOffset < len(chunks)
	remaining := len(snapshot.Items) - searchPageItemsBefore(chunks, nextOffset)
	pageText := renderSearchPage(snapshot.Kind, page, len(snapshot.Items), len(page), remaining, hasMore,
		searchCursor{Kind: snapshot.Kind, ID: snapshot.ID, Offset: nextOffset}, grouped, snapshot)
	if len(pageText) > searchPreviewBytes {
		// An individual result can still be too large to fit after the
		// bounded line/path safeguards. Keep its cursor at the same offset so
		// no later result is silently skipped; the model can narrow the search
		// and retry. This branch deliberately reports zero displayed results.
		page = nil
		nextOffset = cursor.Offset
		hasMore = searchStore != nil && nextOffset < len(chunks)
		remaining = len(snapshot.Items) - searchPageItemsBefore(chunks, nextOffset)
		pageText = renderSearchPage(snapshot.Kind, page, len(snapshot.Items), 0, remaining, hasMore,
			searchCursor{Kind: snapshot.Kind, ID: snapshot.ID, Offset: nextOffset}, grouped, snapshot)
		pageText += fmt.Sprintf("\n[next result exceeds the %d-byte preview; narrow the search before continuing]", searchPreviewBytes)
	}
	// The selector above only accepts complete rendered items. Keep this as a
	// final defensive assertion against future changes to renderSearchPage;
	// truncating here would make the cursor metadata dishonest again.
	if len(pageText) > searchPreviewBytes {
		pageText = fmt.Sprintf("%s: results exceed the %d-byte preview; narrow the search and retry", snapshot.Kind, searchPreviewBytes)
		page = nil
		nextOffset = cursor.Offset
		remaining = len(snapshot.Items) - searchPageItemsBefore(chunks, nextOffset)
		hasMore = searchStore != nil && nextOffset < len(chunks)
	}
	fullText := renderSearchAll(snapshot, grouped)
	capture := NewTextCapture(maxArtifactBytes)
	capture.WriteString(fullText)
	result := capturedResult(capture.String(), pageText, capture.OriginalBytes(), snapshot.Complete && capture.Complete(), 0)
	result.Metadata = map[string]string{
		"search_id":               snapshot.ID,
		"search_kind":             snapshot.Kind,
		"search_displayed":        strconv.Itoa(len(page)),
		"search_remaining":        strconv.Itoa(max(remaining, 0)),
		"search_incomplete":       strconv.FormatBool(!snapshot.Complete),
		"search_cursor_available": strconv.FormatBool(searchStore != nil),
	}
	if hasMore {
		result.Metadata["search_cursor"] = searchCursorString(searchCursor{Kind: snapshot.Kind, ID: snapshot.ID, Offset: nextOffset})
	}
	return MarkUntrusted(result, snapshot.Kind)
}

// searchPageChunks is the immutable sequence addressed by a search cursor.
// Grouped grep pages use per-file chunks so a page does not split a small file
// group; path-only tools use one item per cursor position.
func searchPageChunks(items []search.Item, capPerFile int, grouped bool) [][]search.Item {
	if !grouped {
		chunks := make([][]search.Item, len(items))
		for i := range items {
			chunks[i] = []search.Item{items[i]}
		}
		return chunks
	}
	return groupedSearchChunks(items, capPerFile)
}

// selectSearchPage accepts only complete rendered pages. The cursor advances
// after the last item that actually fits inside the model-facing byte ceiling;
// it never advances past a result that bounded rendering would cut away.
func selectSearchPage(snapshot search.Snapshot, chunks [][]search.Item, offset, size int, cursorAvailable, grouped bool) ([]search.Item, int) {
	page := make([]search.Item, 0, min(size, len(snapshot.Items)))
	nextOffset := offset
	for i := offset; i < len(chunks) && len(page) < size; i++ {
		chunk := chunks[i]
		if len(page) > 0 && len(page)+len(chunk) > size {
			break
		}
		candidate := append(slices.Clone(page), chunk...)
		candidateOffset := i + 1
		candidateHasMore := cursorAvailable && candidateOffset < len(chunks)
		candidateRemaining := len(snapshot.Items) - searchPageItemsBefore(chunks, candidateOffset)
		candidateText := renderSearchPage(snapshot.Kind, candidate, len(snapshot.Items), len(candidate), candidateRemaining,
			candidateHasMore, searchCursor{Kind: snapshot.Kind, ID: snapshot.ID, Offset: candidateOffset},
			grouped, snapshot)
		if len(candidateText) > searchPreviewBytes {
			break
		}
		page = candidate
		nextOffset = candidateOffset
	}
	return page, nextOffset
}

func searchPageItemsBefore(chunks [][]search.Item, end int) int {
	end = min(max(end, 0), len(chunks))
	total := 0
	for _, chunk := range chunks[:end] {
		total += len(chunk)
	}
	return total
}

func groupedSearchChunks(items []search.Item, capPerFile int) [][]search.Item {
	if capPerFile <= 0 {
		return [][]search.Item{slices.Clone(items)}
	}
	chunks := make([][]search.Item, 0)
	for i := 0; i < len(items); {
		end := i + 1
		for end < len(items) && items[end].Path == items[i].Path {
			end++
		}
		for start := i; start < end; start += capPerFile {
			chunkEnd := min(start+capPerFile, end)
			chunks = append(chunks, slices.Clone(items[start:chunkEnd]))
		}
		i = end
	}
	return chunks
}

func renderSearchPage(kind string, items []search.Item, total, displayed, remaining int, hasMore bool, next searchCursor, grouped bool, snapshot search.Snapshot) string {
	var b strings.Builder
	if total == 0 {
		b.WriteString(kind + ": (no matches)")
	} else {
		fmt.Fprintf(&b, "%s: showing %d/%d results", kind, displayed, total)
		if remaining > 0 {
			fmt.Fprintf(&b, " (%d remaining; result limit reached for page)", remaining)
		}
		b.WriteByte('\n')
		if grouped {
			lastPath := ""
			for _, item := range items {
				if item.Path != lastPath {
					if lastPath != "" {
						b.WriteByte('\n')
					}
					b.WriteString(item.Path)
					b.WriteString(":\n")
					lastPath = item.Path
				}
				fmt.Fprintf(&b, "  %d:%s\n", item.Line, item.Text)
			}
		} else {
			for _, item := range items {
				b.WriteString(item.Path)
				b.WriteByte('\n')
			}
		}
	}
	if hasMore {
		fmt.Fprintf(&b, "[cursor=%s]", searchCursorString(next))
	}
	if !snapshot.Complete {
		if b.Len() > 0 {
			b.WriteByte('\n')
		}
		fmt.Fprintf(&b, "[incomplete search snapshot: %s; omitted results are unavailable]", snapshot.Reason)
	}
	return strings.TrimSuffix(b.String(), "\n")
}

func renderSearchAll(snapshot search.Snapshot, grouped bool) string {
	if len(snapshot.Items) == 0 {
		return snapshot.Kind + ": (no matches)\n"
	}
	var b strings.Builder
	if grouped {
		lastPath := ""
		for _, item := range snapshot.Items {
			if item.Path != lastPath {
				b.WriteString(item.Path + ":\n")
				lastPath = item.Path
			}
			fmt.Fprintf(&b, "  %d:%s\n", item.Line, item.Text)
		}
	} else {
		for _, item := range snapshot.Items {
			b.WriteString(item.Path + "\n")
		}
	}
	if !snapshot.Complete {
		fmt.Fprintf(&b, "[incomplete search snapshot: %s; omitted results are unavailable]\n", snapshot.Reason)
	}
	return b.String()
}

func grepSnapshotFile(ctx context.Context, fsys fs.FS, name, display string, matcher *regexp.Regexp, out *searchCollector) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	f, err := fsys.Open(name)
	if err != nil {
		return fmt.Errorf("open %s: %w", display, err)
	}
	defer func() { _ = f.Close() }()

	reader := bufio.NewReaderSize(f, 64<<10)
	probe, probeErr := reader.Peek(maxBinaryProbeBytes)
	if probeErr != nil && !errors.Is(probeErr, io.EOF) {
		return fmt.Errorf("inspect %s: %w", display, probeErr)
	}
	if bytes.IndexByte(probe, 0) >= 0 {
		return nil
	}

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
			if err := out.add(ctx, search.Item{Path: display, Line: lineNumber, Text: text}); err != nil {
				return err
			}
		}
		if eof {
			return nil
		}
	}
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
		return a.Line < b.Line
	})
}

type searchRank struct {
	priority   int
	depth      int
	pathLength int
}

func searchItemRank(display string, scope *searchScope, requested string, touched, modified map[string]struct{}) searchRank {
	abs := display
	if filepath.IsAbs(display) == false {
		if candidate, err := filepath.Abs(display); err == nil {
			abs = candidate
		}
	}
	abs = canonicalPathHintForSearch(abs)
	priority := 4
	// A single-file scope is genuinely explicit. A directory supplied as the
	// search root is not: every result is already inside that root, so touched
	// and modified-file hints must still be able to improve its first page.
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
	// Never retain entries indefinitely: prune expired entries on access.
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
		c.entries[canonical] = gitStatusEntry{
			paths:     paths,
			expiresAt: c.now().Add(c.ttl),
		}
		c.mu.Unlock()
	}()

	paths = c.loader(ctx, root)
	return paths
}

func uncachedGitModifiedPaths(ctx context.Context, root string) map[string]struct{} {
	set := make(map[string]struct{})
	gitCtx, cancel := context.WithTimeout(ctx, 250*time.Millisecond)
	defer cancel()
	cmd := exec.CommandContext(gitCtx, "git", "-C", root, "status", "--porcelain=v1", "-z", "--untracked-files=all")
	if runtime := RuntimeFromContext(ctx); runtime != nil && runtime.Policy != nil {
		wrapped, err := runtime.WrapCommand(sandbox.CommandSpec{
			Program: "git",
			Args:    []string{"-C", root, "status", "--porcelain=v1", "-z", "--untracked-files=all"},
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

func openSearchScope(ctx context.Context, requested string) (*searchScope, error) {
	if strings.TrimSpace(requested) == "" {
		requested = "."
	}
	abs, err := filepath.Abs(requested)
	if err != nil {
		return nil, fmt.Errorf("resolve search path: %w", err)
	}
	authorized := abs
	if runtime := RuntimeFromContext(ctx); runtime != nil && runtime.Policy != nil {
		authorized, err = runtime.Policy.Authorize(abs, sandbox.AccessRead, true)
		if err != nil {
			return nil, err
		}
	}
	info, err := os.Lstat(abs)
	if err != nil {
		return nil, fmt.Errorf("search path %q: %w", requested, err)
	}
	if info.Mode()&os.ModeSymlink != 0 {
		return nil, fmt.Errorf("search path %q is a symlink; symlinks are not followed", requested)
	}
	abs = authorized
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

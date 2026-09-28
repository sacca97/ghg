package tools

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/sacca97/ghg/internal/sandbox"
	"github.com/sacca97/ghg/internal/search"
)

type searchKind string

const (
	grepKind                searchKind = "grep"
	globKind                searchKind = "glob"
	findFilesKind           searchKind = "find_files"
	defaultSearchMaxResults            = 25
	maxSearchPageSize                  = 250
	maxSearchResults                   = 2000
	maxSearchEntries                   = 100000
	maxMatchLineBytes                  = 4 << 10
	maxSearchPatternBytes              = 16 << 10
	searchPreviewBytes                 = 16 << 10
	searchPerFileCap                   = 4
)

var errSearchLimit = errors.New("search limit reached")

type searchQuery struct {
	Kind     searchKind
	Cursor   string
	PageSize int
}

type searchCursor struct {
	Kind   searchKind
	ID     string
	Offset int
}

type searchCollector struct {
	mu       sync.Mutex
	items    []search.Item
	bytes    int64
	complete bool
	reason   string
}

func searchAvailability(_ *ToolRuntime) (bool, string) {
	if _, ok := rgAvailable(); ok {
		return true, ""
	}
	return false, "repository search unavailable: rg is not on PATH"
}

func newSearchCollector() *searchCollector {
	return &searchCollector{items: make([]search.Item, 0, defaultSearchMaxResults), complete: true}
}

func (c *searchCollector) add(ctx context.Context, item search.Item) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return err
	}
	if len(c.items) >= maxSearchResults {
		c.stopLocked(fmt.Sprintf("result set limited to %d matches", maxSearchResults))
		return errSearchLimit
	}
	// Bound the immutable cursor snapshot independently of the model-facing page.
	itemBytes := int64(len(item.Path) + len(item.Text) + 32)
	if c.bytes+itemBytes > maxOutputBytes {
		c.stopLocked(fmt.Sprintf("search snapshot limited to %d bytes", maxOutputBytes))
		return errSearchLimit
	}
	c.items = append(c.items, item)
	c.bytes += itemBytes
	return nil
}

func (c *searchCollector) stop(reason string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.stopLocked(reason)
}

func (c *searchCollector) stopLocked(reason string) {
	c.complete = false
	if c.reason == "" {
		c.reason = reason
	}
}

func finishSearchSnapshot(ctx context.Context, kind searchKind, collector *searchCollector, patternLabels []string) (search.Snapshot, error) {
	snapshot := search.Snapshot{
		ID:        search.NewID(string(kind)),
		Kind:      string(kind),
		Items:     collector.items,
		Patterns:  patternLabels,
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

func parseSearchCursor(raw string) (searchCursor, error) {
	parts := strings.Split(raw, "/")
	if len(parts) != 3 || parts[0] == "" || parts[1] == "" {
		return searchCursor{}, errors.New("invalid search cursor")
	}
	offset, err := strconv.Atoi(parts[2])
	if err != nil || offset < 0 {
		return searchCursor{}, errors.New("invalid search cursor offset")
	}
	return searchCursor{Kind: searchKind(parts[0]), ID: parts[1], Offset: offset}, nil
}

func searchCursorString(c searchCursor) string {
	return string(c.Kind) + "/" + c.ID + "/" + strconv.Itoa(c.Offset)
}

func loadSearchPage(ctx context.Context, kind searchKind, raw string) (search.Snapshot, searchCursor, error) {
	cursor, err := parseSearchCursor(raw)
	if err != nil {
		return search.Snapshot{}, searchCursor{}, err
	}
	if cursor.Kind != kind {
		return search.Snapshot{}, searchCursor{}, fmt.Errorf("cursor belongs to %s, not %s", cursor.Kind, kind)
	}
	sessionID, store := searchContextFor(ctx)
	if store == nil {
		return search.Snapshot{}, searchCursor{}, errors.New("search cursor requires an active agent session; run the search again")
	}
	snapshot, err := store.Load(ctx, sessionID, cursor.ID)
	if err != nil {
		return search.Snapshot{}, searchCursor{}, fmt.Errorf("load search cursor: %w", err)
	}
	if snapshot.Kind != string(kind) || snapshot.ID != cursor.ID {
		return search.Snapshot{}, searchCursor{}, errors.New("search cursor does not match its snapshot")
	}
	return snapshot, cursor, nil
}

func pageSize(n int) int {
	if n <= 0 {
		return defaultSearchMaxResults
	}
	if n > maxSearchPageSize {
		return maxSearchPageSize
	}
	return n
}

func resumeOrCollect(ctx context.Context, query searchQuery, collect func() (search.Snapshot, error), options searchPageOptions) (ToolResult, error) {
	var snapshot search.Snapshot
	var cursor searchCursor
	var err error
	if query.Cursor != "" {
		snapshot, cursor, err = loadSearchPage(ctx, query.Kind, query.Cursor)
	} else {
		snapshot, err = collect()
		cursor = searchCursor{Kind: query.Kind, ID: snapshot.ID}
	}
	if err != nil {
		return ToolResult{}, err
	}
	return renderSearchResult(ctx, snapshot, cursor, query.PageSize, options), nil
}

func cleanFSPath(name string) string {
	name = strings.ReplaceAll(name, "\\", "/")
	name = path.Clean(name)
	if name == "" {
		return "."
	}
	return name
}

func relativeFSPath(base, name string) (string, bool) {
	base = cleanFSPath(base)
	name = cleanFSPath(name)
	if base == "." {
		return name, true
	}
	if name == base {
		return ".", true
	}
	prefix := base + "/"
	if !strings.HasPrefix(name, prefix) {
		return "", false
	}
	return strings.TrimPrefix(name, prefix), true
}

type searchScope struct {
	root     *os.Root
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
		if errors.Is(err, os.ErrNotExist) {
			return nil, fmt.Errorf("search path %q does not exist; locate the path with glob or find_files instead of guessing", requested)
		}
		return nil, fmt.Errorf("search path %q: %w", requested, err)
	}
	if info.Mode()&os.ModeSymlink != 0 {
		return nil, fmt.Errorf("search path %q is a symlink; symlinks are not followed", requested)
	}
	resolved, err := filepath.EvalSymlinks(authorized)
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

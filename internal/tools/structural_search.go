package tools

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/sacca97/ghg/internal/models"
	"github.com/sacca97/ghg/internal/sandbox"
	"github.com/sacca97/ghg/internal/search"
	"github.com/sacca97/ghg/internal/structuralsearch"
	"golang.org/x/sync/errgroup"
)

const (
	structuralSearchKind  = "structural_search"
	maxStructuralFileSize = 16 << 20
	maxStructuralFiles    = 10000
	// ponytail: keep the worker count fixed; raise it only from a measured bottleneck.
	maxStructuralWorkers = 4
)

type structuralSearchArgs struct {
	Patterns   []string `json:"patterns"`
	Language   string   `json:"language"`
	Path       string   `json:"path"`
	MaxResults int      `json:"max_results"`
	Cursor     string   `json:"cursor"`
	Observe    bool     `json:"observe"`
}

func structuralSearchTool() Tool {
	return resultTool(models.NewTool(structuralSearchKind,
		"Search Go source structurally using bounded code patterns and metavariables. Results are grouped by file and paginate with cursor.",
		`{"type":"object","properties":{"patterns":{"type":"array","items":{"type":"string"},"description":"Small list of Go code patterns; supports $NAME, $_, and $$$ARGS metavariables"},"language":{"type":"string","enum":["go"],"description":"Source language; V1 supports Go only"},"path":{"type":"string","description":"Go file or directory to search (default: current working directory)"},"max_results":{"type":"integer","description":"Matches per page (default 25, maximum 250)"},"cursor":{"type":"string","description":"Opaque cursor returned by an earlier structural_search page"},"observe":{"type":"boolean","description":"Issue edit-authorizing observations for visible results (default false)"}},"required":["patterns","language"]}`),
		runStructuralSearchResult)
}

func runStructuralSearchResult(ctx context.Context, args json.RawMessage) (ToolResult, error) {
	var a structuralSearchArgs
	if err := json.Unmarshal(args, &a); err != nil {
		return ToolResult{}, err
	}
	if a.Cursor != "" {
		snapshot, cursor, err := loadSearchPage(ctx, structuralSearchKind, a.Cursor)
		if err != nil {
			return ToolResult{}, err
		}
		return renderStructuralSearchResult(ctx, snapshot, cursor, pageSize(a.MaxResults), a.Observe), nil
	}
	if len(a.Patterns) == 0 {
		return ToolResult{}, errors.New("patterns is required")
	}
	if strings.TrimSpace(a.Language) == "" {
		return ToolResult{}, errors.New("language is required")
	}
	query := structuralsearch.Query{Language: a.Language, Patterns: a.Patterns}
	matcher, err := structuralsearch.Compile(query)
	if err != nil {
		return ToolResult{}, err
	}
	snapshot, err := collectStructuralSnapshot(ctx, a, matcher)
	if err != nil {
		return ToolResult{}, err
	}
	return renderStructuralSearchResult(ctx, snapshot, searchCursor{Kind: structuralSearchKind, ID: snapshot.ID}, pageSize(a.MaxResults), a.Observe), nil
}

func collectStructuralSnapshot(ctx context.Context, args structuralSearchArgs, matcher *structuralsearch.Matcher) (search.Snapshot, error) {
	scope, err := openSearchScope(ctx, args.Path)
	if err != nil {
		return search.Snapshot{}, err
	}
	defer func() { _ = scope.Close() }()
	if scope.single && path.Ext(scope.start) != ".go" {
		return search.Snapshot{}, fmt.Errorf("structural_search path %q is not a Go source file", args.Path)
	}

	collector := newSearchCollector()
	var collectorMu sync.Mutex
	jobs := make(chan string, maxStructuralWorkers)
	group, workCtx := errgroup.WithContext(ctx)
	group.SetLimit(maxStructuralWorkers)
	for i := 0; i < maxStructuralWorkers; i++ {
		group.Go(func() error {
			for {
				select {
				case <-workCtx.Done():
					return workCtx.Err()
				case name, ok := <-jobs:
					if !ok {
						return nil
					}
					if err := structuralSearchFile(workCtx, scope, name, matcher, collector, &collectorMu); err != nil {
						return err
					}
				}
			}
		})
	}

	scheduled := 0
	schedule := func(name string) error {
		if scheduled >= maxStructuralFiles {
			collectorMu.Lock()
			collector.stop(fmt.Sprintf("scan limited to %d Go files", maxStructuralFiles))
			collectorMu.Unlock()
			return errSearchLimit
		}
		scheduled++
		select {
		case jobs <- name:
			return nil
		case <-workCtx.Done():
			return workCtx.Err()
		}
	}

	if scope.single {
		err = schedule(scope.start)
	} else {
		walker := newSearchWalker(scope)
		err = walker.walk(workCtx, func(name string, entry fs.DirEntry, ignored bool) error {
			if ignored || entry.IsDir() || !isRegularEntry(entry) || path.Ext(name) != ".go" {
				return nil
			}
			return schedule(name)
		})
		if walker.scanLimited {
			collectorMu.Lock()
			collector.stop(fmt.Sprintf("scan limited to %d entries", maxSearchEntries))
			collectorMu.Unlock()
		}
	}
	close(jobs)
	groupErr := group.Wait()
	if err != nil && !errors.Is(err, errSearchLimit) && !errors.Is(err, context.Canceled) {
		return search.Snapshot{}, err
	}
	if groupErr != nil && !errors.Is(groupErr, errSearchLimit) {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return search.Snapshot{}, ctxErr
		}
		return search.Snapshot{}, groupErr
	}
	if ctxErr := ctx.Err(); ctxErr != nil {
		return search.Snapshot{}, ctxErr
	}

	var modified map[string]struct{}
	if len(collector.items) > 0 {
		modified = gitModifiedPaths(ctx, scope.rootPath)
	}
	rankSearchItems(collector.items, scope, args.Path, searchHintsFor(ctx), modified)
	snapshot := search.Snapshot{
		ID:        search.NewID(structuralSearchKind),
		Kind:      structuralSearchKind,
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

func structuralSearchFile(ctx context.Context, scope *searchScope, name string, matcher *structuralsearch.Matcher, collector *searchCollector, collectorMu *sync.Mutex) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	f, err := scope.fsys.Open(name)
	if err != nil {
		return fmt.Errorf("open %s: %w", scope.displayPath(name), err)
	}
	defer func() { _ = f.Close() }()
	info, err := f.Stat()
	if err != nil {
		return fmt.Errorf("stat %s: %w", scope.displayPath(name), err)
	}
	if info.Size() > maxStructuralFileSize {
		collectorMu.Lock()
		collector.stop(fmt.Sprintf("skipped %s: file exceeds %d-byte limit", scope.displayPath(name), maxStructuralFileSize))
		collectorMu.Unlock()
		return nil
	}
	source, err := io.ReadAll(io.LimitReader(f, maxStructuralFileSize+1))
	if err != nil {
		return fmt.Errorf("read %s: %w", scope.displayPath(name), err)
	}
	if len(source) > maxStructuralFileSize {
		collectorMu.Lock()
		collector.stop(fmt.Sprintf("skipped %s: file exceeds %d-byte limit", scope.displayPath(name), maxStructuralFileSize))
		collectorMu.Unlock()
		return nil
	}
	matches, err := matcher.Search(ctx, source)
	if err != nil {
		return fmt.Errorf("search %s: %w", scope.displayPath(name), err)
	}
	display := scope.displayPath(name)
	seen := make(map[[2]int]struct{}, len(matches))
	positions := structuralPositioner{source: source, line: 1}
	for _, match := range matches {
		if match.StartByte < 0 || match.EndByte < match.StartByte || match.EndByte > len(source) {
			continue
		}
		key := [2]int{match.StartByte, match.EndByte}
		if _, ok := seen[key]; ok {
			continue
		}
		seen[key] = struct{}{}
		startLine, startColumn := positions.at(match.StartByte)
		endLine, endColumn := positions.at(match.EndByte)
		item := search.Item{
			Path:        display,
			Line:        startLine,
			StartColumn: startColumn,
			EndLine:     endLine,
			EndColumn:   endColumn,
			StartByte:   match.StartByte,
			EndByte:     match.EndByte,
			Pattern:     match.Pattern,
			Text:        string(source[match.StartByte:match.EndByte]),
		}
		collectorMu.Lock()
		err := collector.add(ctx, item)
		collectorMu.Unlock()
		if err != nil {
			return err
		}
	}
	return nil
}

type structuralPositioner struct {
	source    []byte
	offset    int
	line      int
	lineStart int
}

func (p *structuralPositioner) at(offset int) (int, int) {
	offset = max(offset, 0)
	if offset > len(p.source) {
		offset = len(p.source)
	}
	if offset < p.offset {
		p.offset, p.line, p.lineStart = 0, 1, 0
	}
	for p.offset < offset {
		if p.source[p.offset] == '\n' {
			p.line++
			p.lineStart = p.offset + 1
		}
		p.offset++
	}
	return p.line, offset - p.lineStart + 1
}

func renderStructuralSearchResult(ctx context.Context, snapshot search.Snapshot, cursor searchCursor, size int, observe bool) ToolResult {
	if !observe {
		return renderSearchResult(ctx, snapshot, cursor, size, searchPerFileCap, true)
	}
	if _, store := observationContextFor(ctx); store == nil {
		return renderSearchResult(ctx, snapshot, cursor, size, searchPerFileCap, true)
	}
	if err := ctx.Err(); err != nil {
		return errorToolResult(err)
	}
	if size <= 0 {
		size = defaultSearchMaxResults
	}
	chunks := searchPageChunks(snapshot.Items, searchPerFileCap, true)
	if cursor.Offset < 0 || cursor.Offset > len(chunks) {
		return errorToolResult(errors.New("search cursor offset is out of range"))
	}
	_, searchStore := searchContextFor(ctx)
	page, nextOffset := selectSearchPage(snapshot, chunks, cursor.Offset, size, searchStore != nil, true)
	hasMore := searchStore != nil && nextOffset < len(chunks)
	remaining := len(snapshot.Items) - searchPageItemsBefore(chunks, nextOffset)
	if preview := renderSearchPage(snapshot.Kind, page, len(snapshot.Items), len(page), remaining, hasMore,
		searchCursor{Kind: snapshot.Kind, ID: snapshot.ID, Offset: nextOffset}, true, snapshot); len(preview) > searchPreviewBytes {
		page = nil
		nextOffset = cursor.Offset
		hasMore = searchStore != nil && nextOffset < len(chunks)
		remaining = len(snapshot.Items) - searchPageItemsBefore(chunks, nextOffset)
	}
	page = observeStructuralPage(ctx, page)
	if err := ctx.Err(); err != nil {
		return errorToolResult(err)
	}
	pageText := renderSearchPage(snapshot.Kind, page, len(snapshot.Items), len(page), remaining, hasMore,
		searchCursor{Kind: snapshot.Kind, ID: snapshot.ID, Offset: nextOffset}, true, snapshot)
	if len(pageText) > searchPreviewBytes {
		page = nil
		nextOffset = cursor.Offset
		hasMore = searchStore != nil && nextOffset < len(chunks)
		remaining = len(snapshot.Items) - searchPageItemsBefore(chunks, nextOffset)
		pageText = renderSearchPage(snapshot.Kind, page, len(snapshot.Items), 0, remaining, hasMore,
			searchCursor{Kind: snapshot.Kind, ID: snapshot.ID, Offset: nextOffset}, true, snapshot)
		pageText += fmt.Sprintf("\n[next result exceeds the %d-byte preview; narrow the search before continuing]", searchPreviewBytes)
	}
	result := capturedResult(pageText, pageText, int64(len(pageText)), snapshot.Complete, 0)
	result.Metadata = map[string]string{
		"search_id":               snapshot.ID,
		"search_kind":             snapshot.Kind,
		"search_displayed":        fmt.Sprint(len(page)),
		"search_remaining":        fmt.Sprint(max(remaining, 0)),
		"search_incomplete":       fmt.Sprint(!snapshot.Complete),
		"search_cursor_available": fmt.Sprint(searchStore != nil),
	}
	if hasMore {
		result.Metadata["search_cursor"] = searchCursorString(searchCursor{Kind: snapshot.Kind, ID: snapshot.ID, Offset: nextOffset})
	}
	return MarkUntrusted(result, snapshot.Kind)
}

func observeStructuralPage(ctx context.Context, page []search.Item) []search.Item {
	observed := slices.Clone(page)
	for start := 0; start < len(observed); {
		end := start + 1
		for end < len(observed) && observed[end].Path == observed[start].Path {
			end++
		}
		canonical, err := authorizedObservationPath(ctx, observed[start].Path, sandbox.AccessRead, false)
		if err == nil {
			if data, readErr := os.ReadFile(canonical); readErr == nil {
				if id, ok := structuralObservation(ctx, canonical, observed[start].Path, data, observed[start:end]); ok {
					for i := start; i < end; i++ {
						observed[i].ObservationID = id
					}
				}
			}
		}
		start = end
	}
	return observed
}

func structuralObservation(ctx context.Context, canonical, display string, source []byte, items []search.Item) (string, bool) {
	startLine, endLine := 0, 0
	for _, item := range items {
		if item.StartByte < 0 || item.EndByte < item.StartByte || item.EndByte > len(source) || string(source[item.StartByte:item.EndByte]) != item.Text {
			return "", false
		}
		if startLine == 0 || item.Line < startLine {
			startLine = item.Line
		}
		if item.EndLine > endLine {
			endLine = item.EndLine
		}
	}
	if startLine <= 0 || endLine < startLine || endLine-startLine+1 > maxReadLines {
		return "", false
	}
	result, err := readObservedContent(ctx, canonical, display, bytes.NewReader(source), startLine, endLine-startLine+1)
	if err != nil {
		return "", false
	}
	return result.Metadata["observation_id"], result.Metadata["observation_id"] != ""
}

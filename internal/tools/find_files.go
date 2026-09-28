package tools

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/sacca97/ghg/internal/models"
	"github.com/sacca97/ghg/internal/search"
)

type findFilesArgs struct {
	Query      string `json:"query"`
	Path       string `json:"path"`
	MaxResults int    `json:"max_results"`
	Cursor     string `json:"cursor"`
}

func findFilesTool() Tool {
	return withAvailability(resultTool(models.NewTool("find_files",
		"Find files by fuzzy path or filename match. Every candidate is scored before the best results are selected; use glob for exact patterns. Results paginate with an opaque cursor. Never construct or infer a cursor; pass it only when this tool explicitly returned one and copy it exactly.",
		`{"type":"object","properties":{"query":{"type":"string","description":"Filename or path text to match fuzzily"},"path":{"type":"string","description":"Directory to search (default: current working directory)"},"max_results":{"type":"integer","description":"Paths per page (default 25, maximum 250)"},"cursor":{"type":"string","description":"Opaque cursor returned by this same tool in an earlier result; copy it exactly and do not infer or construct one"}},"required":["query"]}`),
		runFindFilesResult), searchAvailability)
}

func runFindFilesResult(ctx context.Context, args json.RawMessage) (ToolResult, error) {
	var a findFilesArgs
	if err := json.Unmarshal(args, &a); err != nil {
		return ToolResult{}, err
	}
	return resumeOrCollect(ctx, searchQuery{
		Kind: findFilesKind, Cursor: a.Cursor, PageSize: pageSize(a.MaxResults),
	}, func() (search.Snapshot, error) {
		return collectFindFilesSnapshot(ctx, a)
	}, searchPageOptions{})
}

func collectFindFilesSnapshot(ctx context.Context, args findFilesArgs) (search.Snapshot, error) {
	if strings.TrimSpace(args.Query) == "" {
		return search.Snapshot{}, errors.New("query is required")
	}
	if len(args.Query) > maxSearchPatternBytes {
		return search.Snapshot{}, fmt.Errorf("query exceeds %d-byte limit", maxSearchPatternBytes)
	}
	scope, err := openSearchScope(ctx, args.Path)
	if err != nil {
		return search.Snapshot{}, err
	}
	defer func() { _ = scope.Close() }()
	paths := make(map[string]string)
	err = listFilesRG(ctx, scope, func(name string) error {
		rel, err := rgRelativePath(scope, name)
		if err != nil {
			return err
		}
		paths[scope.matchPath(rel)] = scope.displayPath(rel)
		return nil
	})
	incomplete := errors.Is(err, errSearchLimit)
	if err != nil && !incomplete {
		return search.Snapshot{}, err
	}
	candidates := make([]string, 0, len(paths))
	for name := range paths {
		candidates = append(candidates, name)
	}
	hits := search.FuzzyPaths(candidates, args.Query, maxSearchResults)
	items := make([]search.Item, 0, len(hits))
	for _, hit := range hits {
		items = append(items, search.Item{Path: paths[hit]})
	}
	collector := newSearchCollector()
	collector.items = items
	if incomplete {
		collector.stop(fmt.Sprintf("scan limited to %d entries", maxSearchEntries))
	}
	return finishSearchSnapshot(ctx, findFilesKind, collector, nil)
}

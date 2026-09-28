package tools

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"path"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"unicode/utf8"

	"github.com/sacca97/ghg/internal/models"
	"github.com/sacca97/ghg/internal/search"
)

type globArgs struct {
	Pattern    string `json:"pattern"`
	Path       string `json:"path"`
	MaxResults int    `json:"max_results"`
	Cursor     string `json:"cursor"`
}

func globTool() Tool {
	return withAvailability(resultTool(models.NewTool("glob",
		"Find regular files by deterministic slash-aware glob. Use ** for recursive paths. It respects nested .gitignore files, never follows symlinks, and paginates with an opaque cursor. Never construct or infer a cursor; pass it only when this tool explicitly returned one and copy it exactly.",
		`{"type":"object","properties":{"pattern":{"type":"string","description":"Glob pattern relative to path, for example **/*.go"},"path":{"type":"string","description":"Directory or file to search (default: current working directory)"},"max_results":{"type":"integer","description":"Paths per page (default 25, maximum 250)"},"cursor":{"type":"string","description":"Opaque cursor returned by this same tool in an earlier result; copy it exactly and do not infer or construct one"}},"required":["pattern"]}`),
		runGlobResult), searchAvailability)
}

func runGlobResult(ctx context.Context, args json.RawMessage) (ToolResult, error) {
	var a globArgs
	if err := json.Unmarshal(args, &a); err != nil {
		return ToolResult{}, err
	}
	return resumeOrCollect(ctx, searchQuery{
		Kind: globKind, Cursor: a.Cursor, PageSize: pageSize(a.MaxResults),
	}, func() (search.Snapshot, error) {
		return collectGlobSnapshot(ctx, a)
	}, searchPageOptions{})
}

func collectGlobSnapshot(ctx context.Context, args globArgs) (search.Snapshot, error) {
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
	err = listFilesRG(ctx, scope, func(name string) error {
		rel, err := rgRelativePath(scope, name)
		if err != nil {
			return err
		}
		if !matcher.matches(scope.matchPath(rel)) {
			return nil
		}
		return collector.add(ctx, search.Item{Path: scope.displayPath(rel)})
	})
	if errors.Is(err, errSearchLimit) {
		collector.stop(fmt.Sprintf("scan limited to %d entries", maxSearchEntries))
	}
	if err != nil && !errors.Is(err, errSearchLimit) {
		return search.Snapshot{}, err
	}
	sort.SliceStable(collector.items, func(i, j int) bool {
		return collector.items[i].Path < collector.items[j].Path
	})
	return finishSearchSnapshot(ctx, globKind, collector, nil)
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
			return nil, errors.New("glob pattern must not contain '..'")
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

func compileInclude(pattern string) (*searchPattern, error) {
	if pattern == "" {
		return nil, nil
	}
	return compileSearchPattern(pattern, true)
}

func compileGlobPattern(pattern string) (*regexp.Regexp, error) {
	var b strings.Builder
	b.WriteByte('^')
	for i := 0; i < len(pattern); {
		switch pattern[i] {
		case '*':
			if i+1 < len(pattern) && pattern[i+1] == '*' {
				i += 2
				for i < len(pattern) && pattern[i] == '*' {
					i++
				}
				if i < len(pattern) && pattern[i] == '/' {
					b.WriteString(`(?:.*/)?`)
					i++
				} else {
					b.WriteString(`.*`)
				}
				continue
			}
			b.WriteString(`[^/]*`)
			i++
		case '?':
			b.WriteString(`[^/]`)
			i++
		case '[':
			end, ok := globClassEnd(pattern, i+1)
			if !ok {
				return nil, fmt.Errorf("unterminated character class")
			}
			class := pattern[i : end+1]
			if len(class) > 1 && class[1] == '!' {
				class = "[^" + class[2:]
			}
			b.WriteString(class)
			i = end + 1
		case '\\':
			if i+1 >= len(pattern) {
				b.WriteString(`\\`)
				i++
				continue
			}
			r, size := utf8.DecodeRuneInString(pattern[i+1:])
			b.WriteString(regexp.QuoteMeta(string(r)))
			i += 1 + size
		default:
			r, size := utf8.DecodeRuneInString(pattern[i:])
			b.WriteString(regexp.QuoteMeta(string(r)))
			i += size
		}
	}
	b.WriteByte('$')
	return regexp.Compile(b.String())
}

func globClassEnd(pattern string, start int) (int, bool) {
	for i := start; i < len(pattern); i++ {
		if pattern[i] == '\\' {
			i++
			continue
		}
		if pattern[i] == ']' && i > start {
			return i, true
		}
	}
	return 0, false
}

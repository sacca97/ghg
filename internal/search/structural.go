package search

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"math"
	"slices"
	"strings"
	"sync/atomic"
	"time"

	"github.com/odvcencio/gotreesitter"
	"github.com/odvcencio/gotreesitter/grammars"
	"github.com/odvcencio/gotreesitter/grep"
)

const maxParseTime = 250 * time.Millisecond

// Query describes a structural search. The caller owns authorization and
// supplies one already-authorized source buffer at a time.
type Query struct {
	Language string
	Patterns []string
}

// Match is one half-open byte range in the source buffer.
type Match struct {
	StartByte int
	EndByte   int
	Pattern   int
}

// Matcher is a compiled structural query that can be reused for many source
// buffers in one authorized search request.
type Matcher struct {
	language           *gotreesitter.Language
	tokenSourceFactory func([]byte, *gotreesitter.Language) gotreesitter.TokenSource
	patterns           []*grep.CompiledPattern
}

// Compile validates and compiles a structural query once for reuse.
func Compile(query Query) (*Matcher, error) {
	entry, err := validateQuery(query)
	if err != nil {
		return nil, err
	}

	lang := entry.Language()
	compiled := make([]*grep.CompiledPattern, len(query.Patterns))
	for i, pattern := range query.Patterns {
		compiledPattern, err := grep.Compile(lang, pattern)
		if err != nil {
			return nil, fmt.Errorf("compile pattern %d: %w", i, err)
		}
		compiled[i] = compiledPattern
	}
	return &Matcher{language: lang, tokenSourceFactory: entry.TokenSourceFactory, patterns: compiled}, nil
}

// Search parses one source buffer with the previously compiled query.
func (m *Matcher) Search(ctx context.Context, source []byte) ([]Match, error) {
	if m == nil || m.language == nil || len(m.patterns) == 0 {
		return nil, errors.New("structural search matcher is nil or empty")
	}
	if len(source) > MaxSourceBytes {
		return nil, fmt.Errorf("source exceeds %d-byte limit", MaxSourceBytes)
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	var cancelled uint32
	stopWatcher := context.AfterFunc(ctx, func() {
		atomic.StoreUint32(&cancelled, 1)
	})
	defer stopWatcher()

	parser := gotreesitter.NewParser(m.language)
	parser.SetCancellationFlag(&cancelled)
	parser.SetTimeoutMicros(uint64(parseTimeout(ctx) / time.Microsecond))
	var tree *gotreesitter.Tree
	var err error
	if m.tokenSourceFactory != nil {
		tree, err = parser.ParseWithTokenSource(source, m.tokenSourceFactory(source, m.language))
	} else {
		tree, err = parser.Parse(source)
	}
	if err != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return nil, ctxErr
		}
		return nil, fmt.Errorf("parse Go source: %w", err)
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if tree == nil {
		return nil, errors.New("parse Go source: parser returned no tree")
	}
	if tree.ParseStoppedEarly() {
		if tree.ParseStopReason() == gotreesitter.ParseStopTimeout {
			return nil, errors.New("parse Go source: parse budget exceeded")
		}
		return nil, errors.New("parse Go source: incomplete parse")
	}
	bound := gotreesitter.Bind(tree)
	defer bound.Release()
	root := bound.RootNode()
	if root == nil {
		return nil, nil
	}
	if root.HasError() {
		return nil, errors.New("parse Go source: syntax errors or an incomplete parse")
	}

	matches := make([]Match, 0)
	for patternIndex, compiledPattern := range m.patterns {
		cursor := compiledPattern.Query.Exec(root, m.language, source)
		cursor.SetMatchLimit(maxMatches)
		for {
			queryMatch, ok := cursor.NextMatch()
			if !ok {
				break
			}
			if err := ctx.Err(); err != nil {
				return nil, err
			}
			start, end, ok := matchRange(root, queryMatch, compiledPattern.SExpr, m.language)
			if !ok || uint64(end) > uint64(len(source)) {
				continue
			}
			matches = append(matches, Match{
				StartByte: int(start),
				EndByte:   int(end),
				Pattern:   patternIndex,
			})
		}
		if err := ctx.Err(); err != nil {
			return nil, err
		}
	}
	slices.SortStableFunc(matches, compareMatches)
	return matches, nil
}

func compareMatches(a, b Match) int {
	return cmp.Or(
		cmp.Compare(a.StartByte, b.StartByte),
		cmp.Compare(a.EndByte, b.EndByte),
		cmp.Compare(a.Pattern, b.Pattern),
	)
}

func validateQuery(query Query) (*grammars.LangEntry, error) {
	if strings.TrimSpace(query.Language) != "go" {
		return nil, fmt.Errorf("unsupported structural search language %q; only go is supported", query.Language)
	}
	entry := grammars.DetectLanguageByName("go")
	if entry == nil || entry.Language == nil {
		return nil, errors.New("go grammar is unavailable")
	}
	if len(query.Patterns) == 0 {
		return nil, errors.New("at least one structural search pattern is required")
	}
	if len(query.Patterns) > maxPatterns {
		return nil, fmt.Errorf("structural search accepts at most %d patterns", maxPatterns)
	}
	total := 0
	for i, pattern := range query.Patterns {
		if strings.TrimSpace(pattern) == "" {
			return nil, fmt.Errorf("structural search pattern %d is empty", i)
		}
		total += len(pattern)
		if total > maxPatternBytes {
			return nil, fmt.Errorf("structural search patterns exceed %d-byte limit", maxPatternBytes)
		}
	}
	return entry, nil
}

func matchRange(root *gotreesitter.Node, queryMatch gotreesitter.QueryMatch, sexpr string, lang *gotreesitter.Language) (uint32, uint32, bool) {
	start := uint32(math.MaxUint32)
	var end uint32
	var first *gotreesitter.Node
	for _, capture := range queryMatch.Captures {
		if capture.Node == nil {
			continue
		}
		if first == nil {
			first = capture.Node
		}
		if byte := capture.Node.StartByte(); byte < start {
			start = byte
		}
		if byte := capture.Node.EndByte(); byte > end {
			end = byte
		}
	}
	if start == uint32(math.MaxUint32) || end < start || first == nil {
		return 0, 0, false
	}
	if rootType := sExprRootType(sexpr); rootType != "" && rootType != "_" {
		if enclosingStart, enclosingEnd, ok := enclosing(root, first, lang, rootType, start, end); ok {
			return enclosingStart, enclosingEnd, true
		}
	}
	return enclosing(root, first, lang, "", start, end)
}

func enclosing(root, from *gotreesitter.Node, lang *gotreesitter.Language, wantType string, start, end uint32) (uint32, uint32, bool) {
	for node := from; node != nil; node = node.Parent() {
		if (wantType == "" || node.Type(lang) == wantType) && node.StartByte() <= start && node.EndByte() >= end {
			return node.StartByte(), node.EndByte(), true
		}
		if node == root {
			break
		}
	}
	if wantType == "" {
		return start, end, true
	}
	return 0, 0, false
}

func sExprRootType(sexpr string) string {
	sexpr = strings.TrimSpace(sexpr)
	if len(sexpr) < 2 || sexpr[0] != '(' {
		return ""
	}
	sexpr = strings.TrimSpace(sexpr[1:])
	if end := strings.IndexAny(sexpr, " \t\r\n)"); end >= 0 {
		return sexpr[:end]
	}
	return sexpr
}

func parseTimeout(ctx context.Context) time.Duration {
	timeout := maxParseTime
	if deadline, ok := ctx.Deadline(); ok {
		if remaining := time.Until(deadline); remaining < timeout {
			timeout = remaining
		}
	}
	if timeout <= 0 {
		return time.Microsecond
	}
	return timeout
}

package tools

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"

	"github.com/sacca97/ghg/internal/search"
)

type searchPageOptions struct {
	perFileCap int
	grouped    bool
}

func renderSearchResult(ctx context.Context, snapshot search.Snapshot, cursor searchCursor, size int, opts searchPageOptions) ToolResult {
	if err := ctx.Err(); err != nil {
		return errorToolResult(err)
	}
	if size <= 0 {
		size = defaultSearchMaxResults
	}
	if opts.grouped && opts.perFileCap > size {
		opts.perFileCap = size
	}
	patternGrouped := len(snapshot.Patterns) > 1
	chunks := searchPageChunks(snapshot.Items, opts.perFileCap, opts.grouped, patternGrouped)
	if cursor.Offset < 0 || cursor.Offset > len(chunks) {
		return errorToolResult(errors.New("search cursor offset is out of range"))
	}
	_, searchStore := searchContextFor(ctx)
	page, nextOffset := selectSearchPage(snapshot, chunks, cursor.Offset, size, searchStore != nil, opts.grouped)
	hasMore := searchStore != nil && nextOffset < len(chunks)
	remaining := len(snapshot.Items) - searchPageItemsBefore(chunks, nextOffset)
	next := searchCursor{Kind: searchKind(snapshot.Kind), ID: snapshot.ID, Offset: nextOffset}
	pageText := renderSearchPage(snapshot.Kind, page, len(snapshot.Items), len(page), remaining, hasMore, next, opts.grouped, snapshot)
	if len(pageText) > searchPreviewBytes {
		page = nil
		nextOffset = cursor.Offset
		hasMore = searchStore != nil && nextOffset < len(chunks)
		remaining = len(snapshot.Items) - searchPageItemsBefore(chunks, nextOffset)
		next.Offset = nextOffset
		pageText = renderSearchPage(snapshot.Kind, page, len(snapshot.Items), len(page), remaining, hasMore, next, opts.grouped, snapshot)
	}
	if len(pageText) > searchPreviewBytes {
		// Keep the cursor at the same offset so no later result is silently skipped.
		page = nil
		nextOffset = cursor.Offset
		hasMore = searchStore != nil && nextOffset < len(chunks)
		remaining = len(snapshot.Items) - searchPageItemsBefore(chunks, nextOffset)
		next.Offset = nextOffset
		pageText = renderSearchPage(snapshot.Kind, page, len(snapshot.Items), 0, remaining, hasMore, next, opts.grouped, snapshot)
		pageText += fmt.Sprintf("\n[next result exceeds the %d-byte preview; narrow the search before continuing]", searchPreviewBytes)
	}
	if len(pageText) > searchPreviewBytes {
		pageText = fmt.Sprintf("%s: results exceed the %d-byte preview; narrow the search and retry", snapshot.Kind, searchPreviewBytes)
		page = nil
		nextOffset = cursor.Offset
		remaining = len(snapshot.Items) - searchPageItemsBefore(chunks, nextOffset)
		hasMore = searchStore != nil && nextOffset < len(chunks)
	}
	result := NewTextResult(pageText, 0)
	result.Complete = result.Complete && snapshot.Complete
	result.Metadata = map[string]string{
		"search_id":               snapshot.ID,
		"search_kind":             snapshot.Kind,
		"search_displayed":        strconv.Itoa(len(page)),
		"search_remaining":        strconv.Itoa(max(remaining, 0)),
		"search_incomplete":       strconv.FormatBool(!snapshot.Complete),
		"search_cursor_available": strconv.FormatBool(searchStore != nil),
	}
	if hasMore {
		result.Metadata["search_cursor"] = searchCursorString(searchCursor{Kind: searchKind(snapshot.Kind), ID: snapshot.ID, Offset: nextOffset})
	}
	return MarkUntrusted(result, snapshot.Kind)
}

func searchPageChunks(items []search.Item, capPerFile int, grouped, patternGrouped bool) [][]search.Item {
	if !grouped {
		chunks := make([][]search.Item, len(items))
		for i := range items {
			chunks[i] = []search.Item{items[i]}
		}
		return chunks
	}
	return groupedSearchChunks(items, capPerFile, patternGrouped)
}

func selectSearchPage(snapshot search.Snapshot, chunks [][]search.Item, offset, size int, cursorAvailable, grouped bool) ([]search.Item, int) {
	page := make([]search.Item, 0, min(size, len(snapshot.Items)))
	nextOffset := offset
	estimate := newSearchPageSizer()
	for i := offset; i < len(chunks) && len(page) < size; i++ {
		chunk := chunks[i]
		if len(page) > 0 && len(page)+len(chunk) > size {
			break
		}
		candidate := estimate
		candidate.add(snapshot, chunk, grouped)
		if candidate.bytes > searchPreviewBytes {
			break
		}
		estimate = candidate
		page = append(page, chunk...)
		nextOffset = i + 1
	}
	return page, nextOffset
}

type searchPageSizer struct {
	bytes       int
	lastPath    string
	lastPattern int
}

func newSearchPageSizer() searchPageSizer {
	return searchPageSizer{bytes: 256}
}

func (s *searchPageSizer) add(snapshot search.Snapshot, items []search.Item, grouped bool) {
	if !grouped {
		for _, item := range items {
			s.bytes += len(item.Path) + 1
		}
		return
	}
	for _, item := range items {
		if len(snapshot.Patterns) > 1 && item.Pattern != s.lastPattern {
			s.bytes += len(searchPatternHeader(snapshot, item.Pattern))
			s.lastPattern = item.Pattern
			s.lastPath = ""
		}
		if item.Path != s.lastPath {
			s.bytes += len(item.Path) + 3
			s.lastPath = item.Path
		}
		s.bytes += len(item.Text) + 12
	}
}

func searchPageItemsBefore(chunks [][]search.Item, end int) int {
	end = min(max(end, 0), len(chunks))
	total := 0
	for _, chunk := range chunks[:end] {
		total += len(chunk)
	}
	return total
}

func groupedSearchChunks(items []search.Item, capPerFile int, patternGrouped bool) [][]search.Item {
	if capPerFile <= 0 {
		return [][]search.Item{slices.Clone(items)}
	}
	chunks := make([][]search.Item, 0)
	for i := 0; i < len(items); {
		end := i + 1
		for end < len(items) && items[end].Path == items[i].Path && (!patternGrouped || items[end].Pattern == items[i].Pattern) {
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
	sizer := newSearchPageSizer()
	sizer.add(snapshot, items, grouped)
	b.Grow(sizer.bytes)
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
			lastPattern := 0
			for _, item := range items {
				if len(snapshot.Patterns) > 1 && item.Pattern != lastPattern {
					if lastPattern != 0 {
						b.WriteByte('\n')
					}
					b.WriteString(searchPatternHeader(snapshot, item.Pattern))
					lastPattern = item.Pattern
					lastPath = ""
				}
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

func searchPatternHeader(snapshot search.Snapshot, pattern int) string {
	if pattern <= 0 || pattern > len(snapshot.Patterns) {
		return "pattern:\n"
	}
	return fmt.Sprintf("pattern %q:\n", snapshot.Patterns[pattern-1])
}

package tools

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/sacca97/ghg/internal/observation"
)

const (
	defaultReadLines   = 200
	maxReadLines       = 1000
	maxReadBytes       = 32 << 10
	maxReadLineBytes   = 1 << 20
	maxObservationPath = 4 << 10
	readHeaderBudget   = 128
)

func runObservedRead(ctx context.Context, args struct {
	Path   string `json:"path"`
	Offset int    `json:"offset"`
	Limit  int    `json:"limit"`
}) (ToolResult, error) {
	if err := ctx.Err(); err != nil {
		return ToolResult{}, err
	}
	if strings.TrimSpace(args.Path) == "" {
		return ToolResult{}, fmt.Errorf("path is required")
	}
	start := args.Offset
	if start <= 0 {
		start = 1
	}
	limit := args.Limit
	if limit <= 0 {
		limit = defaultReadLines
	}
	if limit > maxReadLines {
		limit = maxReadLines
	}

	canonical, err := canonicalObservationPath(args.Path)
	if err != nil {
		return ToolResult{}, err
	}
	f, err := os.Open(canonical)
	if err != nil {
		return ToolResult{}, err
	}
	defer func() { _ = f.Close() }()

	reader := bufio.NewReaderSize(f, 64<<10)
	var numbered strings.Builder
	var content strings.Builder
	payloadBudget := maxReadBytes - len(canonical) - readHeaderBudget
	if payloadBudget <= 0 {
		return ToolResult{}, fmt.Errorf("read path leaves no room within the %d-byte output budget", maxReadBytes)
	}
	lineNo := 0
	selected := 0
	nextOffset := 0
	limitedByBytes := false
	for {
		if err := ctx.Err(); err != nil {
			return ToolResult{}, err
		}
		line, eof, err := readCompleteLine(reader, maxReadLineBytes)
		if err != nil {
			return ToolResult{}, fmt.Errorf("read %s: %w", args.Path, err)
		}
		if line == nil && eof {
			break
		}
		lineNo++
		if lineNo < start {
			if eof {
				break
			}
			continue
		}
		if selected >= limit {
			nextOffset = lineNo
			break
		}
		numberedLine := fmt.Sprintf("%d\t", lineNo)
		if numbered.Len()+len(numberedLine)+len(line) > payloadBudget {
			if selected == 0 {
				return ToolResult{}, fmt.Errorf("read line %d exceeds the %d-byte read budget; request a narrower range or use a dedicated tool", lineNo, maxReadBytes)
			}
			limitedByBytes = true
			nextOffset = lineNo
			break
		}
		numbered.WriteString(numberedLine)
		numbered.Write(line)
		content.Write(line)
		selected++
		if eof {
			break
		}
		if selected >= limit {
			nextOffset = lineNo + 1
			break
		}
	}
	if lineNo < start || selected == 0 {
		return ToolResult{}, fmt.Errorf("offset %d past end of file (%d lines)", args.Offset, lineNo)
	}
	if nextOffset > 0 && !limitedByBytes && selected < limit {
		nextOffset = 0
	}
	if limitedByBytes && nextOffset <= 0 {
		nextOffset = lineNo
	}

	id := observation.NewID()
	header := fmt.Sprintf("[observation %s path=%s lines=%d-%d next_offset=%d]\n", id, filepath.ToSlash(canonical), start, start+selected-1, nextOffset)
	raw := header + numbered.String()
	if len(raw) > maxObservationPath+maxReadBytes {
		return ToolResult{}, fmt.Errorf("read result exceeded its bounded output budget")
	}
	record := observation.Record{
		ID:          id,
		Path:        canonical,
		StartLine:   start,
		EndLine:     start + selected - 1,
		NextOffset:  nextOffset,
		IssuedBytes: len(raw),
		Content:     content.String(),
		Complete:    !limitedByBytes,
	}
	sessionID, store := observationContextFor(ctx)
	if store != nil {
		if err := store.Save(ctx, sessionID, record); err != nil {
			return ToolResult{}, fmt.Errorf("persist read observation: %w", err)
		}
	}
	result := TextResultWithSize(raw, raw, int64(len(raw)), true, 0)
	result.Metadata = map[string]string{
		"observation_id":          id,
		"observation_path":        canonical,
		"observation_start":       fmt.Sprint(record.StartLine),
		"observation_end":         fmt.Sprint(record.EndLine),
		"observation_next_offset": fmt.Sprint(record.NextOffset),
		"observation_complete":    fmt.Sprint(record.Complete),
	}
	return MarkUntrusted(result, "read"), nil
}

func canonicalObservationPath(name string) (string, error) {
	if len(name) > maxObservationPath {
		return "", fmt.Errorf("path exceeds %d-byte limit", maxObservationPath)
	}
	abs, err := filepath.Abs(name)
	if err != nil {
		return "", fmt.Errorf("resolve %s: %w", name, err)
	}
	info, err := os.Stat(abs)
	if err != nil {
		return "", err
	}
	if !info.Mode().IsRegular() {
		return "", fmt.Errorf("%s is not a regular file", name)
	}
	canonical, err := filepath.EvalSymlinks(abs)
	if err != nil {
		return "", fmt.Errorf("resolve %s: %w", name, err)
	}
	return filepath.Clean(canonical), nil
}

func readCompleteLine(reader *bufio.Reader, limit int) ([]byte, bool, error) {
	var line []byte
	for {
		chunk, err := reader.ReadSlice('\n')
		if len(line)+len(chunk) > limit {
			return nil, false, fmt.Errorf("line exceeds %d-byte limit", limit)
		}
		line = append(line, chunk...)
		switch err {
		case nil:
			return line, false, nil
		case bufio.ErrBufferFull:
			continue
		case io.EOF:
			if len(line) == 0 {
				return nil, true, nil
			}
			return line, true, nil
		default:
			return nil, false, err
		}
	}
}

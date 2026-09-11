package tools

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"github.com/sacca97/ghg/internal/models"
	"github.com/sacca97/ghg/internal/observation"
	"github.com/sacca97/ghg/internal/sandbox"
)

const (
	defaultReadLines     = 250
	maxReadLines         = 1000
	maxReadBytes         = 16 << 10
	maxReadLineBytes     = 1 << 20
	maxObservationPath   = 4 << 10
	readHeaderBudget     = 128
	maxReadRanges        = 32
	maxBatchReadBytes    = 32 << 10
	maxReadWorkers       = 4
	maxBatchFailureBytes = 512
)

type readRangeArgs struct {
	Path   string `json:"path"`
	Offset int    `json:"offset"`
	Limit  int    `json:"limit"`
}

type readArgs struct {
	readRangeArgs
	Ranges []readRangeArgs `json:"ranges"`
}

type pendingObservedRead struct {
	result    ToolResult
	record    observation.Record
	canonical string
	duplicate bool
}

func readTool() Tool {
	return resultTool(models.NewTool("read",
		"Read one or more bounded ranges of complete lines and issue observation ids for later range-authorized edits. Prefer the ranges form for every request, including one range; use the legacy path/offset/limit form only for compatibility. Use offset/limit to continue a file. If a batched result lists unprocessed ranges, retry only those ranges.",
		fmt.Sprintf(`{"type":"object","properties":{"path":{"type":"string","description":"Legacy single-file form; prefer ranges even for one file"},"offset":{"type":"number","description":"1-based line to start from (default 1)"},"limit":{"type":"number","description":"Max complete lines to return (default 250, maximum 1000)"},"ranges":{"type":"array","minItems":1,"maxItems":%d,"description":"Preferred form: independent ranges returned in request order; use a one-element array for one range","items":{"type":"object","properties":{"path":{"type":"string","description":"Path to the file"},"offset":{"type":"number","description":"1-based line to start from (default 1)"},"limit":{"type":"number","description":"Max complete lines to return (default 250, maximum 1000)"}},"required":["path"]}}},"oneOf":[{"required":["ranges"],"not":{"required":["path"]}},{"required":["path"],"not":{"required":["ranges"]}}]}`, maxReadRanges)),
		runReadResult)
}

func runReadResult(ctx context.Context, args json.RawMessage) (ToolResult, error) {
	var a readArgs
	if err := json.Unmarshal(args, &a); err != nil {
		return ToolResult{}, err
	}
	if a.Ranges != nil {
		if len(a.Ranges) == 0 {
			return ToolResult{}, fmt.Errorf("read ranges cannot be empty")
		}
		if len(a.Ranges) > maxReadRanges {
			return ToolResult{}, fmt.Errorf("read supports at most %d ranges", maxReadRanges)
		}
		return runObservedReadBatch(ctx, a.Ranges)
	}
	return runObservedRead(ctx, a.readRangeArgs)
}

func runObservedRead(ctx context.Context, args readRangeArgs) (ToolResult, error) {
	if err := ctx.Err(); err != nil {
		return ToolResult{}, err
	}
	if strings.TrimSpace(args.Path) == "" {
		return ToolResult{}, fmt.Errorf("path is required")
	}
	pending, err := prepareObservedRead(ctx, args)
	if err != nil {
		return ToolResult{}, err
	}
	if err := persistObservedRead(ctx, pending); err != nil {
		return ToolResult{}, err
	}
	return pending.result, nil
}

func prepareObservedRead(ctx context.Context, args readRangeArgs) (pendingObservedRead, error) {
	canonical, err := authorizedObservationPath(ctx, args.Path, sandbox.AccessRead, false)
	if err != nil {
		return pendingObservedRead{}, err
	}
	f, err := os.Open(canonical)
	if err != nil {
		return pendingObservedRead{}, err
	}
	defer func() { _ = f.Close() }()

	return prepareObservedContent(ctx, canonical, args.Path, f, args.Offset, args.Limit)
}

func runObservedReadBatch(ctx context.Context, ranges []readRangeArgs) (ToolResult, error) {
	if err := ctx.Err(); err != nil {
		return ToolResult{}, err
	}
	type outcome struct {
		read pendingObservedRead
		err  error
	}
	outcomes := make([]outcome, len(ranges))
	jobs := make(chan int)
	workers := min(len(ranges), maxReadWorkers)
	var wg sync.WaitGroup
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for index := range jobs {
				read, err := prepareObservedRead(ctx, ranges[index])
				outcomes[index] = outcome{read: read, err: err}
			}
		}()
	}
	for index := range ranges {
		select {
		case jobs <- index:
		case <-ctx.Done():
			close(jobs)
			wg.Wait()
			return ToolResult{}, ctx.Err()
		}
	}
	close(jobs)
	wg.Wait()
	if err := ctx.Err(); err != nil {
		return ToolResult{}, err
	}

	var output strings.Builder
	selected := 0
	complete := true
	observations := make([]map[string]any, 0, len(ranges))
	failureCount := 0
	for _, outcome := range outcomes {
		if outcome.err != nil {
			failureCount++
		}
	}
	contentBudget := maxBatchReadBytes - failureCount*maxBatchFailureBytes
	var successfulBytes int
	for _, outcome := range outcomes {
		if outcome.err == nil {
			successfulBytes += len(outcome.read.result.Preview)
		}
	}
	const omissionReserve = 8 << 10
	if successfulBytes > contentBudget {
		contentBudget -= omissionReserve
		if contentBudget < 0 {
			contentBudget = 0
		}
	}
	var omitted []int
	appendFailure := func(index int, message string) {
		complete = false
		path := strings.NewReplacer("\n", " ", "\r", " ").Replace(ranges[index].Path)
		message = strings.NewReplacer("\n", " ", "\r", " ").Replace(message)
		note := fmt.Sprintf("[range %d path=%s error=%s]\n", index+1, path, message)
		if len(note) > maxBatchFailureBytes {
			note = note[:maxBatchFailureBytes-len("...\n")] + "...\n"
		}
		if output.Len()+len(note) <= maxBatchReadBytes {
			output.WriteString(note)
		}
	}
	for index, outcome := range outcomes {
		if outcome.err != nil {
			appendFailure(index, outcome.err.Error())
			continue
		}
		if !outcome.read.record.Complete {
			complete = false
		}
		text := outcome.read.result.Preview
		if output.Len()+len(text) > contentBudget {
			complete = false
			omitted = append(omitted, index)
			continue
		}
		if err := persistObservedRead(ctx, outcome.read); err != nil {
			appendFailure(index, err.Error())
			continue
		}
		output.WriteString(text)
		selected++
		observations = append(observations, map[string]any{
			"id":          outcome.read.record.ID,
			"path":        outcome.read.record.Path,
			"start_line":  outcome.read.record.StartLine,
			"end_line":    outcome.read.record.EndLine,
			"next_offset": outcome.read.record.NextOffset,
			"complete":    outcome.read.record.Complete,
		})
	}
	omissionNote := ""
	if len(omitted) > 0 {
		var note strings.Builder
		note.WriteString("[unprocessed ranges:")
		for _, index := range omitted {
			rangeArgs := ranges[index]
			path := strings.NewReplacer("\n", " ", "\r", " ").Replace(rangeArgs.Path)
			if len(path) > 128 {
				path = path[:128] + "..."
			}
			start := rangeArgs.Offset
			if start <= 0 {
				start = 1
			}
			limit := rangeArgs.Limit
			if limit <= 0 {
				limit = defaultReadLines
			}
			fmt.Fprintf(&note, " %d:%s:%d-%d", index+1, path, start, start+limit-1)
		}
		note.WriteString("]\n")
		if note.Len() > omissionReserve {
			noteStr := strings.ToValidUTF8(note.String()[:omissionReserve-len("...\n")], "") + "...\n"
			note.Reset()
			note.WriteString(noteStr)
		}
		omissionNote = note.String()
	}
	raw := omissionNote + output.String()
	result := TextResultWithSize(raw, raw, int64(len(raw)), complete, 0)
	if selected == 0 {
		result.ExitCode = 1
	}
	encoded, err := json.Marshal(observations)
	if err != nil {
		return ToolResult{}, fmt.Errorf("encode read observations: %w", err)
	}
	result.Metadata = map[string]string{
		"observations":      string(encoded),
		"observation_count": fmt.Sprint(selected),
	}
	return MarkUntrusted(result, "read"), nil
}

func readObservedContent(ctx context.Context, canonical, display string, r io.Reader, offset, limit int) (ToolResult, error) {
	pending, err := prepareObservedContent(ctx, canonical, display, r, offset, limit)
	if err != nil {
		return ToolResult{}, err
	}
	if err := persistObservedRead(ctx, pending); err != nil {
		return ToolResult{}, err
	}
	return pending.result, nil
}

func prepareObservedContent(ctx context.Context, canonical, display string, r io.Reader, offset, limit int) (pendingObservedRead, error) {
	if err := ctx.Err(); err != nil {
		return pendingObservedRead{}, err
	}
	start := offset
	if start <= 0 {
		start = 1
	}
	if limit <= 0 {
		limit = defaultReadLines
	}
	if limit > maxReadLines {
		limit = maxReadLines
	}

	reader := bufio.NewReaderSize(r, 64<<10)
	var numbered strings.Builder
	var content strings.Builder
	payloadBudget := maxReadBytes - len(canonical) - readHeaderBudget
	if payloadBudget <= 0 {
		return pendingObservedRead{}, fmt.Errorf("read path leaves no room within the %d-byte output budget", maxReadBytes)
	}
	lineNo := 0
	selected := 0
	nextOffset := 0
	limitedByBytes := false
	for {
		if err := ctx.Err(); err != nil {
			return pendingObservedRead{}, err
		}
		line, eof, err := readCompleteLine(reader, maxReadLineBytes)
		if err != nil {
			return pendingObservedRead{}, fmt.Errorf("read %s: %w", display, err)
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
				return pendingObservedRead{}, fmt.Errorf("read line %d exceeds the %d-byte read budget; request a narrower range or use a dedicated tool", lineNo, maxReadBytes)
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
	if lineNo == 0 && start == 1 {
		return pendingObservedRead{}, fmt.Errorf("%s is empty", display)
	}
	if lineNo < start || selected == 0 {
		return pendingObservedRead{}, fmt.Errorf("offset %d past end of file (%d lines)", offset, lineNo)
	}
	if nextOffset > 0 && !limitedByBytes && selected < limit {
		nextOffset = 0
	}
	if limitedByBytes && nextOffset <= 0 {
		nextOffset = lineNo
	}

	id := observation.NewID()
	sessionID, store := observationContextFor(ctx)
	isDuplicate := false
	if store != nil {
		if finder, ok := store.(interface {
			FindLatest(string, string, int, int) (observation.Record, bool)
		}); ok {
			if prior, found := finder.FindLatest(sessionID, canonical, start, start+selected-1); found && prior.Content == content.String() {
				id = prior.ID
				isDuplicate = true
			}
		}
	}
	header := fmt.Sprintf("[observation %s path=%s lines=%d-%d next_offset=%d]\n", id, filepath.ToSlash(canonical), start, start+selected-1, nextOffset)
	raw := header + numbered.String()
	if len(raw) > maxObservationPath+maxReadBytes {
		return pendingObservedRead{}, fmt.Errorf("read result exceeded its bounded output budget")
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
	result := TextResultWithSize(raw, raw, int64(len(raw)), true, 0)
	result.Metadata = map[string]string{
		"observation_id":          id,
		"observation_path":        canonical,
		"observation_start":       fmt.Sprint(record.StartLine),
		"observation_end":         fmt.Sprint(record.EndLine),
		"observation_next_offset": fmt.Sprint(record.NextOffset),
		"observation_complete":    fmt.Sprint(record.Complete),
	}
	return pendingObservedRead{
		result:    MarkUntrusted(result, "read"),
		record:    record,
		canonical: canonical,
		duplicate: isDuplicate,
	}, nil
}

func persistObservedRead(ctx context.Context, pending pendingObservedRead) error {
	sessionID, store := observationContextFor(ctx)
	if store != nil && !pending.duplicate {
		if err := store.Save(ctx, sessionID, pending.record); err != nil {
			return fmt.Errorf("persist read observation: %w", err)
		}
	}
	if runtime := RuntimeFromContext(ctx); runtime != nil && runtime.LanguageService != nil {
		runtime.LanguageService.Warm(ctx, pending.canonical)
	}
	return nil
}

func canonicalObservationPath(name string) (string, error) {
	if len(name) > maxObservationPath {
		return "", fmt.Errorf("path exceeds %d-byte limit", maxObservationPath)
	}
	abs, err := filepath.Abs(name)
	if err != nil {
		return "", fmt.Errorf("resolve %s: %w", name, err)
	}
	canonical, err := filepath.EvalSymlinks(abs)
	if err != nil {
		return "", fmt.Errorf("resolve %s: %w", name, err)
	}
	canonical = filepath.Clean(canonical)
	if err := requireRegularFile(canonical, name); err != nil {
		return "", err
	}
	return canonical, nil
}

func authorizedObservationPath(ctx context.Context, name string, access sandbox.Access, allowMissing bool) (string, error) {
	if runtime := RuntimeFromContext(ctx); runtime != nil && runtime.Policy != nil {
		canonical, err := runtime.Policy.Authorize(name, access, allowMissing)
		if err != nil {
			return "", err
		}
		if err := requireRegularFile(canonical, name); err != nil {
			return "", err
		}
		return canonical, nil
	}
	return canonicalObservationPath(name)
}

func requireRegularFile(path, display string) error {
	info, err := os.Stat(path)
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() {
		return fmt.Errorf("%s is not a regular file", display)
	}
	return nil
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

package tools

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"

	"github.com/sacca97/ghg/internal/models"
	"github.com/sacca97/ghg/internal/observation"
	"github.com/sacca97/ghg/internal/sandbox"
)

type editRequest struct {
	Mode       string          `json:"mode"`
	Path       string          `json:"path"`
	OldString  string          `json:"old_string"`
	NewString  string          `json:"new_string"`
	ReplaceAll bool            `json:"replace_all"`
	Edits      []editOperation `json:"edits"`
}

type editOperation struct {
	Observation string `json:"observation"`
	Path        string `json:"path"`
	StartLine   int    `json:"start_line"`
	EndLine     int    `json:"end_line"`
	Operation   string `json:"operation"`
	Content     string `json:"content"`
	NewContent  string `json:"new_content"`
}

func editTool() Tool {
	return resultTool(models.NewTool("edit",
		"Apply one or more observed line-range edits atomically. Each primary edit references a read observation; use mode=exact only for temporary unique old_string compatibility.",
		`{"type":"object","properties":{"mode":{"type":"string","enum":["observed","exact"],"description":"observed is the primary range-authorized mode; exact is compatibility mode"},"edits":{"type":"array","description":"Observed operations to apply atomically across one or more files","items":{"type":"object","properties":{"observation":{"type":"string"},"path":{"type":"string"},"start_line":{"type":"integer"},"end_line":{"type":"integer"},"operation":{"type":"string","enum":["replace","delete","insert_before","insert_after"],"description":"Defaults to replace"},"content":{"type":"string","description":"Replacement or insertion text; replace requires non-empty content"}},"required":["observation","path","start_line","end_line"]}},"path":{"type":"string","description":"Compatibility-mode file path"},"old_string":{"type":"string","description":"Compatibility-mode exact text"},"new_string":{"type":"string","description":"Compatibility-mode replacement"},"replace_all":{"type":"boolean","description":"Compatibility-mode replace every occurrence"}},"required":["mode"]}`),
		runEdit)
}

func runEdit(ctx context.Context, args json.RawMessage) (ToolResult, error) {
	var request editRequest
	if err := json.Unmarshal(args, &request); err != nil {
		return ToolResult{}, err
	}
	switch strings.ToLower(strings.TrimSpace(request.Mode)) {
	case "exact":
		return runExactEdit(ctx, request)
	case "observed":
		return runObservedEdit(ctx, request)
	case "":
		return ToolResult{}, errors.New("edit mode is required: use mode=observed with edits, or mode=exact for compatibility")
	default:
		return ToolResult{}, fmt.Errorf("unsupported edit mode %q", request.Mode)
	}
}

func editDiff(oldS, newS string) string {
	o := strings.Split(strings.TrimSuffix(oldS, "\n"), "\n")
	n := strings.Split(strings.TrimSuffix(newS, "\n"), "\n")
	p := 0
	for p < len(o) && p < len(n) && o[p] == n[p] {
		p++
	}
	s := 0
	for s < len(o)-p && s < len(n)-p && o[len(o)-1-s] == n[len(n)-1-s] {
		s++
	}
	if p == len(o) && p == len(n) {
		return ""
	}
	var b strings.Builder
	ctxLine := func(prefix, line string) {
		if len(line) > 200 {
			line = line[:200] + "…"
		}
		b.WriteString(prefix + line + "\n")
	}
	if p > 0 {
		ctxLine(" ", o[p-1])
	}
	writeCappedDiffLines(&b, "-", o[p:len(o)-s])
	writeCappedDiffLines(&b, "+", n[p:len(n)-s])
	if s > 0 {
		ctxLine(" ", o[len(o)-1])
	}
	return strings.TrimSuffix(b.String(), "\n")
}

const maxEditDiffLines = 40

func writeCappedDiffLines(b *strings.Builder, prefix string, lines []string) {
	if len(lines) <= maxEditDiffLines {
		for _, line := range lines {
			if len(line) > 200 {
				line = line[:200] + "…"
			}
			b.WriteString(prefix + line + "\n")
		}
		return
	}
	head := maxEditDiffLines / 2
	tail := maxEditDiffLines - head
	for _, line := range lines[:head] {
		if len(line) > 200 {
			line = line[:200] + "…"
		}
		b.WriteString(prefix + line + "\n")
	}
	fmt.Fprintf(b, "%s... [%d lines omitted]\n", prefix, len(lines)-maxEditDiffLines)
	for _, line := range lines[len(lines)-tail:] {
		if len(line) > 200 {
			line = line[:200] + "…"
		}
		b.WriteString(prefix + line + "\n")
	}
}

func lspDiagnostics(ctx context.Context, path string) string {
	runtime := RuntimeFromContext(ctx)
	if runtime == nil || runtime.LanguageService == nil {
		return ""
	}
	return runtime.LanguageService.WaitDiagnostics(ctx, path)
}

func runExactEdit(ctx context.Context, request editRequest) (ToolResult, error) {
	if request.Path == "" || request.OldString == "" {
		return ToolResult{}, errors.New("exact edit requires path and a non-empty old_string")
	}
	canonical, err := authorizedObservationPath(ctx, request.Path, sandbox.AccessWrite, false)
	if err != nil {
		return ToolResult{}, err
	}
	if deny := checkGate(ctx, "edit", canonical); deny != "" {
		return ToolResult{}, errors.New(deny)
	}
	original, err := os.ReadFile(canonical)
	if err != nil {
		return ToolResult{}, err
	}
	info, err := os.Stat(canonical)
	if err != nil {
		return ToolResult{}, err
	}
	n := bytes.Count(original, []byte(request.OldString))
	if n == 0 {
		return ToolResult{}, fmt.Errorf("old_string not found in %s", canonical)
	}
	if n > 1 && !request.ReplaceAll {
		return ToolResult{}, fmt.Errorf("old_string appears %d times in %s; make it unique or set replace_all", n, canonical)
	}
	updated := bytes.ReplaceAll(original, []byte(request.OldString), []byte(request.NewString))
	if err := publishEditFiles(ctx, []editPublication{{path: canonical, original: original, updated: updated, mode: info.Mode()}}); err != nil {
		return ToolResult{}, err
	}
	final, readErr := os.ReadFile(canonical)
	if readErr != nil {
		return ToolResult{}, fmt.Errorf("read back %s: %w", canonical, readErr)
	}
	out := fmt.Sprintf("Replaced %d occurrence(s) in %s", n, canonical)
	if d := editDiff(string(original), string(final)); d != "" {
		out += "\n```diff\n" + d + "\n```"
	}
	out += lspDiagnostics(ctx, canonical)
	return textResult(out, Truncate(out), 0), nil
}

type observedEdit struct {
	operation string
	content   string
	start     int
	end       int
	line      int
	path      string
	target    []byte
	opIndex   int
}

type editFilePlan struct {
	path       string
	original   []byte
	updated    []byte
	mode       os.FileMode
	operations []observedEdit
}

type editPublication struct {
	path     string
	original []byte
	updated  []byte
	mode     os.FileMode
}

// renameEditFile is a narrow publication seam for deterministic rollback
// tests. Production publication retains os.Rename's same-filesystem behavior.
var renameEditFile = os.Rename

func runObservedEdit(ctx context.Context, request editRequest) (ToolResult, error) {
	if len(request.Edits) == 0 {
		return ToolResult{}, errors.New("observed edit requires a non-empty edits array")
	}
	sessionID, store := observationContextFor(ctx)
	if store == nil {
		return ToolResult{}, errors.New("observed edit requires a fresh read observation in the active session")
	}

	plans := make(map[string]*editFilePlan)
	canonicalPaths := make([]string, 0, len(request.Edits))
	fail := func(index int, err error) (ToolResult, error) {
		return ToolResult{}, fmt.Errorf("edit %d: %w", index+1, err)
	}

	for i, operation := range request.Edits {
		if err := ctx.Err(); err != nil {
			return ToolResult{}, err
		}
		if operation.Path == "" || operation.Observation == "" {
			return fail(i, errors.New("each observed edit needs observation and path"))
		}
		if operation.StartLine <= 0 || operation.EndLine < operation.StartLine {
			return fail(i, fmt.Errorf("invalid authorized line range %d-%d", operation.StartLine, operation.EndLine))
		}
		opName := strings.ToLower(strings.TrimSpace(operation.Operation))
		if opName == "" {
			opName = "replace"
		}
		switch opName {
		case "replace", "delete", "insert_before", "insert_after":
		default:
			return fail(i, fmt.Errorf("unsupported observed edit operation %q", operation.Operation))
		}
		canonical, err := authorizedObservationPath(ctx, operation.Path, sandbox.AccessWrite, false)
		if err != nil {
			return fail(i, err)
		}
		var record observation.Record
		if operation.Observation != "" {
			var loadErr error
			record, loadErr = store.Load(ctx, sessionID, operation.Observation)
			if loadErr != nil || record.ID == "" {
				return fail(i, fmt.Errorf("observation %q not found for %s: read the file first to obtain a valid observation ID", operation.Observation, canonical))
			}
			if record.SessionID != "" && record.SessionID != sessionID {
				return fail(i, errors.New("observation belongs to another session"))
			}
		}
		recordPath := ""
		if record.Path != "" {
			recordPath, err = authorizedObservationPath(ctx, record.Path, sandbox.AccessRead, true)
			if err != nil {
				return fail(i, err)
			}
		}
		if recordPath != canonical {
			return fail(i, fmt.Errorf("observation %q covers %s, not %s", operation.Observation, record.Path, canonical))
		}
		if operation.StartLine < record.StartLine || operation.EndLine > record.EndLine {
			return fail(i, fmt.Errorf("observation %q covers lines %d-%d, but requested range is %d-%d", operation.Observation, record.StartLine, record.EndLine, operation.StartLine, operation.EndLine))
		}
		content := operation.Content
		if content == "" && operation.NewContent != "" {
			content = operation.NewContent
		}
		if opName == "replace" && content == "" {
			return fail(i, errors.New("replace requires non-empty content; use delete to remove the selected range"))
		}
		plan := plans[canonical]
		if plan == nil {
			if deny := checkGate(ctx, "edit", canonical); deny != "" {
				return fail(i, errors.New(deny))
			}
			original, err := os.ReadFile(canonical)
			if err != nil {
				return fail(i, err)
			}
			info, err := os.Stat(canonical)
			if err != nil {
				return fail(i, err)
			}
			plan = &editFilePlan{path: canonical, original: original, mode: info.Mode()}
			plans[canonical] = plan
			canonicalPaths = append(canonicalPaths, canonical)
		}
		resolved, err := resolveObservedEdit(operation, opName, content, record, plan.original)
		if err != nil {
			return fail(i, err)
		}
		resolved.opIndex = i
		plan.operations = append(plan.operations, resolved)
	}
	// The permission calls above intentionally happen before any staged or
	// published bytes. Sort the paths now so every caller's output is stable.
	sort.Strings(canonicalPaths)
	for _, path := range canonicalPaths {
		plan := plans[path]
		for i := 0; i < len(plan.operations); i++ {
			for j := i + 1; j < len(plan.operations); j++ {
				if rangesIntersect(plan.operations[i].start, plan.operations[i].end, plan.operations[j].start, plan.operations[j].end) {
					return fail(plan.operations[j].opIndex, fmt.Errorf("intersects edit %d in %s", plan.operations[i].opIndex+1, path))
				}
			}
		}
	}

	for _, path := range canonicalPaths {
		plan := plans[path]
		updated, err := applyObservedOperations(plan.original, plan.operations)
		if err != nil {
			return ToolResult{}, fmt.Errorf("%s: %w", path, err)
		}
		plan.updated = updated
	}

	publications := make([]editPublication, 0, len(canonicalPaths))
	for _, path := range canonicalPaths {
		plan := plans[path]
		publications = append(publications, editPublication{path: path, original: plan.original, updated: plan.updated, mode: plan.mode})
	}
	if err := publishEditFiles(ctx, publications); err != nil {
		return ToolResult{}, err
	}

	var out strings.Builder
	var retained strings.Builder
	for _, path := range canonicalPaths {
		plan := plans[path]
		header := fmt.Sprintf("Edited %s (%d operation(s))\n", path, len(plan.operations))
		out.WriteString(header)
		retained.WriteString(header)
		out.WriteString("readback:\n")
		retained.WriteString("readback:\n")
		final, err := os.ReadFile(path)
		if err != nil {
			if os.IsNotExist(err) {
				out.WriteString("(file was removed before readback)\n")
				retained.WriteString("(file was removed before readback)\n")
			} else {
				out.WriteString("(readback failed: " + err.Error() + ")\n")
				retained.WriteString("(readback failed: " + err.Error() + ")\n")
			}
		} else {
			startByte := plan.operations[0].start
			for _, op := range plan.operations[1:] {
				if op.start < startByte {
					startByte = op.start
				}
			}
			startLine := 1 + bytes.Count(final[:min(startByte, len(final))], []byte{'\n'})
			readRes, readErr := readObservedContent(ctx, path, path, bytes.NewReader(final), startLine, maxEditReadbackLines)
			if readErr != nil {
				note := "(no readback observation issued; run read before editing again)\n"
				out.WriteString(note)
				retained.WriteString(note)
				rb := editReadback(final, plan.operations)
				out.WriteString(rb)
				retained.WriteString(rb)
			} else {
				out.WriteString(readRes.Preview)
				retained.WriteString(readRes.Preview)
				if !strings.HasSuffix(readRes.Preview, "\n") {
					out.WriteByte('\n')
					retained.WriteByte('\n')
				}
			}
			if d := editDiff(string(plan.original), string(final)); d != "" {
				retained.WriteString("```diff\n")
				retained.WriteString(d)
				retained.WriteString("\n```\n")
			}
			diag := lspDiagnostics(ctx, path)
			out.WriteString(diag)
			retained.WriteString(diag)
		}
	}
	preview := strings.TrimSuffix(out.String(), "\n")
	retainedStr := strings.TrimSuffix(retained.String(), "\n")
	return textResult(retainedStr, Truncate(preview), 0), nil
}

// publishEditFiles stages every replacement before renaming any of them, then
// publishes in lexical order.
func publishEditFiles(ctx context.Context, files []editPublication) error {
	ordered := append([]editPublication(nil), files...)
	sort.Slice(ordered, func(i, j int) bool { return ordered[i].path < ordered[j].path })
	staged := make(map[string]string, len(ordered))
	cleanup := func() {
		for _, name := range staged {
			_ = os.Remove(name)
		}
	}
	for _, file := range ordered {
		if err := ctx.Err(); err != nil {
			cleanup()
			return err
		}
		tmp, err := stageEditFile(file.path, file.updated, file.mode)
		if err != nil {
			cleanup()
			return fmt.Errorf("stage %s: %w", file.path, err)
		}
		staged[file.path] = tmp
	}
	published := make([]editPublication, 0, len(ordered))
	for _, file := range ordered {
		if err := verifyEditFile(file); err != nil {
			rollbackErr := rollbackEditFiles(published)
			cleanup()
			if rollbackErr != nil {
				return fmt.Errorf("edit publication at %s aborted: %w; rollback failed: %v", file.path, err, rollbackErr)
			}
			if len(published) > 0 {
				return fmt.Errorf("edit publication at %s aborted: %w; published files were rolled back", file.path, err)
			}
			return fmt.Errorf("edit publication at %s aborted: %w", file.path, err)
		}
		if err := renameEditFile(staged[file.path], file.path); err != nil {
			_ = os.Remove(staged[file.path])
			rollbackErr := rollbackEditFiles(published)
			cleanup()
			if rollbackErr != nil {
				return fmt.Errorf("partial edit publication at %s: %w; rollback failed: %v", file.path, err, rollbackErr)
			}
			return fmt.Errorf("partial edit publication at %s: %w; published files were rolled back", file.path, err)
		}
		delete(staged, file.path)
		published = append(published, file)
	}
	cleanup()
	return nil
}

func verifyEditFile(file editPublication) error {
	current, err := os.ReadFile(file.path)
	if err != nil {
		return fmt.Errorf("file changed or was removed; re-read and retry: %w", err)
	}
	if !bytes.Equal(current, file.original) {
		return errors.New("file changed since it was read; re-read and retry")
	}
	return nil
}

func resolveObservedEdit(operation editOperation, opName, content string, record observation.Record, current []byte) (observedEdit, error) {
	observationLines := lineSpans([]byte(record.Content))
	if len(observationLines) == 0 {
		return observedEdit{}, fmt.Errorf("observation %s contains no complete lines", record.ID)
	}
	relStart := operation.StartLine - record.StartLine + 1
	relEnd := operation.EndLine - record.StartLine + 1
	expectedStart, expectedEnd, err := spanRange(observationLines, relStart, relEnd)
	if err != nil {
		return observedEdit{}, fmt.Errorf("observation %s: %w", record.ID, err)
	}
	expected := []byte(record.Content)[expectedStart:expectedEnd]
	currentLines := lineSpans(current)
	currentStart, currentEnd, err := spanRange(currentLines, operation.StartLine, operation.EndLine)
	if err == nil && bytes.Equal(current[currentStart:currentEnd], expected) {
		// Validate only the requested range. An unrelated line in the same
		// observation may change without invalidating this edit.
	} else {
		currentStart, currentEnd, err = uniqueByteBlock(current, expected)
		if err != nil {
			return observedEdit{}, fmt.Errorf("observation %s cannot be applied: %w; run read again", record.ID, err)
		}
	}
	relCurrentStart, relCurrentEnd := currentStart, currentEnd
	start, end := relCurrentStart, relCurrentEnd
	if opName == "insert_before" {
		end = start
	} else if opName == "insert_after" {
		start = end
	}
	return observedEdit{
		operation: opName,
		content:   content,
		start:     start,
		end:       end,
		line:      operation.StartLine,
		path:      record.Path,
		target:    bytes.Clone(current[relCurrentStart:relCurrentEnd]),
	}, nil
}

func uniqueByteBlock(data, expected []byte) (int, int, error) {
	if len(expected) == 0 {
		return 0, 0, errors.New("empty observation cannot be relocated")
	}
	locations := make([]int, 0, 8)
	count := 0
	for offset := 0; offset <= len(data)-len(expected); {
		index := bytes.Index(data[offset:], expected)
		if index < 0 {
			break
		}
		first := offset + index
		count++
		if len(locations) < cap(locations) {
			locations = append(locations, first)
		}
		offset = first + len(expected)
	}
	if count == 0 {
		return 0, 0, errors.New("issued bytes are no longer present")
	}
	if count > 1 {
		lines := make([]string, len(locations))
		for i, location := range locations {
			lines[i] = fmt.Sprint(1 + bytes.Count(data[:location], []byte{'\n'}))
		}
		if count > len(locations) {
			lines = append(lines, "…")
		}
		return 0, 0, fmt.Errorf("issued bytes occur more than once (%d matches at lines %s)", count, strings.Join(lines, ", "))
	}
	first := locations[0]
	return first, first + len(expected), nil
}

type lineSpan struct{ start, end int }

func lineSpans(data []byte) []lineSpan {
	if len(data) == 0 {
		return nil
	}
	spans := make([]lineSpan, 0, bytes.Count(data, []byte{'\n'})+1)
	start := 0
	for i, b := range data {
		if b == '\n' {
			spans = append(spans, lineSpan{start: start, end: i + 1})
			start = i + 1
		}
	}
	if start < len(data) {
		spans = append(spans, lineSpan{start: start, end: len(data)})
	}
	return spans
}

func spanRange(spans []lineSpan, startLine, endLine int) (int, int, error) {
	if startLine <= 0 || endLine < startLine || endLine > len(spans) {
		return 0, 0, fmt.Errorf("line range %d-%d is outside the issued content", startLine, endLine)
	}
	return spans[startLine-1].start, spans[endLine-1].end, nil
}

func rangesIntersect(aStart, aEnd, bStart, bEnd int) bool {
	if aStart == aEnd && bStart == bEnd {
		return aStart == bStart
	}
	if aStart == aEnd {
		return aStart > bStart && aStart < bEnd
	}
	if bStart == bEnd {
		return bStart > aStart && bStart < aEnd
	}
	return aStart < bEnd && bStart < aEnd
}

func applyObservedOperations(original []byte, edits []observedEdit) ([]byte, error) {
	ordered := slices.Clone(edits)
	sort.SliceStable(ordered, func(i, j int) bool {
		if ordered[i].start != ordered[j].start {
			return ordered[i].start > ordered[j].start
		}
		return ordered[i].end > ordered[j].end
	})
	eol := detectLineEnding(original)
	updated := bytes.Clone(original)
	for _, edit := range ordered {
		if edit.start < 0 || edit.end < edit.start || edit.end > len(updated) {
			return nil, errors.New("edit range is outside the immutable original")
		}
		replacement := editReplacement(edit, edit.target, eol)
		next := make([]byte, 0, len(updated)-edit.end+edit.start+len(replacement))
		next = append(next, updated[:edit.start]...)
		next = append(next, replacement...)
		next = append(next, updated[edit.end:]...)
		updated = next
	}
	return updated, nil
}

func editReplacement(edit observedEdit, target []byte, eol string) []byte {
	content := normalizeLineEndings(edit.content, eol)
	switch edit.operation {
	case "delete":
		return nil
	case "insert_before":
		if content != "" && !strings.HasSuffix(content, eol) {
			content += eol
		}
		return []byte(content)
	case "insert_after":
		if content == "" {
			return nil
		}
		prefix := ""
		if !bytes.HasSuffix(target, []byte(eol)) {
			prefix = eol
		}
		if !strings.HasSuffix(content, eol) {
			content += eol
		}
		return []byte(prefix + content)
	case "replace":
		if content != "" && bytes.HasSuffix(target, []byte(eol)) && !strings.HasSuffix(content, eol) {
			content += eol
		}
		return []byte(content)
	default:
		return nil
	}
}

func detectLineEnding(data []byte) string {
	if bytes.Contains(data, []byte("\r\n")) {
		return "\r\n"
	}
	return "\n"
}

func normalizeLineEndings(content, eol string) string {
	content = strings.ReplaceAll(content, "\r\n", "\n")
	content = strings.ReplaceAll(content, "\r", "\n")
	if eol != "\n" {
		content = strings.ReplaceAll(content, "\n", eol)
	}
	return content
}

func stageEditFile(path string, data []byte, mode os.FileMode) (string, error) {
	tmp, err := os.CreateTemp(filepath.Dir(path), ".ghg-edit-*")
	if err != nil {
		return "", err
	}
	name := tmp.Name()
	remove := true
	defer func() {
		if remove {
			_ = os.Remove(name)
		}
	}()
	if err := tmp.Chmod(preserveModeBits(mode)); err != nil {
		_ = tmp.Close()
		return "", err
	}
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		return "", err
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return "", err
	}
	if err := tmp.Close(); err != nil {
		return "", err
	}
	remove = false
	return name, nil
}

func atomicWriteFile(path string, data []byte, mode os.FileMode) error {
	tmp, err := stageEditFile(path, data, mode)
	if err != nil {
		return err
	}
	if err := os.Rename(tmp, path); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	return nil
}

func preserveModeBits(mode os.FileMode) os.FileMode {
	return mode.Perm() | mode&(os.ModeSetuid|os.ModeSetgid|os.ModeSticky)
}

func rollbackEditFiles(published []editPublication) error {
	var errs []string
	for i := len(published) - 1; i >= 0; i-- {
		file := published[i]
		if err := atomicWriteFile(file.path, file.original, file.mode); err != nil {
			errs = append(errs, file.path+": "+err.Error())
		}
	}
	if len(errs) > 0 {
		return errors.New(strings.Join(errs, "; "))
	}
	return nil
}

func editReadback(data []byte, operations []observedEdit) string {
	if len(data) == 0 {
		return "(file is empty)\n"
	}
	start := operations[0].start
	for _, operation := range operations[1:] {
		if operation.start < start {
			start = operation.start
		}
	}
	line := 1 + bytes.Count(data[:min(start, len(data))], []byte{'\n'})
	spans := lineSpans(data)
	if line > len(spans) {
		line = len(spans)
	}
	end := min(line-1+maxEditReadbackLines, len(spans))
	var b strings.Builder
	for i := line - 1; i < end; i++ {
		text := string(data[spans[i].start:spans[i].end])
		text = strings.TrimSuffix(strings.TrimSuffix(text, "\n"), "\r")
		if len(text) > maxEditReadbackLineBytes {
			text = text[:maxEditReadbackLineBytes] + "…"
		}
		fmt.Fprintf(&b, "%d\t%s\n", i+1, text)
	}
	return b.String()
}

const (
	maxEditReadbackLines     = 8
	maxEditReadbackLineBytes = 200
)

func writeTool() Tool {
	return resultTool(models.NewTool("write",
		"Write content to a file, creating it (and parent directories) or overwriting it.",
		`{"type":"object","properties":{"path":{"type":"string","description":"Path to the file"},"content":{"type":"string","description":"Full file content"}},"required":["path","content"]}`),
		runWriteResult)
}

func runWriteResult(ctx context.Context, args json.RawMessage) (ToolResult, error) {
	var a struct {
		Path    string `json:"path"`
		Content string `json:"content"`
	}
	if err := json.Unmarshal(args, &a); err != nil {
		return ToolResult{}, err
	}
	path, err := AuthorizePath(ctx, a.Path, sandbox.AccessWrite, true)
	if err != nil {
		return ToolResult{}, err
	}
	if deny := checkGate(ctx, "write", path); deny != "" {
		return ToolResult{}, errors.New(deny)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return ToolResult{}, err
	}
	mode := os.FileMode(0o644)
	if info, statErr := os.Stat(path); statErr == nil {
		mode = info.Mode()
	} else if !os.IsNotExist(statErr) {
		return ToolResult{}, statErr
	}
	if err := atomicWriteFile(path, []byte(a.Content), mode); err != nil {
		return ToolResult{}, err
	}
	raw := fmt.Sprintf("Wrote %d bytes to %s", len(a.Content), path)
	if _, statErr := os.Stat(path); statErr == nil {
		raw += lspDiagnostics(ctx, path)
	}
	return textResult(raw, raw, 0), nil
}

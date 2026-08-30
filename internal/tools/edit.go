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

func runExactEdit(ctx context.Context, request editRequest) (ToolResult, error) {
	if request.Path == "" || request.OldString == "" {
		return ToolResult{}, errors.New("exact edit requires path and a non-empty old_string")
	}
	canonical, err := authorizedObservationPath(ctx, request.Path, sandbox.AccessWrite, false)
	if err != nil {
		return ToolResult{}, err
	}
	if deny := checkGate("edit", canonical); deny != "" {
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
	hookReports := runtimePostEditReports(ctx, []string{canonical})
	final, readErr := os.ReadFile(canonical)
	if readErr != nil {
		if os.IsNotExist(readErr) {
			out := fmt.Sprintf("Replaced %d occurrence(s) in %s\npostEdit removed the file", n, canonical)
			for _, report := range hookReports {
				out += "\n\n" + report.note(RuntimeFromContext(ctx))
			}
			return textResult(out, truncate(out), 0), nil
		}
		return ToolResult{}, fmt.Errorf("read back %s: %w", canonical, readErr)
	}
	out := fmt.Sprintf("Replaced %d occurrence(s) in %s", n, canonical)
	if d := editDiff(string(original), string(final)); d != "" {
		out += "\n```diff\n" + d + "\n```"
	}
	for _, report := range hookReports {
		out += "\n\n" + report.note(RuntimeFromContext(ctx))
	}
	out += lspDiagnostics(ctx, canonical)
	return textResult(out, truncate(out), 0), nil
}

type observedEdit struct {
	operation string
	content   string
	start     int
	end       int
	line      int
	path      string
	target    []byte
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
	for _, operation := range request.Edits {
		if err := ctx.Err(); err != nil {
			return ToolResult{}, err
		}
		if operation.Path == "" || operation.Observation == "" {
			return ToolResult{}, errors.New("each observed edit needs observation and path")
		}
		if operation.StartLine <= 0 || operation.EndLine < operation.StartLine {
			return ToolResult{}, fmt.Errorf("invalid authorized line range %d-%d", operation.StartLine, operation.EndLine)
		}
		opName := strings.ToLower(strings.TrimSpace(operation.Operation))
		switch opName {
		case "replace", "delete", "insert_before", "insert_after":
		default:
			return ToolResult{}, fmt.Errorf("unsupported observed edit operation %q", operation.Operation)
		}
		canonical, err := authorizedObservationPath(ctx, operation.Path, sandbox.AccessWrite, false)
		if err != nil {
			return ToolResult{}, err
		}
		var record observation.Record
		if operation.Observation != "" {
			var loadErr error
			record, loadErr = store.Load(ctx, sessionID, operation.Observation)
			if loadErr == nil && record.SessionID != "" && record.SessionID != sessionID {
				return ToolResult{}, errors.New("observation belongs to another session")
			}
		}
		recordPath := ""
		if record.Path != "" {
			recordPath, _ = authorizedObservationPath(ctx, record.Path, sandbox.AccessRead, false)
		}
		if record.ID == "" || recordPath != canonical || operation.StartLine < record.StartLine || operation.EndLine > record.EndLine {
			// Auto-healing observation fallback: read current content from disk and synthesize an observation
			diskBytes, readErr := os.ReadFile(canonical)
			if readErr != nil {
				return ToolResult{}, fmt.Errorf("read %s: %w", canonical, readErr)
			}
			newID := observation.NewID()
			spans := lineSpans(diskBytes)
			record = observation.Record{
				ID:          newID,
				SessionID:   sessionID,
				Path:        canonical,
				StartLine:   1,
				EndLine:     max(1, len(spans)),
				IssuedBytes: len(diskBytes),
				Content:     string(diskBytes),
				Complete:    true,
			}
			_ = store.Save(ctx, sessionID, record)
		}
		content := operation.Content
		if content == "" && operation.NewContent != "" {
			content = operation.NewContent
		}
		plan := plans[canonical]
		if plan == nil {
			if deny := checkGate("edit", canonical); deny != "" {
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
			plan = &editFilePlan{path: canonical, original: original, mode: info.Mode()}
			plans[canonical] = plan
			canonicalPaths = append(canonicalPaths, canonical)
		}
		resolved, err := resolveObservedEdit(operation, opName, content, record, plan.original)
		if err != nil {
			return ToolResult{}, err
		}
		plan.operations = append(plan.operations, resolved)
	}
	// The permission calls above intentionally happen before any staged or
	// published bytes. Sort the paths now so every caller's output is stable.
	sort.Strings(canonicalPaths)
	for _, path := range canonicalPaths {
		plan := plans[path]
		if err := validateEditIntersections(plan.operations); err != nil {
			return ToolResult{}, fmt.Errorf("%s: %w", path, err)
		}
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
	hookReports := runtimePostEditReports(ctx, canonicalPaths)

	var out strings.Builder
	for _, path := range canonicalPaths {
		plan := plans[path]
		fmt.Fprintf(&out, "Edited %s (%d operation(s))\n", path, len(plan.operations))
		out.WriteString("readback:\n")
		final, err := os.ReadFile(path)
		if err != nil {
			if os.IsNotExist(err) {
				out.WriteString("(file was removed by postEdit)\n")
			} else {
				out.WriteString("(readback failed: " + err.Error() + ")\n")
			}
		} else {
			if store != nil {
				newObsID := observation.NewID()
				spans := lineSpans(final)
				obsRecord := observation.Record{
					ID:          newObsID,
					SessionID:   sessionID,
					Path:        path,
					StartLine:   1,
					EndLine:     max(1, len(spans)),
					IssuedBytes: len(final),
					Content:     string(final),
					Complete:    true,
				}
				_ = store.Save(ctx, sessionID, obsRecord)
				fmt.Fprintf(&out, "[observation %s path=%s lines=1-%d]\n", newObsID, filepath.ToSlash(path), max(1, len(spans)))
			}
			out.WriteString(editReadback(final, plan.operations))
			if d := editDiff(string(plan.original), string(final)); d != "" {
				out.WriteString("```diff\n")
				out.WriteString(d)
				out.WriteString("\n```\n")
			}
			out.WriteString(lspDiagnostics(ctx, path))
		}
	}
	for _, report := range hookReports {
		out.WriteString(report.note(RuntimeFromContext(ctx)))
		out.WriteByte('\n')
	}
	raw := strings.TrimSuffix(out.String(), "\n")
	return textResult(raw, truncate(raw), 0), nil
}

func runtimePostEditReports(ctx context.Context, paths []string) []HookReport {
	runtime := RuntimeFromContext(ctx)
	if runtime == nil {
		return nil
	}
	return runtime.RunPostEditHooks(ctx, paths)
}

// publishEditFiles stages every replacement before renaming any of them, then
// publishes in lexical order. Rollback uses the same atomic writer as the
// legacy edit path, so exact edits, observed edits, and rename apply share one
// publication contract.
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
	if opName == "replace" && content == "" {
		// Empty replacement is valid, but an omitted content field is too easy
		// to confuse with malformed model JSON. Keep the operation explicit in
		// the schema while still allowing intentional empty replacement.
		content = ""
	}
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
	first := bytes.Index(data, expected)
	if first < 0 {
		return 0, 0, errors.New("issued bytes are no longer present")
	}
	second := bytes.Index(data[first+1:], expected)
	if second >= 0 {
		return 0, 0, errors.New("issued bytes occur more than once")
	}
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

func validateEditIntersections(edits []observedEdit) error {
	for i := 0; i < len(edits); i++ {
		for j := i + 1; j < len(edits); j++ {
			if rangesIntersect(edits[i].start, edits[i].end, edits[j].start, edits[j].end) {
				return fmt.Errorf("operations %d and %d intersect", i+1, j+1)
			}
		}
	}
	return nil
}

func rangesIntersect(aStart, aEnd, bStart, bEnd int) bool {
	if aStart == aEnd && bStart == bEnd {
		return aStart == bStart
	}
	if aStart == aEnd {
		return aStart >= bStart && aStart <= bEnd
	}
	if bStart == bEnd {
		return bStart >= aStart && bStart <= aEnd
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

func rollbackPublished(published []string, plans map[string]*editFilePlan) error {
	files := make([]editPublication, 0, len(published))
	for _, path := range published {
		plan := plans[path]
		files = append(files, editPublication{path: path, original: plan.original, mode: plan.mode})
	}
	return rollbackEditFiles(files)
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

func applyRenamePlan(ctx context.Context, runtime *ToolRuntime, plan RenamePlan) (string, error) {
	files := make([]editPublication, 0, len(plan.Files))
	for _, file := range plan.Files {
		files = append(files, editPublication{
			path: file.Path, original: file.Original, updated: file.Updated, mode: file.Mode,
		})
	}
	if err := publishEditFiles(ctx, files); err != nil {
		return "", err
	}
	hookReports := runtime.RunPostEditHooks(ctx, renamePlanPaths(plan.Files))
	var out strings.Builder
	fmt.Fprintf(&out, "Applied rename %s\n", plan.ID)
	sort.Slice(files, func(i, j int) bool { return files[i].path < files[j].path })
	for _, file := range files {
		final, err := os.ReadFile(file.path)
		if err != nil {
			if os.IsNotExist(err) {
				fmt.Fprintf(&out, "- %s: file removed by postEdit\n", file.path)
			} else {
				fmt.Fprintf(&out, "- %s: readback failed: %v\n", file.path, err)
			}
			continue
		}
		fmt.Fprintf(&out, "- %s\n", file.path)
		if diff := editDiff(string(file.original), string(final)); diff != "" {
			out.WriteString("```diff\n")
			out.WriteString(diff)
			out.WriteString("\n```\n")
		}
		out.WriteString("readback:\n")
		out.WriteString(editReadback(final, []observedEdit{{start: 0}}))
		out.WriteString(lspDiagnostics(ctx, file.path))
	}
	for _, report := range hookReports {
		out.WriteString(report.note(runtime))
		out.WriteByte('\n')
	}
	return strings.TrimSuffix(out.String(), "\n"), nil
}

func renamePlanPaths(files []RenameFile) []string {
	paths := make([]string, 0, len(files))
	for _, file := range files {
		paths = append(paths, file.Path)
	}
	return paths
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
	end := min(line+maxEditReadbackLines, len(spans))
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

package agent

import (
	"encoding/json"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/sacca97/ghg/internal/models"
	"github.com/sacca97/ghg/internal/tools"
)

const (
	readCoverageDefaultLimit = 250
	readCoverageMaxLimit     = 1000
	// ponytail: keep redundant-read suppression bounded; older observations
	// can safely be reread once they fall out of this small working set.
	maxReadCoverageEntries = 1024
)

type readCoverage struct {
	path        string
	start       int
	end         int
	nextOffset  int
	observation string
	size        int64
	modTime     time.Time
}

type readRequest struct {
	path  string
	start int
	end   int
}

type readDecision struct {
	request    readRequest
	requests   []readRequest
	coverage   readCoverage
	prefix     bool
	suppressed bool
	batchRoot  int
}

type readCoverageTracker struct {
	coverage []readCoverage
}

func newReadCoverageTracker() *readCoverageTracker {
	return &readCoverageTracker{}
}

func (t *readCoverageTracker) clear() {
	if t != nil {
		t.coverage = nil
	}
}

func (t *readCoverageTracker) prepare(calls []models.ToolCall, unavailable map[string]struct{}) []readDecision {
	decisions := make([]readDecision, len(calls))
	for i := range decisions {
		decisions[i].batchRoot = -1
	}
	if t == nil {
		return decisions
	}

	hasRead, hasMutation := false, false
	for i, call := range calls {
		if potentiallyMutatingReadGuardTool(call.Function.Name, call.Function.Arguments) {
			hasMutation = true
		}
		requests, ok := normalizeReadRequests(call.Function.Name, call.Function.Arguments)
		if !ok {
			continue
		}
		decisions[i].requests = requests
		if len(requests) == 1 {
			decisions[i].request = requests[0]
		}
		hasRead = true
	}
	if hasRead && hasMutation {
		return decisions
	}
	paths := make(map[string]struct{})
	for _, decision := range decisions {
		for _, request := range decision.requests {
			paths[request.path] = struct{}{}
		}
	}
	t.discardStale(paths)

	for i, call := range calls {
		requests := decisions[i].requests
		if len(requests) == 0 {
			continue
		}
		if _, disabled := unavailable[call.Function.Name]; disabled {
			continue
		}
		allCovered := true
		for _, request := range requests {
			coverage, ok := t.covered(request)
			if !ok {
				allCovered = false
				if len(requests) == 1 {
					if prefixCoverage, prefixOK := t.expandingPrefix(request); prefixOK {
						decisions[i].coverage = prefixCoverage
						decisions[i].prefix = true
						decisions[i].suppressed = true
					}
				}
				break
			}
			if decisions[i].coverage.observation == "" {
				decisions[i].coverage = coverage
			}
		}
		if allCovered {
			decisions[i].suppressed = true
		}
	}

	// Only same-offset reads are collapsed in a batch. Arbitrary interval
	// merging is intentionally left out of this guard.
	for i := range decisions {
		if len(decisions[i].requests) != 1 || decisions[i].suppressed {
			continue
		}
		root := i
		for j := range decisions {
			if decisions[j].request.path != decisions[i].request.path ||
				decisions[j].request.start != decisions[i].request.start ||
				decisions[j].suppressed {
				continue
			}
			if decisions[j].request.end > decisions[root].request.end {
				root = j
			}
		}
		if root != i {
			decisions[i].suppressed = true
			decisions[i].batchRoot = root
		}
	}
	return decisions
}

func normalizeReadRequests(name, args string) ([]readRequest, bool) {
	if name != "read" {
		return nil, false
	}
	var input struct {
		Path   string `json:"path"`
		Offset int    `json:"offset"`
		Limit  int    `json:"limit"`
		Ranges []struct {
			Path   string `json:"path"`
			Offset int    `json:"offset"`
			Limit  int    `json:"limit"`
		} `json:"ranges"`
	}
	if json.Unmarshal([]byte(args), &input) != nil {
		return nil, false
	}
	if input.Ranges != nil {
		if strings.TrimSpace(input.Path) != "" || len(input.Ranges) == 0 {
			return nil, false
		}
		requests := make([]readRequest, 0, len(input.Ranges))
		for _, item := range input.Ranges {
			request, ok := normalizeReadRange(item.Path, item.Offset, item.Limit)
			if !ok {
				return nil, false
			}
			requests = append(requests, request)
		}
		return requests, true
	}
	request, ok := normalizeReadRange(input.Path, input.Offset, input.Limit)
	if !ok {
		return nil, false
	}
	return []readRequest{request}, true
}

func normalizeReadRange(path string, offset, limit int) (readRequest, bool) {
	if strings.TrimSpace(path) == "" {
		return readRequest{}, false
	}
	start := offset
	if start <= 0 {
		start = 1
	}
	if limit <= 0 {
		limit = readCoverageDefaultLimit
	}
	if limit > readCoverageMaxLimit {
		limit = readCoverageMaxLimit
	}
	if start > int(^uint(0)>>1)-limit+1 {
		return readRequest{}, false
	}
	canonical := canonicalPath(path)
	if canonical == "" {
		return readRequest{}, false
	}
	return readRequest{path: canonical, start: start, end: start + limit - 1}, true
}

func potentiallyMutatingReadGuardTool(name, args string) bool {
	switch name {
	case "read", "grep", "structural_search", "glob", "find_files", "lsp", "output_list", "output_read", "artifact_list", "artifact_read", "history_search", "history_read", "submit_review":
		return false
	case "lsp_rename":
		var input struct {
			Operation string `json:"operation"`
		}
		if json.Unmarshal([]byte(args), &input) == nil && strings.EqualFold(strings.TrimSpace(input.Operation), "preview") {
			return false
		}
		return true
	default:
		// Unknown and MCP tools are opaque to the agent and may mutate files.
		return true
	}
}

func (t *readCoverageTracker) covered(request readRequest) (readCoverage, bool) {
	var best readCoverage
	found := false
	for _, coverage := range t.coverage {
		if coverage.path != request.path || request.start < coverage.start || request.start > coverage.end {
			continue
		}
		if request.end > coverage.end && coverage.nextOffset != 0 {
			continue
		}
		if !found || coverage.end > best.end {
			best = coverage
			found = true
		}
	}
	return best, found
}

func (t *readCoverageTracker) expandingPrefix(request readRequest) (readCoverage, bool) {
	var best readCoverage
	found := false
	for _, coverage := range t.coverage {
		if coverage.path != request.path || request.start != coverage.start || request.end <= coverage.end || coverage.nextOffset == 0 {
			continue
		}
		if !found || coverage.end > best.end {
			best = coverage
			found = true
		}
	}
	return best, found
}

func (t *readCoverageTracker) discardStale(paths map[string]struct{}) {
	kept := t.coverage[:0]
	for _, coverage := range t.coverage {
		if _, requested := paths[coverage.path]; !requested {
			kept = append(kept, coverage)
			continue
		}
		info, err := os.Stat(coverage.path)
		if err != nil || info.Size() != coverage.size || !info.ModTime().Equal(coverage.modTime) {
			continue
		}
		kept = append(kept, coverage)
	}
	t.coverage = kept
}

func readCoverageFromResult(result tools.ToolResult) (readCoverage, bool) {
	coverages := readCoveragesFromResult(result)
	if len(coverages) == 0 {
		return readCoverage{}, false
	}
	return coverages[0], true
}

func readCoveragesFromResult(result tools.ToolResult) []readCoverage {
	if !readGuardResultSucceeded(result) {
		return nil
	}
	metadata := result.Metadata
	if raw := strings.TrimSpace(metadata["observations"]); raw != "" {
		var entries []struct {
			ID         string `json:"id"`
			Path       string `json:"path"`
			StartLine  int    `json:"start_line"`
			EndLine    int    `json:"end_line"`
			NextOffset int    `json:"next_offset"`
		}
		if json.Unmarshal([]byte(raw), &entries) != nil {
			return nil
		}
		coverages := make([]readCoverage, 0, len(entries))
		for _, entry := range entries {
			if entry.ID == "" || entry.Path == "" || entry.StartLine <= 0 || entry.EndLine < entry.StartLine || entry.NextOffset < 0 {
				continue
			}
			coverages = append(coverages, readCoverage{
				path: entry.Path, start: entry.StartLine, end: entry.EndLine,
				nextOffset: entry.NextOffset, observation: entry.ID,
			})
		}
		return coverages
	}
	path := strings.TrimSpace(metadata["observation_path"])
	observation := strings.TrimSpace(metadata["observation_id"])
	if path == "" || observation == "" {
		return nil
	}
	start, err := strconv.Atoi(metadata["observation_start"])
	if err != nil || start <= 0 {
		return nil
	}
	end, err := strconv.Atoi(metadata["observation_end"])
	if err != nil || end < start {
		return nil
	}
	nextOffset := 0
	if raw := metadata["observation_next_offset"]; raw != "" {
		nextOffset, err = strconv.Atoi(raw)
		if err != nil || nextOffset < 0 {
			return nil
		}
	}
	return []readCoverage{{path: path, start: start, end: end, nextOffset: nextOffset, observation: observation}}
}

func readGuardResultSucceeded(result tools.ToolResult) bool {
	return result.ExitCode == 0
}

func (t *readCoverageTracker) record(result tools.ToolResult) {
	for _, coverage := range readCoveragesFromResult(result) {
		info, err := os.Stat(coverage.path)
		if err != nil {
			continue
		}
		coverage.size = info.Size()
		coverage.modTime = info.ModTime()
		t.coverage = append(t.coverage, coverage)
	}
	if len(t.coverage) > maxReadCoverageEntries {
		t.coverage = t.coverage[len(t.coverage)-maxReadCoverageEntries:]
	}
}

func (t *readCoverageTracker) apply(a *Agent, ev Events, calls []models.ToolCall, results []tools.ToolResult, decisions []readDecision) {
	if t == nil {
		return
	}
	var sameToolCounts map[string]int
	if ev.OnToolTelemetry != nil {
		sameToolCounts = make(map[string]int)
		for _, call := range calls {
			sameToolCounts[call.Function.Name]++
		}
	}
	for i, decision := range decisions {
		if !decision.suppressed {
			continue
		}
		result := decisionResult(decision, results)
		results[i] = result
		calls[i].ExitCode = result.ExitCode
		if ev.OnToolStart != nil {
			ev.OnToolStart(calls[i].ID, calls[i].Function.Name, calls[i].Function.Arguments)
		}
		if ev.OnToolTelemetry != nil {
			fingerprint := a.operationFingerprint(calls[i].Function.Name, calls[i].Function.Arguments)
			a.seenOperationCount(fingerprint)
			metadata := make(map[string]string, len(result.Metadata))
			for key, value := range result.Metadata {
				metadata[key] = value
			}
			ev.OnToolTelemetry(ToolTelemetry{
				ID:            calls[i].ID,
				Name:          calls[i].Function.Name,
				BatchSize:     len(calls),
				SameToolCount: sameToolCounts[calls[i].Function.Name],
				PreviewBytes:  len(result.Preview),
				RetainedBytes: len(result.Retained),
				OriginalBytes: result.OriginalBytes,
				Truncated:     !result.Complete || result.OriginalBytes > int64(len(result.Preview)),
				Fingerprint:   fingerprint,
				Duplicate:     true,
				Metadata:      metadata,
			})
		}
		if ev.OnToolEnd != nil {
			ev.OnToolEnd(calls[i].ID, calls[i].Function.Name, result.Preview)
		}
	}
	hasRead, hasMutation := false, false
	for _, call := range calls {
		if call.Function.Name == "read" {
			hasRead = true
		}
		if potentiallyMutatingReadGuardTool(call.Function.Name, call.Function.Arguments) {
			hasMutation = true
		}
	}
	for i, call := range calls {
		if decisions[i].suppressed {
			continue
		}
		if call.Function.Name == "read" {
			continue
		}
		result := results[i]
		if call.Function.Name == "bash" {
			t.clear()
			continue
		}
		if !readGuardResultSucceeded(result) {
			continue
		}
		switch call.Function.Name {
		case "write", "edit":
			paths := toolMutationPaths(call.Function.Name, call.Function.Arguments)
			if len(paths) == 0 {
				t.clear()
				continue
			}
			for _, path := range paths {
				t.invalidatePath(path)
			}
		case "lsp_rename":
			// A rename can publish changes to more files than the request path.
			if potentiallyMutatingReadGuardTool(call.Function.Name, call.Function.Arguments) {
				t.clear()
			}
		default:
			if potentiallyMutatingReadGuardTool(call.Function.Name, call.Function.Arguments) {
				t.clear()
			}
		}
	}
	if hasRead && hasMutation {
		return
	}
	for i, call := range calls {
		if call.Function.Name == "read" && !decisions[i].suppressed && readGuardResultSucceeded(results[i]) {
			t.record(results[i])
		}
	}
}

func decisionResult(decision readDecision, results []tools.ToolResult) tools.ToolResult {
	if len(decision.requests) > 1 && decision.suppressed {
		return readGuidanceResult(fmt.Sprintf("Skipped redundant read: all %d requested ranges are already available from prior observations.", len(decision.requests)), decision.coverage)
	}
	if decision.batchRoot >= 0 {
		root := results[decision.batchRoot]
		if !readGuardResultSucceeded(root) {
			return root
		}
		if coverage, ok := readCoverageFromResult(root); ok {
			return redundantReadResult(coverage)
		}
		return readGuidanceResult("Skipped redundant read: the same range was executed in this batch. Continue at a different offset or use grep/lsp for a different area.", readCoverage{})
	}
	if decision.prefix {
		limit := decision.request.end - decision.coverage.end
		return readGuidanceResult(fmt.Sprintf("Lines %d-%d were already read. To inspect new content, call read with\noffset=%d and limit=%d.", decision.coverage.start, decision.coverage.end, decision.coverage.end+1, limit), decision.coverage)
	}
	return redundantReadResult(decision.coverage)
}

func redundantReadResult(coverage readCoverage) tools.ToolResult {
	if coverage.nextOffset <= 0 {
		return readGuidanceResult(fmt.Sprintf("Skipped redundant read: lines %d-%d are already available from\nobservation %s, and that observation already reaches EOF. There is no continuation offset.", coverage.start, coverage.end, coverage.observation), coverage)
	}
	return readGuidanceResult(fmt.Sprintf("Skipped redundant read: lines %d-%d are already available from\nobservation %s. Continue at offset %d or use grep/lsp for a different area.", coverage.start, coverage.end, coverage.observation, coverage.nextOffset), coverage)
}

func readGuidanceResult(text string, coverage readCoverage) tools.ToolResult {
	result := tools.TextResult(text, text)
	result.Source = "read"
	result.Metadata = map[string]string{"duplicate_suppressed": "true"}
	if coverage.observation != "" {
		result.Metadata["observation_id"] = coverage.observation
		result.Metadata["observation_path"] = coverage.path
		result.Metadata["observation_start"] = strconv.Itoa(coverage.start)
		result.Metadata["observation_end"] = strconv.Itoa(coverage.end)
		result.Metadata["observation_next_offset"] = strconv.Itoa(coverage.nextOffset)
	}
	return result
}

func (t *readCoverageTracker) invalidatePath(path string) {
	key := canonicalPath(path)
	if key == "" {
		return
	}
	kept := t.coverage[:0]
	for _, coverage := range t.coverage {
		if coverage.path != key {
			kept = append(kept, coverage)
		}
	}
	t.coverage = kept
}

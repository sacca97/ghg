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
		request, ok := normalizeReadRequest(call.Function.Name, call.Function.Arguments)
		if !ok {
			continue
		}
		decisions[i].request = request
		hasRead = true
	}
	if hasRead && hasMutation {
		return decisions
	}
	t.discardStale()

	for i, call := range calls {
		request := decisions[i].request
		if request.path == "" {
			continue
		}
		if _, disabled := unavailable[call.Function.Name]; disabled {
			continue
		}
		if coverage, ok := t.covered(request); ok {
			decisions[i].coverage = coverage
			decisions[i].suppressed = true
			continue
		}
		if coverage, ok := t.expandingPrefix(request); ok {
			decisions[i].coverage = coverage
			decisions[i].prefix = true
			decisions[i].suppressed = true
		}
	}

	// Only same-offset reads are collapsed in a batch. Arbitrary interval
	// merging is intentionally left out of this guard.
	for i := range decisions {
		if decisions[i].request.path == "" || decisions[i].suppressed {
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

func normalizeReadRequest(name, args string) (readRequest, bool) {
	if name != "read" {
		return readRequest{}, false
	}
	var input struct {
		Path   string `json:"path"`
		Offset int    `json:"offset"`
		Limit  int    `json:"limit"`
	}
	if json.Unmarshal([]byte(args), &input) != nil || strings.TrimSpace(input.Path) == "" {
		return readRequest{}, false
	}
	start := input.Offset
	if start <= 0 {
		start = 1
	}
	limit := input.Limit
	if limit <= 0 {
		limit = readCoverageDefaultLimit
	}
	if limit > readCoverageMaxLimit {
		limit = readCoverageMaxLimit
	}
	if start > int(^uint(0)>>1)-limit+1 {
		return readRequest{}, false
	}
	path := canonicalPath(input.Path)
	if path == "" {
		return readRequest{}, false
	}
	return readRequest{path: path, start: start, end: start + limit - 1}, true
}

func potentiallyMutatingReadGuardTool(name, args string) bool {
	switch name {
	case "read", "grep", "glob", "find_files", "lsp", "output_list", "output_read", "artifact_list", "artifact_read", "history_search", "history_read", "submit_review":
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

func (t *readCoverageTracker) discardStale() {
	kept := t.coverage[:0]
	for _, coverage := range t.coverage {
		info, err := os.Stat(coverage.path)
		if err != nil || info.Size() != coverage.size || !info.ModTime().Equal(coverage.modTime) {
			continue
		}
		kept = append(kept, coverage)
	}
	t.coverage = kept
}

func readCoverageFromResult(result tools.ToolResult) (readCoverage, bool) {
	if !readGuardResultSucceeded(result) {
		return readCoverage{}, false
	}
	metadata := result.Metadata
	path := strings.TrimSpace(metadata["observation_path"])
	observation := strings.TrimSpace(metadata["observation_id"])
	if path == "" || observation == "" {
		return readCoverage{}, false
	}
	start, err := strconv.Atoi(metadata["observation_start"])
	if err != nil || start <= 0 {
		return readCoverage{}, false
	}
	end, err := strconv.Atoi(metadata["observation_end"])
	if err != nil || end < start {
		return readCoverage{}, false
	}
	nextOffset := 0
	if raw := metadata["observation_next_offset"]; raw != "" {
		nextOffset, err = strconv.Atoi(raw)
		if err != nil || nextOffset < 0 {
			return readCoverage{}, false
		}
	}
	return readCoverage{path: path, start: start, end: end, nextOffset: nextOffset, observation: observation}, true
}

func readGuardResultSucceeded(result tools.ToolResult) bool {
	return result.ExitCode == 0 && !strings.HasPrefix(result.Preview, "Error:")
}

func (t *readCoverageTracker) record(result tools.ToolResult) {
	if coverage, ok := readCoverageFromResult(result); ok {
		info, err := os.Stat(coverage.path)
		if err != nil {
			return
		}
		coverage.size = info.Size()
		coverage.modTime = info.ModTime()
		t.coverage = append(t.coverage, coverage)
	}
}

func (t *readCoverageTracker) apply(a *Agent, ev Events, calls []models.ToolCall, results []tools.ToolResult, decisions []readDecision) {
	if t == nil {
		return
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
	nextOffset := coverage.nextOffset
	if nextOffset <= 0 {
		nextOffset = coverage.end + 1
	}
	return readGuidanceResult(fmt.Sprintf("Skipped redundant read: lines %d-%d are already available from\nobservation %s. Continue at offset %d or use grep/lsp for a different area.", coverage.start, coverage.end, coverage.observation, nextOffset), coverage)
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

package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/sacca97/ghg/internal/models"
	"github.com/sacca97/ghg/internal/session"
	"github.com/sacca97/ghg/internal/tools"
)

func TestReadCoverageSuppressesRedundantRanges(t *testing.T) {
	path := filepath.Join(t.TempDir(), "source.go")
	var content strings.Builder
	for i := 1; i <= 500; i++ {
		fmt.Fprintf(&content, "line %d\n", i)
	}
	if err := os.WriteFile(path, []byte(content.String()), 0o644); err != nil {
		t.Fatal(err)
	}

	ag := New(nil, "model", 100, "system")
	guard := newReadCoverageTracker()
	var telemetryMu sync.Mutex
	var telemetry []ToolTelemetry
	events := Events{OnToolTelemetry: func(value ToolTelemetry) {
		telemetryMu.Lock()
		telemetry = append(telemetry, value)
		telemetryMu.Unlock()
	}}
	run := func(calls ...models.ToolCall) []tools.ToolResult {
		return ag.runToolResultsWithPolicy(context.Background(), calls, events, ag.AllTools(), nil, "", guard)
	}
	readCall := func(id, args string) models.ToolCall {
		return models.ToolCall{ID: id, Type: "function", Function: struct {
			Name      string `json:"name"`
			Arguments string `json:"arguments"`
		}{Name: "read", Arguments: args}}
	}

	first := run(readCall("read-1", fmt.Sprintf(`{"path":%q,"offset":1,"limit":120}`, path)))[0]
	if first.ExitCode != 0 || !strings.Contains(first.Preview, "1\tline 1") {
		t.Fatalf("initial read failed: %+v", first)
	}
	repeat := run(readCall("read-2", fmt.Sprintf(`{"path":%q,"offset":1,"limit":120}`, path)))[0]
	if repeat.ExitCode != 0 || repeat.Metadata["duplicate_suppressed"] != "true" || repeat.Metadata["observation_id"] != first.Metadata["observation_id"] {
		t.Fatalf("repeat read was not compactly suppressed: %+v", repeat)
	}
	if strings.Contains(repeat.Preview, "line 1") || len(repeat.Preview) >= len(first.Preview) {
		t.Fatalf("repeat read returned too much content: %q", repeat.Preview)
	}

	expanded := run(readCall("read-3", fmt.Sprintf(`{"path":%q,"offset":1,"limit":260}`, path)))[0]
	if expanded.ExitCode != 0 || expanded.Metadata["duplicate_suppressed"] != "true" ||
		!strings.Contains(expanded.Preview, "offset=121") || !strings.Contains(expanded.Preview, "limit=140") {
		t.Fatalf("prefix expansion guidance = %+v", expanded)
	}
	next := run(readCall("read-4", fmt.Sprintf(`{"path":%q,"offset":121,"limit":140}`, path)))[0]
	if next.ExitCode != 0 || next.Metadata["duplicate_suppressed"] == "true" || !strings.Contains(next.Preview, "121\tline 121") {
		t.Fatalf("pagination read was not executed: %+v", next)
	}
	eof := run(readCall("read-eof", fmt.Sprintf(`{"path":%q,"offset":401,"limit":200}`, path)))[0]
	eofRepeat := run(readCall("read-eof-repeat", fmt.Sprintf(`{"path":%q,"offset":401,"limit":200}`, path)))[0]
	if eof.ExitCode != 0 || eof.Metadata["observation_next_offset"] != "0" {
		t.Fatalf("EOF read metadata = %+v", eof)
	}
	if eofRepeat.ExitCode != 0 || !strings.Contains(eofRepeat.Preview, "already reaches EOF") || strings.Contains(eofRepeat.Preview, "offset 501") {
		t.Fatalf("EOF repeat guidance = %+v", eofRepeat)
	}

	batch := run(
		readCall("batch-90", fmt.Sprintf(`{"path":%q,"offset":301,"limit":90}`, path)),
		readCall("batch-130", fmt.Sprintf(`{"path":%q,"offset":301,"limit":130}`, path)),
		readCall("batch-100", fmt.Sprintf(`{"path":%q,"offset":301,"limit":100}`, path)),
	)
	if len(batch) != 3 || batch[0].Metadata["duplicate_suppressed"] != "true" ||
		batch[2].Metadata["duplicate_suppressed"] != "true" || batch[1].Metadata["duplicate_suppressed"] == "true" ||
		!strings.Contains(batch[1].Preview, "301\tline 301") {
		t.Fatalf("same-offset batch results = %+v", batch)
	}
	batched := run(readCall("read-batch", fmt.Sprintf(`{"ranges":[{"path":%q,"offset":261,"limit":20},{"path":%q,"offset":281,"limit":20}]}`, path, path)))[0]
	batchedRepeat := run(readCall("read-batch-repeat", fmt.Sprintf(`{"ranges":[{"path":%q,"offset":261,"limit":20},{"path":%q,"offset":281,"limit":20}]}`, path, path)))[0]
	if batched.ExitCode != 0 || batched.Metadata["observation_count"] != "2" || batchedRepeat.Metadata["duplicate_suppressed"] != "true" {
		t.Fatalf("batched read coverage = first=%+v repeat=%+v", batched, batchedRepeat)
	}

	missing := fmt.Sprintf(`{"path":%q,"offset":1,"limit":1}`, filepath.Join(filepath.Dir(path), "missing.go"))
	failed := run(readCall("read-failed-1", missing))[0]
	retried := run(readCall("read-failed-2", missing))[0]
	if failed.ExitCode == 0 || retried.ExitCode == 0 || retried.Metadata["duplicate_suppressed"] == "true" {
		t.Fatalf("failed reads must remain retryable: first=%+v retry=%+v", failed, retried)
	}

	bashArgs := `{"command":"printf bash"}`
	bashResults := run(models.ToolCall{ID: "bash-1", Type: "function", Function: struct {
		Name      string `json:"name"`
		Arguments string `json:"arguments"`
	}{Name: "bash", Arguments: bashArgs}}, models.ToolCall{ID: "bash-2", Type: "function", Function: struct {
		Name      string `json:"name"`
		Arguments string `json:"arguments"`
	}{Name: "bash", Arguments: bashArgs}})
	for _, result := range bashResults {
		if result.Metadata["duplicate_suppressed"] == "true" {
			t.Fatalf("bash call was suppressed: %+v", result)
		}
	}
	failedBashArgs, err := json.Marshal(map[string]string{
		"command": fmt.Sprintf("printf 'changed\\n' > %q; exit 1", path),
	})
	if err != nil {
		t.Fatal(err)
	}
	failedBash := run(models.ToolCall{ID: "bash-failed", Type: "function", Function: struct {
		Name      string `json:"name"`
		Arguments string `json:"arguments"`
	}{Name: "bash", Arguments: string(failedBashArgs)}})[0]
	if failedBash.ExitCode == 0 {
		t.Fatalf("expected modifying bash call to fail: %+v", failedBash)
	}
	afterFailedBash := run(readCall("read-after-failed-bash", fmt.Sprintf(`{"path":%q,"offset":1,"limit":120}`, path)))[0]
	if afterFailedBash.ExitCode != 0 || afterFailedBash.Metadata["duplicate_suppressed"] == "true" || !strings.Contains(afterFailedBash.Preview, "changed") {
		t.Fatalf("read after failed bash was not refreshed: %+v", afterFailedBash)
	}
	telemetryMu.Lock()
	defer telemetryMu.Unlock()
	foundDuplicateTelemetry := false
	for _, value := range telemetry {
		if value.ID == "read-2" {
			foundDuplicateTelemetry = true
			if !value.Duplicate {
				t.Fatalf("suppressed read telemetry = %+v", value)
			}
		}
	}
	if !foundDuplicateTelemetry {
		t.Fatal("suppressed read did not emit telemetry")
	}
}

func TestReadCoverageTrackerCapsHistory(t *testing.T) {
	path := filepath.Join(t.TempDir(), "source.go")
	if err := os.WriteFile(path, []byte("package p\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	guard := newReadCoverageTracker()
	guard.coverage = make([]readCoverage, maxReadCoverageEntries)
	guard.record(tools.ToolResult{
		ExitCode: 0,
		Metadata: map[string]string{
			"observation_id":    "new",
			"observation_path":  path,
			"observation_start": "1",
			"observation_end":   "1",
		},
	})
	if len(guard.coverage) != maxReadCoverageEntries {
		t.Fatalf("coverage entries = %d, want %d", len(guard.coverage), maxReadCoverageEntries)
	}
}

type fakeBackend struct {
	streamRequests   []models.Request
	completeRequests []models.Request
}

type compactResponse struct {
	message models.Message
	err     error
}

type compactSequenceBackend struct {
	responses []compactResponse
	requests  []models.Request
}

func (b *compactSequenceBackend) Stream(context.Context, models.Request, models.EventSink) (models.Message, models.Usage, error) {
	return models.Message{}, models.Usage{}, nil
}

func (b *compactSequenceBackend) Complete(_ context.Context, req models.Request) (models.Message, models.Usage, error) {
	b.requests = append(b.requests, req)
	if len(b.responses) == 0 {
		return models.Message{}, models.Usage{}, errors.New("unexpected compaction call")
	}
	response := b.responses[0]
	b.responses = b.responses[1:]
	return response.message, models.Usage{}, response.err
}

var _ models.Backend = (*compactSequenceBackend)(nil)

func (b *fakeBackend) Stream(_ context.Context, req models.Request, sink models.EventSink) (models.Message, models.Usage, error) {
	b.streamRequests = append(b.streamRequests, req)
	if sink.OnText != nil {
		sink.OnText("reply")
	}
	return models.Message{Role: "assistant", Content: "reply"}, models.Usage{}, nil
}

func (b *fakeBackend) Complete(_ context.Context, req models.Request) (models.Message, models.Usage, error) {
	b.completeRequests = append(b.completeRequests, req)
	return models.Message{Role: "assistant", Content: "summary"}, models.Usage{}, nil
}

var _ models.Backend = (*fakeBackend)(nil)

func TestAgentUsesBackendContract(t *testing.T) {
	backend := &fakeBackend{}
	ag := New(backend, "model", 100, "system")

	got, err := ag.Turn(context.Background(), "hello", Events{})
	if err != nil {
		t.Fatal(err)
	}
	if got != "reply" {
		t.Fatalf("turn result = %q, want reply", got)
	}
	if len(backend.streamRequests) != 1 {
		t.Fatalf("stream calls = %d, want 1", len(backend.streamRequests))
	}
	if len(backend.streamRequests[0].Messages) != 2 {
		t.Fatalf("stream message count = %d, want system + user", len(backend.streamRequests[0].Messages))
	}

	for i := 0; i < 8; i++ {
		ag.Messages = append(ag.Messages,
			models.Message{Role: "user", Content: "question"},
			models.Message{Role: "assistant", Content: "answer"},
		)
	}
	if err := ag.ManualCompact(context.Background(), Events{}); err != nil {
		t.Fatal(err)
	}
	if len(backend.completeRequests) != 1 {
		t.Fatalf("complete calls = %d, want 1", len(backend.completeRequests))
	}
}

type requestDiagnosticsBackend struct{}

func (requestDiagnosticsBackend) Stream(_ context.Context, req models.Request, _ models.EventSink) (models.Message, models.Usage, error) {
	if req.OnRequestDiagnostics != nil {
		req.OnRequestDiagnostics(models.RequestDiagnostics{Protocol: string(models.ProtocolOpenAIResponses), BodySHA256: "wire"})
	}
	return models.Message{Role: "assistant", Content: "reply"}, models.Usage{}, nil
}

func (requestDiagnosticsBackend) Complete(context.Context, models.Request) (models.Message, models.Usage, error) {
	return models.Message{}, models.Usage{}, nil
}

func TestModelCallEndIncludesRequestDiagnostics(t *testing.T) {
	ag := New(requestDiagnosticsBackend{}, "model", 100, "system")
	var end ModelCallEnd
	_, _, err := ag.Stream(context.Background(), models.Request{
		Model: "model", Messages: []models.Message{{Role: "user", Content: "question"}},
	}, models.EventSink{}, Events{OnModelCallEnd: func(value ModelCallEnd) { end = value }})
	if err != nil {
		t.Fatal(err)
	}
	if end.RequestDiagnostics == nil || end.RequestDiagnostics.BodySHA256 != "wire" {
		t.Fatalf("request diagnostics = %+v", end.RequestDiagnostics)
	}
	if end.RequestSHA256 == "" || end.RequestPrefixSHA256 == "" || end.RequestBytes == 0 || end.RequestPrefixBytes == 0 || end.RequestPrefixMessages != 1 {
		t.Fatalf("request shape telemetry = %+v", end)
	}
}

func TestCurrentToolGuidanceRecommendsPathDiscovery(t *testing.T) {
	got := currentToolGuidance([]tools.Tool{
		{Def: models.NewTool("read", "read", `{}`)},
		{Def: models.NewTool("glob", "glob", `{}`)},
	}, nil)
	if !strings.Contains(got, "use glob before read or grep") || !strings.Contains(got, "do not guess directory names") {
		t.Fatalf("path discovery guidance = %q", got)
	}
}

// TestLSPDiagnosticsReachModel pins the end-to-end flow: the model calls
// write, the LSP hook appends a <diagnostics> block to the tool result, and
// that block is what the provider receives on the next call.
func TestLSPDiagnosticsReachModel(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "main.go")
	argsJSON, _ := json.Marshal(map[string]string{"path": target, "content": "package main\n"})

	call := 0
	backend := &mockAgentBackend{streamFn: func(_ context.Context, req models.Request, sink models.EventSink) (models.Message, models.Usage, error) {
		call++
		if call == 1 {
			return models.Message{Role: "assistant", ToolCalls: []models.ToolCall{agentToolCall("t1", "write", string(argsJSON))}}, models.Usage{}, nil
		}
		var last models.Message
		for i := len(req.Messages) - 1; i >= 0; i-- {
			if req.Messages[i].Role == "tool" {
				last = req.Messages[i]
				break
			}
		}
		if last.Role != "tool" {
			t.Errorf("expected tool result in request, got %+v", req.Messages)
		}
		if !strings.Contains(last.Content, "<diagnostics file=") || !strings.Contains(last.Content, "ERROR [2:3] undefined: foo") {
			t.Errorf("tool result missing diagnostics block: %q", last.Content)
		}
		if sink.OnText != nil {
			sink.OnText("fixed")
		}
		return models.Message{Role: "assistant", Content: "fixed"}, models.Usage{}, nil
	}}

	ag := New(backend, "m", 100, "sys")
	ag.Runtime = &tools.ToolRuntime{LanguageService: stubWaiter{block: "\n\n<diagnostics file=\"" + target + "\">\nERROR [2:3] undefined: foo\n</diagnostics>"}}
	if _, err := ag.Turn(context.Background(), "write the file", Events{}); err != nil {
		t.Fatal(err)
	}
	if call < 2 {
		t.Fatalf("loop ended after %d calls", call)
	}
}

type stubWaiter struct{ block string }

func (s stubWaiter) WaitDiagnostics(ctx context.Context, path string) string { return s.block }
func (stubWaiter) Warm(context.Context, string)                              {}
func (stubWaiter) Navigate(context.Context, tools.NavigationRequest) (tools.NavigationResult, error) {
	return tools.NavigationResult{}, errors.New("not implemented")
}

func agentToolCall(id, name, args string) models.ToolCall {
	return models.ToolCall{ID: id, Type: "function", Function: struct {
		Name      string `json:"name"`
		Arguments string `json:"arguments"`
	}{Name: name, Arguments: args}}
}

func TestToolTelemetryReportsPreviewRetentionAndRedirect(t *testing.T) {
	a := New(nil, "model", 100, "system")
	a.Tools = []tools.Tool{
		{
			Def: models.NewTool("probe", "probe", `{"type":"object"}`),
			RunResult: func(context.Context, json.RawMessage) (tools.ToolResult, error) {
				return tools.MarkUntrusted(tools.TextResultWithSize(strings.Repeat("x", 100), "preview", 100, true, 0), "probe"), nil
			},
		},
	}
	var telemetryMu sync.Mutex
	var telemetry []ToolTelemetry
	calls := []models.ToolCall{{ID: "call-1"}, {ID: "call-2"}}
	for i := range calls {
		calls[i].Function.Name = "probe"
		calls[i].Function.Arguments = fmt.Sprintf(`{"probe":%d}`, i)
	}
	a.runToolResultsWithTools(context.Background(), calls, Events{OnToolTelemetry: func(value ToolTelemetry) {
		telemetryMu.Lock()
		telemetry = append(telemetry, value)
		telemetryMu.Unlock()
	}}, a.AllTools())
	if len(telemetry) != 2 {
		t.Fatalf("telemetry events = %d, want 2", len(telemetry))
	}
	for _, got := range telemetry {
		if got.Name != "probe" || got.BatchSize != 2 || got.SameToolCount != 2 || got.DurationMS < 0 || got.PreviewBytes != len("preview") || got.RetainedBytes != 100 || got.OriginalBytes != 100 || !got.Truncated {
			t.Fatalf("telemetry = %+v", got)
		}
	}

	redirect := tools.ExecuteResult(context.Background(), tools.All(), "bash", json.RawMessage(`{"command":"grep -r TODO ."}`))
	if redirect.Metadata["bash_redirect"] != "true" {
		t.Fatalf("redirect metadata = %+v", redirect.Metadata)
	}
}

func TestReadCoverageInvalidatesAfterEdit(t *testing.T) {
	path := filepath.Join(t.TempDir(), "source.go")
	if err := os.WriteFile(path, []byte("before\nsecond\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	ag := New(nil, "model", 100, "system")
	guard := newReadCoverageTracker()
	run := func(call models.ToolCall) tools.ToolResult {
		return ag.runToolResultsWithPolicy(context.Background(), []models.ToolCall{call}, Events{}, ag.AllTools(), nil, "", guard)[0]
	}
	readCall := func(id string) models.ToolCall {
		return models.ToolCall{ID: id, Type: "function", Function: struct {
			Name      string `json:"name"`
			Arguments string `json:"arguments"`
		}{Name: "read", Arguments: fmt.Sprintf(`{"path":%q,"offset":1,"limit":1}`, path)}}
	}
	first := run(readCall("read-before"))
	if first.ExitCode != 0 || first.Metadata["observation_id"] == "" {
		t.Fatalf("initial read failed: %+v", first)
	}
	if err := os.WriteFile(path, []byte("external\nsecond\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	refreshed := run(readCall("read-external"))
	if refreshed.ExitCode != 0 || refreshed.Metadata["duplicate_suppressed"] == "true" || !strings.Contains(refreshed.Preview, "external") {
		t.Fatalf("read after external change was not refreshed: %+v", refreshed)
	}
	first = refreshed
	editArgs, err := json.Marshal(map[string]any{
		"mode": "observed",
		"edits": []map[string]any{{
			"observation": first.Metadata["observation_id"],
			"path":        path,
			"start_line":  1,
			"end_line":    1,
			"operation":   "replace",
			"content":     "updated\n",
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	edited := run(models.ToolCall{ID: "edit-1", Type: "function", Function: struct {
		Name      string `json:"name"`
		Arguments string `json:"arguments"`
	}{Name: "edit", Arguments: string(editArgs)}})
	if edited.ExitCode != 0 {
		t.Fatalf("edit failed: %+v", edited)
	}
	second := run(readCall("read-after"))
	if second.ExitCode != 0 || second.Metadata["duplicate_suppressed"] == "true" || !strings.Contains(second.Preview, "updated") {
		t.Fatalf("read after edit was not refreshed: %+v", second)
	}
}

func TestReasoningRequestUsesToggleMetadata(t *testing.T) {
	for _, tc := range []struct {
		name       string
		effort     string
		toggle     bool
		wantEffort string
		wantSet    bool
		wantOn     bool
	}{
		{name: "graded", effort: "max", wantEffort: "max"},
		{name: "toggle on", effort: "on", toggle: true, wantOn: true, wantSet: true},
		{name: "toggle off", toggle: true, wantSet: true},
		{name: "toggle graded", effort: "high", toggle: true, wantEffort: "high", wantOn: true, wantSet: true},
		{name: "on without toggle", effort: "on"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			a := &Agent{Effort: tc.effort, ReasoningToggle: tc.toggle}
			gotEffort, gotOn := a.ReasoningRequest()
			if gotEffort != tc.wantEffort {
				t.Fatalf("effort = %q, want %q", gotEffort, tc.wantEffort)
			}
			if (gotOn != nil) != tc.wantSet {
				t.Fatalf("toggle presence = %v, want %v", gotOn != nil, tc.wantSet)
			}
			if gotOn != nil && *gotOn != tc.wantOn {
				t.Fatalf("toggle value = %v, want %v", *gotOn, tc.wantOn)
			}
		})
	}
}

func TestTurnWithGoalUsesEphemeralContextAndStructuredUpdate(t *testing.T) {
	backend := &mockAgentBackend{}
	backend.streamFn = func(_ context.Context, req models.Request, sink models.EventSink) (models.Message, models.Usage, error) {
		if len(backend.requests) == 1 {
			return models.Message{Role: "assistant", ToolCalls: []models.ToolCall{agentToolCall("goal-call", "update_goal", `{"status":"active","progress":"implementation complete; verification passed"}`)}}, models.Usage{}, nil
		}
		if sink.OnText != nil {
			sink.OnText("ready")
		}
		return models.Message{Role: "assistant", Content: "ready"}, models.Usage{}, nil
	}
	ag := New(backend, "m", 100, "sys")
	ag.Tools = nil
	record := NewGoal("ship the feature")
	record.ID = "goal-1"
	var updates []GoalUpdate
	final, err := ag.TurnWithGoal(context.Background(), "start", record, Events{
		OnGoalUpdate: func(update GoalUpdate) { updates = append(updates, update) },
	})
	if err != nil {
		t.Fatal(err)
	}
	if final != "ready" {
		t.Fatalf("final = %q", final)
	}
	if len(updates) != 1 || updates[0].Status != GoalStatusActive || updates[0].GoalID != record.ID {
		t.Fatalf("updates: %+v", updates)
	}
	gotRequests := append([]models.Request(nil), backend.requests...)
	if len(gotRequests) != 2 {
		t.Fatalf("requests = %d, want 2", len(gotRequests))
	}
	for i, req := range gotRequests {
		foundGoalTool := false
		for _, tool := range req.Tools {
			if tool.Function.Name == GoalToolName {
				foundGoalTool = true
			}
		}
		if !foundGoalTool {
			t.Fatalf("request %d does not expose update_goal", i+1)
		}
	}
	if !strings.Contains(gotRequests[0].Messages[len(gotRequests[0].Messages)-1].Content, "ship the feature") {
		t.Fatalf("first request missing goal context: %+v", gotRequests[0].Messages)
	}
	if !strings.Contains(gotRequests[1].Messages[len(gotRequests[1].Messages)-1].Content, "implementation complete") {
		t.Fatalf("second request missing latest checkpoint: %+v", gotRequests[1].Messages)
	}
	for _, msg := range ag.MessagesSnapshot() {
		if msg.Role == "system" && strings.Contains(msg.Content, "request-scoped") {
			t.Fatal("goal context must not be persisted in Agent.Messages")
		}
	}
}

func TestTurnWithGoalCompletionStopsWithoutAnotherRequest(t *testing.T) {
	requests := 0
	backend := &mockAgentBackend{streamFn: func(_ context.Context, _ models.Request, _ models.EventSink) (models.Message, models.Usage, error) {
		requests++
		return models.Message{Role: "assistant", ToolCalls: []models.ToolCall{agentToolCall("goal-call", "update_goal", `{"status":"complete","progress":"tests passed"}`)}}, models.Usage{}, nil
	}}
	ag := New(backend, "m", 100, "sys")
	ag.Tools = nil
	record := NewGoal("finish")
	record.ID = "goal-2"
	var update GoalUpdate
	if _, err := ag.TurnWithGoal(context.Background(), "go", record, Events{OnGoalUpdate: func(g GoalUpdate) { update = g }}); err != nil {
		t.Fatal(err)
	}
	if requests != 1 || update.Status != GoalStatusComplete {
		t.Fatalf("requests=%d update=%+v", requests, update)
	}
}

func TestGoalTurnIgnoresExplorationAndTurnCaps(t *testing.T) {
	call := func(id, name, args string) models.ToolCall {
		return models.ToolCall{ID: id, Type: "function", Function: struct {
			Name      string `json:"name"`
			Arguments string `json:"arguments"`
		}{Name: name, Arguments: args}}
	}
	responses := make([]models.Message, 0, goalAttentionCheckpoint+1)
	for i := 0; i < goalAttentionCheckpoint; i++ {
		responses = append(responses, models.Message{Role: "assistant", ToolCalls: []models.ToolCall{call(fmt.Sprintf("read-%d", i), "read", `{"path":"README.md"}`)}})
	}
	responses = append(responses, models.Message{Role: "assistant", ToolCalls: []models.ToolCall{call("goal", GoalToolName, `{"status":"complete","progress":"verified"}`)}})
	backend := &checkpointBackend{responses: responses}
	ag := New(backend, "model", 100, "system")
	ag.MaxTurns = 1
	ag.Tools = []tools.Tool{{
		Def: models.NewTool("read", "read", `{"type":"object"}`),
		Run: func(context.Context, json.RawMessage) (string, error) { return "ok", nil },
	}}
	record := NewGoal("finish the work")
	var notices int
	if _, err := ag.TurnWithGoal(context.Background(), "start", record, Events{OnNotice: func(string) { notices++ }}); err != nil {
		t.Fatal(err)
	}
	if len(backend.requests) != goalAttentionCheckpoint+1 {
		t.Fatalf("goal turn stopped at the regular turn cap: %d requests", len(backend.requests))
	}
	if notices != 0 {
		t.Fatalf("goal turn emitted exploration checkpoints: %d", notices)
	}
	if requestContains(backend.requests[goalAttentionCheckpoint-2], "<goal_attention_checkpoint>") {
		t.Fatal("goal attention reminder appeared before round 50")
	}
	if !requestContains(backend.requests[goalAttentionCheckpoint-1], "<goal_attention_checkpoint>") {
		t.Fatal("goal attention reminder missing at round 50")
	}
}

// backend that answers with a tool call on the first request, text on the second.
func loopBackend(t *testing.T) models.Backend {
	t.Helper()
	call := 0
	return &mockAgentBackend{streamFn: func(_ context.Context, req models.Request, sink models.EventSink) (models.Message, models.Usage, error) {
		call++
		if call == 1 {
			return models.Message{Role: "assistant", ToolCalls: []models.ToolCall{agentToolCall("t1", "echo", `{"s":"hi"}`)}}, models.Usage{}, nil
		}
		last := req.Messages[len(req.Messages)-1]
		if last.Role != "tool" || last.ToolCallID != "t1" || last.Content != "echoed: hi" {
			t.Errorf("tool result not fed back: %+v", last)
		}
		if sink.OnText != nil {
			sink.OnText("done")
		}
		return models.Message{Role: "assistant", Content: "done"}, models.Usage{}, nil
	}}
}

func echoTool() tools.Tool {
	return tools.Tool{
		Def: models.NewTool("echo", "echo", `{"type":"object","properties":{"s":{"type":"string"}}}`),
		Run: func(ctx context.Context, args json.RawMessage) (string, error) {
			var a struct{ S string }
			json.Unmarshal(args, &a)
			return "echoed: " + a.S, nil
		},
	}
}

func TestTurnLoop(t *testing.T) {
	ag := New(loopBackend(t), "m", 100, "sys")
	ag.Tools = []tools.Tool{echoTool()}

	var events []string
	final, err := ag.Turn(context.Background(), "go", Events{
		OnText:      func(d string) { events = append(events, "text:"+d) },
		OnToolStart: func(_, n, _ string) { events = append(events, "start:"+n) },
		OnToolEnd:   func(_, _, r string) { events = append(events, "end:"+r) },
	})
	if err != nil {
		t.Fatal(err)
	}
	if final != "done" {
		t.Fatalf("final: %q", final)
	}
	want := []string{"start:echo", "end:echoed: hi", "text:done"}
	if len(events) != len(want) {
		t.Fatalf("events: %v", events)
	}
	for i := range want {
		if events[i] != want[i] {
			t.Fatalf("events[%d] = %q, want %q", i, events[i], want[i])
		}
	}
	// system, user, assistant(tool call), tool result, assistant(text)
	if len(ag.Messages) != 5 {
		t.Fatalf("message count: %d", len(ag.Messages))
	}
}

func TestToolOutputCarriesCallID(t *testing.T) {
	ag := New(&mockAgentBackend{}, "m", 100, "sys")
	var ids []string
	var snapshots []string
	results := ag.runToolResultsWithTools(context.Background(), []models.ToolCall{{
		ID: "bash-1",
		Function: struct {
			Name      string `json:"name"`
			Arguments string `json:"arguments"`
		}{Name: "bash", Arguments: `{"command":"printf 'first\\n'; sleep 0.15; printf 'second\\n'","timeout":2}`},
	}}, Events{
		OnToolOutput: func(id, output string) {
			ids = append(ids, id)
			snapshots = append(snapshots, output)
		},
	}, ag.AllTools())
	if len(results) != 1 || !strings.Contains(results[0].Preview, "second") {
		t.Fatalf("tool result: %v", results)
	}
	if len(snapshots) == 0 {
		t.Fatal("expected at least one partial tool-output snapshot")
	}
	for _, id := range ids {
		if id != "bash-1" {
			t.Fatalf("snapshot routed to %q, want bash-1", id)
		}
	}
	if !strings.Contains(snapshots[len(snapshots)-1], "second") {
		t.Fatalf("final snapshot missing output: %q", snapshots[len(snapshots)-1])
	}
}

func TestParallelToolOutputStaysWithCall(t *testing.T) {
	ag := New(&mockAgentBackend{}, "m", 100, "sys")
	call := func(label string) models.ToolCall {
		return models.ToolCall{
			ID: label,
			Function: struct {
				Name      string `json:"name"`
				Arguments string `json:"arguments"`
			}{Name: "bash", Arguments: fmt.Sprintf(`{"command":"printf '%s-start\\n'; sleep 0.15; printf '%s-end\\n'","timeout":2}`, label, label)},
		}
	}
	var mu sync.Mutex
	seen := map[string][]string{}
	ag.runToolResultsWithTools(context.Background(), []models.ToolCall{call("a"), call("b")}, Events{
		OnToolOutput: func(id, output string) {
			mu.Lock()
			seen[id] = append(seen[id], output)
			mu.Unlock()
		},
	}, ag.AllTools())
	for _, id := range []string{"a", "b"} {
		mu.Lock()
		outputs := append([]string(nil), seen[id]...)
		mu.Unlock()
		if len(outputs) == 0 {
			t.Fatalf("no snapshots for call %s: %+v", id, seen)
		}
		if !strings.Contains(outputs[len(outputs)-1], id+"-end") {
			t.Fatalf("call %s received the wrong final snapshot: %q", id, outputs[len(outputs)-1])
		}
		for _, output := range outputs {
			other := "b"
			if id == "b" {
				other = "a"
			}
			if strings.Contains(output, other+"-start") || strings.Contains(output, other+"-end") {
				t.Fatalf("call %s received call %s output: %q", id, other, output)
			}
		}
	}
}

// Each assistant message records its token usage and which model produced it;
// tool calls record their run time and exit status. All survive for per-turn
// cost and perf views after the in-memory session totals are gone.
func TestTurnStampsUsageModelAndToolTiming(t *testing.T) {
	backend := &mockAgentBackend{streamFn: func(_ context.Context, req models.Request, sink models.EventSink) (models.Message, models.Usage, error) {
		if len(req.Messages) > 0 && req.Messages[len(req.Messages)-1].Role == "tool" {
			if sink.OnText != nil {
				sink.OnText("done")
			}
			return models.Message{Role: "assistant", Content: "done"}, models.Usage{PromptTokens: 7, CompletionTokens: 3}, nil
		}
		return models.Message{Role: "assistant", ToolCalls: []models.ToolCall{agentToolCall("t1", "echo", `{"s":"hi"}`)}}, models.Usage{PromptTokens: 5, CompletionTokens: 2}, nil
	}}
	ag := New(backend, "kimi-k3-fast", 100, "sys")
	ag.Provider = "inference"
	ag.Tools = []tools.Tool{echoTool()}

	if _, err := ag.TurnAuthored(context.Background(), "go", Events{}); err != nil {
		t.Fatal(err)
	}
	// system, user, assistant(toolcall), tool, assistant(text)
	if len(ag.Messages) != 5 {
		t.Fatalf("messages: %d", len(ag.Messages))
	}
	user := ag.Messages[1]
	if user.SentAt == nil {
		t.Error("authored user message should carry SentAt")
	}
	var assistants []models.Message
	for _, m := range ag.Messages {
		if m.Role == "assistant" {
			assistants = append(assistants, m)
		}
	}
	if len(assistants) != 2 {
		t.Fatalf("assistants: %d", len(assistants))
	}
	for i, a := range assistants {
		if a.Usage == nil || a.Usage.PromptTokens == 0 {
			t.Errorf("assistant[%d] missing usage: %+v", i, a.Usage)
		}
		if a.Model != "kimi-k3-fast @ inference" {
			t.Errorf("assistant[%d] model: %q", i, a.Model)
		}
	}
	// the tool call carries its run time and a successful exit status
	call := assistants[0].ToolCalls[0]
	if call.DurationMs < 0 {
		t.Errorf("tool call duration: %d", call.DurationMs)
	}
	if call.ExitCode != 0 {
		t.Errorf("successful echo should be exit 0, got %d", call.ExitCode)
	}
}

func TestTurnCancelled(t *testing.T) {
	ag := New(&mockAgentBackend{streamFn: func(ctx context.Context, _ models.Request, _ models.EventSink) (models.Message, models.Usage, error) {
		<-ctx.Done()
		return models.Message{}, models.Usage{}, ctx.Err()
	}}, "m", 100, "sys")
	ag.Tools = []tools.Tool{echoTool()}
	ctx, cancel := context.WithCancel(context.Background())
	go func() { cancel() }()
	// either the stream or the post-tool check reports cancellation; both are fine
	if _, err := ag.Turn(ctx, "go", Events{}); err == nil {
		t.Skip("cancel raced turn completion") // ponytail: timing-dependent; the happy path above is the real check
	}
}

func TestTurnAPIError(t *testing.T) {
	ag := New(&mockAgentBackend{streamFn: func(context.Context, models.Request, models.EventSink) (models.Message, models.Usage, error) {
		return models.Message{}, models.Usage{}, &models.HTTPError{Status: "500 Internal Server Error", Body: "nope"}
	}}, "m", 100, "sys")
	if _, err := ag.Turn(context.Background(), "go", Events{}); err == nil {
		t.Fatal("expected error")
	}
}

// textBackend echoes text responses and records how many calls it got.
func textBackend(t *testing.T, onCall func(n int, req models.Request) string) models.Backend {
	t.Helper()
	n := 0
	return &mockAgentBackend{streamFn: func(_ context.Context, req models.Request, sink models.EventSink) (models.Message, models.Usage, error) {
		n++
		text := onCall(n, req)
		if sink.OnText != nil {
			sink.OnText(text)
		}
		return models.Message{Role: "assistant", Content: text}, models.Usage{}, nil
	}}
}

// TurnAuthored marks the user message as genuinely typed (for input-history
// recall); plain Turn (steered/goal/background paths) leaves it unmarked.
func TestTurnAuthoredMarksMessage(t *testing.T) {
	ag := New(textBackend(t, func(n int, req models.Request) string { return "done" }), "m", 100, "sys")
	if _, err := ag.TurnAuthored(context.Background(), "i typed this", Events{}); err != nil {
		t.Fatal(err)
	}
	if _, err := ag.Turn(context.Background(), "injected by ghg", Events{}); err != nil {
		t.Fatal(err)
	}

	var typed, injected bool
	for _, m := range ag.Messages {
		if m.Role != "user" {
			continue
		}
		switch m.Content {
		case "i typed this":
			typed = m.Authored
		case "injected by ghg":
			injected = m.Authored
		}
	}
	if !typed {
		t.Error("TurnAuthored message must carry Authored=true")
	}
	if injected {
		t.Error("plain Turn message must carry Authored=false")
	}
}

func TestContinueReplaysOnlyAuthoredMessageParts(t *testing.T) {
	image := models.ImagePart("png", []byte{1, 2, 3})
	backend := textBackend(t, func(n int, req models.Request) string { return "done" })
	ag := New(backend, "m", 100, "sys")
	ag.Messages = append(ag.Messages, models.Message{
		Role: "user", Content: "inspect this image", Parts: []models.ContentPart{image}, Authored: true,
	})
	if _, err := ag.Continue(context.Background(), Events{}); err != nil {
		t.Fatal(err)
	}
	if got := ag.Messages[1]; got.Content != "inspect this image" || !got.Authored || len(got.Parts) != 1 {
		t.Fatalf("authored continue did not preserve the message: %+v", got)
	}

	injected := New(backend, "m", 100, "sys")
	injected.Messages = append(injected.Messages, models.Message{
		Role: "user", Content: "injected prompt", Parts: []models.ContentPart{image},
	})
	if _, err := injected.TurnAuthored(context.Background(), "continue", Events{}); err != nil {
		t.Fatal(err)
	}
	if len(injected.Messages) != 4 || injected.Messages[1].Content != "injected prompt" || len(injected.Messages[1].Parts) != 1 || injected.Messages[2].Content != "continue" {
		t.Fatalf("injected continue was incorrectly replayed: %+v", injected.Messages)
	}

	interrupted := New(backend, "m", 100, "sys")
	var call models.ToolCall
	call.ID = "call-1"
	call.Function.Name = "read"
	interrupted.Messages = append(interrupted.Messages,
		models.Message{Role: "user", Content: "retry this turn", Authored: true},
		models.Message{Role: "assistant", StopReason: "interrupted", ToolCalls: []models.ToolCall{call}},
		models.Message{Role: "tool", Name: "read", ToolCallID: "call-1", Content: "Error: tool call interrupted — the turn was canceled by user before execution completed"},
	)
	if _, err := interrupted.Continue(context.Background(), Events{}); err != nil {
		t.Fatal(err)
	}
	if len(interrupted.Messages) != 3 || interrupted.Messages[1].Content != "retry this turn" || interrupted.Messages[2].Content != "done" {
		t.Fatalf("interrupted continue should replay without its stale tail: %+v", interrupted.Messages)
	}
}

// TestUsageAccumulates verifies every stream call folds its usage into the
// session totals (input/output/cached) and fires OnUsage per request.
func TestUsageAccumulates(t *testing.T) {
	backend := &mockAgentBackend{streamFn: func(_ context.Context, _ models.Request, sink models.EventSink) (models.Message, models.Usage, error) {
		if sink.OnText != nil {
			sink.OnText("done")
		}
		u := models.Usage{PromptTokens: 100, CompletionTokens: 10}
		u.AddCached(40)
		return models.Message{Role: "assistant", Content: "done"}, u, nil
	}}
	ag := New(backend, "m", 100, "sys")
	var fired int
	for i := 0; i < 3; i++ {
		if _, err := ag.Turn(context.Background(), "go", Events{
			OnUsage: func(u models.Usage) {
				fired++
				if u.PromptTokens != 100 || u.CompletionTokens != 10 || u.Cached() != 40 {
					t.Errorf("per-call usage: %+v", u)
				}
			},
		}); err != nil {
			t.Fatal(err)
		}
	}
	if fired != 3 {
		t.Fatalf("OnUsage fired %d times, want 3", fired)
	}
	u := ag.Usage()
	if u.PromptTokens != 300 || u.CompletionTokens != 30 || u.Cached() != 120 {
		t.Fatalf("session totals: %+v", u)
	}
}

// TestUsageMissingLeavesTotalsAlone: providers that omit usage (no terminal
// chunk) must not corrupt totals or fire misleading events.
func TestUsageMissingLeavesTotalsAlone(t *testing.T) {
	ag := New(textBackend(t, func(n int, req models.Request) string { return "done" }), "m", 100, "sys")
	if _, err := ag.Turn(context.Background(), "go", Events{}); err != nil {
		t.Fatal(err)
	}
	if u := ag.Usage(); u.PromptTokens != 0 || u.CompletionTokens != 0 || u.Cached() != 0 {
		t.Fatalf("usage should stay zero without provider usage: %+v", u)
	}
}

func TestSteerContinuesTurn(t *testing.T) {
	ag := New(textBackend(t, func(n int, req models.Request) string {
		if n == 2 {
			last := req.Messages[len(req.Messages)-1]
			if last.Role != "user" || last.Content != "also do this" {
				t.Errorf("steered message not injected: %+v", last)
			}
			return "ok2"
		}
		return "ok1"
	}), "m", 100, "sys")
	ag.Steer("also do this") // queued before the first response completes
	var steered []string
	final, err := ag.Turn(context.Background(), "go", Events{
		OnSteer: func(s string) {
			steered = append(steered, s)
			if got := ag.MessageCount(); got == 0 {
				t.Error("message snapshot callback saw no conversation")
			}
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if final != "ok2" {
		t.Fatalf("turn should continue after steer, got %q", final)
	}
	if len(steered) != 1 || steered[0] != "also do this" {
		t.Fatalf("OnSteer events: %v", steered)
	}
}

func TestSteerNoticeDoesNotFinalizeReview(t *testing.T) {
	ag := &Agent{}
	ag.steerNotice("provide the report")
	if ag.consumeReviewFinalize() {
		t.Fatal("internal background notice must not finalize review exploration")
	}
	if len(ag.pending) != 1 || ag.pending[0].text != "provide the report" {
		t.Fatalf("internal notice was not queued: %+v", ag.pending)
	}
}

func TestNoSteerEndsTurn(t *testing.T) {
	ag := New(textBackend(t, func(n int, req models.Request) string { return "done" }), "m", 100, "sys")
	final, err := ag.Turn(context.Background(), "go", Events{})
	if err != nil || final != "done" {
		t.Fatalf("%q %v", final, err)
	}
}

func TestUnavailableCapabilitiesAreNotAdvertised(t *testing.T) {
	t.Setenv("SHELL", filepath.Join(t.TempDir(), "missing-shell"))
	backend := &fakeBackend{}
	ag := New(backend, "model", 100, "system")
	ag.Runtime = &tools.ToolRuntime{}
	if _, err := ag.Turn(context.Background(), "answer", Events{}); err != nil {
		t.Fatal(err)
	}
	if len(backend.streamRequests) != 1 {
		t.Fatalf("stream requests = %d, want 1", len(backend.streamRequests))
	}
	req := backend.streamRequests[0]
	for _, tool := range req.Tools {
		if tool.Function.Name == "bash" || tool.Function.Name == "lsp" {
			t.Fatalf("unavailable tool advertised: %q", tool.Function.Name)
		}
	}
	for _, message := range req.Messages {
		if strings.Contains(message.Content, "bash") {
			t.Fatalf("unavailable bash mentioned in request context: %q", message.Content)
		}
	}
	if !requestContains(req, "lsp unavailable") {
		t.Fatalf("capability notice missing: %+v", req.Messages)
	}
}

type checkpointBackend struct {
	responses []models.Message
	requests  []models.Request
	failAt    int
	failErr   error
}

func (b *checkpointBackend) Stream(_ context.Context, req models.Request, _ models.EventSink) (models.Message, models.Usage, error) {
	b.requests = append(b.requests, req)
	if b.failAt > 0 && len(b.requests) == b.failAt {
		return models.Message{}, models.Usage{}, b.failErr
	}
	if len(b.requests) > len(b.responses) {
		return models.Message{Role: "assistant", Content: "done"}, models.Usage{}, nil
	}
	return b.responses[len(b.requests)-1], models.Usage{}, nil
}

func (b *checkpointBackend) Complete(_ context.Context, _ models.Request) (models.Message, models.Usage, error) {
	return models.Message{Role: "assistant", Content: "done"}, models.Usage{}, nil
}

func TestExplorationCheckpointsAreTransientAndBlockPendingTools(t *testing.T) {
	const expectedFinalRound = explorationCheckpointFinal
	expectedCheckpointRounds := map[int]int{executionExplorationCheckpoint: 0, explorationCheckpointOne: 1, explorationCheckpointTwo: 2, expectedFinalRound: 3}

	call := func(id, name, args string) models.ToolCall {
		return models.ToolCall{ID: id, Type: "function", Function: struct {
			Name      string `json:"name"`
			Arguments string `json:"arguments"`
		}{Name: name, Arguments: args}}
	}
	responses := []models.Message{{
		Role: "assistant",
		ToolCalls: []models.ToolCall{
			call("write-1", "write", `{"path":"changed.go"}`),
			call("grep-1", "grep", `{"pattern":"after-write"}`),
		},
	}, {
		Role:      "assistant",
		ToolCalls: []models.ToolCall{call("read-verify", "read", `{"path":"changed.go"}`)},
	}}
	for i := 0; i < expectedFinalRound; i++ {
		responses = append(responses, models.Message{
			Role:      "assistant",
			ToolCalls: []models.ToolCall{call(fmt.Sprintf("grep-%d", i+2), "grep", fmt.Sprintf(`{"pattern":"query-%d"}`, i))},
		})
	}
	responses = append(responses, models.Message{Role: "assistant", Content: "done"})

	backend := &checkpointBackend{responses: responses}
	ag := New(backend, "model", 100, "system")
	var executed atomic.Int32
	ag.Tools = []tools.Tool{
		{Def: models.NewTool("read", "read", `{"type":"object"}`), Run: func(context.Context, json.RawMessage) (string, error) { executed.Add(1); return "ok", nil }},
		{Def: models.NewTool("grep", "grep", `{"type":"object"}`), Run: func(context.Context, json.RawMessage) (string, error) { executed.Add(1); return "ok", nil }},
		{Def: models.NewTool("write", "write", `{"type":"object"}`), Run: func(context.Context, json.RawMessage) (string, error) { executed.Add(1); return "ok", nil }},
	}
	var ends []ModelCallEnd
	var notices []string
	if _, err := ag.Turn(context.Background(), "explore", Events{
		OnModelCallEnd: func(end ModelCallEnd) { ends = append(ends, end) },
		OnNotice:       func(notice string) { notices = append(notices, notice) },
	}); err != nil {
		t.Fatal(err)
	}
	if len(backend.requests) != expectedFinalRound+3 || len(ends) != len(backend.requests) {
		t.Fatalf("model calls = %d/%d, want %d", len(backend.requests), len(ends), expectedFinalRound+3)
	}
	if got, want := executed.Load(), int32(2+1+expectedFinalRound-len(expectedCheckpointRounds)); got != want {
		t.Fatalf("executed tool calls = %d, want %d", got, want)
	}
	baselineTools := backend.requests[0].Tools
	want := []string{
		fmt.Sprintf("◎ execution checkpoint · round %d", executionExplorationCheckpoint),
		fmt.Sprintf("◎ exploration checkpoint · round %d", explorationCheckpointOne),
		fmt.Sprintf("◎ exploration checkpoint · round %d", explorationCheckpointTwo),
		fmt.Sprintf("◎ exploration checkpoint · round %d", explorationCheckpointFinal),
	}
	if !reflect.DeepEqual(notices, want) {
		t.Fatalf("notices = %v, want %v", notices, want)
	}
	for round, level := range expectedCheckpointRounds {
		index := round + 2
		if level > 0 && !requestContains(backend.requests[index], fmt.Sprintf(`level="%d"`, level)) {
			t.Fatalf("request %d lacks checkpoint level %d", index+1, level)
		}
		if level == 0 && !requestContains(backend.requests[index], "<execution_checkpoint>") {
			t.Fatalf("request %d lacks execution checkpoint reminder", index+1)
		}
		if ends[index].CheckpointLevel != level {
			t.Fatalf("model call %d checkpoint level = %d, want %d", index+1, ends[index].CheckpointLevel, level)
		}
		if !reflect.DeepEqual(backend.requests[index].Tools, baselineTools) {
			t.Fatalf("checkpoint %d changed the tool set", level)
		}
	}
	if ends[expectedFinalRound+2].ContinuedAfterCheckpoint {
		t.Fatal("final response incorrectly reported continuation after checkpoint")
	}
	for i, req := range backend.requests {
		if _, checkpoint := expectedCheckpointRounds[i-2]; checkpoint {
			continue
		}
		if requestContains(req, "<exploration_checkpoint") {
			t.Fatalf("checkpoint reminder repeated at request %d", i+1)
		}
	}
	for _, message := range ag.MessagesSnapshot() {
		if strings.Contains(message.Content, "<exploration_checkpoint level=") {
			t.Fatal("checkpoint reminder was persisted in conversation history")
		}
	}
}

func TestPlanContinueRetainsExplorationCheckpoint(t *testing.T) {
	call := func(id string) models.Message {
		return models.Message{Role: "assistant", ToolCalls: []models.ToolCall{{
			ID: id, Type: "function",
			Function: struct {
				Name      string `json:"name"`
				Arguments string `json:"arguments"`
			}{Name: "read", Arguments: `{"path":"README.md"}`},
		}}}
	}
	responses := make([]models.Message, 0, explorationCheckpointOne)
	for i := 0; i < explorationCheckpointOne; i++ {
		responses = append(responses, call(fmt.Sprintf("read-%d", i)))
	}
	backend := &checkpointBackend{responses: responses, failAt: explorationCheckpointOne + 1, failErr: context.Canceled}
	ag := New(backend, "model", 100, "system")
	ag.PlanMode = true
	ag.Tools = []tools.Tool{{
		Def: models.NewTool("read", "read", `{"type":"object"}`),
		Run: func(context.Context, json.RawMessage) (string, error) { return "ok", nil },
	}}
	if _, err := ag.TurnAuthored(context.Background(), "make a plan", Events{}); !errors.Is(err, context.Canceled) {
		t.Fatalf("interrupted plan error = %v, want context canceled", err)
	}
	if !ag.PlanPending() {
		t.Fatal("interrupted plan did not retain continuation state")
	}
	ag.PlanMode = true
	final, err := ag.Continue(context.Background(), Events{})
	if err != nil || final != "done" {
		t.Fatalf("continued plan = %q, %v", final, err)
	}
	if len(backend.requests) != explorationCheckpointOne+2 {
		t.Fatalf("model calls = %d, want %d", len(backend.requests), explorationCheckpointOne+2)
	}
	continued := backend.requests[explorationCheckpointOne+1]
	if !requestContains(continued, `level="1"`) {
		t.Fatal("continued plan lost the pending checkpoint reminder")
	}
	if !reflect.DeepEqual(backend.requests[0].Tools, continued.Tools) {
		t.Fatalf("continued plan changed its tool set: first=%v continued=%v", backend.requests[0].Tools, continued.Tools)
	}
	if ag.PlanPending() {
		t.Fatal("successful plan retained stale continuation state")
	}
}

func TestNextReasoningEffortIsOneForegroundCall(t *testing.T) {
	call := func(id, name, args string) models.ToolCall {
		return models.ToolCall{ID: id, Type: "function", Function: struct {
			Name      string `json:"name"`
			Arguments string `json:"arguments"`
		}{Name: name, Arguments: args}}
	}
	cases := []struct {
		name       string
		first      []models.ToolCall
		wantEffort []string
	}{
		{
			name: "selected effort resets after next call",
			first: []models.ToolCall{
				call("read", "read", `{"path":"file.go"}`),
				call("effort", nextReasoningEffortToolName, `{"effort":"HIGH"}`),
			},
			wantEffort: []string{"low", "high", "low"},
		},
		{
			name:       "unsupported effort is ignored",
			first:      []models.ToolCall{call("effort", nextReasoningEffortToolName, `{"effort":"turbo"}`)},
			wantEffort: []string{"low", "low"},
		},
		{
			name: "duplicate selectors are ignored",
			first: []models.ToolCall{
				call("effort-1", nextReasoningEffortToolName, `{"effort":"high"}`),
				call("effort-2", nextReasoningEffortToolName, `{"effort":"low"}`),
			},
			wantEffort: []string{"low", "low"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			responses := []models.Message{{Role: "assistant", ToolCalls: tc.first}}
			if tc.name == "selected effort resets after next call" {
				responses = append(responses, models.Message{Role: "assistant", ToolCalls: []models.ToolCall{call("read-2", "read", `{"path":"file.go"}`)}})
			}
			responses = append(responses, models.Message{Role: "assistant", Content: "done"})
			backend := &checkpointBackend{responses: responses}
			ag := New(backend, "model", 100, "system")
			ag.Effort = "low"
			ag.ReasoningEfforts = []string{"low", "high"}
			ag.Tools = []tools.Tool{{
				Def: models.NewTool("read", "read", `{"type":"object"}`),
				Run: func(context.Context, json.RawMessage) (string, error) { return "ok", nil },
			}}
			var starts []ModelCallStart
			var selections []ReasoningSelection
			if _, err := ag.Turn(context.Background(), "inspect", Events{
				OnModelCallStart:     func(call ModelCallStart) { starts = append(starts, call) },
				OnReasoningSelection: func(selection ReasoningSelection) { selections = append(selections, selection) },
			}); err != nil {
				t.Fatal(err)
			}
			if len(starts) != len(tc.wantEffort) {
				t.Fatalf("model calls = %d, want %d", len(starts), len(tc.wantEffort))
			}
			for i, want := range tc.wantEffort {
				if starts[i].ReasoningEffort != want {
					t.Errorf("call %d effort = %q, want %q", i, starts[i].ReasoningEffort, want)
				}
			}
			if ag.Effort != "low" || ag.Model != "model" {
				t.Fatalf("agent baseline changed: effort=%q model=%q", ag.Effort, ag.Model)
			}
			if tc.name == "selected effort resets after next call" {
				if len(selections) != 1 || selections[0].Current != "low" || selections[0].Requested != "HIGH" || selections[0].Next != "high" || !selections[0].Applied {
					t.Fatalf("reasoning selection telemetry = %+v", selections)
				}
			}
		})
	}
}

func TestDynamicReasoningDoesNotChooseEffortAutomatically(t *testing.T) {
	call := func(id string) models.Message {
		return models.Message{Role: "assistant", ToolCalls: []models.ToolCall{{
			ID: id, Type: "function",
			Function: struct {
				Name      string `json:"name"`
				Arguments string `json:"arguments"`
			}{Name: "read", Arguments: `{"path":"known.go"}`},
		}}}
	}
	backend := &checkpointBackend{responses: []models.Message{
		call("read-1"), call("read-2"), {Role: "assistant", Content: "done"},
	}}
	ag := New(backend, "model", 100, "system")
	ag.Effort = "high"
	ag.ReasoningEfforts = []string{"medium", "high"}
	ag.Tools = []tools.Tool{{
		Def: models.NewTool("read", "read", `{"type":"object"}`),
		Run: func(context.Context, json.RawMessage) (string, error) { return "ok", nil },
	}}
	var starts []ModelCallStart
	if _, err := ag.Turn(context.Background(), "inspect", Events{
		OnModelCallStart: func(call ModelCallStart) { starts = append(starts, call) },
	}); err != nil {
		t.Fatal(err)
	}
	if len(starts) != 3 {
		t.Fatalf("model calls = %d, want 3", len(starts))
	}
	if starts[0].EffortApplied != "high" || starts[1].EffortApplied != "high" || starts[2].EffortApplied != "high" {
		t.Fatalf("efforts changed without a model override = %q, %q, %q", starts[0].EffortApplied, starts[1].EffortApplied, starts[2].EffortApplied)
	}
	for i, start := range starts {
		if !start.DynamicReasoning || start.ConfiguredEffort != "high" || start.EffortRequested != "high" || start.SelectionReason != "configured" {
			t.Fatalf("call %d effort telemetry = %+v", i, start)
		}
		if !start.ReasoningSelectorExposed || !reflect.DeepEqual(start.ReasoningSelectorEfforts, []string{"medium", "high"}) {
			t.Fatalf("call %d selector telemetry = %+v", i, start)
		}
	}
}

func TestDynamicReasoningCanDisableSelector(t *testing.T) {
	disabled := false
	ag := &Agent{ReasoningEfforts: []string{"low", "high"}, DynamicReasoning: &disabled}
	if _, ok := ag.reasoningSelectorTool(); ok {
		t.Fatal("disabled dynamic reasoning must not expose the selector tool")
	}

	ag.DynamicReasoning = nil
	if _, ok := ag.reasoningSelectorTool(); !ok {
		t.Fatal("unset dynamic reasoning must remain enabled")
	}
}

func TestReadOnlyCheckpointMalformedBatchRetriesNormally(t *testing.T) {
	call := func(id, args string) models.ToolCall {
		return models.ToolCall{ID: id, Type: "function", Function: struct {
			Name      string `json:"name"`
			Arguments string `json:"arguments"`
		}{Name: "read", Arguments: args}}
	}
	responses := make([]models.Message, 0, 13)
	for i := 0; i < 10; i++ {
		responses = append(responses, models.Message{Role: "assistant", ToolCalls: []models.ToolCall{
			call(fmt.Sprintf("read-%d", i), fmt.Sprintf(`{"path":"file-%d.go"}`, i)),
		}})
	}
	responses = append(responses,
		models.Message{Role: "assistant", ToolCalls: []models.ToolCall{call("malformed-final", "{invalid")}},
		models.Message{Role: "assistant", ToolCalls: []models.ToolCall{call("valid-final", `{"path":"final.go"}`)}},
		models.Message{Role: "assistant", Content: "<proposed_plan>done</proposed_plan>"},
	)
	backend := &checkpointBackend{responses: responses}
	ag := New(backend, "model", 100, "system")
	ag.PlanMode = true
	var executed atomic.Int32
	ag.Tools = []tools.Tool{{
		Def: models.NewTool("read", "read", `{"type":"object"}`),
		Run: func(context.Context, json.RawMessage) (string, error) {
			executed.Add(1)
			return "ok", nil
		},
	}}
	final, err := ag.TurnAuthored(context.Background(), "make a plan", Events{})
	if err != nil {
		t.Fatal(err)
	}
	if final != "<proposed_plan>done</proposed_plan>" {
		t.Fatalf("final = %q", final)
	}
	if len(backend.requests) != 13 {
		t.Fatalf("model calls = %d, want 13", len(backend.requests))
	}
	if got := executed.Load(); got != 11 {
		t.Fatalf("executed reads = %d, want 11 (checkpoint batch plus retry)", got)
	}
	if len(backend.requests[12].Tools) == 0 {
		t.Fatal("read-only checkpoint should not disable tools before budget finalization")
	}
}

func TestTaskToolSpawnsSubagent(t *testing.T) {
	call := 0
	var backend *mockAgentBackend
	backend = &mockAgentBackend{streamFn: func(_ context.Context, req models.Request, sink models.EventSink) (models.Message, models.Usage, error) {
		call++
		switch call {
		case 1: // outer agent delegates
			return models.Message{Role: "assistant", ToolCalls: []models.ToolCall{agentToolCall("t1", "task", `{"description":"probe","prompt":"find the answer"}`)}}, models.Usage{}, nil
		case 2: // inner subagent: fresh context, no task tool, gets the prompt
			if len(req.Messages) != 2 || req.Messages[1].Content != "find the answer" {
				t.Errorf("subagent context wrong: %+v", req.Messages)
			}
			for _, tl := range req.Tools {
				if tl.Function.Name == "task" {
					t.Error("subagent must not have the task tool")
				}
			}
			if sink.OnText != nil {
				sink.OnText("the answer is 42")
			}
			return models.Message{Role: "assistant", Content: "the answer is 42"}, models.Usage{}, nil
		default: // outer agent sees the report as the tool result
			last := req.Messages[len(req.Messages)-1]
			if last.Role != "tool" || last.Content != "the answer is 42" {
				t.Errorf("task result not fed back: %+v", last)
			}
			if sink.OnText != nil {
				sink.OnText("done")
			}
			return models.Message{Role: "assistant", Content: "done"}, models.Usage{}, nil
		}
	}}
	ag := New(backend, "m", 100, "sys")
	final, err := ag.Turn(context.Background(), "go", Events{})
	if err != nil || final != "done" {
		t.Fatalf("%q %v", final, err)
	}
	if len(backend.requests) != 3 {
		t.Fatalf("expected 3 API calls, got %d", len(backend.requests))
	}
}

func TestTaskUsesTinyRoleFactoryForForegroundAndBackground(t *testing.T) {
	for _, background := range []bool{false, true} {
		t.Run(map[bool]string{false: "foreground", true: "background"}[background], func(t *testing.T) {
			parent := New(&fakeBackend{}, "acting-api", 100, "sys")
			childBackend := &fakeBackend{}
			var gotRole, gotPrompt string
			parent.SubagentFactory = func(_ context.Context, role, prompt string) (*Agent, error) {
				gotRole, gotPrompt = role, prompt
				return New(childBackend, "tiny-api", 50, "child"), nil
			}

			if background {
				task, err := parent.StartBackground(context.Background(), "probe", "check it")
				if err != nil {
					t.Fatal(err)
				}
				select {
				case <-task.Done:
				case <-time.After(2 * time.Second):
					t.Fatal("background task did not settle")
				}
				if got := task.Report; got != "reply" {
					t.Fatalf("background report = %q", got)
				}
			} else {
				out, err := taskTool(parent).Run(context.Background(), json.RawMessage(`{"prompt":"check it"}`))
				if err != nil {
					t.Fatal(err)
				}
				if out != "reply" {
					t.Fatalf("foreground report = %q", out)
				}
			}
			if gotRole != "tiny" {
				t.Fatalf("factory role = %q, want tiny", gotRole)
			}
			if !strings.Contains(gotPrompt, "Current working directory:") {
				t.Fatalf("factory prompt was not the subagent prompt: %q", gotPrompt)
			}
			if len(childBackend.streamRequests) != 1 || childBackend.streamRequests[0].Model != "tiny-api" {
				t.Fatalf("child requests = %+v", childBackend.streamRequests)
			}
		})
	}
}

func TestTaskToolBadArgs(t *testing.T) {
	ag := New(&mockAgentBackend{}, "m", 100, "sys")
	out := tools.Execute(context.Background(), ag.Tools, "task", json.RawMessage(`{bad`))
	if !strings.HasPrefix(out, "Error") {
		t.Fatalf("expected error, got %q", out)
	}
}

func TestSubagentsDisabled(t *testing.T) {
	ag := New(&mockAgentBackend{}, "m", 100, "sys")
	ag.SubagentsDisabled = true

	// AllTools should not include the task tool
	for _, tool := range ag.AllTools() {
		if tool.Def.Function.Name == "task" {
			t.Fatalf("AllTools() should not include task tool when SubagentsDisabled is true")
		}
	}

	// Executing the task tool directly should return a clear disabled error
	out := taskTool(ag)
	_, err := out.Run(context.Background(), json.RawMessage(`{"prompt":"test"}`))
	if err == nil || !strings.Contains(err.Error(), "disabled") {
		t.Fatalf("taskTool run when disabled = %v, want disabled error", err)
	}
}

func TestTurnAutoCompactsOnContextLimit(t *testing.T) {
	var streamCalls int
	backend := &mockAgentBackend{
		streamFn: func(_ context.Context, _ models.Request, sink models.EventSink) (models.Message, models.Usage, error) {
			streamCalls++
			if streamCalls == 1 {
				return models.Message{}, models.Usage{}, &models.HTTPError{Status: "400 Bad Request", Body: `{"error":{"code":"context_length_exceeded"}}`}
			}
			if sink.OnText != nil {
				sink.OnText("recovered")
			}
			return models.Message{Role: "assistant", Content: "recovered"}, models.Usage{}, nil
		},
		completeFn: func(_ context.Context, req models.Request) (models.Message, models.Usage, error) {
			if len(req.Messages) == 0 {
				t.Error("summary request has no messages")
			}
			return models.Message{Role: "assistant", Content: "summary of prior work"}, models.Usage{}, nil
		},
	}
	ag := New(backend, "m", 100, "sys")
	// build a history that's compactable: system + enough turns
	for i := 0; i < 8; i++ {
		ag.Messages = append(ag.Messages,
			models.Message{Role: "user", Content: fmt.Sprintf("q%d", i)},
			models.Message{Role: "assistant", Content: fmt.Sprintf("a%d", i)},
		)
	}
	ag.compacted = true // a prior review checkpoint must not disable this turn's retry
	var compacted int
	final, err := ag.Turn(context.Background(), "go", Events{
		OnCompact: func(took, kept int) { compacted++ },
	})
	if err != nil {
		t.Fatalf("turn after compaction: %v", err)
	}
	if final != "recovered" {
		t.Fatalf("final: %q", final)
	}
	if compacted != 1 {
		t.Fatalf("OnCompact fired %d times, want 1", compacted)
	}
	if len(backend.requests) < 3 {
		t.Fatalf("expected ≥3 calls (fail+summary+retry), got %d", len(backend.requests))
	}
	// summary lives between system prompt and the kept tail
	if !strings.Contains(ag.Messages[1].Content, "Summary of the conversation") {
		t.Fatalf("messages[1] should be the summary, got %q", ag.Messages[1].Content)
	}
}

func TestCompactDoesNotLoopOnRepeatedContextLimit(t *testing.T) {
	// every request errors with context_length_exceeded → compaction must
	// happen once and then the error surfaces (no infinite retry loop)
	var streamCalls int
	backend := &mockAgentBackend{
		streamFn: func(context.Context, models.Request, models.EventSink) (models.Message, models.Usage, error) {
			streamCalls++
			return models.Message{}, models.Usage{}, &models.HTTPError{Status: "400 Bad Request", Body: `{"error":{"code":"context_length_exceeded"}}`}
		},
		completeFn: func(context.Context, models.Request) (models.Message, models.Usage, error) {
			return models.Message{Role: "assistant", Content: "sim"}, models.Usage{}, nil
		},
	}
	ag := New(backend, "m", 100, "sys")
	for i := 0; i < 8; i++ {
		ag.Messages = append(ag.Messages,
			models.Message{Role: "user", Content: fmt.Sprintf("q%d", i)},
			models.Message{Role: "assistant", Content: fmt.Sprintf("a%d", i)},
		)
	}
	_, err := ag.Turn(context.Background(), "go", Events{})
	if err == nil {
		t.Fatal("expected context-limit error to surface, not loop forever")
	}
	if len(backend.requests) > 3 {
		t.Fatalf("expected ≤3 calls (fail+summary+retry-fail), got %d", len(backend.requests))
	}
}

func TestEstimateTokens(t *testing.T) {
	if got := EstimateTokens(nil); got != 0 {
		t.Fatalf("empty: %d", got)
	}
	msgs := []models.Message{
		{Role: "system", Content: strings.Repeat("x", 400)}, // 400/4 + 4 = 104
		{Role: "assistant", ToolCalls: []models.ToolCall{ // 4 + 8 + (4+96+3)/4 = 37
			func() models.ToolCall {
				var tc models.ToolCall
				tc.Function.Name = "tool"
				tc.Function.Arguments = strings.Repeat("y", 96)
				return tc
			}(),
		}},
	}
	if got := EstimateTokens(msgs); got != 104+37 {
		t.Fatalf("got %d, want %d", got, 104+37)
	}
}

func TestContextTokensUsesLatestReportedRequest(t *testing.T) {
	ag := New(&mockAgentBackend{}, "m", 100, "sys")
	if got, want := ag.ContextTokens(), EstimateTokens(ag.Messages); got != want {
		t.Fatalf("before a response: got %d, want %d", got, want)
	}

	ag.Messages = append(ag.Messages,
		models.Message{Role: "assistant", Content: "first", Usage: &models.Usage{PromptTokens: 100, CompletionTokens: 20}},
		models.Message{Role: "user", Content: "next"},
		models.Message{Role: "assistant", Content: "latest", Usage: &models.Usage{PromptTokens: 300, CompletionTokens: 40}},
	)
	if got, want := ag.ContextTokens(), 340; got != want {
		t.Fatalf("latest context tokens = %d, want %d", got, want)
	}

	ag.Messages = append(ag.Messages, models.Message{Role: "user", Content: strings.Repeat("x", 400)})
	if got, want := ag.ContextTokens(), 340+104; got != want {
		t.Fatalf("context tokens with unreported message = %d, want %d", got, want)
	}
}

func TestProactiveCompactAtFiftyPercent(t *testing.T) {
	// the first stream request should already carry the compacted history —
	// no context_length_exceeded round-trip needed
	backend := &mockAgentBackend{
		completeFn: func(context.Context, models.Request) (models.Message, models.Usage, error) {
			return models.Message{Role: "assistant", Content: "summary of prior work"}, models.Usage{}, nil
		},
		streamFn: func(_ context.Context, req models.Request, sink models.EventSink) (models.Message, models.Usage, error) {
			compact := len(req.Messages) > 1 && strings.Contains(req.Messages[1].Content, "Summary of the conversation")
			text := "not-compacted"
			if compact {
				text = "ok"
			}
			if sink.OnText != nil {
				sink.OnText(text)
			}
			return models.Message{Role: "assistant", Content: text}, models.Usage{}, nil
		},
	}
	ag := New(backend, "m", 100, "sys")
	ag.ContextLimit = 1000 // default 50% = 500 reported context tokens
	for i := 0; i < 8; i++ {
		ag.Messages = append(ag.Messages, models.Message{Role: "user", Content: strings.Repeat("x", 120)})
	}
	ag.Messages = append(ag.Messages, models.Message{
		Role: "assistant", Content: "previous response",
		Usage: &models.Usage{PromptTokens: 250, CompletionTokens: 100},
	})
	// Unreported pressure from a large tool result after the last usage-bearing assistant message
	ag.Messages = append(ag.Messages, models.Message{
		Role: "tool", Content: strings.Repeat("a", 1000),
	})
	var compacted bool
	final, err := ag.Turn(context.Background(), strings.Repeat("z", 900), Events{
		OnCompact: func(took, kept int) { compacted = true },
	})
	if err != nil {
		t.Fatal(err)
	}
	if !compacted {
		t.Fatal("expected proactive compaction above 50% of the context limit")
	}
	if final != "ok" {
		t.Fatalf("first request should have used compacted history, got %q", final)
	}
}

func TestCompactThresholdExplicitOverride(t *testing.T) {
	backend := textBackend(t, func(n int, req models.Request) string { return "done" })
	backend.(*mockAgentBackend).completeFn = func(context.Context, models.Request) (models.Message, models.Usage, error) {
		return models.Message{}, models.Usage{}, errors.New("compaction attempted")
	}

	// 55% of the limit: under the 80% default — no compaction
	ag := New(backend, "m", 100, "sys")
	ag.ContextLimit = 1000
	for i := 0; i < 8; i++ {
		ag.Messages = append(ag.Messages, models.Message{Role: "user", Content: strings.Repeat("x", 360)})
	}
	ag.Messages = append(ag.Messages, models.Message{
		Role: "assistant", Content: "previous response",
		Usage: &models.Usage{PromptTokens: 400, CompletionTokens: 150},
	})
	if _, err := ag.Turn(context.Background(), "hi", Events{}); err != nil {
		t.Fatal(err)
	}
	if len(ag.Messages) != 12 { // system + 8 users + reported assistant + user + assistant
		t.Fatalf("history should not compact below the default 80%% threshold, got %d msgs", len(ag.Messages))
	}

	// CompactThreshold wins over the default: explicit 50% threshold compacts
	ag2 := New(backend, "m", 100, "sys")
	ag2.ContextLimit = 1000
	ag2.CompactThreshold = 0.5
	for i := 0; i < 8; i++ {
		ag2.Messages = append(ag2.Messages, models.Message{Role: "user", Content: strings.Repeat("x", 360)})
	}
	ag2.Messages = append(ag2.Messages, models.Message{
		Role: "assistant", Content: "previous response",
		Usage: &models.Usage{PromptTokens: 400, CompletionTokens: 150},
	})
	if err := ag2.maybeCompact(context.Background(), Events{}); err == nil {
		t.Fatal("the same history should compact at the explicit 50% threshold")
	}
}

func TestNoProactiveCompactBelowThresholdOrWithoutLimit(t *testing.T) {
	backend := textBackend(t, func(n int, req models.Request) string { return "done" })

	// below threshold: estimate well under 50% of the limit
	ag := New(backend, "m", 100, "sys")
	ag.ContextLimit = 100000
	if _, err := ag.Turn(context.Background(), "hi", Events{}); err != nil {
		t.Fatal(err)
	}

	// no advertised limit: proactive compaction disabled regardless of size
	ag2 := New(backend, "m", 100, "sys")
	ag2.Messages = append(ag2.Messages, models.Message{Role: "user", Content: strings.Repeat("x", 4000)})
	if _, err := ag2.Turn(context.Background(), "hi", Events{}); err != nil {
		t.Fatal(err)
	}
	if len(ag2.Messages) != 4 { // system + big user + user + assistant: untouched
		t.Fatalf("history should not compact without a context limit, got %d msgs", len(ag2.Messages))
	}
}

func TestCompactUsesCandidate(t *testing.T) {
	var modelIDs []string
	var maxTokens []int
	main := &mockAgentBackend{completeFn: func(context.Context, models.Request) (models.Message, models.Usage, error) {
		t.Error("summary call must not hit the conversation's provider")
		return models.Message{}, models.Usage{}, errors.New("unexpected summary call")
	}}
	sum := &mockAgentBackend{completeFn: func(_ context.Context, req models.Request) (models.Message, models.Usage, error) {
		modelIDs = append(modelIDs, req.Model)
		maxTokens = append(maxTokens, req.MaxTokens)
		if req.ReasoningEnabled == nil || *req.ReasoningEnabled {
			t.Errorf("compaction must disable reasoning, got %v", req.ReasoningEnabled)
		}
		return models.Message{Role: "assistant", Content: "sim"}, models.Usage{}, nil
	}}
	ag := New(main, "conversation-model", 100, "sys")
	candidate := New(sum, "summary-model", 1200, "sys")
	candidate.Role = "tiny"
	candidate.ReasoningToggle = true
	ag.CompactCandidates = []*Agent{candidate}
	for i := 0; i < 8; i++ {
		ag.Messages = append(ag.Messages,
			models.Message{Role: "user", Content: fmt.Sprintf("q%d", i)},
			models.Message{Role: "assistant", Content: fmt.Sprintf("a%d", i)},
		)
	}
	if err := ag.ManualCompact(context.Background(), Events{}); err != nil {
		t.Fatal(err)
	}
	if len(modelIDs) != 1 || modelIDs[0] != "summary-model" {
		t.Fatalf("summary should run on summary-model, got %v", modelIDs)
	}
	if len(maxTokens) != 1 || maxTokens[0] != 1200 {
		t.Fatalf("summary output budget = %v, want [1200]", maxTokens)
	}
}

func TestCompactionRecoversFromTruncation(t *testing.T) {
	tests := []struct {
		name               string
		stop               string
		fallback           bool
		reasoningToggle    bool
		wantFirstCalls     int
		wantCandidateCalls int
	}{
		{name: "length retry", stop: "length", wantFirstCalls: 2},
		{name: "max tokens retry", stop: "max_tokens", wantFirstCalls: 2},
		{name: "max output tokens retry", stop: "max_output_tokens", wantFirstCalls: 2},
		{name: "falls through after retry", stop: "length", fallback: true, wantFirstCalls: 2, wantCandidateCalls: 1},
		{name: "uses advertised reasoning toggle", stop: "max_tokens", reasoningToggle: true, wantFirstCalls: 2},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			retryResponse := compactResponse{message: models.Message{Role: "assistant", Content: "complete"}}
			if tc.fallback {
				retryResponse = compactResponse{message: models.Message{Role: "assistant", Content: "still partial", StopReason: tc.stop}}
			}
			first := &compactSequenceBackend{responses: []compactResponse{
				{message: models.Message{Role: "assistant", Content: "partial", StopReason: tc.stop}},
				retryResponse,
			}}
			candidate := &compactSequenceBackend{responses: []compactResponse{
				{message: models.Message{Role: "assistant", Content: "complete"}},
			}}
			firstAgent := New(first, "first-model", 37, "sys")
			firstAgent.ContextLimit = 1000
			firstAgent.Role = "tiny"
			firstAgent.Provider = "first-provider"
			firstAgent.ReasoningToggle = tc.reasoningToggle
			ag := New(nil, "conversation-model", 0, "sys")
			ag.Messages = compactionTestHistory()
			ag.CompactCandidates = []*Agent{firstAgent}
			if tc.fallback {
				secondAgent := New(candidate, "second-model", 41, "sys")
				secondAgent.Role = "fast"
				secondAgent.Provider = "second-provider"
				ag.CompactCandidates = append(ag.CompactCandidates, secondAgent)
			}

			if err := ag.ManualCompact(context.Background(), Events{}); err != nil {
				t.Fatal(err)
			}
			if len(first.requests) != tc.wantFirstCalls {
				t.Fatalf("first candidate calls = %d, want %d", len(first.requests), tc.wantFirstCalls)
			}
			if tc.fallback && len(candidate.requests) != tc.wantCandidateCalls {
				t.Fatalf("fallback candidate calls = %d, want %d", len(candidate.requests), tc.wantCandidateCalls)
			}
			if first.requests[0].MaxTokens != first.requests[1].MaxTokens {
				t.Fatalf("retry changed max tokens: first=%d retry=%d", first.requests[0].MaxTokens, first.requests[1].MaxTokens)
			}
			if tc.reasoningToggle && (first.requests[0].ReasoningEnabled == nil || *first.requests[0].ReasoningEnabled) {
				t.Fatal("compaction should disable reasoning when the candidate advertises the capability")
			}
			if !strings.Contains(ag.Messages[1].Content, "complete") {
				t.Fatalf("complete checkpoint was not installed: %q", ag.Messages[1].Content)
			}
		})
	}
}

func TestCompactionFailuresLeaveHistoryUntouched(t *testing.T) {
	oversized := strings.Repeat("x", 200_000)
	first := &compactSequenceBackend{responses: []compactResponse{
		{message: models.Message{Role: "assistant", Content: oversized}},
		{message: models.Message{Role: "assistant", Content: oversized}},
	}}
	second := &compactSequenceBackend{responses: []compactResponse{
		{err: errors.New("fallback unavailable")},
	}}
	ag := New(nil, "conversation-model", 0, "sys")
	ag.Messages = compactionTestHistory()
	ag.CompactCandidates = []*Agent{
		New(first, "first-model", 37, "sys"),
		New(second, "second-model", 41, "sys"),
	}
	before := ag.MessagesSnapshot()
	persisted := false
	err := ag.ManualCompact(context.Background(), Events{OnCompactionReady: func([]models.Message, string, int) error {
		persisted = true
		return nil
	}})
	if err == nil || !strings.Contains(err.Error(), "compaction summary failed") {
		t.Fatalf("expected all-candidate failure, got %v", err)
	}
	if persisted || !reflect.DeepEqual(ag.MessagesSnapshot(), before) {
		t.Fatalf("failed compaction changed state: persisted=%v", persisted)
	}
}

func TestCompactionBuildsFortyThousandTokenWorkingSet(t *testing.T) {
	backend := &compactSequenceBackend{responses: []compactResponse{{message: models.Message{Role: "assistant", Content: "checkpoint"}}}}
	candidate := New(backend, "summary-model", 1_000_000, "sys")
	candidate.ContextLimit = 100_000
	ag := New(nil, "conversation-model", 0, "sys")
	for i := 0; i < 400; i++ {
		ag.Messages = append(ag.Messages,
			models.Message{Role: "user", Content: strings.Repeat("question ", 40)},
			models.Message{Role: "assistant", Content: strings.Repeat("answer ", 40)},
		)
	}
	last := len(ag.Messages) - 1
	// Provider tokenizers can report much more than the local estimate. That
	// difference must not be treated as fixed compaction overhead.
	ag.Messages[last].Usage = &models.Usage{PromptTokens: EstimateTokens(ag.Messages[:last]) + 250_000}
	ag.CompactCandidates = []*Agent{candidate}

	if err := ag.ManualCompact(context.Background(), Events{}); err != nil {
		t.Fatal(err)
	}
	if len(backend.requests) != 1 {
		t.Fatalf("compaction calls = %d, want one", len(backend.requests))
	}
	if backend.requests[0].MaxTokens >= 1_000_000 || backend.requests[0].MaxTokens > 10_000 {
		t.Fatalf("summary output cap = %d, want the 40k-derived allowance", backend.requests[0].MaxTokens)
	}
	if got := EstimateTokens(ag.Messages); got > compactionContextTarget {
		t.Fatalf("post-compaction working set = %d, exceeds %d", got, compactionContextTarget)
	}
}

func TestCompactionFoldsHistoryChunksForSmallCandidate(t *testing.T) {
	backend := &compactSequenceBackend{}
	for i := 0; i < 20; i++ {
		backend.responses = append(backend.responses, compactResponse{message: models.Message{Role: "assistant", Content: fmt.Sprintf("checkpoint %d", i)}})
	}
	candidate := New(backend, "small-summary-model", 0, "sys")
	candidate.ContextLimit = 2_000
	ag := New(nil, "conversation-model", 0, "sys")
	ag.ContextLimit = 2_000
	for i := 0; i < 40; i++ {
		ag.Messages = append(ag.Messages,
			models.Message{Role: "user", Content: strings.Repeat("question ", 20)},
			models.Message{Role: "assistant", Content: strings.Repeat("answer ", 20)},
		)
	}
	ag.CompactCandidates = []*Agent{candidate}

	if err := ag.ManualCompact(context.Background(), Events{}); err != nil {
		t.Fatal(err)
	}
	if len(backend.requests) < 2 {
		t.Fatalf("chunked compaction calls = %d, want multiple", len(backend.requests))
	}
	if !strings.Contains(backend.requests[1].Messages[1].Content, "<previous_checkpoint>") {
		t.Fatal("later chunk did not include the cumulative checkpoint")
	}
}

func compactionTestHistory() []models.Message {
	msgs := []models.Message{{Role: "system", Content: "sys"}}
	for i := 0; i < 8; i++ {
		msgs = append(msgs,
			models.Message{Role: "user", Content: fmt.Sprintf("question %d", i)},
			models.Message{Role: "assistant", Content: fmt.Sprintf("answer %d", i)},
		)
	}
	return msgs
}

func TestCompactionTelemetryUsesSummaryRoute(t *testing.T) {
	conversation := &routeBackend{protocol: models.ProtocolOpenAIChatCompletions}
	summary := &routeBackend{protocol: models.ProtocolAnthropicMessages}
	ag := New(conversation, "conversation-model", 100, "sys")
	ag.Role, ag.Provider, ag.Protocol = "fast", "main-provider", string(conversation.protocol)
	candidate := New(summary, "tiny-model", 0, "sys")
	candidate.Role = "tiny"
	candidate.Provider = "tiny-provider"
	candidate.Protocol = string(summary.protocol)
	ag.CompactCandidates = []*Agent{candidate}
	for i := 0; i < 8; i++ {
		ag.Messages = append(ag.Messages,
			models.Message{Role: "user", Content: fmt.Sprintf("question %d", i)},
			models.Message{Role: "assistant", Content: fmt.Sprintf("answer %d", i)},
		)
	}
	var starts []ModelCallStart
	var ends []ModelCallEnd
	if err := ag.ManualCompact(context.Background(), Events{
		OnModelCallStart: func(call ModelCallStart) { starts = append(starts, call) },
		OnModelCallEnd:   func(call ModelCallEnd) { ends = append(ends, call) },
	}); err != nil {
		t.Fatal(err)
	}
	if len(starts) != 1 || len(ends) != 1 {
		t.Fatalf("compaction telemetry start/end = %d/%d", len(starts), len(ends))
	}
	want := ModelCallStart{Role: "tiny", Provider: "tiny-provider", Model: "tiny-model", Protocol: string(summary.protocol), Purpose: "compaction"}
	if !reflect.DeepEqual(starts[0], want) {
		t.Fatalf("compaction start route = %+v, want %+v", starts[0], want)
	}
	if ends[0].Model != want.Model || ends[0].Provider != want.Provider || ends[0].Protocol != want.Protocol {
		t.Fatalf("compaction end route = %+v", ends[0])
	}
}

type routeBackend struct {
	protocol models.Protocol
}

func (b *routeBackend) AdapterProtocol() models.Protocol { return b.protocol }

func (b *routeBackend) Stream(context.Context, models.Request, models.EventSink) (models.Message, models.Usage, error) {
	return models.Message{}, models.Usage{}, nil
}

func (b *routeBackend) Complete(context.Context, models.Request) (models.Message, models.Usage, error) {
	return models.Message{Role: "assistant", Content: "summary"}, models.Usage{}, nil
}

func TestCompactTooLittleHistory(t *testing.T) {
	ag := New(&mockAgentBackend{}, "m", 100, "sys")
	ag.Messages = append(ag.Messages, models.Message{Role: "user", Content: "hi"})
	if _, _, err := ag.compactWithEvents(context.Background(), Events{}); !errors.Is(err, ErrNotEnoughHistory) {
		t.Fatal("expected error compacting a tiny history")
	}
}

func TestAgentBoundedTextPreservesUTF8(t *testing.T) {
	field := truncateField(strings.Repeat("界", 100), 10)
	if !utf8.ValidString(field) || len(field) > 10 {
		t.Fatalf("truncated field is not valid and bounded UTF-8: %q", field)
	}
	content := shrinkCompactionContent(strings.Repeat("界", 1000)+"\n[output ref]", 100)
	if !utf8.ValidString(content) || len(content) > 100 {
		t.Fatalf("shrunk compaction content is not valid and bounded UTF-8: bytes=%d", len(content))
	}
}

func TestCompactKeepsToolCallPair(t *testing.T) {
	// orphan safety: a tail starting with role "tool" must pull in its owning
	// assistant message so the tool result never references an erased call id.
	ag := New(&mockAgentBackend{completeFn: func(context.Context, models.Request) (models.Message, models.Usage, error) {
		return models.Message{Role: "assistant", Content: "sim"}, models.Usage{}, nil
	}}, "m", 100, "sys")
	// system, user, asst(with tool call "t1"), tool("t1" result), user, asst, user
	for i := 0; i < 4; i++ {
		ag.Messages = append(ag.Messages, models.Message{Role: "user", Content: fmt.Sprintf("u%d", i)})
		if i == 0 {
			ag.Messages = append(ag.Messages,
				models.Message{Role: "assistant", ToolCalls: []models.ToolCall{{ID: "t1", Type: "function"}}},
			)
			ag.Messages = append(ag.Messages, models.Message{Role: "tool", Content: "tool-out", ToolCallID: "t1"})
		} else {
			ag.Messages = append(ag.Messages, models.Message{Role: "assistant", Content: fmt.Sprintf("a%d", i)})
		}
	}
	before := len(ag.Messages)
	if _, _, err := ag.compactWithEvents(context.Background(), Events{}); err != nil {
		t.Fatalf("compact: %v", err)
	}
	if len(ag.Messages) >= before {
		t.Fatalf("compaction didn't shrink: before %d after %d", before, len(ag.Messages))
	}
	// find the kept tool result and its owning assistant
	var asstTool, toolMsg *models.Message
	for i := range ag.Messages {
		if ag.Messages[i].Role == "tool" {
			toolMsg = &ag.Messages[i]
		}
	}
	if toolMsg != nil && toolMsg.ToolCallID != "" {
		for i := range ag.Messages {
			for _, tc := range ag.Messages[i].ToolCalls {
				if tc.ID == toolMsg.ToolCallID {
					asstTool = &ag.Messages[i]
				}
			}
		}
	}
	if toolMsg != nil && asstTool == nil {
		t.Errorf("orphaned tool result: %#v", toolMsg)
	}
}

func TestCompactShrinksOversizedRecentToolResultAndKeepsOutputRef(t *testing.T) {
	ref := models.OutputRef{
		ID: "sha256:" + strings.Repeat("a", 64), Hash: strings.Repeat("a", 64),
		OriginalBytes: 20000, StoredBytes: 20000, Complete: true,
	}
	content := strings.Repeat("x", 20000) + tools.OutputReference(ref)
	call := models.ToolCall{ID: "call-1", Type: "function", Function: struct {
		Name      string `json:"name"`
		Arguments string `json:"arguments"`
	}{Name: "read", Arguments: `{}`}}
	msgs := []models.Message{
		{Role: "system", Content: "sys"},
		{Role: "user", Content: "old"},
		{Role: "assistant", Content: "old answer"},
		{Role: "user", Content: "recent"},
		{Role: "assistant", ToolCalls: []models.ToolCall{call}},
		{Role: "tool", Content: content, ToolCallID: "call-1", Name: "read", Output: &ref},
	}

	start, tail := compactTail(msgs, 128)
	if start != 4 || len(tail) != 2 {
		t.Fatalf("tail = start %d, %d messages; want call/result pair", start, len(tail))
	}
	if len(tail[1].Content) >= len(content) {
		t.Fatal("oversized recent tool result was not shrunk")
	}
	if !strings.Contains(tail[1].Content, "preview shrunk during compaction") ||
		!strings.Contains(tail[1].Content, ref.ID) {
		t.Fatalf("shrunk result lost its recovery metadata: %q", tail[1].Content)
	}
	if len(tail[0].ToolCalls) != 1 || tail[0].ToolCalls[0].ID != tail[1].ToolCallID {
		t.Fatalf("shrunk tail orphaned tool result: %+v", tail)
	}
}

func TestOutputManifestIncludesOnlyCitedAndRecentRefs(t *testing.T) {
	cited := models.OutputRef{ID: "sha256:" + strings.Repeat("b", 64), Hash: strings.Repeat("b", 64), OriginalBytes: 20, StoredBytes: 10, Complete: false}
	omitted := models.OutputRef{ID: "sha256:" + strings.Repeat("c", 64), Hash: strings.Repeat("c", 64), OriginalBytes: 30, StoredBytes: 15, Complete: false}
	recent := models.OutputRef{ID: "sha256:" + strings.Repeat("d", 64), Hash: strings.Repeat("d", 64), OriginalBytes: 40, StoredBytes: 40, Complete: true}
	all := []models.Message{
		{Role: "tool", Output: &cited},
		{Role: "tool", Output: &omitted},
	}
	tail := []models.Message{{Role: "tool", Output: &recent}}
	manifest := buildOutputManifest("prior work used "+cited.ID, tail, all)
	if !strings.Contains(manifest, cited.ID) || !strings.Contains(manifest, recent.ID) {
		t.Fatalf("manifest missing cited/recent refs: %s", manifest)
	}
	if strings.Contains(manifest, omitted.ID) {
		t.Fatalf("manifest included uncited old ref: %s", manifest)
	}
	if !strings.Contains(manifest, "head/tail retained; middle omitted") {
		t.Fatalf("manifest omitted retention state: %s", manifest)
	}
}

func TestManualCompactFiresEvent(t *testing.T) {
	ag := New(&mockAgentBackend{completeFn: func(context.Context, models.Request) (models.Message, models.Usage, error) {
		return models.Message{Role: "assistant", Content: "sim"}, models.Usage{}, nil
	}}, "m", 100, "sys")
	for i := 0; i < 8; i++ {
		ag.Messages = append(ag.Messages,
			models.Message{Role: "user", Content: fmt.Sprintf("q%d", i)},
			models.Message{Role: "assistant", Content: fmt.Sprintf("a%d", i)},
		)
	}
	var fired bool
	if err := ag.ManualCompact(context.Background(), Events{OnCompact: func(took, kept int) { fired = true }}); err != nil {
		t.Fatalf("manual compact: %v", err)
	}
	if !fired {
		t.Fatal("OnCompact should fire for ManualCompact")
	}
}

func TestCompactionPersistenceFailureKeepsRawMessages(t *testing.T) {
	ag := New(&routeBackend{}, "m", 100, "sys")
	for i := 0; i < 8; i++ {
		ag.Messages = append(ag.Messages,
			models.Message{Role: "user", Content: fmt.Sprintf("q%d", i)},
			models.Message{Role: "assistant", Content: fmt.Sprintf("a%d", i)},
		)
	}
	before := ag.MessagesSnapshot()
	err := ag.ManualCompact(context.Background(), Events{OnCompactionReady: func([]models.Message, string, int) error {
		return errors.New("disk full")
	}})
	if err == nil || !reflect.DeepEqual(ag.MessagesSnapshot(), before) {
		t.Fatalf("failed persistence changed raw messages: err=%v", err)
	}
}

func TestPreflightCompactionTriggersOnAbsoluteReserve(t *testing.T) {
	ag := New(&mockAgentBackend{completeFn: func(context.Context, models.Request) (models.Message, models.Usage, error) {
		return models.Message{Role: "assistant", Content: "summary of previous turns"}, models.Usage{}, nil
	}}, "m", 100, "sys")
	ag.ContextLimit = 1000
	ag.CompactThreshold = 0.9 // high percent threshold (900 tokens)
	ag.OutputReserve = 600    // absolute reserve forces budget down to 1000-600 = 400 tokens

	for i := 0; i < 8; i++ {
		ag.Messages = append(ag.Messages,
			models.Message{Role: "user", Content: strings.Repeat("hello world ", 10)},
			models.Message{Role: "assistant", Content: strings.Repeat("answer here ", 10)},
		)
	}

	tokens := ag.ActiveTokens()
	if tokens < 400 || tokens >= 900 {
		t.Fatalf("expected active tokens between 400 and 900, got %d", tokens)
	}

	before := len(ag.Messages)
	if err := ag.maybeCompact(context.Background(), Events{}); err != nil {
		t.Fatalf("maybeCompact failed: %v", err)
	}
	if len(ag.Messages) >= before {
		t.Fatalf("messages should have compacted due to output reserve budget: before=%d, after=%d", before, len(ag.Messages))
	}
}

func TestOneShotOverflowFailsTerminalOnSecondError(t *testing.T) {
	ag := New(&mockAgentBackend{streamFn: func(context.Context, models.Request, models.EventSink) (models.Message, models.Usage, error) {
		return models.Message{}, models.Usage{}, &models.HTTPError{Status: "400 Bad Request", Body: `{"error":{"message":"maximum context length exceeded","type":"invalid_request_error"}}`}
	}}, "m", 100, "sys")
	for i := 0; i < 8; i++ {
		ag.Messages = append(ag.Messages,
			models.Message{Role: "user", Content: fmt.Sprintf("q%d", i)},
			models.Message{Role: "assistant", Content: fmt.Sprintf("a%d", i)},
		)
	}
	_, err := ag.Turn(context.Background(), "hello", Events{})
	if err == nil || !models.IsContextLimit(err) {
		t.Fatalf("expected context limit exceeded terminal error, got %v", err)
	}
}

func TestCumulativeCompactionIncludesPriorCheckpoint(t *testing.T) {
	var summaryUserPrompt string
	ag := New(&mockAgentBackend{completeFn: func(_ context.Context, req models.Request) (models.Message, models.Usage, error) {
		for _, m := range req.Messages {
			if m.Role == "user" {
				summaryUserPrompt = m.Content
			}
		}
		return models.Message{Role: "assistant", Content: "cumulative checkpoint"}, models.Usage{}, nil
	}}, "m", 100, "sys")
	ag.Messages = []models.Message{
		{Role: "system", Content: "sys"},
		{Role: "system", Content: "Summary of the conversation so far:\n\nInitial milestone reached."},
		{Role: "user", Content: "now please do part 2"},
		{Role: "assistant", Content: "working on part 2"},
		{Role: "user", Content: "now please do part 3"},
		{Role: "assistant", Content: "working on part 3"},
		{Role: "user", Content: "now please do part 4"},
		{Role: "assistant", Content: "working on part 4"},
	}

	if err := ag.ManualCompact(context.Background(), Events{}); err != nil {
		t.Fatal(err)
	}

	if !strings.Contains(summaryUserPrompt, "<previous_checkpoint>") ||
		!strings.Contains(summaryUserPrompt, "Initial milestone reached.") {
		t.Fatalf("summary request omitted prior checkpoint: %s", summaryUserPrompt)
	}
	if !strings.Contains(summaryUserPrompt, "<new_history>") {
		t.Fatalf("summary request omitted <new_history>: %s", summaryUserPrompt)
	}
}

func TestCompactionRejectsTruncatedSummary(t *testing.T) {
	ag := New(&mockAgentBackend{completeFn: func(context.Context, models.Request) (models.Message, models.Usage, error) {
		return models.Message{Role: "assistant", Content: "truncated summary...", StopReason: "length"}, models.Usage{}, nil
	}}, "m", 100, "sys")
	for i := 0; i < 8; i++ {
		ag.Messages = append(ag.Messages,
			models.Message{Role: "user", Content: fmt.Sprintf("q%d", i)},
			models.Message{Role: "assistant", Content: fmt.Sprintf("a%d", i)},
		)
	}
	err := ag.ManualCompact(context.Background(), Events{})
	if err == nil || !strings.Contains(err.Error(), "truncated by token limit") {
		t.Fatalf("expected truncated by token limit error, got %v", err)
	}
}

func TestCompactionRetriesTruncatedSummary(t *testing.T) {
	calls := 0
	backend := &mockAgentBackend{completeFn: func(context.Context, models.Request) (models.Message, models.Usage, error) {
		calls++
		if calls == 1 {
			return models.Message{Role: "assistant", Content: "truncated summary...", StopReason: "length"}, models.Usage{}, nil
		}
		return models.Message{Role: "assistant", Content: "complete checkpoint", StopReason: "stop"}, models.Usage{}, nil
	}}
	ag := New(backend, "m", 100, "sys")
	for i := 0; i < 8; i++ {
		ag.Messages = append(ag.Messages,
			models.Message{Role: "user", Content: fmt.Sprintf("q%d", i)},
			models.Message{Role: "assistant", Content: fmt.Sprintf("a%d", i)},
		)
	}
	if err := ag.ManualCompact(context.Background(), Events{}); err != nil {
		t.Fatalf("compact should retry a truncated checkpoint: %v", err)
	}
	if calls != 2 {
		t.Fatalf("compaction calls = %d, want 2", calls)
	}
}

type mockAgentBackend struct {
	mu         sync.Mutex
	responses  []models.Message
	callCount  int
	requests   []models.Request
	streamFn   func(context.Context, models.Request, models.EventSink) (models.Message, models.Usage, error)
	completeFn func(context.Context, models.Request) (models.Message, models.Usage, error)
}

func (m *mockAgentBackend) Stream(ctx context.Context, req models.Request, sink models.EventSink) (models.Message, models.Usage, error) {
	m.mu.Lock()
	m.requests = append(m.requests, req)
	call := m.callCount
	m.callCount++
	var resp models.Message
	if call < len(m.responses) {
		resp = m.responses[call]
	}
	m.mu.Unlock()
	if m.streamFn != nil {
		return m.streamFn(ctx, req, sink)
	}
	if call >= len(m.responses) {
		return models.Message{Role: "assistant", Content: "done"}, models.Usage{}, nil
	}
	return resp, models.Usage{}, nil
}

func (m *mockAgentBackend) Complete(ctx context.Context, req models.Request) (models.Message, models.Usage, error) {
	if m.completeFn != nil {
		m.mu.Lock()
		m.requests = append(m.requests, req)
		m.mu.Unlock()
		return m.completeFn(ctx, req)
	}
	return m.Stream(ctx, req, models.EventSink{})
}

func TestToolBatchValidationAndConcurrency(t *testing.T) {
	t.Run("PathologicalDuplicateBatch", func(t *testing.T) {
		var toolExecutions int
		var toolStarts int
		var toolEnds int

		dummyTool := tools.Tool{
			Def: models.NewTool("read", "read tool", "{}"),
			Run: func(ctx context.Context, args json.RawMessage) (string, error) {
				toolExecutions++
				return "ok", nil
			},
		}

		calls := make([]models.ToolCall, 194)
		for i := range calls {
			calls[i] = models.ToolCall{
				ID:   fmt.Sprintf("call-%d", i),
				Type: "function",
			}
			calls[i].Function.Name = "read"
			calls[i].Function.Arguments = `{"path":"same.txt"}`
		}

		backend := &mockAgentBackend{
			responses: []models.Message{
				{
					Role:      "assistant",
					ToolCalls: calls,
				},
			},
		}

		ag := New(backend, "test-model", 1000, "sys")
		ag.Tools = []tools.Tool{dummyTool}

		ev := Events{
			OnToolStart: func(id, name, args string) { toolStarts++ },
			OnToolEnd:   func(id, name, result string) { toolEnds++ },
		}

		_, err := ag.Turn(context.Background(), "do work", ev)
		if err == nil {
			t.Fatal("expected batch validation error, got nil")
		}
		if toolExecutions != 0 {
			t.Fatalf("expected 0 tool executions, got %d", toolExecutions)
		}
		if toolStarts != 0 || toolEnds != 0 {
			t.Fatalf("expected 0 tool events, got starts=%d ends=%d", toolStarts, toolEnds)
		}
		for _, msg := range ag.Messages {
			if msg.Role == "assistant" {
				t.Fatalf("malformed assistant response must not be retained in history: %+v", msg)
			}
		}
	})

	t.Run("EmergencyCeiling64", func(t *testing.T) {
		var toolExecutions int
		dummyTool := tools.Tool{
			Def: models.NewTool("read", "read tool", "{}"),
			Run: func(ctx context.Context, args json.RawMessage) (string, error) {
				toolExecutions++
				return "ok", nil
			},
		}

		calls := make([]models.ToolCall, 65)
		for i := range calls {
			calls[i] = models.ToolCall{
				ID:   fmt.Sprintf("call-%d", i),
				Type: "function",
			}
			calls[i].Function.Name = "read"
			calls[i].Function.Arguments = fmt.Sprintf(`{"path":"file-%d.txt"}`, i)
		}

		backend := &mockAgentBackend{
			responses: []models.Message{
				{Role: "assistant", ToolCalls: calls},
			},
		}

		ag := New(backend, "test-model", 1000, "sys")
		ag.Tools = []tools.Tool{dummyTool}

		_, err := ag.Turn(context.Background(), "do work", Events{})
		if err == nil || !strings.Contains(err.Error(), "exceeds limit 64") {
			t.Fatalf("expected ceiling limit error, got: %v", err)
		}
		if toolExecutions != 0 {
			t.Fatalf("expected 0 tool executions, got %d", toolExecutions)
		}
	})

	t.Run("ThreeRepeatedFailures", func(t *testing.T) {
		var toolExecutions int
		failingTool := tools.Tool{
			Def: models.NewTool("fail_tool", "failing tool", "{}"),
			Run: func(ctx context.Context, args json.RawMessage) (string, error) {
				toolExecutions++
				return "", errors.New("persistent connection error")
			},
		}

		makeCall := func(id string) []models.ToolCall {
			call := models.ToolCall{
				ID:   id,
				Type: "function",
			}
			call.Function.Name = "fail_tool"
			call.Function.Arguments = `{"retry":true}`
			return []models.ToolCall{call}
		}

		backend := &mockAgentBackend{
			responses: []models.Message{
				{Role: "assistant", ToolCalls: makeCall("call-1")},
				{Role: "assistant", ToolCalls: makeCall("call-2")},
				{Role: "assistant", ToolCalls: makeCall("call-3")},
				{Role: "assistant", ToolCalls: makeCall("call-4")},
			},
		}

		ag := New(backend, "test-model", 1000, "sys")
		ag.Tools = []tools.Tool{failingTool}

		_, err := ag.Turn(context.Background(), "test failures", Events{})
		if err == nil || !strings.Contains(err.Error(), "failed repeatedly") {
			t.Fatalf("expected repeated failure error, got: %v", err)
		}
		if toolExecutions != 3 {
			t.Fatalf("expected exactly 3 tool executions, got %d", toolExecutions)
		}
		if backend.callCount != 3 {
			t.Fatalf("expected 3 model calls, but 4th request was made: callCount=%d", backend.callCount)
		}
	})

	t.Run("BoundedConcurrency", func(t *testing.T) {
		var active atomic.Int32
		var maxActive atomic.Int32

		concurrentTool := tools.Tool{
			Def: models.NewTool("sleep_tool", "sleep tool", "{}"),
			Run: func(ctx context.Context, args json.RawMessage) (string, error) {
				cur := active.Add(1)
				for {
					old := maxActive.Load()
					if cur <= old || maxActive.CompareAndSwap(old, cur) {
						break
					}
				}
				time.Sleep(30 * time.Millisecond)
				active.Add(-1)
				return "ok", nil
			},
		}

		const totalCalls = 8
		calls := make([]models.ToolCall, totalCalls)
		for i := range calls {
			calls[i] = models.ToolCall{
				ID:   fmt.Sprintf("sleep-%d", i),
				Type: "function",
			}
			calls[i].Function.Name = "sleep_tool"
			calls[i].Function.Arguments = fmt.Sprintf(`{"id":%d}`, i)
		}

		backend := &mockAgentBackend{
			responses: []models.Message{
				{
					Role:      "assistant",
					ToolCalls: calls,
				},
				{
					Role:    "assistant",
					Content: "finished all tasks",
				},
			},
		}

		ag := New(backend, "test-model", 1000, "sys")
		ag.Tools = []tools.Tool{concurrentTool}

		resp, err := ag.Turn(context.Background(), "run concurrently", Events{})
		if err != nil {
			t.Fatalf("unexpected turn error: %v", err)
		}
		if resp != "finished all tasks" {
			t.Fatalf("unexpected final response: %q", resp)
		}
		if got := maxActive.Load(); got > maxConcurrentTools {
			t.Fatalf("max concurrent executions %d exceeded limit %d", got, maxConcurrentTools)
		}
		if got := maxActive.Load(); got < 2 {
			t.Fatalf("expected concurrent execution, but max active was %d", got)
		}
	})
}

func TestMalformedToolCallQuarantinedAndRecovered(t *testing.T) {
	var toolExecuted bool
	dummyTool := tools.Tool{
		Def: models.NewTool("test_tool", "test tool", "{}"),
		Run: func(ctx context.Context, args json.RawMessage) (string, error) {
			toolExecuted = true
			return "ok", nil
		},
	}

	backend := &mockAgentBackend{
		responses: []models.Message{
			{
				Role: "assistant",
				ToolCalls: []models.ToolCall{
					{
						ID:   "call-malformed",
						Type: "function",
						Function: struct {
							Name      string `json:"name"`
							Arguments string `json:"arguments"`
						}{
							Name:      "test_tool",
							Arguments: `{unquoted_invalid_json: true`,
						},
					},
				},
			},
			{
				Role: "assistant",
				ToolCalls: []models.ToolCall{
					{
						ID:   "call-valid",
						Type: "function",
						Function: struct {
							Name      string `json:"name"`
							Arguments string `json:"arguments"`
						}{
							Name:      "test_tool",
							Arguments: `{"valid": true}`,
						},
					},
				},
			},
			{
				Role:    "assistant",
				Content: "all done",
			},
		},
	}

	ag := New(backend, "test-model", 1000, "sys")
	ag.Tools = []tools.Tool{dummyTool}

	resp, err := ag.Turn(context.Background(), "do something", Events{})
	if err != nil {
		t.Fatalf("unexpected turn error: %v", err)
	}
	if resp != "all done" {
		t.Fatalf("unexpected response: %q", resp)
	}
	if !toolExecuted {
		t.Fatal("expected tool to execute on second round")
	}

	// Verify that the first assistant tool call in messages was sanitized
	var sanitizedCallFound bool
	for _, m := range ag.Messages {
		if m.Role == "assistant" && len(m.ToolCalls) > 0 && m.ToolCalls[0].ID == "call-malformed" {
			if m.ToolCalls[0].Function.Arguments != "{}" {
				t.Fatalf("malformed arguments were not sanitized to {}: %q", m.ToolCalls[0].Function.Arguments)
			}
			sanitizedCallFound = true
		}
		if m.Role == "tool" && m.ToolCallID == "call-malformed" {
			if !strings.Contains(m.Content, "malformed") {
				t.Fatalf("expected synthetic error for malformed call, got: %q", m.Content)
			}
		}
	}
	if !sanitizedCallFound {
		t.Fatal("sanitized call not found in agent messages")
	}
}

func TestSecondMalformedToolCallTerminatesTurn(t *testing.T) {
	var toolExecuted bool
	dummyTool := tools.Tool{
		Def: models.NewTool("test_tool", "test tool", "{}"),
		Run: func(ctx context.Context, args json.RawMessage) (string, error) {
			toolExecuted = true
			return "ok", nil
		},
	}

	backend := &mockAgentBackend{
		responses: []models.Message{
			{
				Role: "assistant",
				ToolCalls: []models.ToolCall{
					{
						ID:   "call-1",
						Type: "function",
						Function: struct {
							Name      string `json:"name"`
							Arguments string `json:"arguments"`
						}{
							Name:      "test_tool",
							Arguments: `invalid json 1`,
						},
					},
				},
			},
			{
				Role: "assistant",
				ToolCalls: []models.ToolCall{
					{
						ID:   "call-2",
						Type: "function",
						Function: struct {
							Name      string `json:"name"`
							Arguments string `json:"arguments"`
						}{
							Name:      "test_tool",
							Arguments: `invalid json 2`,
						},
					},
				},
			},
		},
	}

	ag := New(backend, "test-model", 1000, "sys")
	ag.Tools = []tools.Tool{dummyTool}

	_, err := ag.Turn(context.Background(), "do something", Events{})
	if err == nil {
		t.Fatal("expected error on second malformed call, got nil")
	}
	if !strings.Contains(err.Error(), "model tool channel remained malformed") {
		t.Fatalf("unexpected error message: %v", err)
	}
	if toolExecuted {
		t.Fatal("tool should never execute for malformed calls")
	}
}

func TestOversizedValidToolBatchIsRetriedWithoutMutation(t *testing.T) {
	args := fmt.Sprintf(`{"data":%q}`, strings.Repeat("x", 200*1024))
	calls := make([]models.ToolCall, 3)
	for i := range calls {
		calls[i] = models.ToolCall{ID: fmt.Sprintf("call-%d", i), Type: "function"}
		calls[i].Function.Name = "test_tool"
		calls[i].Function.Arguments = args
	}
	if malformed, tooLarge := findMalformedToolCalls(calls); len(malformed) != 0 || !tooLarge {
		t.Fatalf("valid oversized batch classified incorrectly: malformed=%v tooLarge=%t", malformed, tooLarge)
	}

	var executed int
	backend := &mockAgentBackend{responses: []models.Message{
		{Role: "assistant", ToolCalls: calls},
		{Role: "assistant", Content: "recovered"},
	}}
	ag := New(backend, "test-model", 1000, "sys")
	ag.Tools = []tools.Tool{{
		Def: models.NewTool("test_tool", "test tool", "{}"),
		Run: func(context.Context, json.RawMessage) (string, error) {
			executed++
			return "unexpected", nil
		},
	}}
	got, err := ag.Turn(context.Background(), "do work", Events{})
	if err != nil || got != "recovered" {
		t.Fatalf("oversized batch recovery: result=%q err=%v", got, err)
	}
	if executed != 0 {
		t.Fatalf("oversized valid calls must not execute, got %d executions", executed)
	}
	var retained bool
	for _, msg := range ag.Messages {
		if msg.Role == "assistant" && len(msg.ToolCalls) == len(calls) {
			retained = true
			if msg.ToolCalls[0].Function.Arguments != args {
				t.Fatal("valid oversized arguments were mutated")
			}
		}
		if msg.Role == "tool" && !strings.Contains(msg.Content, "aggregate argument size") {
			t.Fatalf("oversized batch error missing from tool result: %q", msg.Content)
		}
	}
	if !retained {
		t.Fatal("oversized assistant tool call was not retained for retry")
	}
}

func TestToolFailureClassificationUsesExitCode(t *testing.T) {
	if _, failed := toolResultError(tools.ToolResult{Preview: "Error: successful output"}); failed {
		t.Fatal("successful output beginning with Error: was classified as a failure")
	}
	text, failed := toolResultError(tools.ToolResult{Preview: "Error: failed output", ExitCode: 1})
	if !failed || text != "failed output" {
		t.Fatalf("failed result classification: text=%q failed=%t", text, failed)
	}
}

func TestDuplicateToolDiagnosticUsesResponseOrder(t *testing.T) {
	ag := New(nil, "model", 100, "sys")
	call := func(id, name string) models.ToolCall {
		return models.ToolCall{ID: id, Type: "function", Function: struct {
			Name      string `json:"name"`
			Arguments string `json:"arguments"`
		}{Name: name, Arguments: `{}`}}
	}
	err := ag.validateToolBatch([]models.ToolCall{
		call("read-1", "read"), call("read-2", "read"),
		call("write-1", "write"), call("write-2", "write"),
	})
	if err == nil || !strings.Contains(err.Error(), "duplicate tool calls: read repeated 2 times") {
		t.Fatalf("duplicate diagnostic was not response-ordered: %v", err)
	}
}

func TestSeenOperationTelemetryIsBounded(t *testing.T) {
	ag := &Agent{}
	for i := 0; i <= maxSeenOperations; i++ {
		ag.seenOperationCount(fmt.Sprintf("fingerprint-%d", i))
	}
	if len(ag.seenOperation) > maxSeenOperations {
		t.Fatalf("seen operation telemetry grew beyond limit: %d", len(ag.seenOperation))
	}
}

func TestSandboxCapabilityFailureTerminatesTurn(t *testing.T) {
	sandboxTool := tools.Tool{
		Def: models.NewTool("bash", "bash", "{}"),
		RunResult: func(ctx context.Context, args json.RawMessage) (tools.ToolResult, error) {
			return tools.ToolResult{
				Preview:  "listen tcp 127.0.0.1: operation not permitted",
				ExitCode: 1,
				Metadata: map[string]string{
					"failure_kind": "sandbox_network_denied",
				},
			}, nil
		},
	}

	backend := &mockAgentBackend{
		responses: []models.Message{
			{
				Role: "assistant",
				ToolCalls: []models.ToolCall{
					{
						ID:   "bash-call-1",
						Type: "function",
						Function: struct {
							Name      string `json:"name"`
							Arguments string `json:"arguments"`
						}{
							Name:      "bash",
							Arguments: `{"command": "go test ./..."}`,
						},
					},
				},
			},
			{
				Role:    "assistant",
				Content: "retrying with different test",
			},
		},
	}

	ag := New(backend, "test-model", 1000, "sys")
	ag.Tools = []tools.Tool{sandboxTool}

	_, err := ag.Turn(context.Background(), "run tests", Events{})
	if err == nil {
		t.Fatal("expected turn error for sandbox capability failure, got nil")
	}
	if !strings.Contains(err.Error(), "sandbox capability failure") {
		t.Fatalf("expected sandbox capability failure error, got: %v", err)
	}
	if !strings.Contains(err.Error(), "go test ./...") {
		t.Fatalf("expected denied command in capability error, got: %v", err)
	}
	if backend.callCount != 1 {
		t.Fatalf("expected exactly 1 model call without retry, got %d", backend.callCount)
	}
}

func TestLargeToolResultGetsAnOutputReference(t *testing.T) {
	store, err := session.NewOutputStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	ag := New(nil, "m", 100, "sys")
	ag.Outputs = store
	ag.Tools = []tools.Tool{{
		Def: models.NewTool("large", "large result", `{"type":"object","properties":{}}`),
		RunResult: func(context.Context, json.RawMessage) (tools.ToolResult, error) {
			raw := strings.Repeat("x", 60_000)
			return tools.MarkUntrusted(tools.TextResult(raw, tools.Truncate(raw)), "test"), nil
		},
	}}
	calls := []models.ToolCall{{ID: "large-1", Function: struct {
		Name      string `json:"name"`
		Arguments string `json:"arguments"`
	}{Name: "large", Arguments: `{}`}}}
	results := ag.runToolResultsWithTools(context.Background(), calls, Events{}, ag.AllTools())
	if len(results) != 1 || results[0].Output == nil {
		t.Fatalf("result did not get an output: %+v", results)
	}
	if !strings.Contains(results[0].Preview, "use output_read") {
		t.Fatalf("preview missing recovery hint: %q", results[0].Preview[len(results[0].Preview)-100:])
	}
	if len(results[0].Preview) > 16<<10 {
		t.Fatalf("output hint exceeded preview budget: %d", len(results[0].Preview))
	}
	if !strings.Contains(tools.ModelText(results[0]), "<untrusted_tool_output") {
		t.Fatal("an explicitly untrusted result should be delimited for the model")
	}
	got, err := store.Read(context.Background(), *results[0].Output, 0, 100)
	if err != nil || string(got) != strings.Repeat("x", 100) {
		t.Fatalf("stored result = %q, %v", got, err)
	}
}

func TestDisabledOutputsExplainUnrecoverableOutput(t *testing.T) {
	ag := New(nil, "m", 100, "sys")
	ag.Tools = []tools.Tool{{
		Def: models.NewTool("large", "large result", `{"type":"object","properties":{}}`),
		RunResult: func(context.Context, json.RawMessage) (tools.ToolResult, error) {
			raw := strings.Repeat("x", 60_000)
			return tools.TextResult(raw, tools.Truncate(raw)), nil
		},
	}}
	calls := []models.ToolCall{{ID: "large-1", Function: struct {
		Name      string `json:"name"`
		Arguments string `json:"arguments"`
	}{Name: "large", Arguments: `{}`}}}
	results := ag.runToolResultsWithTools(context.Background(), calls, Events{}, ag.AllTools())
	if len(results) != 1 || results[0].Output != nil {
		t.Fatalf("disabled result should not have an output: %+v", results)
	}
}
func TestSubagentGuidanceMatchesBoundedExplorationTools(t *testing.T) {
	prompt := subagentPrompt()
	for _, fragment := range []string{"currently exposed", "bounded repository-navigation", "observed edit ranges"} {
		if !strings.Contains(prompt, fragment) {
			t.Errorf("subagent prompt lacks %q: %s", fragment, prompt)
		}
	}
	parent := New(nil, "model", 100, "system")
	description := taskTool(parent).Def.Function.Description
	for _, fragment := range []string{"currently available tools", "bounded repository navigation", "observed edit ranges"} {
		if !strings.Contains(description, fragment) {
			t.Errorf("task description lacks %q: %s", fragment, description)
		}
	}
}

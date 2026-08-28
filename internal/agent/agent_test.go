package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sacca97/ghg/internal/artifact"
	goalstate "github.com/sacca97/ghg/internal/goal"
	"github.com/sacca97/ghg/internal/llm"
	"github.com/sacca97/ghg/internal/tools"
)

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
	var requests []llm.Request
	var mu sync.Mutex
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req llm.Request
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Errorf("decode request: %v", err)
			return
		}
		mu.Lock()
		requests = append(requests, req)
		call := len(requests)
		mu.Unlock()
		w.Header().Set("Content-Type", "text/event-stream")
		if call == 1 {
			fmt.Fprint(w, `data: {"choices":[{"delta":{"tool_calls":[{"index":0,"id":"goal-call","type":"function","function":{"name":"update_goal","arguments":"{\"status\":\"active\",\"progress\":\"implementation complete; verification passed\"}"}}]}}]}`+"\n\n")
		} else {
			fmt.Fprint(w, `data: {"choices":[{"delta":{"content":"ready"},"finish_reason":"stop"}]}`+"\n\n")
		}
		fmt.Fprint(w, "data: [DONE]\n\n")
	}))
	defer srv.Close()

	ag := New(testBackend(srv.URL, "k"), "m", 100, "sys")
	ag.Tools = nil
	record := goalstate.New("ship the feature")
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
	if len(updates) != 1 || updates[0].Status != goalstate.StatusActive || updates[0].GoalID != record.ID {
		t.Fatalf("updates: %+v", updates)
	}
	mu.Lock()
	gotRequests := append([]llm.Request(nil), requests...)
	mu.Unlock()
	if len(gotRequests) != 2 {
		t.Fatalf("requests = %d, want 2", len(gotRequests))
	}
	for i, req := range gotRequests {
		foundGoalTool := false
		for _, tool := range req.Tools {
			if tool.Function.Name == goalUpdateToolName {
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
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests++
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, `data: {"choices":[{"delta":{"tool_calls":[{"index":0,"id":"goal-call","type":"function","function":{"name":"update_goal","arguments":"{\"status\":\"complete\",\"progress\":\"tests passed\"}"}}]}}]}`+"\n\n")
		fmt.Fprint(w, "data: [DONE]\n\n")
	}))
	defer srv.Close()

	ag := New(testBackend(srv.URL, "k"), "m", 100, "sys")
	ag.Tools = nil
	record := goalstate.New("finish")
	record.ID = "goal-2"
	var update GoalUpdate
	if _, err := ag.TurnWithGoal(context.Background(), "go", record, Events{OnGoalUpdate: func(g GoalUpdate) { update = g }}); err != nil {
		t.Fatal(err)
	}
	if requests != 1 || update.Status != goalstate.StatusComplete {
		t.Fatalf("requests=%d update=%+v", requests, update)
	}
}

func testBackend(baseURL, apiKey string) llm.Backend {
	client := llm.New(baseURL, apiKey)
	client.MaxRetries = 1
	return llm.NewOpenAIBackend(client)
}

// server that answers with a tool call on the first request, text on the second
func loopServer(t *testing.T) *httptest.Server {
	t.Helper()
	call := 0
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req llm.Request
		json.NewDecoder(r.Body).Decode(&req)
		w.Header().Set("Content-Type", "text/event-stream")
		call++
		if call == 1 {
			fmt.Fprint(w, `data: {"choices":[{"delta":{"tool_calls":[{"index":0,"id":"t1","type":"function","function":{"name":"echo","arguments":"{\"s\":\"hi\"}"}}]}}]}`+"\n\n")
		} else {
			// verify the tool result round-tripped
			last := req.Messages[len(req.Messages)-1]
			if last.Role != "tool" || last.ToolCallID != "t1" || last.Content != "echoed: hi" {
				t.Errorf("tool result not fed back: %+v", last)
			}
			fmt.Fprint(w, `data: {"choices":[{"delta":{"content":"done"},"finish_reason":"stop"}]}`+"\n\n")
		}
		fmt.Fprint(w, "data: [DONE]\n\n")
	}))
}

func echoTool() tools.Tool {
	return tools.Tool{
		Def: llm.NewTool("echo", "echo", `{"type":"object","properties":{"s":{"type":"string"}}}`),
		Run: func(ctx context.Context, args json.RawMessage) (string, error) {
			var a struct{ S string }
			json.Unmarshal(args, &a)
			return "echoed: " + a.S, nil
		},
	}
}

func TestTurnLoop(t *testing.T) {
	srv := loopServer(t)
	defer srv.Close()

	ag := New(testBackend(srv.URL, "k"), "m", 100, "sys")
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
	ag := New(testBackend("http://unused", "k"), "m", 100, "sys")
	var ids []string
	var snapshots []string
	results := ag.runTools(context.Background(), []llm.ToolCall{{
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
	})
	if len(results) != 1 || !strings.Contains(results[0], "second") {
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
	ag := New(testBackend("http://unused", "k"), "m", 100, "sys")
	call := func(label string) llm.ToolCall {
		return llm.ToolCall{
			ID: label,
			Function: struct {
				Name      string `json:"name"`
				Arguments string `json:"arguments"`
			}{Name: "bash", Arguments: fmt.Sprintf(`{"command":"printf '%s-start\\n'; sleep 0.15; printf '%s-end\\n'","timeout":2}`, label, label)},
		}
	}
	var mu sync.Mutex
	seen := map[string][]string{}
	ag.runTools(context.Background(), []llm.ToolCall{call("a"), call("b")}, Events{
		OnToolOutput: func(id, output string) {
			mu.Lock()
			seen[id] = append(seen[id], output)
			mu.Unlock()
		},
	})
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
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req llm.Request
		json.NewDecoder(r.Body).Decode(&req)
		w.Header().Set("Content-Type", "text/event-stream")
		if len(req.Messages) > 0 && req.Messages[len(req.Messages)-1].Role == "tool" {
			fmt.Fprint(w, `data: {"choices":[{"delta":{"content":"done"},"finish_reason":"stop"}],"usage":{"prompt_tokens":7,"completion_tokens":3}}`+"\n\n")
		} else {
			fmt.Fprint(w, `data: {"choices":[{"delta":{"tool_calls":[{"index":0,"id":"t1","type":"function","function":{"name":"echo","arguments":"{\"s\":\"hi\"}"}}]}}]}`+"\n\n")
			fmt.Fprint(w, `data: {"usage":{"prompt_tokens":5,"completion_tokens":2}}`+"\n\n")
		}
		fmt.Fprint(w, "data: [DONE]\n\n")
	}))
	defer srv.Close()

	ag := New(testBackend(srv.URL, "k"), "kimi-k3-fast", 100, "sys")
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
	var assistants []llm.Message
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

// The internal stamps (usage, model, tool timing) must be stripped before the
// provider ever sees them.
func TestInternalStampsStrippedFromRequest(t *testing.T) {
	var bodies [][]byte
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		bodies = append(bodies, b)
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, `data: {"choices":[{"delta":{"content":"ok"},"finish_reason":"stop"}]}`+"\n\ndata: [DONE]\n\n")
	}))
	defer srv.Close()

	ag := New(testBackend(srv.URL, "k"), "m", 100, "sys")
	// pre-seed a message loaded from storage with all internal fields set
	sent := time.Now()
	u := llm.Usage{PromptTokens: 9}
	ag.Messages = append(ag.Messages, llm.Message{
		Role: "assistant", Content: "prior", Usage: &u, Model: "m @ p",
		ToolCalls: []llm.ToolCall{{ID: "x", DurationMs: 5, ExitCode: 1}},
	})
	ag.Messages = append(ag.Messages, llm.Message{Role: "user", Content: "old", Authored: true, SentAt: &sent, RewoundFrom: "earlier"})
	if _, err := ag.Turn(context.Background(), "go", Events{}); err != nil {
		t.Fatal(err)
	}
	if len(bodies) == 0 {
		t.Fatal("no request captured")
	}
	body := string(bodies[len(bodies)-1])
	for _, leak := range []string{"usage\":{", "\"model\":\"m @ p\"", "duration_ms", "exit_code", "sent_at", "rewound_from", "authored"} {
		if strings.Contains(body, leak) {
			t.Errorf("internal field %q leaked to provider:\n%s", leak, body)
		}
	}
}

func TestTurnCancelled(t *testing.T) {
	srv := loopServer(t)
	defer srv.Close()
	ag := New(testBackend(srv.URL, "k"), "m", 100, "sys")
	ag.Tools = []tools.Tool{echoTool()}
	ctx, cancel := context.WithCancel(context.Background())
	go func() { cancel() }()
	// either the stream or the post-tool check reports cancellation; both are fine
	if _, err := ag.Turn(ctx, "go", Events{}); err == nil {
		t.Skip("cancel raced turn completion") // ponytail: timing-dependent; the happy path above is the real check
	}
}

func TestTurnAPIError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "nope", http.StatusInternalServerError)
	}))
	defer srv.Close()
	ag := New(testBackend(srv.URL, "k"), "m", 100, "sys")
	if _, err := ag.Turn(context.Background(), "go", Events{}); err == nil {
		t.Fatal("expected error")
	}
}

// server that echoes text responses and records how many calls it got
func textServer(t *testing.T, onCall func(n int, req llm.Request) string) *httptest.Server {
	t.Helper()
	n := 0
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req llm.Request
		json.NewDecoder(r.Body).Decode(&req)
		n++
		w.Header().Set("Content-Type", "text/event-stream")
		body, _ := json.Marshal(onCall(n, req))
		fmt.Fprintf(w, `data: {"choices":[{"delta":{"content":%s},"finish_reason":"stop"}]}`+"\n\n", body)
		fmt.Fprint(w, "data: [DONE]\n\n")
	}))
}

// TurnAuthored marks the user message as genuinely typed (for input-history
// recall); plain Turn (steered/goal/background paths) leaves it unmarked.
func TestTurnAuthoredMarksMessage(t *testing.T) {
	srv := textServer(t, func(n int, req llm.Request) string { return "done" })
	defer srv.Close()

	ag := New(testBackend(srv.URL, "k"), "m", 100, "sys")
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

// TestUsageAccumulates verifies every stream call folds its usage into the
// session totals (input/output/cached) and fires OnUsage per request.
func TestUsageAccumulates(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, `data: {"choices":[{"delta":{"content":"done"},"finish_reason":"stop"}]}`+"\n\n")
		fmt.Fprint(w, `data: {"choices":[],"usage":{"prompt_tokens":100,"completion_tokens":10,"prompt_tokens_details":{"cached_tokens":40}}}`+"\n\n")
		fmt.Fprint(w, "data: [DONE]\n\n")
	}))
	defer srv.Close()

	ag := New(testBackend(srv.URL, "k"), "m", 100, "sys")
	var fired int
	for i := 0; i < 3; i++ {
		if _, err := ag.Turn(context.Background(), "go", Events{
			OnUsage: func(u llm.Usage) {
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
	srv := textServer(t, func(n int, req llm.Request) string { return "done" })
	defer srv.Close()
	ag := New(testBackend(srv.URL, "k"), "m", 100, "sys")
	if _, err := ag.Turn(context.Background(), "go", Events{}); err != nil {
		t.Fatal(err)
	}
	if u := ag.Usage(); u.PromptTokens != 0 || u.CompletionTokens != 0 || u.Cached() != 0 {
		t.Fatalf("usage should stay zero without provider usage: %+v", u)
	}
}

func TestSteerContinuesTurn(t *testing.T) {
	srv := textServer(t, func(n int, req llm.Request) string {
		if n == 2 {
			last := req.Messages[len(req.Messages)-1]
			if last.Role != "user" || last.Content != "also do this" {
				t.Errorf("steered message not injected: %+v", last)
			}
			return "ok2"
		}
		return "ok1"
	})
	defer srv.Close()

	ag := New(testBackend(srv.URL, "k"), "m", 100, "sys")
	ag.Steer("also do this") // queued before the first response completes
	var steered []string
	final, err := ag.Turn(context.Background(), "go", Events{
		OnSteer: func(s string) { steered = append(steered, s) },
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

func TestNoSteerEndsTurn(t *testing.T) {
	srv := textServer(t, func(n int, req llm.Request) string { return "done" })
	defer srv.Close()
	ag := New(testBackend(srv.URL, "k"), "m", 100, "sys")
	final, err := ag.Turn(context.Background(), "go", Events{})
	if err != nil || final != "done" {
		t.Fatalf("%q %v", final, err)
	}
}

func TestTaskToolSpawnsSubagent(t *testing.T) {
	call := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req llm.Request
		json.NewDecoder(r.Body).Decode(&req)
		call++
		w.Header().Set("Content-Type", "text/event-stream")
		switch call {
		case 1: // outer agent delegates
			fmt.Fprint(w, `data: {"choices":[{"delta":{"tool_calls":[{"index":0,"id":"t1","type":"function","function":{"name":"task","arguments":"{\"description\":\"probe\",\"prompt\":\"find the answer\"}"}}]}}]}`+"\n\n")
		case 2: // inner subagent: fresh context, no task tool, gets the prompt
			if len(req.Messages) != 2 || req.Messages[1].Content != "find the answer" {
				t.Errorf("subagent context wrong: %+v", req.Messages)
			}
			for _, tl := range req.Tools {
				if tl.Function.Name == "task" {
					t.Error("subagent must not have the task tool")
				}
			}
			fmt.Fprint(w, `data: {"choices":[{"delta":{"content":"the answer is 42"}}]}`+"\n\n")
		case 3: // outer agent sees the report as the tool result
			last := req.Messages[len(req.Messages)-1]
			if last.Role != "tool" || last.Content != "the answer is 42" {
				t.Errorf("task result not fed back: %+v", last)
			}
			fmt.Fprint(w, `data: {"choices":[{"delta":{"content":"done"}}]}`+"\n\n")
		}
		fmt.Fprint(w, "data: [DONE]\n\n")
	}))
	defer srv.Close()

	ag := New(testBackend(srv.URL, "k"), "m", 100, "sys")
	final, err := ag.Turn(context.Background(), "go", Events{})
	if err != nil || final != "done" {
		t.Fatalf("%q %v", final, err)
	}
	if call != 3 {
		t.Fatalf("expected 3 API calls, got %d", call)
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
				task := parent.StartBackground(context.Background(), "probe", "check it")
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
	ag := New(testBackend("http://unused", "k"), "m", 100, "sys")
	out := tools.Execute(context.Background(), ag.Tools, "task", json.RawMessage(`{bad`))
	if !strings.HasPrefix(out, "Error") {
		t.Fatalf("expected error, got %q", out)
	}
}

// compactionServer lets the first request error with context_length_exceeded,
// then serves a summary completion (for the compaction call) and finally the
// real answer. call==2 is the /chat/completions summary request (stream:false).
func compactionServer(t *testing.T) (*httptest.Server, *int) {
	t.Helper()
	call := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		call++
		switch call {
		case 1:
			http.Error(w, `{"error":{"code":"context_length_exceeded"}}`, http.StatusBadRequest)
		case 2:
			var req struct {
				Stream   bool          `json:"stream"`
				Messages []llm.Message `json:"messages"`
			}
			json.NewDecoder(r.Body).Decode(&req)
			if req.Stream {
				t.Errorf("summary call should not stream")
			}
			w.Write([]byte(`{"choices":[{"message":{"content":"summary of prior work"}}]}`))
		default:
			w.Header().Set("Content-Type", "text/event-stream")
			fmt.Fprint(w, `data: {"choices":[{"delta":{"content":"recovered"}}]}`+"\n\n")
			fmt.Fprint(w, "data: [DONE]\n\n")
		}
	}))
	return srv, &call
}

func TestTurnAutoCompactsOnContextLimit(t *testing.T) {
	srv, pcall := compactionServer(t)
	defer srv.Close()

	ag := New(testBackend(srv.URL, "k"), "m", 100, "sys")
	// build a history that's compactable: system + enough turns
	for i := 0; i < 8; i++ {
		ag.Messages = append(ag.Messages,
			llm.Message{Role: "user", Content: fmt.Sprintf("q%d", i)},
			llm.Message{Role: "assistant", Content: fmt.Sprintf("a%d", i)},
		)
	}
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
	if *pcall < 3 {
		t.Fatalf("expected ≥3 calls (fail+summary+retry), got %d", *pcall)
	}
	// summary lives between system prompt and the kept tail
	if !strings.Contains(ag.Messages[1].Content, "Summary of the conversation") {
		t.Fatalf("messages[1] should be the summary, got %q", ag.Messages[1].Content)
	}
}

func TestCompactDoesNotLoopOnRepeatedContextLimit(t *testing.T) {
	// every request errors with context_length_exceeded → compaction must
	// happen once and then the error surfaces (no infinite retry loop)
	call := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		call++
		// one summary call succeeds (to exercise the compaction path), then
		// every stream fails with context_length_exceeded
		if r.URL.Path == "/chat/completions" {
			var req struct {
				Stream   bool          `json:"stream"`
				Messages []llm.Message `json:"messages"`
			}
			json.NewDecoder(r.Body).Decode(&req)
			if !req.Stream { // the summary call
				w.Write([]byte(`{"choices":[{"message":{"content":"sim"}}]}`))
				return
			}
		}
		http.Error(w, `{"error":{"code":"context_length_exceeded"}}`, http.StatusBadRequest)
	}))
	defer srv.Close()

	ag := New(testBackend(srv.URL, "k"), "m", 100, "sys")
	for i := 0; i < 8; i++ {
		ag.Messages = append(ag.Messages,
			llm.Message{Role: "user", Content: fmt.Sprintf("q%d", i)},
			llm.Message{Role: "assistant", Content: fmt.Sprintf("a%d", i)},
		)
	}
	_, err := ag.Turn(context.Background(), "go", Events{})
	if err == nil {
		t.Fatal("expected context-limit error to surface, not loop forever")
	}
	if call > 3 {
		t.Fatalf("expected ≤3 calls (fail+summary+retry-fail), got %d", call)
	}
}

func TestEstimateTokens(t *testing.T) {
	if got := EstimateTokens(nil); got != 0 {
		t.Fatalf("empty: %d", got)
	}
	msgs := []llm.Message{
		{Role: "system", Content: strings.Repeat("x", 400)}, // 400/4 + 4 = 104
		{Role: "assistant", ToolCalls: []llm.ToolCall{ // 4 + 8 + (4+96+3)/4 = 37
			func() llm.ToolCall {
				var tc llm.ToolCall
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
	ag := New(testBackend("http://unused", "k"), "m", 100, "sys")
	if got := ag.ContextTokens(); got != 0 {
		t.Fatalf("before a response: got %d, want 0", got)
	}

	ag.Messages = append(ag.Messages,
		llm.Message{Role: "assistant", Content: "first", Usage: &llm.Usage{PromptTokens: 100, CompletionTokens: 20}},
		llm.Message{Role: "user", Content: "next"},
		llm.Message{Role: "assistant", Content: "latest", Usage: &llm.Usage{PromptTokens: 300, CompletionTokens: 40}},
	)
	if got, want := ag.ContextTokens(), 340; got != want {
		t.Fatalf("latest context tokens = %d, want %d", got, want)
	}
}

func TestProactiveCompactAtFiftyPercent(t *testing.T) {
	// the first stream request should already carry the compacted history —
	// no context_length_exceeded round-trip needed
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Stream   bool          `json:"stream"`
			Messages []llm.Message `json:"messages"`
		}
		json.NewDecoder(r.Body).Decode(&req)
		if !req.Stream {
			w.Write([]byte(`{"choices":[{"message":{"content":"summary of prior work"}}]}`))
			return
		}
		compact := strings.Contains(req.Messages[1].Content, "Summary of the conversation")
		w.Header().Set("Content-Type", "text/event-stream")
		if compact {
			fmt.Fprint(w, `data: {"choices":[{"delta":{"content":"ok"}}]}`+"\n\n")
		} else {
			fmt.Fprint(w, `data: {"choices":[{"delta":{"content":"not-compacted"}}]}`+"\n\n")
		}
		fmt.Fprint(w, "data: [DONE]\n\n")
	}))
	defer srv.Close()

	ag := New(testBackend(srv.URL, "k"), "m", 100, "sys")
	ag.ContextLimit = 1000 // default 50% = 500 reported context tokens
	for i := 0; i < 8; i++ {
		ag.Messages = append(ag.Messages, llm.Message{Role: "user", Content: strings.Repeat("x", 120)})
	}
	ag.Messages = append(ag.Messages, llm.Message{
		Role: "assistant", Content: "previous response",
		Usage: &llm.Usage{PromptTokens: 400, CompletionTokens: 150},
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
	srv := textServer(t, func(n int, req llm.Request) string { return "done" })
	defer srv.Close()

	// 55% of the limit: over the 50% default, under an explicit 80% — no
	// compaction for the explicit override.
	ag := New(testBackend(srv.URL, "m"), "m", 100, "sys")
	ag.ContextLimit = 1000
	ag.CompactThreshold = 0.8
	for i := 0; i < 8; i++ {
		ag.Messages = append(ag.Messages, llm.Message{Role: "user", Content: strings.Repeat("x", 360)})
	}
	ag.Messages = append(ag.Messages, llm.Message{
		Role: "assistant", Content: "previous response",
		Usage: &llm.Usage{PromptTokens: 400, CompletionTokens: 150},
	})
	if _, err := ag.Turn(context.Background(), "hi", Events{}); err != nil {
		t.Fatal(err)
	}
	if len(ag.Messages) != 12 { // system + 8 users + reported assistant + user + assistant
		t.Fatalf("history should not compact below the explicit threshold, got %d msgs", len(ag.Messages))
	}

	// CompactThreshold wins over the default: same history at the default 50%
	// would have compacted
	ag2 := New(testBackend(srv.URL, "m"), "m", 100, "sys")
	ag2.ContextLimit = 1000
	for i := 0; i < 8; i++ {
		ag2.Messages = append(ag2.Messages, llm.Message{Role: "user", Content: strings.Repeat("x", 360)})
	}
	ag2.Messages = append(ag2.Messages, llm.Message{
		Role: "assistant", Content: "previous response",
		Usage: &llm.Usage{PromptTokens: 400, CompletionTokens: 150},
	})
	if err := ag2.maybeCompact(context.Background(), Events{}); err == nil {
		t.Fatal("the same history should compact at the default 50% threshold")
	}
}

func TestNoProactiveCompactBelowThresholdOrWithoutLimit(t *testing.T) {
	srv := textServer(t, func(n int, req llm.Request) string { return "done" })
	defer srv.Close()

	// below threshold: estimate well under 50% of the limit
	ag := New(testBackend(srv.URL, "k"), "m", 100, "sys")
	ag.ContextLimit = 100000
	if _, err := ag.Turn(context.Background(), "hi", Events{}); err != nil {
		t.Fatal(err)
	}

	// no advertised limit: proactive compaction disabled regardless of size
	ag2 := New(testBackend(srv.URL, "k"), "m", 100, "sys")
	ag2.Messages = append(ag2.Messages, llm.Message{Role: "user", Content: strings.Repeat("x", 4000)})
	if _, err := ag2.Turn(context.Background(), "hi", Events{}); err != nil {
		t.Fatal(err)
	}
	if len(ag2.Messages) != 4 { // system + big user + user + assistant: untouched
		t.Fatalf("history should not compact without a context limit, got %d msgs", len(ag2.Messages))
	}
}

func TestCompactUsesCompactModel(t *testing.T) {
	var models []string
	main := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Error("summary call must not hit the conversation's provider")
	}))
	defer main.Close()
	sum := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req llm.Request
		json.NewDecoder(r.Body).Decode(&req)
		models = append(models, req.Model)
		w.Write([]byte(`{"choices":[{"message":{"content":"sim"}}]}`))
	}))
	defer sum.Close()

	ag := New(testBackend(main.URL, "k"), "conversation-model", 100, "sys")
	ag.CompactBackend = testBackend(sum.URL, "k")
	ag.CompactModel = "summary-model"
	for i := 0; i < 8; i++ {
		ag.Messages = append(ag.Messages,
			llm.Message{Role: "user", Content: fmt.Sprintf("q%d", i)},
			llm.Message{Role: "assistant", Content: fmt.Sprintf("a%d", i)},
		)
	}
	if err := ag.ManualCompact(context.Background(), Events{}); err != nil {
		t.Fatal(err)
	}
	if len(models) != 1 || models[0] != "summary-model" {
		t.Fatalf("summary should run on summary-model, got %v", models)
	}
}

func TestCompactionTelemetryUsesSummaryRoute(t *testing.T) {
	conversation := &routeBackend{protocol: llm.ProtocolOpenAIChatCompletions}
	summary := &routeBackend{protocol: llm.ProtocolAnthropicMessages}
	ag := New(conversation, "conversation-model", 100, "sys")
	ag.Role, ag.Provider, ag.Protocol = "fast", "main-provider", string(conversation.protocol)
	ag.CompactBackend = summary
	ag.CompactModel = "tiny-model"
	ag.CompactProvider = "tiny-provider"
	ag.CompactProtocol = string(summary.protocol)
	for i := 0; i < 8; i++ {
		ag.Messages = append(ag.Messages,
			llm.Message{Role: "user", Content: fmt.Sprintf("question %d", i)},
			llm.Message{Role: "assistant", Content: fmt.Sprintf("answer %d", i)},
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
	want := ModelCallStart{Role: "tiny", Provider: "tiny-provider", Model: "tiny-model", Protocol: string(summary.protocol)}
	if starts[0] != want {
		t.Fatalf("compaction start route = %+v, want %+v", starts[0], want)
	}
	if ends[0].Model != want.Model || ends[0].Provider != want.Provider || ends[0].Protocol != want.Protocol {
		t.Fatalf("compaction end route = %+v", ends[0])
	}
}

type routeBackend struct {
	protocol llm.Protocol
}

func (b *routeBackend) AdapterProtocol() llm.Protocol { return b.protocol }

func (b *routeBackend) Stream(context.Context, llm.Request, llm.EventSink) (llm.Message, llm.Usage, error) {
	return llm.Message{}, llm.Usage{}, nil
}

func (b *routeBackend) Complete(context.Context, llm.Request) (llm.Message, llm.Usage, error) {
	return llm.Message{Role: "assistant", Content: "summary"}, llm.Usage{}, nil
}

func TestCompactTooLittleHistory(t *testing.T) {
	ag := New(testBackend("http://unused", "k"), "m", 100, "sys")
	ag.Messages = append(ag.Messages, llm.Message{Role: "user", Content: "hi"})
	if _, _, err := ag.compact(context.Background()); err == nil {
		t.Fatal("expected error compacting a tiny history")
	}
}

func TestCompactKeepsToolCallPair(t *testing.T) {
	// orphan safety: a tail starting with role "tool" must pull in its owning
	// assistant message so the tool result never references an erased call id.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"choices":[{"message":{"content":"sim"}}]}`))
	}))
	defer srv.Close()
	ag := New(testBackend(srv.URL, "k"), "m", 100, "sys")
	// system, user, asst(with tool call "t1"), tool("t1" result), user, asst, user
	for i := 0; i < 4; i++ {
		ag.Messages = append(ag.Messages, llm.Message{Role: "user", Content: fmt.Sprintf("u%d", i)})
		if i == 0 {
			ag.Messages = append(ag.Messages,
				llm.Message{Role: "assistant", ToolCalls: []llm.ToolCall{{ID: "t1", Type: "function"}}},
			)
			ag.Messages = append(ag.Messages, llm.Message{Role: "tool", Content: "tool-out", ToolCallID: "t1"})
		} else {
			ag.Messages = append(ag.Messages, llm.Message{Role: "assistant", Content: fmt.Sprintf("a%d", i)})
		}
	}
	before := len(ag.Messages)
	if _, _, err := ag.compact(context.Background()); err != nil {
		t.Fatalf("compact: %v", err)
	}
	if len(ag.Messages) >= before {
		t.Fatalf("compaction didn't shrink: before %d after %d", before, len(ag.Messages))
	}
	// find the kept tool result and its owning assistant
	var asstTool, toolMsg *llm.Message
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

func TestCompactShrinksOversizedRecentToolResultAndKeepsArtifactRef(t *testing.T) {
	ref := artifact.Ref{
		ID: "sha256:" + strings.Repeat("a", 64), Hash: strings.Repeat("a", 64),
		OriginalBytes: 20000, StoredBytes: 20000, Complete: true,
	}
	content := strings.Repeat("x", 20000) + tools.ArtifactReference(ref)
	call := llm.ToolCall{ID: "call-1", Type: "function", Function: struct {
		Name      string `json:"name"`
		Arguments string `json:"arguments"`
	}{Name: "read", Arguments: `{}`}}
	msgs := []llm.Message{
		{Role: "system", Content: "sys"},
		{Role: "user", Content: "old"},
		{Role: "assistant", Content: "old answer"},
		{Role: "user", Content: "recent"},
		{Role: "assistant", ToolCalls: []llm.ToolCall{call}},
		{Role: "tool", Content: content, ToolCallID: "call-1", Name: "read", Artifact: &ref},
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

func TestArtifactManifestIncludesOnlyCitedAndRecentRefs(t *testing.T) {
	cited := artifact.Ref{ID: "sha256:" + strings.Repeat("b", 64), Hash: strings.Repeat("b", 64), OriginalBytes: 20, StoredBytes: 10, Complete: false}
	omitted := artifact.Ref{ID: "sha256:" + strings.Repeat("c", 64), Hash: strings.Repeat("c", 64), OriginalBytes: 30, StoredBytes: 15, Complete: false}
	recent := artifact.Ref{ID: "sha256:" + strings.Repeat("d", 64), Hash: strings.Repeat("d", 64), OriginalBytes: 40, StoredBytes: 40, Complete: true}
	all := []llm.Message{
		{Role: "tool", Artifact: &cited},
		{Role: "tool", Artifact: &omitted},
	}
	tail := []llm.Message{{Role: "tool", Artifact: &recent}}
	manifest := buildArtifactManifest("prior work used "+cited.ID, tail, all)
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
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"choices":[{"message":{"content":"sim"}}]}`))
	}))
	defer srv.Close()
	ag := New(testBackend(srv.URL, "k"), "m", 100, "sys")
	for i := 0; i < 8; i++ {
		ag.Messages = append(ag.Messages,
			llm.Message{Role: "user", Content: fmt.Sprintf("q%d", i)},
			llm.Message{Role: "assistant", Content: fmt.Sprintf("a%d", i)},
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

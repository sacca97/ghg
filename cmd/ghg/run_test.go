package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/sacca97/ghg/internal/llm"
	"github.com/sacca97/ghg/internal/session"
)

// runFixture writes a config pointing the default model at an SSE test
// server that replies with reply (and records each request into reqs).
func runFixture(t *testing.T, reply string, reqs *[]llm.Request) {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req llm.Request
		json.NewDecoder(r.Body).Decode(&req)
		if reqs != nil {
			*reqs = append(*reqs, req)
		}
		w.Header().Set("Content-Type", "text/event-stream")
		body, _ := json.Marshal(reply)
		fmt.Fprintf(w, `data: {"choices":[{"delta":{"content":%s},"finish_reason":"stop"}]}`+"\n\n", body)
		fmt.Fprint(w, "data: [DONE]\n\n")
	}))
	t.Cleanup(srv.Close)

	home := t.TempDir()
	t.Setenv("GHG_HOME", home)
	cfg := fmt.Sprintf(`{
		"defaultModel": "test",
		"providers": {"testprov": {"baseUrl": %q, "api": "openai-completions", "apiKey": "k"}},
		"models": {"test": {"providers": ["testprov"], "maxOut": 100}}
	}`, srv.URL)
	if err := os.WriteFile(filepath.Join(home, "config.json"), []byte(cfg), 0o600); err != nil {
		t.Fatal(err)
	}
}

func runPlanFixture(t *testing.T, reqs *[]llm.Request) {
	t.Helper()
	planArgs := `{"goal":"ship it","steps":["write code"],"acceptance_checks":["tests pass"]}`
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		data, err := io.ReadAll(r.Body)
		if err != nil {
			t.Errorf("read request: %v", err)
			return
		}
		var wire struct {
			Stream bool `json:"stream"`
		}
		if err := json.Unmarshal(data, &wire); err != nil {
			t.Errorf("decode request: %v", err)
			return
		}
		if reqs != nil {
			var req llm.Request
			if err := json.Unmarshal(data, &req); err != nil {
				t.Errorf("decode provider-neutral request: %v", err)
			} else {
				*reqs = append(*reqs, req)
			}
		}
		if !wire.Stream {
			w.Header().Set("Content-Type", "application/json")
			fmt.Fprintf(w, `{"choices":[{"message":{"role":"assistant","tool_calls":[{"id":"plan-1","type":"function","function":{"name":"submit_plan","arguments":%q}}]},"finish_reason":"tool_calls"}],"usage":{"prompt_tokens":2,"completion_tokens":3}}`, planArgs)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		body, _ := json.Marshal("executed")
		fmt.Fprintf(w, `data: {"choices":[{"delta":{"content":%s},"finish_reason":"stop"}],"usage":{"prompt_tokens":4,"completion_tokens":5}}`+"\n\n", body)
		fmt.Fprint(w, "data: [DONE]\n\n")
	}))
	t.Cleanup(srv.Close)

	home := t.TempDir()
	t.Setenv("GHG_HOME", home)
	cfg := fmt.Sprintf(`{
		"defaultModel": "test",
		"providers": {"testprov": {"baseUrl": %q, "api": "openai-completions", "apiKey": "k"}},
		"models": {"test": {"providers": ["testprov"], "maxOut": 100}}
	}`, srv.URL)
	if err := os.WriteFile(filepath.Join(home, "config.json"), []byte(cfg), 0o600); err != nil {
		t.Fatal(err)
	}
}

// runCapture swaps stdout/stdin for the duration of runCLI and returns what
// the run printed on stdout. stdinData is piped in ("" still leaves a
// non-TTY empty stdin, like `ghg run "…" < /dev/null`).
func runCapture(t *testing.T, stdinData string, args ...string) (string, error) {
	t.Helper()

	oldIn := os.Stdin
	inR, inW, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := inW.WriteString(stdinData); err != nil {
		t.Fatal(err)
	}
	inW.Close()
	os.Stdin = inR
	defer func() { os.Stdin = oldIn; inR.Close() }()

	oldOut := os.Stdout
	outR, outW, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	os.Stdout = outW
	defer func() { os.Stdout = oldOut }()
	var buf bytes.Buffer
	done := make(chan struct{})
	go func() { io.Copy(&buf, outR); close(done) }()

	runErr := runCLI(args)

	outW.Close()
	<-done
	outR.Close()
	return buf.String(), runErr
}

// text mode streams the assistant reply to stdout.
func TestRunTextOutput(t *testing.T) {
	runFixture(t, "hello world", nil)

	out, err := runCapture(t, "", "say hi")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "hello world") {
		t.Fatalf("stdout should stream the reply, got %q", out)
	}
}

// --format json emits newline-delimited events: a text event per delta and a
// final done event carrying the full reply.
func TestRunJSONStream(t *testing.T) {
	runFixture(t, "all done", nil)

	out, err := runCapture(t, "", "--format", "json", "go")
	if err != nil {
		t.Fatal(err)
	}
	var sawText, sawDone bool
	for _, line := range strings.Split(strings.TrimSpace(out), "\n") {
		var ev map[string]any
		if err := json.Unmarshal([]byte(line), &ev); err != nil {
			t.Fatalf("line not JSON: %q: %v", line, err)
		}
		switch ev["type"] {
		case "text":
			sawText = true
		case "done":
			sawDone = true
			if text, _ := ev["text"].(string); text != "all done" {
				t.Fatalf("done text: %q", ev["text"])
			}
		}
	}
	if !sawText || !sawDone {
		t.Fatalf("want a text event and a done event, got:\n%s", out)
	}
}

func TestRunPlanOnlyUsesReadOnlyPlannerAndExits(t *testing.T) {
	var reqs []llm.Request
	runPlanFixture(t, &reqs)
	out, err := runCapture(t, "", "--plan-only", "--format", "json", "ship it")
	if err != nil {
		t.Fatal(err)
	}
	if len(reqs) != 1 {
		t.Fatalf("plan-only should make one request, got %d", len(reqs))
	}
	var names []string
	for _, tool := range reqs[0].Tools {
		names = append(names, tool.Function.Name)
	}
	if got := strings.Join(names, ","); got != "read,grep,glob,lsp,submit_plan" {
		t.Fatalf("planner tools = %q", got)
	}
	for _, forbidden := range []string{"bash", "write", "edit", "task"} {
		if strings.Contains(strings.Join(names, ","), forbidden) {
			t.Fatalf("planner exposed forbidden tool %q", forbidden)
		}
	}
	var sawPlan, sawDone bool
	for _, line := range strings.Split(strings.TrimSpace(out), "\n") {
		var event map[string]any
		if err := json.Unmarshal([]byte(line), &event); err != nil {
			t.Fatalf("invalid plan event %q: %v", line, err)
		}
		switch event["type"] {
		case "plan":
			sawPlan = true
		case "done":
			sawDone = true
		}
	}
	if !sawPlan || !sawDone {
		t.Fatalf("plan-only events should include plan and done:\n%s", out)
	}
}

func TestRunPlanThenExecuteEmitsBothRoles(t *testing.T) {
	var reqs []llm.Request
	runPlanFixture(t, &reqs)
	out, err := runCapture(t, "", "--plan", "--format", "json", "ship it")
	if err != nil {
		t.Fatal(err)
	}
	if len(reqs) != 2 {
		t.Fatalf("plan execution should make planning and acting requests, got %d", len(reqs))
	}
	var sawPlan, sawText, sawDone, sawSmart, sawFast bool
	for _, line := range strings.Split(strings.TrimSpace(out), "\n") {
		var event map[string]any
		if err := json.Unmarshal([]byte(line), &event); err != nil {
			t.Fatalf("invalid plan event %q: %v", line, err)
		}
		switch event["type"] {
		case "plan":
			sawPlan = true
		case "text":
			sawText = true
		case "done":
			sawDone = true
		case "model_call_start":
			switch event["role"] {
			case "smart":
				sawSmart = true
			case "fast":
				sawFast = true
			}
		}
	}
	if !sawPlan || !sawText || !sawDone || !sawSmart || !sawFast {
		t.Fatalf("plan execution events missing plan/text/done/smart/fast:\n%s", out)
	}
}

// Piped stdin is appended to the prompt argument in the user message.
func TestRunStdinAppendsToPrompt(t *testing.T) {
	var reqs []llm.Request
	runFixture(t, "ok", &reqs)

	if _, err := runCapture(t, "piped context\n", "summarize this"); err != nil {
		t.Fatal(err)
	}
	if len(reqs) != 1 {
		t.Fatalf("requests: %d", len(reqs))
	}
	var user string
	for _, m := range reqs[0].Messages {
		if m.Role == "user" {
			user = m.Content
		}
	}
	if !strings.Contains(user, "summarize this") || !strings.Contains(user, "piped context") {
		t.Fatalf("user message should combine the arg prompt and stdin, got %q", user)
	}
}

// -resume continues a persisted session instead of starting fresh; the
// resumed conversation's history precedes the new prompt.
func TestRunResume(t *testing.T) {
	var reqs []llm.Request
	runFixture(t, "first reply", &reqs)
	if _, err := runCapture(t, "", "first question"); err != nil {
		t.Fatal(err)
	}

	// find the session id from the store (same GHG_HOME for both runs)
	dir, _ := configDir()
	st, err := sessionOpen(dir)
	if err != nil {
		t.Fatal(err)
	}
	metas, _ := st.Recent(10)
	if len(metas) != 1 {
		t.Fatalf("one session should exist, got %d", len(metas))
	}
	id := metas[0].ID
	st.Close()

	if _, err := runCapture(t, "", "-resume", id, "follow up"); err != nil {
		t.Fatal(err)
	}
	last := reqs[len(reqs)-1]
	var sawFirst bool
	for _, m := range last.Messages {
		if m.Role == "user" && strings.Contains(m.TextContent(), "first question") {
			sawFirst = true
		}
	}
	if !sawFirst {
		t.Fatal("a resumed run should carry the prior conversation")
	}
}

// -resume with an unknown id errors clearly.
func TestRunResumeUnknown(t *testing.T) {
	runFixture(t, "x", nil)
	if _, err := runCapture(t, "", "-resume", "nosuchsession", "hi"); err == nil || !strings.Contains(err.Error(), "no session") {
		t.Fatalf("unknown session should error clearly, got %v", err)
	}
}

// -system overrides the prompt; -system-file wins over -system.
func TestRunSystemOverride(t *testing.T) {
	var reqs []llm.Request
	runFixture(t, "ok", &reqs)
	if _, err := runCapture(t, "", "-system", "You are a pirate.", "hi"); err != nil {
		t.Fatal(err)
	}
	if got := reqs[len(reqs)-1].Messages[0].Content; got != "You are a pirate." {
		t.Fatalf("-system should replace the prompt, got %q", got)
	}

	f := filepath.Join(t.TempDir(), "sys.md")
	os.WriteFile(f, []byte("You are a poet."), 0o644)
	runFixture(t, "ok", &reqs)
	if _, err := runCapture(t, "", "-system", "pirate", "-system-file", f, "hi"); err != nil {
		t.Fatal(err)
	}
	if got := reqs[len(reqs)-1].Messages[0].Content; got != "You are a poet." {
		t.Fatalf("-system-file should win over -system, got %q", got)
	}
}

// -max-turns caps the tool loop; a capped run errors non-zero.
func TestRunMaxTurns(t *testing.T) {
	// a server that always calls a tool (never finishes)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, `data: {"choices":[{"delta":{"tool_calls":[{"index":0,"id":"t1","type":"function","function":{"name":"read","arguments":"{\"path\":\"/tmp/x\"}"}}]}}]}`+"\n\n")
		fmt.Fprint(w, `data: {"choices":[{"delta":{},"finish_reason":"tool_calls"}]}`+"\n\n")
		fmt.Fprint(w, "data: [DONE]\n\n")
	}))
	defer srv.Close()
	home := t.TempDir()
	t.Setenv("GHG_HOME", home)
	cfg := fmt.Sprintf(`{
		"defaultModel": "test",
		"providers": {"testprov": {"baseUrl": %q, "api": "openai-completions", "apiKey": "k"}},
		"models": {"test": {"providers": ["testprov"], "maxOut": 100}}
	}`, srv.URL)
	os.WriteFile(filepath.Join(home, "config.json"), []byte(cfg), 0o600)

	_, err := runCapture(t, "", "-max-turns", "2", "-no-session", "loop forever")
	if err == nil || !strings.Contains(err.Error(), "max turns") {
		t.Fatalf("a capped run should error with 'max turns', got %v", err)
	}
}

// -timeout cancels an in-flight run and reports the timeout.
func TestRunTimeout(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(3 * time.Second) // hang past the timeout
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, "data: [DONE]\n\n")
	}))
	defer srv.Close()
	home := t.TempDir()
	t.Setenv("GHG_HOME", home)
	cfg := fmt.Sprintf(`{
		"defaultModel": "test",
		"providers": {"testprov": {"baseUrl": %q, "api": "openai-completions", "apiKey": "k"}},
		"models": {"test": {"providers": ["testprov"], "maxOut": 100}}
	}`, srv.URL)
	os.WriteFile(filepath.Join(home, "config.json"), []byte(cfg), 0o600)

	_, err := runCapture(t, "", "-timeout", "200ms", "-no-session", "hi")
	if err == nil || !strings.Contains(err.Error(), "timed out") {
		t.Fatalf("a timed-out run should say so, got %v", err)
	}
}

// -no-session leaves no row in the session store.
func TestRunNoSession(t *testing.T) {
	runFixture(t, "ok", nil)
	if _, err := runCapture(t, "", "-no-session", "one-off"); err != nil {
		t.Fatal(err)
	}
	dir, _ := configDir()
	st, _ := sessionOpen(dir)
	defer st.Close()
	metas, _ := st.Recent(10)
	if len(metas) != 0 {
		t.Fatalf("-no-session should leave no sessions, got %d", len(metas))
	}
}

// -quiet -format json: clean NDJSON on stdout, nothing on stderr.
func TestRunQuietJSON(t *testing.T) {
	runFixture(t, "quiet reply", nil)
	out, err := runCapture(t, "", "-quiet", "-format", "json", "go")
	if err != nil {
		t.Fatal(err)
	}
	for _, line := range strings.Split(strings.TrimSpace(out), "\n") {
		var ev map[string]any
		if err := json.Unmarshal([]byte(line), &ev); err != nil {
			t.Fatalf("stdout should be clean NDJSON, got line %q: %v", line, err)
		}
	}
}

func TestRunUnusableSessionDatabaseFailsUnlessNoSession(t *testing.T) {
	runFixture(t, "ok", nil)
	home := os.Getenv("GHG_HOME")
	dbPath := filepath.Join(home, "sessions.db")
	_ = os.WriteFile(dbPath, []byte("not a sqlite database file"), 0o600)

	_, err := runCapture(t, "", "hello")
	if err == nil || (!strings.Contains(err.Error(), "session database") && !strings.Contains(err.Error(), "file is not a database")) {
		t.Fatalf("expected session database error, got: %v", err)
	}

	_, err = runCapture(t, "", "-no-session", "hello")
	if err != nil {
		t.Fatalf("-no-session should bypass session database error, got: %v", err)
	}
}

func configDir() (string, error) { return os.Getenv("GHG_HOME"), nil }

func sessionOpen(dir string) (*session.Store, error) { return session.Open(dir + "/sessions.db") }

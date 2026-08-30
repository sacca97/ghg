package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/sacca97/ghg/internal/llm"
	"github.com/sacca97/ghg/internal/tools"
)

// TestLSPDiagnosticsReachModel pins the end-to-end flow: the model calls
// write, the LSP hook appends a <diagnostics> block to the tool result, and
// that block is what the provider receives on the next call.
func TestLSPDiagnosticsReachModel(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "main.go")
	argsJSON, _ := json.Marshal(map[string]string{"path": target, "content": "package main\n"})

	call := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req llm.Request
		json.NewDecoder(r.Body).Decode(&req)
		w.Header().Set("Content-Type", "text/event-stream")
		call++
		if call == 1 {
			fmt.Fprintf(w, `data: {"choices":[{"delta":{"tool_calls":[{"index":0,"id":"t1","type":"function","function":{"name":"write","arguments":%s}}]}}]}`+"\n\n",
				jsonString(string(argsJSON)))
		} else {
			last := req.Messages[len(req.Messages)-1]
			if last.Role != "tool" {
				t.Errorf("expected tool result, got %s", last.Role)
			}
			if !strings.Contains(last.Content, "<diagnostics file=") || !strings.Contains(last.Content, "ERROR [2:3] undefined: foo") {
				t.Errorf("tool result missing diagnostics block: %q", last.Content)
			}
			fmt.Fprint(w, `data: {"choices":[{"delta":{"content":"fixed"},"finish_reason":"stop"}]}`+"\n\n")
		}
		fmt.Fprint(w, "data: [DONE]\n\n")
	}))
	defer srv.Close()

	ag := New(testBackend(srv.URL, "k"), "m", 100, "sys")
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
func (stubWaiter) PreviewRename(context.Context, tools.RenameRequest) (tools.RenamePreview, error) {
	return tools.RenamePreview{}, errors.New("not implemented")
}
func (stubWaiter) LookupRename(context.Context, string, string) (tools.RenamePlan, error) {
	return tools.RenamePlan{}, errors.New("not implemented")
}
func (stubWaiter) ValidateRename(context.Context, tools.RenamePlan) error {
	return errors.New("not implemented")
}
func (stubWaiter) ConsumeRename(context.Context, string, string) error { return nil }

func jsonString(s string) string {
	b, _ := json.Marshal(s)
	return string(b)
}

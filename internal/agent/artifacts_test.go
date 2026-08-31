package agent

import (
	"context"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"

	"github.com/sacca97/ghg/internal/artifact"
	"github.com/sacca97/ghg/internal/llm"
	"github.com/sacca97/ghg/internal/session"
	"github.com/sacca97/ghg/internal/tools"
)

func TestArtifactToolsListReadAndScope(t *testing.T) {
	artifacts, err := artifact.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	st, err := session.Open(filepath.Join(t.TempDir(), "sessions.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	id, err := st.Create("/tmp", "m", "p")
	if err != nil {
		t.Fatal(err)
	}
	ref, err := artifacts.Put(context.Background(), artifact.PutRequest{Data: []byte("line one\nline two\n"), Complete: true, MediaType: "text/plain"})
	if err != nil {
		t.Fatal(err)
	}
	msgs := []llm.Message{{Role: "system"}, {Role: "tool", Content: "line one", ToolCallID: "call-1", Name: "bash", Artifact: &ref}}
	if err := st.Save(id, 1, msgs, "m", "p"); err != nil {
		t.Fatal(err)
	}

	ag := New(nil, "m", 100, "sys")
	ag.ArtifactStore = artifacts
	ag.ArtifactCatalog = st
	ag.SetSessionID(id)

	list := tools.ExecuteResult(context.Background(), ag.AllTools(), "artifact_list", json.RawMessage(`{"tool":"bash"}`))
	if !strings.Contains(list.Preview, ref.ID) || !strings.Contains(list.Preview, "call=call-1") {
		t.Fatalf("artifact_list = %q", list.Preview)
	}
	read := tools.ExecuteResult(context.Background(), ag.AllTools(), "artifact_read", json.RawMessage(`{"id":"`+ref.ID+`"}`))
	if !strings.Contains(read.Preview, "line two") || read.Artifact == nil || read.Artifact.ID != ref.ID {
		t.Fatalf("artifact_read = %+v", read)
	}

	other, err := st.Create("/tmp", "m", "p")
	if err != nil {
		t.Fatal(err)
	}
	ag.SetSessionID(other)
	read = tools.ExecuteResult(context.Background(), ag.AllTools(), "artifact_read", json.RawMessage(`{"id":"`+ref.ID+`"}`))
	if !strings.Contains(read.Preview, "not available in the current session") {
		t.Fatalf("cross-session read = %q", read.Preview)
	}
}

func TestArtifactToolsRejectPathsAndUnboundedReads(t *testing.T) {
	ag := New(nil, "m", 100, "sys")
	if got := tools.ExecuteResult(context.Background(), ag.AllTools(), "artifact_read", json.RawMessage(`{"id":"../../secret"}`)); !strings.Contains(got.Preview, "no artifact store") {
		t.Fatalf("path input without catalog = %q", got.Preview)
	}
}

func TestLargeToolResultGetsAnArtifactReference(t *testing.T) {
	store, err := artifact.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	ag := New(nil, "m", 100, "sys")
	ag.ArtifactWriter = store
	ag.Tools = []tools.Tool{{
		Def: llm.NewTool("large", "large result", `{"type":"object","properties":{}}`),
		RunResult: func(context.Context, json.RawMessage) (tools.ToolResult, error) {
			raw := strings.Repeat("x", 60_000)
			return tools.MarkUntrusted(tools.TextResult(raw, tools.Truncate(raw)), "test"), nil
		},
	}}
	calls := []llm.ToolCall{{ID: "large-1", Function: struct {
		Name      string `json:"name"`
		Arguments string `json:"arguments"`
	}{Name: "large", Arguments: `{}`}}}
	results := ag.runToolResultsWithTools(context.Background(), calls, Events{}, ag.AllTools())
	if len(results) != 1 || results[0].Artifact == nil {
		t.Fatalf("result did not get an artifact: %+v", results)
	}
	if !strings.Contains(results[0].Preview, "use artifact_read") {
		t.Fatalf("preview missing recovery hint: %q", results[0].Preview[len(results[0].Preview)-100:])
	}
	if len(results[0].Preview) > 16<<10 {
		t.Fatalf("artifact hint exceeded preview budget: %d", len(results[0].Preview))
	}
	if !strings.Contains(tools.ModelText(results[0]), "<untrusted_tool_output") {
		t.Fatal("an explicitly untrusted result should be delimited for the model")
	}
	got, err := store.Read(context.Background(), *results[0].Artifact, 0, 100)
	if err != nil || string(got) != strings.Repeat("x", 100) {
		t.Fatalf("stored result = %q, %v", got, err)
	}
}

func TestAgingPromotesCompleteToolResult(t *testing.T) {
	store, err := artifact.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	ag := New(nil, "m", 100, "sys")
	ag.ResultAging = true
	ag.ArtifactWriter = store
	raw := strings.Repeat("x", 2_000)
	result := ag.attachArtifact(context.Background(), tools.TextResult(raw, raw))
	if result.Artifact == nil {
		t.Fatal("aging-eligible complete result was not promoted")
	}
}

func TestDisabledArtifactsExplainUnrecoverableOutput(t *testing.T) {
	ag := New(nil, "m", 100, "sys")
	ag.ArtifactsDisabled = true
	ag.Tools = []tools.Tool{{
		Def: llm.NewTool("large", "large result", `{"type":"object","properties":{}}`),
		RunResult: func(context.Context, json.RawMessage) (tools.ToolResult, error) {
			raw := strings.Repeat("x", 60_000)
			return tools.TextResult(raw, tools.Truncate(raw)), nil
		},
	}}
	calls := []llm.ToolCall{{ID: "large-1", Function: struct {
		Name      string `json:"name"`
		Arguments string `json:"arguments"`
	}{Name: "large", Arguments: `{}`}}}
	results := ag.runToolResultsWithTools(context.Background(), calls, Events{}, ag.AllTools())
	if len(results) != 1 || results[0].Artifact != nil {
		t.Fatalf("disabled result should not have an artifact: %+v", results)
	}
	if !strings.Contains(results[0].Preview, "persistence disabled") ||
		!strings.Contains(results[0].Preview, "unrecoverable") {
		t.Fatalf("disabled result missing explicit warning: %q", results[0].Preview[len(results[0].Preview)-100:])
	}
}

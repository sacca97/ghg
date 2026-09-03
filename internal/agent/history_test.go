package agent_test

import (
	"context"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"

	"github.com/sacca97/ghg/internal/agent"
	"github.com/sacca97/ghg/internal/models"
	"github.com/sacca97/ghg/internal/search"
	"github.com/sacca97/ghg/internal/session"
	"github.com/sacca97/ghg/internal/tools"
)

func TestHistoryToolsSearchPaginateAndReadAsUntrustedEvidence(t *testing.T) {
	st, err := session.Open(filepath.Join(t.TempDir(), "sessions.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	id, err := st.Create(t.TempDir(), "m", "p")
	if err != nil {
		t.Fatal(err)
	}
	call := models.ToolCall{ID: "old-call"}
	call.Function.Name = "read"
	call.Function.Arguments = `{"path":"history.go"}`
	msgs := []models.Message{
		{Role: "system", Content: "policy"},
		{Role: "user", Content: "find the history needle"},
		{Role: "assistant", Content: "I will inspect it", ToolCalls: []models.ToolCall{call}},
		{Role: "tool", Content: "needle result", Source: "read", Name: "read", ToolCallID: call.ID},
		{Role: "user", Content: "the follow-up needle"},
	}
	if err := st.Save(id, 0, msgs, "m", "p"); err != nil {
		t.Fatal(err)
	}

	toolSet := agent.HistoryTools(st, func() string { return id }, search.NewRegistry())
	searchArgs, _ := json.Marshal(map[string]any{"query": "needle", "limit": 1})
	first := tools.ExecuteResult(context.Background(), toolSet, "history_search", searchArgs)
	if !tools.IsUntrusted(first) || !strings.Contains(first.Preview, "next_cursor=") {
		t.Fatalf("first history page = %+v", first)
	}
	cursor := strings.TrimSpace(strings.SplitN(first.Preview, "next_cursor=", 2)[1])
	nextArgs, _ := json.Marshal(map[string]any{"cursor": cursor, "limit": 1})
	second := tools.ExecuteResult(context.Background(), toolSet, "history_search", nextArgs)
	if second.Preview == "" || !strings.Contains(second.Preview, "seq=") {
		t.Fatalf("second history page = %q", second.Preview)
	}

	singlePageArgs, _ := json.Marshal(map[string]any{"query": "needle", "limit": 10})
	singlePage := tools.ExecuteResult(context.Background(), toolSet, "history_search", singlePageArgs)
	if !tools.IsUntrusted(singlePage) || strings.Contains(singlePage.Preview, "next_cursor=") {
		t.Fatalf("single page search should not contain next_cursor: %q", singlePage.Preview)
	}
	if !strings.Contains(singlePage.Preview, "history_search: 3 result(s)") {
		t.Fatalf("single page search missing expected results: %q", singlePage.Preview)
	}

	readArgs, _ := json.Marshal(map[string]any{"start_seq": 1, "end_seq": 4})
	read := tools.ExecuteResult(context.Background(), toolSet, "history_read", readArgs)
	if !tools.IsUntrusted(read) || !strings.Contains(read.Preview, `role=tool`) || !strings.Contains(read.Preview, `calls=`) {
		t.Fatalf("history read = %+v", read)
	}
	if strings.Contains(read.Preview, "next_cursor=") {
		t.Fatalf("single page read should not contain next_cursor: %q", read.Preview)
	}
	if strings.Contains(read.Preview, `"tool_calls"`) || strings.Contains(read.Preview, "old-call") {
		t.Fatalf("history read leaked provider protocol data: %q", read.Preview)
	}
	tooBroad := tools.ExecuteResult(context.Background(), toolSet, "history_read", json.RawMessage(`{"start_seq":0,"end_seq":256}`))
	if !strings.Contains(tooBroad.Preview, "too broad") {
		t.Fatalf("inclusive history range limit was not enforced: %q", tooBroad.Preview)
	}
}

func TestHistoryToolsRequireDurableSessionAndRejectForeignCursor(t *testing.T) {
	toolSet := agent.HistoryTools(nil, nil, nil)
	unknown := tools.ExecuteResult(context.Background(), toolSet, "history_search", json.RawMessage(`{"query":"x"}`))
	if !strings.Contains(unknown.Preview, "history recall requires a durable session") {
		t.Fatalf("no-session history result = %q", unknown.Preview)
	}
}

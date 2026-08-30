package session

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"

	"github.com/sacca97/ghg/internal/llm"
)

func TestHistoryIndexSearchReplaceBackfillForkAndRead(t *testing.T) {
	st, err := Open(filepath.Join(t.TempDir(), "sessions.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	ctx := context.Background()
	id, err := st.Create(t.TempDir(), "m", "p")
	if err != nil {
		t.Fatal(err)
	}
	call := llm.ToolCall{ID: "call-1"}
	call.Function.Name = "read"
	call.Function.Arguments = `{"path":"needle.go"}`
	msgs := []llm.Message{
		{Role: "system", Content: "policy"},
		{Role: "user", Content: "needle from the user", Authored: true},
		{Role: "assistant", Content: "needle answer", ToolCalls: []llm.ToolCall{call}},
		{Role: "tool", Content: "needle tool result", Source: "read", ToolCallID: "call-1", Name: "read"},
		{Role: "user", Content: "follow up"},
	}
	if err := st.Save(id, 0, msgs, "m", "p"); err != nil {
		t.Fatal(err)
	}

	hits, err := st.SearchHistory(ctx, id, "needle", "", nil, 200)
	if err != nil {
		t.Fatal(err)
	}
	if len(hits) != 3 || hits[0].Seq == 0 {
		t.Fatalf("history hits = %+v, want the three non-system needle messages", hits)
	}
	var indexed int
	if err := st.db.QueryRow(`SELECT COUNT(*) FROM history_fts WHERE session_id=?`, id).Scan(&indexed); err != nil {
		t.Fatal(err)
	}
	if indexed != 4 {
		t.Fatalf("indexed rows = %d, want 4 eligible messages", indexed)
	}

	// Save replaces the raw row and must replace, not duplicate, its derived hit.
	if err := st.Save(id, 1, []llm.Message{{}, {Role: "user", Content: "replaced text"}}, "m", "p"); err != nil {
		t.Fatal(err)
	}
	hits, err = st.SearchHistory(ctx, id, "needle", "", nil, 200)
	if err != nil || len(hits) != 2 {
		t.Fatalf("after replacement hits = %+v, err=%v; want assistant and tool only", hits, err)
	}
	if err := rebuildHistoryFTS(st.db); err != nil {
		t.Fatal(err)
	}
	if err := st.db.QueryRow(`SELECT COUNT(*) FROM history_fts WHERE session_id=?`, id).Scan(&indexed); err != nil {
		t.Fatal(err)
	}
	if indexed != 4 {
		t.Fatalf("rebuilt rows = %d, want 4 eligible messages", indexed)
	}

	fork, err := st.Fork(id, 3, "branch")
	if err != nil {
		t.Fatal(err)
	}
	forkHits, err := st.SearchHistory(ctx, fork, "needle", "", nil, 200)
	if err != nil || len(forkHits) != 2 {
		t.Fatalf("fork hits = %+v, err=%v", forkHits, err)
	}
	if foreign, err := st.SearchHistory(ctx, fork, "follow", "", nil, 200); err != nil || len(foreign) != 0 {
		t.Fatalf("fork should not include source tail: %+v, err=%v", foreign, err)
	}

	read, diagnostics, err := st.ReadHistory(ctx, id, 1, 4, nil, 100)
	if err != nil || len(diagnostics) != 0 || len(read) != 4 {
		t.Fatalf("read = %+v diagnostics=%v err=%v", read, diagnostics, err)
	}
	if read[1].Message.ToolCalls[0].ID != "call-1" {
		t.Fatal("raw read should retain provider call ids for descriptive rendering")
	}
}

func TestHistorySearchRejectsMalformedQuery(t *testing.T) {
	st, err := Open(filepath.Join(t.TempDir(), "sessions.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	id, err := st.Create(t.TempDir(), "m", "p")
	if err != nil {
		t.Fatal(err)
	}
	_, err = st.SearchHistory(context.Background(), id, `"`, "", nil, 10)
	if !errors.Is(err, ErrInvalidHistoryQuery) || strings.Contains(err.Error(), "sqlite") {
		t.Fatalf("malformed query error = %v", err)
	}
}

func TestHistorySearchConcurrentWithSaveDoesNotLockDatabase(t *testing.T) {
	st, err := Open(filepath.Join(t.TempDir(), "sessions.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	id, err := st.Create(t.TempDir(), "m", "p")
	if err != nil {
		t.Fatal(err)
	}

	msgs := []llm.Message{
		{Role: "user", Content: "concurrent search query text", Authored: true},
	}
	if err := st.Save(id, 0, msgs, "m", "p"); err != nil {
		t.Fatal(err)
	}

	done := make(chan struct{})
	go func() {
		defer close(done)
		for i := 0; i < 20; i++ {
			_ = st.Save(id, 1, []llm.Message{{Role: "assistant", Content: "assistant reply"}}, "m", "p")
		}
	}()

	for i := 0; i < 20; i++ {
		hits, err := st.SearchHistory(context.Background(), id, "concurrent", "", nil, 10)
		if err != nil {
			t.Fatalf("concurrent search failed on iteration %d: %v", i, err)
		}
		if len(hits) == 0 {
			t.Fatalf("expected at least 1 hit, got 0")
		}
	}
	<-done
}


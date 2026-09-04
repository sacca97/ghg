package session

import (
	"context"
	"path/filepath"
	"strings"
	"testing"

	"github.com/sacca97/ghg/internal/models"
)

func TestOutputMetadataRoundTripForkAndRewind(t *testing.T) {
	st, err := Open(filepath.Join(t.TempDir(), "s.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	id, err := st.Create("/tmp", "m", "p")
	if err != nil {
		t.Fatal(err)
	}
	ref := models.OutputRef{
		ID: "sha256:" + strings.Repeat("a", 64), Hash: strings.Repeat("a", 64),
		OriginalBytes: 200, StoredBytes: 100, Complete: false, MediaType: "text/plain",
		Metadata: map[string]string{"source": "bash", "cwd": "/tmp"},
	}
	msgs := []models.Message{
		{Role: "system", Content: "sys"},
		{Role: "user", Content: "question"},
		{Role: "tool", Content: "preview", ToolCallID: "call-1", Name: "bash", Output: &ref},
		{Role: "tool", Content: "later", ToolCallID: "call-2", Name: "read"},
	}
	if err := st.Save(id, 1, msgs, "m", "p"); err != nil {
		t.Fatal(err)
	}

	ctx := context.Background()
	got, err := st.ListOutputs(ctx, id, OutputFilter{ToolName: "bash"}, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].ID != ref.ID || got[0].MessageSeq != 2 || got[0].ToolCallID != "call-1" || got[0].Path != "sha256/aa/"+strings.Repeat("a", 64) {
		t.Fatalf("output metadata = %+v", got)
	}
	if got[0].Complete || got[0].OriginalBytes != 200 || got[0].StoredBytes != 100 {
		t.Fatalf("output sizes/retention = %+v", got[0])
	}
	if got[0].Metadata["source"] != "bash" || got[0].Metadata["cwd"] != "/tmp" {
		t.Fatalf("output metadata = %+v", got[0].Metadata)
	}
	looked, err := st.LookupOutput(ctx, id, ref.ID)
	if err != nil || looked.ToolName != "bash" {
		t.Fatalf("lookup = %+v, %v", looked, err)
	}

	_, loaded, err := st.Load(id)
	if err != nil {
		t.Fatal(err)
	}
	if len(loaded) != 3 || loaded[1].Output == nil || loaded[1].Output.ID != ref.ID {
		t.Fatalf("loaded output message = %+v", loaded)
	}

	forkID, err := st.Fork(id, 2, "fork")
	if err != nil {
		t.Fatal(err)
	}
	forked, err := st.ListOutputs(ctx, forkID, OutputFilter{}, 10)
	if err != nil || len(forked) != 1 || forked[0].ID != ref.ID {
		t.Fatalf("fork outputs = %+v, %v", forked, err)
	}

	if err := st.DeleteFrom(id, 2, nil); err != nil {
		t.Fatal(err)
	}
	remaining, err := st.ListOutputs(ctx, id, OutputFilter{}, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(remaining) != 0 {
		t.Fatalf("rewind should remove output at seq 2, got %+v", remaining)
	}
}

func TestListOutputsIsBoundedAndSessionScoped(t *testing.T) {
	st, err := Open(filepath.Join(t.TempDir(), "s.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	first, _ := st.Create("/tmp", "m", "p")
	second, _ := st.Create("/tmp", "m", "p")
	for i, id := range []string{first, second} {
		ref := models.OutputRef{ID: "sha256:" + strings.Repeat(string(rune('a'+i)), 64), Hash: strings.Repeat(string(rune('a'+i)), 64), OriginalBytes: 1, StoredBytes: 1, Complete: true}
		msgs := []models.Message{{Role: "system"}, {Role: "tool", Content: "x", ToolCallID: "call", Name: "read", Output: &ref}}
		if err := st.Save(id, 1, msgs, "m", "p"); err != nil {
			t.Fatal(err)
		}
	}
	got, err := st.ListOutputs(context.Background(), first, OutputFilter{}, 1)
	if err != nil || len(got) != 1 || got[0].SessionID != first {
		t.Fatalf("bounded/session-scoped list = %+v, %v", got, err)
	}
	if _, err := st.LookupOutput(context.Background(), first, "sha256:"+strings.Repeat("b", 64)); err == nil {
		t.Fatal("lookup must not cross sessions")
	}
}

func TestCompactionViewWithoutSystemAppendsToRawTail(t *testing.T) {
	st, err := Open(filepath.Join(t.TempDir(), "s.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	id, err := st.Create("/tmp", "m", "p")
	if err != nil {
		t.Fatal(err)
	}
	raw := []models.Message{
		{Role: "system", Content: "sys"},
		{Role: "user", Content: "q1"},
		{Role: "assistant", Content: "a1"},
		{Role: "user", Content: "q2"},
		{Role: "assistant", Content: "a2"},
		{Role: "user", Content: "q3"},
		{Role: "assistant", Content: "a3"},
	}
	if err := st.Save(id, 1, raw, "m", "p"); err != nil {
		t.Fatal(err)
	}
	// The TUI's normal save omits seq 0 (the system prompt), so the cutoff
	// still uses the original Agent.Messages index and the loaded slice does
	// not start with a synthetic system message.
	if err := st.RecordCompaction(id, 4, "q1 and q2 were summarized"); err != nil {
		t.Fatal(err)
	}
	_, view, err := st.Load(id)
	if err != nil {
		t.Fatal(err)
	}
	if len(view) != 4 || view[0].Role != "system" || view[1].Content != "a2" {
		t.Fatalf("compacted no-system view = %+v", view)
	}

	// Emulate Agent.Messages: the system prompt is in memory, while view is
	// what session.Load returned. New messages begin after that derived view.
	agentView := append([]models.Message{{Role: "system", Content: "sys"}}, view...)
	from := len(agentView)
	agentView = append(agentView,
		models.Message{Role: "user", Content: "q4"},
		models.Message{Role: "assistant", Content: "a4"},
	)
	if err := st.Save(id, from, agentView, "m", "p"); err != nil {
		t.Fatal(err)
	}
	rawLoaded := st.RawMessages(id)
	if len(rawLoaded) != 8 || rawLoaded[5].Content != "a3" || rawLoaded[6].Content != "q4" {
		t.Fatalf("post-compaction save rewrote raw log: %+v", rawLoaded)
	}
	_, view, err = st.Load(id)
	if err != nil {
		t.Fatal(err)
	}
	if len(view) != 6 || view[4].Content != "q4" || view[5].Content != "a4" {
		t.Fatalf("view after raw-tail append = %+v", view)
	}

	forkID, err := st.Fork(id, 8, "fork")
	if err != nil {
		t.Fatal(err)
	}
	_, forkView, err := st.Load(forkID)
	if err != nil {
		t.Fatal(err)
	}
	if len(forkView) != 6 || !strings.Contains(forkView[0].Content, "q1 and q2") {
		t.Fatalf("fork did not preserve compaction view: %+v", forkView)
	}
}

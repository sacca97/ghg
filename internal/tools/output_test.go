package tools_test

import (
	"context"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"

	"github.com/sacca97/ghg/internal/models"
	"github.com/sacca97/ghg/internal/session"
	"github.com/sacca97/ghg/internal/tools"
)

func TestOutputToolsListReadAndScope(t *testing.T) {
	outputs, err := session.NewOutputStore(t.TempDir())
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
	ref, err := outputs.Put(context.Background(), []byte("line one\nline two\n"), 0, true, "text/plain")
	if err != nil {
		t.Fatal(err)
	}
	msgs := []models.Message{{Role: "system"}, {Role: "tool", Content: "line one", ToolCallID: "call-1", Name: "bash", Output: &ref}}
	if err := st.Save(id, 1, msgs, "m", "p"); err != nil {
		t.Fatal(err)
	}
	st.Outputs = outputs

	sessionID := id
	currentMessages := msgs
	toolSet := tools.OutputTools(tools.OutputToolConfig{
		SessionID: func() string { return sessionID },
		Catalog:   func() session.OutputCatalog { return st },
		Store:     func() *session.OutputStore { return outputs },
		Messages:  func() []models.Message { return currentMessages },
	})
	list := tools.ExecuteResult(context.Background(), toolSet, "artifact_list", json.RawMessage(`{"tool":"bash"}`))
	if !strings.Contains(list.Preview, ref.ID) || !strings.Contains(list.Preview, "call=call-1") {
		t.Fatalf("output_list = %q", list.Preview)
	}
	read := tools.ExecuteResult(context.Background(), toolSet, "artifact_read", json.RawMessage(`{"id":"`+ref.ID+`"}`))
	if !strings.Contains(read.Preview, "line two") || read.Output == nil || read.Output.ID != ref.ID {
		t.Fatalf("output_read = %+v", read)
	}

	other, err := st.Create("/tmp", "m", "p")
	if err != nil {
		t.Fatal(err)
	}
	sessionID = other
	currentMessages = nil
	read = tools.ExecuteResult(context.Background(), toolSet, "artifact_read", json.RawMessage(`{"id":"`+ref.ID+`"}`))
	if !strings.Contains(read.Preview, "not available in the current session") {
		t.Fatalf("cross-session output read = %q", read.Preview)
	}
}

func TestOutputToolsRejectPathsAndUnboundedReads(t *testing.T) {
	toolSet := tools.OutputTools(tools.OutputToolConfig{})
	if got := tools.ExecuteResult(context.Background(), toolSet, "artifact_read", json.RawMessage(`{"id":"../../secret"}`)); !strings.Contains(got.Preview, "no output store") {
		t.Fatalf("path input without catalog = %q", got.Preview)
	}
}

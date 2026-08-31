package agent

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"

	"github.com/sacca97/ghg/internal/artifact"
	"github.com/sacca97/ghg/internal/llm"
	"github.com/sacca97/ghg/internal/session"
)

func TestAgingDerivesRecoverableStableStubWithoutChangingRawMessages(t *testing.T) {
	ref := artifact.Ref{ID: "sha256:" + strings.Repeat("a", 64), Hash: strings.Repeat("a", 64), OriginalBytes: 20000, StoredBytes: 20000, Complete: true}
	call := llm.ToolCall{ID: "old-call"}
	call.Function.Name = "read"
	call.Function.Arguments = `{"path":"old.go"}`
	raw := strings.Repeat("old output ", 2000)
	msgs := []llm.Message{
		{Role: "system", Content: "policy"},
		{Role: "user", Content: "old request"},
		{Role: "assistant", ToolCalls: []llm.ToolCall{call}},
		{Role: "tool", Content: raw, Name: "read", Source: "read", ToolCallID: call.ID, Artifact: &ref},
		{Role: "user", Content: "current request"},
		{Role: "assistant", Content: "current answer"},
	}
	view := ageResultMessages(msgs, 0)
	if view[3].Content == raw || len(view[3].Content) >= len(raw) {
		t.Fatal("old artifact-backed result was not aged")
	}
	if !strings.Contains(view[3].Content, ref.ID) || !strings.Contains(view[3].Content, "old-call") {
		t.Fatalf("aged stub lost recovery metadata: %q", view[3].Content)
	}
	if msgs[3].Content != raw || msgs[3].Artifact == nil || view[3].ToolCallID != call.ID {
		t.Fatal("aging must preserve raw content and provider pairing metadata")
	}
	if gotAgain := ageResultMessages(msgs, 0); gotAgain[3].Content != view[3].Content {
		t.Fatal("aging should produce a byte-stable stub")
	}
}

func TestAgingLeavesUnrecoverableToolResultsExact(t *testing.T) {
	content := strings.Repeat("cannot recover ", 1000)
	msgs := []llm.Message{
		{Role: "system", Content: "policy"},
		{Role: "user", Content: "old request"},
		{Role: "assistant", Content: "old answer"},
		{Role: "tool", Content: content, Name: "bash"},
		{Role: "user", Content: "current request"},
		{Role: "assistant", Content: "current answer"},
	}
	view := ageResultMessages(msgs, 0)
	if view[3].Content != content {
		t.Fatal("tool output without a recovery artifact must remain exact")
	}
}

func TestAgingSurvivesTodoInjection(t *testing.T) {
	backend := &fakeBackend{}
	ag := New(backend, "m", 100, "policy")
	ag.Todos = []Todo{{ID: "t1", Content: "finish the task", Status: "in_progress"}}
	ref := artifact.Ref{ID: "sha256:" + strings.Repeat("b", 64), OriginalBytes: 20_000, StoredBytes: 20_000, Complete: true}
	call := llm.ToolCall{ID: "old-call"}
	call.Function.Name = "read"
	ag.Messages = append(ag.Messages,
		llm.Message{Role: "user", Content: "old request"},
		llm.Message{Role: "assistant", ToolCalls: []llm.ToolCall{call}},
		llm.Message{Role: "tool", Content: strings.Repeat("old output ", 2_000), Name: "read", ToolCallID: call.ID, Artifact: &ref},
		llm.Message{Role: "user", Content: "recent request"},
		llm.Message{Role: "assistant", Content: "recent answer"},
	)
	if _, err := ag.Turn(context.Background(), "continue", Events{}); err != nil {
		t.Fatal(err)
	}
	if got := backend.streamRequests[0].Messages[3].Content; !strings.HasPrefix(got, "Aged tool result") {
		t.Fatalf("todo injection discarded aged result: %q", got)
	}
}

func TestAgingDeduplicatesOverlappingReadsWithoutChangingRawMessages(t *testing.T) {
	old := "<untrusted_tool_output source=\"read\">\n[observation obs-old path=src/file.go lines=1-4 next_offset=0]\n1\tone\n2\ttwo\n3\told-three\n4\told-four\n</untrusted_tool_output>\n"
	newest := "<untrusted_tool_output source=\"read\">\n[observation obs-new path=src/file.go lines=3-5 next_offset=0]\n3\tnew-three\n4\tnew-four\n5\tfive\n</untrusted_tool_output>\n"
	failed := "Error: read failed"
	msgs := []llm.Message{
		{Role: "system", Content: "policy"},
		{Role: "user", Content: "inspect"},
		{Role: "assistant", Content: "reading"},
		{Role: "tool", Content: old, Name: "read"},
		{Role: "user", Content: "refresh"},
		{Role: "assistant", Content: "reading again"},
		{Role: "tool", Content: newest, Name: "read"},
		{Role: "tool", Content: failed, Name: "read", ExitCode: 1},
	}

	view := ageResultMessages(msgs, 1_000_000)
	if view[6].Content != newest {
		t.Fatalf("newest observation changed: %q", view[6].Content)
	}
	if !strings.Contains(view[3].Content, "1\tone") || !strings.Contains(view[3].Content, "2\ttwo") {
		t.Fatalf("non-overlapping old lines were lost: %q", view[3].Content)
	}
	if strings.Contains(view[3].Content, "3\told-three") || strings.Contains(view[3].Content, "4\told-four") ||
		!strings.Contains(view[3].Content, "lines 3-4 superseded by observation obs-new") {
		t.Fatalf("old overlapping lines were not replaced: %q", view[3].Content)
	}
	provider := view[3].Content + view[6].Content
	for _, line := range []string{"1\tone\n", "2\ttwo\n", "3\tnew-three\n", "4\tnew-four\n", "5\tfive\n"} {
		if strings.Count(provider, line) != 1 {
			t.Fatalf("provider view contains %q %d times: %q", line, strings.Count(provider, line), provider)
		}
	}
	if view[7].Content != failed {
		t.Fatalf("failed read changed: %q", view[7].Content)
	}
	if msgs[3].Content != old || msgs[6].Content != newest {
		t.Fatal("raw messages were mutated")
	}
}

func TestContinuationCheckpointPromptNamesRequiredState(t *testing.T) {
	prompt := buildSummaryPrompt([]llm.Message{{Role: "user", Content: "fix the failing test"}})
	for _, want := range []string{
		"current objective and explicit user constraints",
		"established facts and important discoveries",
		"decisions and their rationale",
		"files modified and relevant symbols or locations",
		"failed approaches and why they failed",
		"verification performed and its result",
		"unresolved problems and blockers",
		"immediate next actions",
		"Preserve artifact IDs and incomplete-retention warnings",
	} {
		if !strings.Contains(prompt, want) {
			t.Fatalf("checkpoint prompt missing %q: %s", want, prompt)
		}
	}
}

func TestEmergencyCutoverKeepsDurableRawHistoryAfterCheckpointFailure(t *testing.T) {
	st, err := session.Open(filepath.Join(t.TempDir(), "sessions.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	id, err := st.Create(t.TempDir(), "m", "p")
	if err != nil {
		t.Fatal(err)
	}
	msgs := []llm.Message{{Role: "system", Content: "policy"}}
	for i := 0; i < 8; i++ {
		msgs = append(msgs,
			llm.Message{Role: "user", Content: strings.Repeat("question ", 40)},
			llm.Message{Role: "assistant", Content: strings.Repeat("answer ", 40)},
		)
	}
	if err := st.Save(id, 0, msgs, "m", "p"); err != nil {
		t.Fatal(err)
	}
	rawBefore := len(st.RawMessages(id))
	backend := &emergencyBackend{}
	ag := New(backend, "m", 100, "policy")
	ag.Messages = append(ag.Messages, msgs[1:]...)
	ag.HistoryCatalog = st
	ag.SetSessionID(id)
	var summary string
	var cutoff int
	_, err = ag.Turn(context.Background(), "continue", Events{OnCompacted: func(value string, at int) {
		summary, cutoff = value, at
		if saveErr := st.RecordCompaction(id, at, value); saveErr != nil {
			t.Errorf("record emergency event: %v", saveErr)
		}
	}})
	if err == nil || backend.streamCalls != 2 || backend.completeCalls != 1 {
		t.Fatalf("turn calls=%d/%d err=%v, want one failed checkpoint and one bounded retry", backend.streamCalls, backend.completeCalls, err)
	}
	if !strings.Contains(summary, "without semantic summarization") || cutoff <= 1 {
		t.Fatalf("emergency event = %q cutoff=%d", summary, cutoff)
	}
	if got := len(st.RawMessages(id)); got != rawBefore {
		t.Fatalf("emergency cutover changed raw history: %d -> %d", rawBefore, got)
	}
	_, view, err := st.Load(id)
	if err != nil || !strings.Contains(view[1].Content, "without semantic summarization") {
		t.Fatalf("resumed emergency view = %+v err=%v", view, err)
	}
}

type emergencyBackend struct {
	streamCalls   int
	completeCalls int
}

func (b *emergencyBackend) Stream(context.Context, llm.Request, llm.EventSink) (llm.Message, llm.Usage, error) {
	b.streamCalls++
	return llm.Message{}, llm.Usage{}, errors.New("context_length_exceeded")
}

func (b *emergencyBackend) Complete(context.Context, llm.Request) (llm.Message, llm.Usage, error) {
	b.completeCalls++
	return llm.Message{}, llm.Usage{}, errors.New("checkpoint unavailable")
}

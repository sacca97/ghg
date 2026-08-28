package agent

import (
	"context"
	"testing"

	"github.com/sacca97/ghg/internal/llm"
)

type fakeBackend struct {
	streamRequests   []llm.Request
	completeRequests []llm.Request
}

func (b *fakeBackend) Stream(_ context.Context, req llm.Request, sink llm.EventSink) (llm.Message, llm.Usage, error) {
	b.streamRequests = append(b.streamRequests, req)
	if sink.OnText != nil {
		sink.OnText("reply")
	}
	return llm.Message{Role: "assistant", Content: "reply"}, llm.Usage{}, nil
}

func (b *fakeBackend) Complete(_ context.Context, req llm.Request) (llm.Message, llm.Usage, error) {
	b.completeRequests = append(b.completeRequests, req)
	return llm.Message{Role: "assistant", Content: "summary"}, llm.Usage{}, nil
}

var _ llm.Backend = (*fakeBackend)(nil)

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
			llm.Message{Role: "user", Content: "question"},
			llm.Message{Role: "assistant", Content: "answer"},
		)
	}
	if err := ag.ManualCompact(context.Background(), Events{}); err != nil {
		t.Fatal(err)
	}
	if len(backend.completeRequests) != 1 {
		t.Fatalf("complete calls = %d, want 1", len(backend.completeRequests))
	}
}

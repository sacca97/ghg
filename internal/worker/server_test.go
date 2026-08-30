package worker

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"runtime"
	"sync"
	"testing"
	"time"
)

type testHandler struct {
	mu           sync.Mutex
	commands     []string
	disconnected chan bool
	onAttached   func()
}

func (h *testHandler) Snapshot(context.Context) (any, error) {
	return map[string]string{"state": "running"}, nil
}

func (h *testHandler) Command(_ context.Context, command Command) (CommandResult, error) {
	h.mu.Lock()
	h.commands = append(h.commands, command.Name)
	h.mu.Unlock()
	if command.Name == CommandDetach {
		return CommandResult{Payload: json.RawMessage(`{"ok":true}`), Detach: true}, nil
	}
	return CommandResult{Payload: json.RawMessage(`{"ok":true}`)}, nil
}

func (h *testHandler) Disconnected(_ context.Context, detached bool) {
	h.disconnected <- detached
}

func (h *testHandler) Attached(context.Context) {
	if h.onAttached != nil {
		h.onAttached()
	}
}

func TestServerAttachControllerAndDetach(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Phase 3.7 first cut uses Unix sockets")
	}
	baseDir, err := os.MkdirTemp("/tmp", "ghg-worker-")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(baseDir)
	rt, err := NewRuntime(baseDir, "session-1")
	if err != nil {
		t.Fatal(err)
	}
	h := &testHandler{disconnected: make(chan bool, 1)}
	server, err := NewServer(rt, h)
	if err != nil {
		t.Fatal(err)
	}
	h.onAttached = func() { _ = server.Sequence() }
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = server.Serve(ctx) }()
	defer server.Close()

	client, err := Dial(context.Background(), rt, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	if frame := nextFrame(t, client); frame.Type != TypeSnapshot || frame.Seq != 0 {
		t.Fatalf("first frame = %+v, want snapshot at sequence 0", frame)
	}
	if frame := nextFrame(t, client); frame.Type != TypeAttached {
		t.Fatalf("second frame = %+v, want attached", frame)
	}

	second, err := Dial(context.Background(), rt, 0)
	if err != nil {
		t.Fatal(err)
	}
	if frame := nextFrame(t, second); frame.Type != TypeAlreadyControlled {
		t.Fatalf("second controller frame = %+v, want already controlled", frame)
	}
	second.Close()

	if _, err := server.Publish("text", map[string]string{"value": "hello"}, true); err != nil {
		t.Fatal(err)
	}
	if frame := nextFrame(t, client); frame.Type != TypeEvent || frame.Seq != 1 {
		t.Fatalf("event frame = %+v, want event sequence 1", frame)
	}

	if err := client.Send(CommandDetach, "req-1", nil); err != nil {
		t.Fatal(err)
	}
	ack := nextFrame(t, client)
	if ack.Type != TypeDetachAck || ack.RequestID != "req-1" || !server.Detached() {
		t.Fatalf("detach response = %+v, detached = %v", ack, server.Detached())
	}
	if err := client.Send(CommandDetach, "req-1", nil); err != nil {
		t.Fatal(err)
	}
	if duplicate := nextFrame(t, client); duplicate.Type != TypeDetachAck || duplicate.RequestID != "req-1" {
		t.Fatalf("duplicate detach response = %+v", duplicate)
	}

	client.Close()
	select {
	case detached := <-h.disconnected:
		if !detached {
			t.Fatal("disconnect after acknowledged detach was not marked detached")
		}
	case <-time.After(time.Second):
		t.Fatal("server did not observe client disconnect")
	}
}

func TestRuntimeRejectsInvalidSessionAndSecondOwner(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Phase 3.7 first cut uses Unix sockets")
	}
	if _, err := NewRuntime(t.TempDir(), "../escape"); !errors.Is(err, ErrInvalidSession) {
		t.Fatalf("invalid session error = %v, want ErrInvalidSession", err)
	}
	baseDir, err := os.MkdirTemp("/tmp", "ghg-worker-")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(baseDir)
	rt, err := NewRuntime(baseDir, "session-2")
	if err != nil {
		t.Fatal(err)
	}
	first, err := rt.Acquire()
	if err != nil {
		t.Fatal(err)
	}
	defer first.Close()
	second, err := rt.Acquire()
	if !errors.Is(err, ErrAlreadyRunning) {
		t.Fatalf("second owner error = %v, want ErrAlreadyRunning", err)
	}
	if second != nil {
		second.Close()
	}
}

func nextFrame(t *testing.T, client *Client) Frame {
	t.Helper()
	select {
	case frame, ok := <-client.Frames():
		if !ok {
			t.Fatal("worker client closed before frame")
		}
		return frame
	case err := <-client.Errors():
		t.Fatalf("worker client error: %v", err)
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for worker frame")
	}
	return Frame{}
}

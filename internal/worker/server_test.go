package worker

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"os"
	"runtime"
	"strings"
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

type oversizedResponseHandler struct {
	testHandler
	payload json.RawMessage
}

func (h *oversizedResponseHandler) Command(_ context.Context, _ Command) (CommandResult, error) {
	return CommandResult{Payload: h.payload}, nil
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
	baseDir, err := os.MkdirTemp("", "gw")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(baseDir)
	rt, err := NewRuntime(baseDir, "s1")
	if err != nil {
		t.Fatal(err)
	}
	h := &testHandler{disconnected: make(chan bool, 1)}
	server, err := NewServer(rt, h)
	if err != nil {
		t.Fatal(err)
	}
	h.onAttached = func() { _ = server.Detached() }
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = server.Serve(ctx) }()
	defer server.Close()

	client, err := Dial(context.Background(), rt)
	if err != nil {
		if strings.Contains(err.Error(), "operation not permitted") {
			t.Skip("unix socket connection not permitted by sandbox environment")
		}
		t.Fatal(err)
	}
	defer client.Close()
	if frame := nextFrame(t, client); frame.Type != TypeSnapshot || frame.Seq != 0 {
		t.Fatalf("first frame = %+v, want snapshot at sequence 0", frame)
	}
	if frame := nextFrame(t, client); frame.Type != TypeAttached {
		t.Fatalf("second frame = %+v, want attached", frame)
	}

	second, err := Dial(context.Background(), rt)
	if err == nil || !strings.Contains(err.Error(), "worker already has a controlling client") {
		if second != nil {
			second.Close()
		}
		t.Fatalf("second controller error = %v, want already controlled", err)
	}

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

func TestRequestCacheScopedToControllerConnection(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Phase 3.7 first cut uses Unix sockets")
	}
	baseDir, err := os.MkdirTemp("", "gw")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(baseDir)
	rt, err := NewRuntime(baseDir, "s1")
	if err != nil {
		t.Fatal(err)
	}
	h := &testHandler{disconnected: make(chan bool, 1)}
	server, err := NewServer(rt, h)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = server.Serve(ctx) }()
	defer server.Close()

	first, err := Dial(context.Background(), rt)
	if err != nil {
		if strings.Contains(err.Error(), "operation not permitted") {
			t.Skip("unix socket connection not permitted by sandbox environment")
		}
		t.Fatal(err)
	}
	nextFrame(t, first) // snapshot
	nextFrame(t, first) // attached
	if err := first.Send(CommandPing, "req-1", nil); err != nil {
		t.Fatal(err)
	}
	if frame := nextFrame(t, first); frame.Type != TypeAck || frame.RequestID != "req-1" {
		t.Fatalf("ping response = %+v", frame)
	}
	first.Close()
	select {
	case <-h.disconnected:
	case <-time.After(time.Second):
		t.Fatal("server did not observe the first controller disconnecting")
	}

	second, err := Dial(context.Background(), rt)
	if err != nil {
		t.Fatal(err)
	}
	defer second.Close()
	nextFrame(t, second) // snapshot
	nextFrame(t, second) // attached
	if err := second.Send(CommandPing, "req-1", nil); err != nil {
		t.Fatal(err)
	}
	if frame := nextFrame(t, second); frame.Type != TypeAck || frame.RequestID != "req-1" {
		t.Fatalf("second ping response = %+v", frame)
	}

	h.mu.Lock()
	dispatched := len(h.commands)
	h.mu.Unlock()
	if dispatched != 2 {
		t.Fatalf("handler dispatched %d commands, want the reused request id to run again on the new connection", dispatched)
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

func TestWriteErrorWithDeadlineTimesOutOnUnreadConn(t *testing.T) {
	serverConn, clientConn := net.Pipe()
	defer serverConn.Close()
	defer clientConn.Close()

	start := time.Now()
	err := writeErrorWithDeadline(serverConn, "s1", "handshake error")
	elapsed := time.Since(start)
	if err == nil {
		t.Fatal("expected write error on unread pipe, got nil")
	}
	if elapsed < 800*time.Millisecond || elapsed > 3*time.Second {
		t.Fatalf("expected write error to time out around 1s, took %v", elapsed)
	}
}

func TestClearControllerNotifiesOnlyOnce(t *testing.T) {
	h := &testHandler{disconnected: make(chan bool, 2)}
	s := &Server{handler: h}
	p := newPeer(nil)
	s.controller = p

	s.clearController(p, false)
	s.clearController(p, false)

	if got := len(h.disconnected); got != 1 {
		t.Fatalf("disconnect notifications = %d, want 1", got)
	}
}

func TestOversizedCommandResponseBecomesErrorFrame(t *testing.T) {
	h := &oversizedResponseHandler{
		testHandler: testHandler{disconnected: make(chan bool, 1)},
		payload:     json.RawMessage(`"` + strings.Repeat("x", MaxFrameBytes) + `"`),
	}
	s := &Server{
		runtime:  Runtime{SessionID: "s1"},
		handler:  h,
		requests: make(map[string]Frame),
	}
	p := newPeer(nil)
	s.handleCommand(p, Frame{
		Version:   ProtocolVersion,
		SessionID: "s1",
		Type:      TypeCommand,
		RequestID: "req-1",
		Payload:   mustPayload(CommandRequest{Name: CommandPing}),
	})

	response := <-p.send
	if response.frame.Type != TypeError || response.frame.RequestID != "req-1" {
		t.Fatalf("response = %+v, want request-correlated error", response.frame)
	}
}

package mcp

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	sdkmcp "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/sacca97/ghg/internal/models"
	"github.com/sacca97/ghg/internal/tools"
)

// newTestServer builds an in-process MCP server with greet/fail/structured/
// media tools for manager tests.
func newTestServer(name string) *sdkmcp.Server {
	srv := sdkmcp.NewServer(&sdkmcp.Implementation{Name: name}, nil)
	{
		type greetIn struct {
			Name string `json:"name"`
		}
		sdkmcp.AddTool(srv, &sdkmcp.Tool{
			Name:        "greet",
			Description: "greets by name",
			InputSchema: map[string]any{"type": "object", "properties": map[string]any{"name": map[string]any{"type": "string"}}},
		}, func(ctx context.Context, req *sdkmcp.CallToolRequest, in greetIn) (*sdkmcp.CallToolResult, any, error) {
			return &sdkmcp.CallToolResult{
				Content: []sdkmcp.Content{&sdkmcp.TextContent{Text: "hi " + in.Name}},
			}, nil, nil
		})
		sdkmcp.AddTool(srv, &sdkmcp.Tool{
			Name:        "fail",
			Description: "always fails",
			InputSchema: map[string]any{"type": "object"},
		}, func(ctx context.Context, req *sdkmcp.CallToolRequest, in struct{}) (*sdkmcp.CallToolResult, any, error) {
			return &sdkmcp.CallToolResult{
				Content: []sdkmcp.Content{&sdkmcp.TextContent{Text: "boom"}},
				IsError: true,
			}, nil, nil
		})
		sdkmcp.AddTool(srv, &sdkmcp.Tool{
			Name:        "structured",
			Description: "returns only structured content",
			InputSchema: map[string]any{"type": "object"},
		}, func(ctx context.Context, req *sdkmcp.CallToolRequest, in struct{}) (*sdkmcp.CallToolResult, any, error) {
			return &sdkmcp.CallToolResult{StructuredContent: map[string]any{"answer": 42}}, nil, nil
		})
		sdkmcp.AddTool(srv, &sdkmcp.Tool{
			Name:        "media",
			Description: "returns an image",
			InputSchema: map[string]any{"type": "object"},
		}, func(ctx context.Context, req *sdkmcp.CallToolRequest, in struct{}) (*sdkmcp.CallToolResult, any, error) {
			return &sdkmcp.CallToolResult{Content: []sdkmcp.Content{
				&sdkmcp.TextContent{Text: "here you go"},
				&sdkmcp.ImageContent{MIMEType: "image/png", Data: []byte{1, 2, 3}},
			}}, nil, nil
		})
	}
	return srv
}

// waitReady blocks until every server settles its first connect attempt.
func waitReady(t *testing.T, m *Manager) {
	t.Helper()
	for _, s := range m.servers {
		select {
		case <-s.ready:
		case <-time.After(10 * time.Second):
			t.Fatalf("server %s never settled", s.name)
		}
	}
}

// newTestManager wires a manager whose connects each get a FRESH in-process
// server+transport (real stdio/HTTP transports are fresh per attempt too — a
// closed in-memory transport can't reconnect).
func newTestManager(t *testing.T, cfgs map[string]ServerConfig) *Manager {
	t.Helper()
	m := NewManager(cfgs)
	m.connectTransport = func(cfg ServerConfig, stderr *ringBuffer) (sdkmcp.Transport, error) {
		return serveTestServer(t, cfg.Command[0]), nil // Command[0] is the server name in tests
	}
	t.Cleanup(m.Close)
	return m
}

// serveTestServer starts one in-process MCP server behind a fresh in-memory
// client transport. The server is connected (not Run) so a client
// disconnect ends just the session, leaving the server able to accept a
// reconnect on a fresh transport — like a real stdio server respawn.
func serveTestServer(t *testing.T, name string) *sdkmcp.InMemoryTransport {
	t.Helper()
	srv := newTestServer(name)
	clientT, serverT := sdkmcp.NewInMemoryTransports()
	ss, err := srv.Connect(context.Background(), serverT, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ss.Close() })
	return clientT
}

func testCfg(name string) ServerConfig {
	return ServerConfig{Command: []string{name}, StartupTimeout: 5, ToolTimeout: 5}
}

func TestManagerConnectAndCall(t *testing.T) {
	m := newTestManager(t, map[string]ServerConfig{"docs": testCfg("docs")})
	m.Start(context.Background())
	waitReady(t, m)

	ts := m.Tools()
	if len(ts) != 4 {
		t.Fatalf("expected 4 tools, got %d: %v", len(ts), toolNames(ts))
	}
	out := tools.Execute(context.Background(), ts, "mcp__docs__greet", json.RawMessage(`{"name":"ghg"}`))
	if out != "hi ghg" {
		t.Errorf("greet = %q", out)
	}

	st := m.Statuses()
	if len(st) != 1 || st[0].Status != StatusReady || st[0].Tools != 4 {
		t.Fatalf("status = %+v", st)
	}
}

func TestManagerToolFailuresAreToolOutput(t *testing.T) {
	m := newTestManager(t, map[string]ServerConfig{"docs": testCfg("docs")})
	m.Start(context.Background())
	waitReady(t, m)
	out := tools.Execute(context.Background(), m.Tools(), "mcp__docs__fail", nil)
	if !strings.HasPrefix(out, "Error: ") || !strings.Contains(out, "boom") {
		t.Errorf("fail = %q", out)
	}
}

func TestManagerStructuredAndMedia(t *testing.T) {
	m := newTestManager(t, map[string]ServerConfig{"docs": testCfg("docs")})
	m.Start(context.Background())
	waitReady(t, m)
	ts := m.Tools()
	out := tools.Execute(context.Background(), ts, "mcp__docs__structured", nil)
	if !strings.Contains(out, `"answer": 42`) {
		t.Errorf("structured = %q", out)
	}
	out = tools.Execute(context.Background(), ts, "mcp__docs__media", nil)
	if !strings.Contains(out, "here you go") || !strings.Contains(out, "[image content omitted: image/png, 3 bytes]") {
		t.Errorf("media = %q", out)
	}
}

func TestManagerFailedServerDegradesToErrorString(t *testing.T) {
	m := NewManager(map[string]ServerConfig{"ghost": testCfg("ghost")})
	// No transport registered for "ghost": connect fails.
	m.connectTransport = func(cfg ServerConfig, stderr *ringBuffer) (sdkmcp.Transport, error) {
		return nil, context.DeadlineExceeded
	}
	t.Cleanup(m.Close)
	m.Start(context.Background())
	waitReady(t, m)

	st := m.Statuses()
	if st[0].Status != StatusFailed || st[0].Err == "" {
		t.Fatalf("status = %+v", st)
	}
	if n := len(m.Tools()); n != 0 {
		t.Fatalf("failed server contributed %d tools", n)
	}
	// A direct call against a tool name for the dead server is an error
	// string, not a hang or panic.
	s := m.servers["ghost"]
	_, err := s.call(context.Background(), "anything", nil)
	if err == nil || !strings.Contains(err.Error(), "unavailable") {
		t.Errorf("call err = %v", err)
	}

	// Executing a stale tool def through the standard tools.Tool interface returns error text.
	staleTool := tools.Tool{
		Def: models.NewTool("mcp__ghost__anything", "stale def", `{"type":"object"}`),
		Run: func(ctx context.Context, args json.RawMessage) (string, error) {
			return s.call(ctx, "anything", args)
		},
	}
	execOut := tools.Execute(context.Background(), []tools.Tool{staleTool}, "mcp__ghost__anything", nil)
	if !strings.HasPrefix(execOut, "Error: ") || !strings.Contains(execOut, "unavailable") {
		t.Errorf("execute stale tool = %q", execOut)
	}
}

func TestManagerReconnect(t *testing.T) {
	m := newTestManager(t, map[string]ServerConfig{"docs": testCfg("docs")})
	m.Start(context.Background())
	waitReady(t, m)
	if st := m.Statuses(); st[0].Status != StatusReady {
		t.Fatal("expected ready")
	}
	// Kill the session; the watcher marks the server failed.
	s := m.servers["docs"]
	s.mu.Lock()
	sess := s.sess
	s.mu.Unlock()
	sess.Close()
	deadline := time.Now().Add(2 * time.Second)
	for m.Statuses()[0].Status != StatusFailed && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if st := m.Statuses(); st[0].Status != StatusFailed {
		t.Fatalf("after session close: %+v", st[0])
	}
	// Reconnect brings it back.
	if !m.Reconnect("docs") {
		t.Fatal("reconnect returned false")
	}
	for m.Statuses()[0].Status != StatusReady && time.Now().Before(deadline.Add(2*time.Second)) {
		time.Sleep(10 * time.Millisecond)
	}
	if st := m.Statuses(); st[0].Status != StatusReady {
		t.Fatalf("after reconnect: %+v", st[0])
	}
	out := tools.Execute(context.Background(), m.Tools(), "mcp__docs__greet", json.RawMessage(`{"name":"back"}`))
	if out != "hi back" {
		t.Errorf("greet after reconnect = %q", out)
	}
	if m.Reconnect("nope") {
		t.Error("reconnect of unknown server should return false")
	}
}

func TestManagerParallelCallsRaceClean(t *testing.T) {
	var calls atomic.Int64
	m := newTestManager(t, map[string]ServerConfig{"a": testCfg("a"), "b": testCfg("b")})
	m.Start(context.Background())
	waitReady(t, m)
	ts := m.Tools()

	done := make(chan struct{}, 32)
	for i := 0; i < 32; i++ {
		go func(i int) {
			name := "mcp__a__greet"
			if i%2 == 1 {
				name = "mcp__b__greet"
			}
			out := tools.Execute(context.Background(), ts, name, json.RawMessage(`{"name":"x"}`))
			if out == "hi x" {
				calls.Add(1)
			}
			done <- struct{}{}
		}(i)
	}
	for i := 0; i < 32; i++ {
		<-done
	}
	if calls.Load() != 32 {
		t.Errorf("only %d/32 calls succeeded", calls.Load())
	}
}

func TestManagerCallRespectsCancel(t *testing.T) {
	m := NewManager(map[string]ServerConfig{"slowpoke": testCfg("slowpoke")})
	// Server that never settles its connect: transport that blocks forever.
	m.connectTransport = func(cfg ServerConfig, stderr *ringBuffer) (sdkmcp.Transport, error) {
		return &hangTransport{}, nil
	}
	t.Cleanup(m.Close)
	m.Start(context.Background())

	s := m.servers["slowpoke"]
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	start := time.Now()
	_, err := s.call(ctx, "greet", nil)
	if err == nil || time.Since(start) > time.Second {
		t.Errorf("call should respect ctx cancel while connecting, err=%v after %s", err, time.Since(start))
	}
}

// TestManagerCallFailFast: a tool call to a failed or disabled server returns
// immediately with an actionable message; a still-connecting server gets only
// the short grace, not the full startup timeout.
func TestManagerCallFailFast(t *testing.T) {
	m := NewManager(map[string]ServerConfig{
		"dead":   {Command: []string{"nope-not-a-binary"}, StartupTimeout: 2},
		"off":    {Command: []string{"true"}, Enabled: boolp(false)},
		"wedged": {Command: []string{"wedged"}, StartupTimeout: 30}, // connect hangs
	})
	m.connectTransport = func(cfg ServerConfig, stderr *ringBuffer) (sdkmcp.Transport, error) {
		if cfg.Command[0] == "wedged" {
			return &hangTransport{}, nil
		}
		return nil, fmt.Errorf("spawn failed: no such binary")
	}
	t.Cleanup(m.Close)
	m.Start(context.Background())

	// Failed server: instant, names the error and the reconnect command.
	<-m.servers["dead"].ready
	start := time.Now()
	_, err := m.servers["dead"].call(context.Background(), "x", nil)
	if err == nil || !strings.Contains(err.Error(), "unavailable") || !strings.Contains(err.Error(), "/mcp dead reconnect") {
		t.Errorf("failed-server call err = %v", err)
	}
	if time.Since(start) > 100*time.Millisecond {
		t.Errorf("failed-server call blocked %s", time.Since(start))
	}

	// Disabled server: instant, names the enable command.
	_, err = m.servers["off"].call(context.Background(), "x", nil)
	if err == nil || !strings.Contains(err.Error(), "disabled") || !strings.Contains(err.Error(), "/mcp off enable") {
		t.Errorf("disabled-server call err = %v", err)
	}

	// Connecting server: capped at connectGrace, then "still connecting".
	start = time.Now()
	_, err = m.servers["wedged"].call(context.Background(), "x", nil)
	if err == nil || !strings.Contains(err.Error(), "still connecting") {
		t.Errorf("connecting-server call err = %v", err)
	}
	if d := time.Since(start); d < connectGrace-500*time.Millisecond || d > connectGrace+2*time.Second {
		t.Errorf("connecting-server grace = %s, want ~%s", d, connectGrace)
	}
}

// TestManagerAutoReconnect: an unexpected session drop triggers a background
// reconnect (no manual /mcp reconnect), with bounded retries on failure.
func TestManagerAutoReconnect(t *testing.T) {
	t.Setenv("GHG_TEST_MCP_BACKOFF_MS", "20")

	m := newTestManager(t, map[string]ServerConfig{"docs": testCfg("docs")})
	m.Start(context.Background())
	waitReady(t, m)
	if st := m.Statuses(); st[0].Status != StatusReady {
		t.Fatal("expected ready")
	}
	// Drop the session; the watcher should fail then auto-reconnect.
	s := m.servers["docs"]
	s.mu.Lock()
	sess := s.sess
	s.mu.Unlock()
	sess.Close()
	// Wait for a fresh live session: ready status AND a stored session AND
	// the reconnect happened (gen advanced past the dropped one).
	s.mu.Lock()
	droppedGen := s.gen
	s.mu.Unlock()
	deadline := time.Now().Add(5 * time.Second)
	recovered := false
	for time.Now().Before(deadline) {
		s.mu.Lock()
		recovered = s.status == StatusReady && s.sess != nil && s.gen > droppedGen
		s.mu.Unlock()
		if recovered {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if !recovered {
		t.Fatalf("auto-reconnect did not recover: %+v", m.Statuses()[0])
	}
	out := tools.Execute(context.Background(), m.Tools(), "mcp__docs__greet", json.RawMessage(`{"name":"auto"}`))
	if out != "hi auto" {
		t.Errorf("call after auto-reconnect = %q", out)
	}
}

// TestManagerAutoReconnectGivesUp: a server that keeps failing exhausts
// autoReconnectMax tries and stays failed (no flapping forever).
func TestManagerAutoReconnectGivesUp(t *testing.T) {
	t.Setenv("GHG_TEST_MCP_BACKOFF_MS", "10")

	var connects atomic.Int64
	m := NewManager(map[string]ServerConfig{"flaky": testCfg("flaky")})
	m.connectTransport = func(cfg ServerConfig, stderr *ringBuffer) (sdkmcp.Transport, error) {
		connects.Add(1)
		if connects.Load() == 1 {
			return serveTestServer(t, "flaky"), nil // first connect succeeds
		}
		return nil, fmt.Errorf("server keeps dying") // every reconnect fails
	}
	t.Cleanup(m.Close)
	m.Start(context.Background())
	waitReady(t, m)

	s := m.servers["flaky"]
	s.mu.Lock()
	sess := s.sess
	s.mu.Unlock()
	sess.Close() // triggers auto-reconnect attempts, all failing

	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		s.mu.Lock()
		tries := s.autoTries
		s.mu.Unlock()
		if tries >= autoReconnectMax {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	// Let any in-flight last attempt settle, then assert: failed, capped.
	time.Sleep(200 * time.Millisecond)
	st := m.Statuses()[0]
	if st.Status != StatusFailed {
		t.Errorf("flaky should end failed, got %v", st.Status)
	}
	if got := connects.Load(); got > int64(autoReconnectMax)+1 {
		t.Errorf("connect attempts = %d, want <= initial + %d retries", got, autoReconnectMax)
	}
}

// hangTransport never finishes Connect (a wedged server process).
type hangTransport struct{}

func (hangTransport) Connect(ctx context.Context) (sdkmcp.Connection, error) {
	<-ctx.Done()
	return nil, ctx.Err()
}

func toolNames(ts []tools.Tool) []string {
	var out []string
	for _, t := range ts {
		out = append(out, t.Def.Function.Name)
	}
	return out
}

func TestNormalizeSchema(t *testing.T) {
	got := normalizeSchema(nil)
	if got != `{"type":"object","properties":{}}` {
		t.Errorf("nil schema = %s", got)
	}
	got = normalizeSchema(map[string]any{"title": "x"})
	var m map[string]any
	if err := json.Unmarshal([]byte(got), &m); err != nil {
		t.Fatal(err)
	}
	if m["type"] != "object" || m["properties"] == nil || m["title"] != "x" {
		t.Errorf("coerced schema = %v", m)
	}
}

func flattenResult(res *sdkmcp.CallToolResult) string {
	return mcpToolResult(res).Preview
}

func TestFlattenTruncates(t *testing.T) {
	big := strings.Repeat("x", 60_000)
	res := &sdkmcp.CallToolResult{Content: []sdkmcp.Content{&sdkmcp.TextContent{Text: big}}}
	out := flattenResult(res)
	if len(out) > 60_0000 || !strings.Contains(out, "[truncated") {
		t.Errorf("truncation missing, len=%d", len(out))
	}
}

// TestServerInstructions: a server that publishes instructions shows up in
// the system-prompt block; servers without instructions don't; sorted by name.
func TestServerInstructions(t *testing.T) {
	// ServerOptions.Instructions flows into the initialize result.
	srvWithInstr := sdkmcp.NewServer(&sdkmcp.Implementation{Name: "docs"}, &sdkmcp.ServerOptions{Instructions: "Call ping to check liveness. Always pass a name."})
	sdkmcp.AddTool(srvWithInstr, &sdkmcp.Tool{Name: "ping", InputSchema: map[string]any{"type": "object"}},
		func(ctx context.Context, req *sdkmcp.CallToolRequest, in struct{}) (*sdkmcp.CallToolResult, any, error) {
			return &sdkmcp.CallToolResult{Content: []sdkmcp.Content{&sdkmcp.TextContent{Text: "pong"}}}, nil, nil
		})

	m := NewManager(map[string]ServerConfig{"docs": testCfg("docs"), "plain": testCfg("plain")})
	m.connectTransport = func(cfg ServerConfig, stderr *ringBuffer) (sdkmcp.Transport, error) {
		if cfg.Command[0] == "docs" {
			ct, st := sdkmcp.NewInMemoryTransports()
			ss, err := srvWithInstr.Connect(context.Background(), st, nil)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { ss.Close() })
			return ct, nil
		}
		return serveTestServer(t, cfg.Command[0]), nil
	}
	t.Cleanup(m.Close)
	m.Start(context.Background())
	waitReady(t, m)

	block := m.InstructionsBlock()
	if !strings.Contains(block, `<server name="docs">`) || !strings.Contains(block, "Call ping to check liveness") {
		t.Errorf("block missing docs instructions:\n%s", block)
	}
	if strings.Contains(block, `"plain"`) {
		t.Errorf("server without instructions must not appear:\n%s", block)
	}

	// After the docs session drops, its instructions leave the block.
	s := m.servers["docs"]
	s.mu.Lock()
	sess := s.sess
	s.mu.Unlock()
	sess.Close()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		s.mu.Lock()
		gone := s.sess == nil
		s.mu.Unlock()
		if gone {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	// Auto-reconnect may restore them; either way the block must track the
	// live session, never a stale one. Wait for a terminal state.
	deadline = time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		s.mu.Lock()
		terminal := s.status == StatusReady || (s.status == StatusFailed && s.autoTries >= autoReconnectMax)
		s.mu.Unlock()
		if terminal {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	s.mu.Lock()
	st, instr := s.status, s.instr
	s.mu.Unlock()
	if st == StatusFailed && instr != "" {
		t.Error("failed server must not keep instructions")
	}
	if st == StatusReady && instr == "" {
		t.Error("reconnected server should restore instructions")
	}
}

// TestProbe: the doctor path — a good server probes ready with tool names; a
// dying one probes failed with its stderr tail.
func TestProbe(t *testing.T) {
	// Good server via the in-process transport seam: Probe uses the default
	// transport factory, so exercise it through a real subprocess only when
	// selfhost is enabled; otherwise verify the failed path (no binary).
	res := Probe(context.Background(), "ghost", ServerConfig{Command: []string{"no-such-binary-xyz"}, StartupTimeout: 3})
	if res.Status != StatusFailed {
		t.Errorf("ghost probe = %+v", res)
	}
	if res.Elapsed <= 0 {
		t.Error("probe should report elapsed time")
	}

	if testing.Short() {
		return
	}
	// A stdio server that starts but speaks no MCP: the connect itself is
	// bounded by the startup timeout; Close() then adds the SDK's stdio
	// terminate window (stdin close → SIGTERM → SIGKILL), which a `sleep`
	// child rides out. Assert the bound loosely — what matters is the probe
	// fails fast and reports why, not the exact teardown cost.
	res = Probe(context.Background(), "silent", ServerConfig{Command: []string{"sh", "-c", "sleep 5"}, StartupTimeout: 1})
	if res.Status != StatusFailed || res.Err == "" {
		t.Errorf("silent probe = %+v", res)
	}
	if res.Elapsed > 15*time.Second {
		t.Errorf("probe blew far past the startup timeout: %s", res.Elapsed)
	}
}

func TestEnableDisableCycle(t *testing.T) {
	m := newTestManager(t, map[string]ServerConfig{"docs": testCfg("docs")})
	m.Start(context.Background())
	waitReady(t, m)

	if !m.Disable("docs") {
		t.Fatal("disable returned false")
	}
	if st := m.Statuses()[0]; st.Status != StatusDisabled {
		t.Fatalf("after disable: %+v", st)
	}
	if len(m.Tools()) != 0 {
		t.Error("disabled server contributes no tools")
	}
	if _, err := m.servers["docs"].call(context.Background(), "greet", nil); err == nil || !strings.Contains(err.Error(), "disabled") {
		t.Errorf("call to disabled server: %v", err)
	}

	if !m.Enable("docs") {
		t.Fatal("enable returned false")
	}
	for i := 0; i < 300; i++ {
		if st := m.Statuses()[0]; st.Status == StatusReady {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if st := m.Statuses()[0]; st.Status != StatusReady {
		t.Fatalf("after enable: %+v", st)
	}
	out := ""
	for _, tool := range m.Tools() {
		if tool.Def.Function.Name == "mcp__docs__greet" {
			out, _ = tool.Run(context.Background(), nil)
		}
	}
	if out == "" {
		t.Error("re-enabled server's tools should work")
	}

	for _, bad := range []string{"nope"} {
		if m.Disable(bad) || m.Enable(bad) {
			t.Error("unknown name should return false")
		}
	}
	if _, ok := m.Config("docs"); !ok {
		t.Error("Config should return the live definition")
	}
	if _, ok := m.Config("nope"); ok {
		t.Error("Config unknown name should be !ok")
	}
}

func TestRingBuffer(t *testing.T) {
	r := newRingBuffer(8)
	r.Write([]byte("abc"))
	r.Write([]byte("defghij")) // wraps: keep last 8
	if got := r.String(); got != "cdefghij" {
		t.Errorf("wrap = %q", got)
	}
	r.Write([]byte("0123456789")) // oversize: keep tail
	if got := r.String(); got != "23456789" {
		t.Errorf("oversize = %q", got)
	}
	zero := newRingBuffer(0)
	zero.Write([]byte("anything"))
	if zero.String() != "" {
		t.Error("zero-capacity sink should discard")
	}
}

func TestFlattenRemainingContentTypes(t *testing.T) {
	res := flattenResult(&sdkmcp.CallToolResult{Content: []sdkmcp.Content{
		&sdkmcp.AudioContent{MIMEType: "audio/wav", Data: []byte{1}},
		&sdkmcp.EmbeddedResource{Resource: &sdkmcp.ResourceContents{URI: "file:///x", Text: "contents"}},
		&sdkmcp.EmbeddedResource{Resource: &sdkmcp.ResourceContents{URI: "file:///bin", Blob: []byte{9, 9}}},
		&sdkmcp.ResourceLink{URI: "https://link", Name: "ref"},
	}})
	for _, want := range []string{"audio content omitted", "[resource file:///x]", "contents", "binary resource omitted", "resource link: https://link (ref)"} {
		if !strings.Contains(res, want) {
			t.Errorf("missing %q in %q", want, res)
		}
	}
}

func TestDefaultTransportResolvesHeaderSecrets(t *testing.T) {
	t.Setenv("GHG_MCP_SECRET_TEST", "resolved-token")

	// References resolve at connect time; the config keeps the raw reference.
	cfg := ServerConfig{URL: "https://mcp.example.com", Headers: map[string]string{
		"Authorization": "${GHG_MCP_SECRET_TEST}",
		"X-Cmd":         "!printf cmd-token",
		"X-Literal":     "plain",
		"X-Dropped":     "$GHG_MCP_SECRET_UNSET",
	}}
	tr, err := defaultTransport(cfg, nil)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Headers["Authorization"] != "${GHG_MCP_SECRET_TEST}" {
		t.Fatalf("config mutated: %q", cfg.Headers["Authorization"])
	}
	st, ok := tr.(*sdkmcp.StreamableClientTransport)
	if !ok {
		t.Fatalf("transport type %T", tr)
	}
	ht, ok := st.HTTPClient.Transport.(headerTransport)
	if !ok {
		t.Fatalf("inner transport type %T", st.HTTPClient.Transport)
	}
	if ht["Authorization"] != "resolved-token" || ht["X-Cmd"] != "cmd-token" || ht["X-Literal"] != "plain" {
		t.Fatalf("resolved headers: %+v", ht)
	}
	if _, present := ht["X-Dropped"]; present {
		t.Fatalf("unresolvable reference must be dropped, got %q", ht["X-Dropped"])
	}
}

func TestStdioServerEndToEnd(t *testing.T) {
	if os.Getenv("GHG_TEST_SELFHOST") == "" {
		t.Skip("set GHG_TEST_SELFHOST=1 to run")
	}
	bin := filepath.Join(t.TempDir(), "ghg")
	if out, err := exec.Command("go", "build", "-o", bin, "../../cmd/ghg").CombinedOutput(); err != nil {
		t.Fatalf("build: %v\n%s", err, out)
	}

	// Happy path: real subprocess connect → tools → call → clean Close.
	m := NewManager(map[string]ServerConfig{"self": {Command: []string{bin, "mcp", "serve"}}})
	m.Start(context.Background())
	s := m.servers["self"]
	select {
	case <-s.ready:
	case <-time.After(30 * time.Second):
		t.Fatal("never settled")
	}
	if st := m.Statuses()[0]; st.Status != StatusReady || st.Tools != 4 {
		t.Fatalf("status = %+v", st)
	}
	out, err := s.call(context.Background(), "read", json.RawMessage(`{"path":"manager.go","limit":3}`))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "package mcp") {
		t.Fatalf("read via MCP = %q", out)
	}
	m.Close() // must return promptly and reap the child
	if st := m.Statuses()[0]; st.Status == StatusReady {
		t.Error("post-Close status should not be ready")
	}

	// Failure path: a command that dies instantly surfaces stderr in /mcp.
	m2 := NewManager(map[string]ServerConfig{"bad": {Command: []string{"sh", "-c", "echo dying-loudly >&2; exit 1"}, StartupTimeout: 5}})
	m2.Start(context.Background())
	defer m2.Close()
	s2 := m2.servers["bad"]
	select {
	case <-s2.ready:
	case <-time.After(10 * time.Second):
		t.Fatal("never settled")
	}
	st2 := m2.Statuses()[0]
	if st2.Status != StatusFailed {
		t.Fatalf("bad server status = %+v", st2)
	}
	if !strings.Contains(st2.Err, "dying-loudly") {
		t.Errorf("stderr tail should be in the failure message: %q", st2.Err)
	}
}

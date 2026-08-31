package lsp

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"maps"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/sacca97/ghg/internal/config"
	"github.com/sacca97/ghg/internal/sandbox"
	"github.com/sacca97/ghg/internal/tools"
)

// diagWait caps how long a write/edit tool call blocks for diagnostics
// (opencode blocks unbounded-ish behind a 3s timeout; we cap at 1.5s and
// return the tool result without diagnostics on timeout).
const diagWait = 1500 * time.Millisecond

// initTimeout bounds the initialize handshake.
const initTimeout = 10 * time.Second

// ServerSpec describes how to match and spawn one language server.
type ServerSpec struct {
	Command     []string          // argv; nil for a disabled entry
	Extensions  []string          // file extensions served, e.g. [".go"]
	RootMarkers []string          // files that mark a project root
	Env         map[string]string // extra env layered over ghg's
	Disabled    bool
}

// builtinServers is the shipped registry. Adding a built-in is one row.
var builtinServers = map[string]ServerSpec{
	"gopls": {
		Command:     []string{"gopls"},
		Extensions:  []string{".go"},
		RootMarkers: []string{"go.work", "go.mod", "go.sum"},
	},
}

// FromConfigMap converts the config-file "lsp" block into specs merged over
// the built-ins: a user entry with disabled=true removes the built-in, an
// entry with a command replaces/extends it (extensions/rootMarkers default to
// the built-in's when omitted). Mirrors mcp.FromConfigMap semantics
// (internal/mcp/config.go:169).
func FromConfigMap(in map[string]config.LSPServer) map[string]ServerSpec {
	out := make(map[string]ServerSpec, len(builtinServers)+len(in))
	for name, s := range builtinServers {
		out[name] = s
	}
	for name, c := range in {
		existing := out[name]
		if c.Enabled != nil && !*c.Enabled {
			delete(out, name)
			continue
		}
		spec := existing
		if len(c.Command) > 0 {
			spec.Command = c.Command
		}
		if len(c.Extensions) > 0 {
			spec.Extensions = c.Extensions
		}
		if len(c.RootMarkers) > 0 {
			spec.RootMarkers = c.RootMarkers
		}
		if len(c.Env) > 0 {
			spec.Env = c.Env
		}
		out[name] = spec
	}
	return out
}

// Status is one row of the /lsp view.
type Status struct {
	Name  string // server id
	Root  string // workspace root once connected
	State string // "connected", "not started", "failed"
	Err   string // failure detail when State == "failed"
}

// Manager owns LSP server processes and the diagnostics cache.
//
// Concurrency (docs/concurrency.md): spawn dedup is a close-to-broadcast
// channel per server key (spawning); diagnostic waiters are channels closed
// by the publish handler, keyed by path — no per-waiter
// goroutines. mu guards the maps only and is never held across I/O; the
// publish handler runs on the client's read goroutine and only takes mu
// briefly to swap caches/close waiters.
type Manager struct {
	mu          sync.Mutex
	specs       map[string]ServerSpec
	clients     map[string]*clientState // key: id + "\x00" + root
	broken      map[string]string       // key -> error message
	spawning    map[string]chan struct{}
	diags       map[string][]Diagnostic    // abs path -> latest pushed set
	diagSeq     map[string]uint64          // abs path -> push sequence
	waiters     map[string][]chan struct{} // abs path -> pending wakes
	keyer       spawnKeyer                 // nil = findRoot (production)
	closed      bool
	runtime     *tools.ToolRuntime
	lifeCtx     context.Context
	cancel      context.CancelFunc
	docMu       sync.Mutex
	warming     map[string]struct{}
	warmWG      sync.WaitGroup
	workspace   string
	renamesMu   sync.Mutex
	renames     map[string]*renameEntry
	renameOrder map[string][]string
}

// SetRuntime attaches the shared restricted process boundary. It should be
// called before the first file touch; lazy-spawned servers retain it.
func (m *Manager) SetRuntime(runtime *tools.ToolRuntime) {
	m.mu.Lock()
	m.runtime = runtime
	m.mu.Unlock()
	if runtime != nil {
		runtime.LanguageService = m
	}
}

type clientState struct {
	cli              *client
	cmd              *exec.Cmd
	root             string
	docs             map[string]int      // abs path -> last sent version
	hashes           map[string][32]byte // abs path -> last sent bytes
	positionEncoding string
}

// spawnKeyer resolves the spawn key (server id + root) for a file; nil =
// findRoot. Tests override it to pin pipe-attached clients to one key.
type spawnKeyer func(serverID, abs string, markers []string) string

// NewManager builds a manager from merged specs (see FromConfigMap).
func NewManager(specs map[string]ServerSpec) *Manager {
	lifeCtx, cancel := context.WithCancel(context.Background())
	return &Manager{
		specs:       specs,
		clients:     map[string]*clientState{},
		broken:      map[string]string{},
		spawning:    map[string]chan struct{}{},
		diags:       map[string][]Diagnostic{},
		diagSeq:     map[string]uint64{},
		waiters:     map[string][]chan struct{}{},
		lifeCtx:     lifeCtx,
		cancel:      cancel,
		warming:     map[string]struct{}{},
		renames:     map[string]*renameEntry{},
		renameOrder: map[string][]string{},
	}
}

// SetWorkspace supplies the canonical workspace used for rename containment
// when a manager is used without a ToolRuntime (primarily focused tests).
func (m *Manager) SetWorkspace(workspace string) error {
	canonical, err := canonicalDocumentPath(workspace)
	if err != nil {
		return err
	}
	info, err := os.Stat(canonical)
	if err != nil {
		return err
	}
	if !info.IsDir() {
		return errors.New("LSP workspace is not a directory")
	}
	m.mu.Lock()
	m.workspace = canonical
	m.mu.Unlock()
	return nil
}

// WaitDiagnostics touches path (didOpen/didChange at current disk content),
// waits up to diagWait for the server to push diagnostics for the new
// version, and returns the rendered block for the tool output — including
// sibling files the edit broke. Returns "" when no server covers the file,
// the server failed, the wait timed out, or there is nothing to report.
// Bounded by ctx: ctrl+c during a turn cancels the wait.
//
// WaitDiagnostics must not be called concurrently for the same path (the
// agent's per-path file lock already guarantees this: writes/edits to one
// path serialize, so their diagnostic waits do too).
func (m *Manager) WaitDiagnostics(ctx context.Context, path string) string {
	if m == nil {
		return ""
	}
	abs, err := documentPath(path)
	if err != nil {
		return ""
	}
	m.mu.Lock()
	beforeSeq := m.diagSeq[abs]
	m.mu.Unlock()
	syncResult, err := m.syncDocument(ctx, abs)
	if err != nil || syncResult.client == nil {
		return ""
	}
	if !syncResult.changed && beforeSeq != 0 {
		m.mu.Lock()
		siblings := siblingErrors(abs, m.diags)
		edited := append([]Diagnostic(nil), m.diags[abs]...)
		m.mu.Unlock()
		return Report(abs, edited, siblings)
	}
	// A sequence counter closes the small race between a synchronous server
	// push and waiter registration without retaining a second document waiter
	// protocol. The document mutex only serializes sync state and notifications.
	wch := make(chan struct{})
	m.mu.Lock()
	if m.closed {
		m.mu.Unlock()
		return ""
	}
	if m.diagSeq[abs] != beforeSeq {
		wch = nil
	} else {
		m.waiters[abs] = append(m.waiters[abs], wch)
	}
	m.mu.Unlock()
	deadline := time.Now().Add(diagWait)
	if wch != nil {
		defer m.removeWaiter(abs, wch)
		remain := time.Until(deadline)
		if remain > 0 {
			timer := time.NewTimer(remain)
			select {
			case <-wch:
				timer.Stop()
				m.mu.Lock()
				closing := m.closed
				m.mu.Unlock()
				if closing {
					return ""
				}
			case <-ctx.Done():
				timer.Stop()
				return ""
			case <-timer.C:
			}
		}
		graceFor := min(50*time.Millisecond, max(time.Until(deadline), 0))
		if graceFor > 0 {
			grace := time.NewTimer(graceFor)
			select {
			case <-ctx.Done():
				grace.Stop()
				return ""
			case <-grace.C:
			}
		}
	}

	m.mu.Lock()
	siblings := siblingErrors(abs, m.diags)
	edited := append([]Diagnostic(nil), m.diags[abs]...)
	m.mu.Unlock()
	return Report(abs, edited, siblings)
}

func (m *Manager) removeWaiter(path string, wanted chan struct{}) {
	m.mu.Lock()
	defer m.mu.Unlock()
	waiters := m.waiters[path]
	for i, waiter := range waiters {
		if waiter != wanted {
			continue
		}
		waiters = append(waiters[:i], waiters[i+1:]...)
		if len(waiters) == 0 {
			delete(m.waiters, path)
		} else {
			m.waiters[path] = waiters
		}
		return
	}
}

type documentSync struct {
	client  *client
	path    string
	root    string
	data    []byte
	version int
	changed bool
	key     string
}

func canonicalDocumentPath(name string) (string, error) {
	abs, err := filepath.Abs(name)
	if err != nil {
		return "", err
	}
	resolved, err := filepath.EvalSymlinks(abs)
	if err != nil {
		return "", err
	}
	return filepath.Clean(resolved), nil
}

func documentPath(name string) (string, error) {
	abs, err := filepath.Abs(name)
	if err != nil {
		return "", err
	}
	return filepath.Clean(abs), nil
}

// syncDocument reads the exact current bytes and sends didOpen once, then
// didChange only when their SHA-256 changes. The coarse mutex is intentional:
// LSP document versions and notifications must stay ordered across concurrent
// navigation calls, and this avoids a per-document state machine.
func (m *Manager) syncDocument(ctx context.Context, path string) (documentSync, error) {
	abs, err := documentPath(path)
	if err != nil {
		return documentSync{}, err
	}
	data, err := os.ReadFile(abs)
	if err != nil {
		return documentSync{}, err
	}
	cs, err := m.clientFor(ctx, abs)
	if err != nil {
		return documentSync{}, err
	}
	if cs == nil {
		return documentSync{path: abs, data: data}, nil
	}
	key := m.clientKey(cs)
	hash := sha256.Sum256(data)
	m.docMu.Lock()
	defer m.docMu.Unlock()
	m.mu.Lock()
	if m.closed {
		m.mu.Unlock()
		return documentSync{}, context.Canceled
	}
	if cs.docs == nil {
		cs.docs = make(map[string]int)
	}
	if cs.hashes == nil {
		cs.hashes = make(map[string][32]byte)
	}
	version := cs.docs[abs]
	previous, hadPrevious := cs.hashes[abs]
	changed := !hadPrevious || previous != hash
	if changed {
		version++
		cs.docs[abs] = version
		cs.hashes[abs] = hash
	}
	m.mu.Unlock()
	if !changed {
		return documentSync{client: cs.cli, path: abs, root: cs.root, data: data, version: version, key: key}, nil
	}
	uri := fileURI(abs)
	if version == 1 {
		cs.cli.notify("textDocument/didOpen", map[string]any{
			"textDocument": map[string]any{
				"uri": uri, "languageId": strings.TrimPrefix(filepath.Ext(abs), "."), "version": version, "text": string(data),
			},
		})
	} else {
		cs.cli.notify("textDocument/didChange", map[string]any{
			"textDocument":   map[string]any{"uri": uri, "version": version},
			"contentChanges": []map[string]any{{"text": string(data)}},
		})
	}
	return documentSync{client: cs.cli, path: abs, root: cs.root, data: data, version: version, changed: true, key: key}, nil
}

func (m *Manager) clientKey(want *clientState) string {
	m.mu.Lock()
	defer m.mu.Unlock()
	for key, cs := range m.clients {
		if cs == want {
			return key
		}
	}
	return ""
}

// Warm starts a covered document sync without delaying a successful read.
// Warm-ups use the manager lifetime rather than a tool-call context, so a
// transient read cancellation cannot leak or abandon a language-server child.
func (m *Manager) Warm(_ context.Context, path string) {
	if m == nil {
		return
	}
	abs, err := canonicalDocumentPath(path)
	if err != nil || !m.covered(abs) {
		return
	}
	m.mu.Lock()
	if m.closed {
		m.mu.Unlock()
		return
	}
	if _, ok := m.warming[abs]; ok {
		m.mu.Unlock()
		return
	}
	m.warming[abs] = struct{}{}
	lifeCtx := m.lifeCtx
	m.warmWG.Add(1)
	m.mu.Unlock()
	go func() {
		defer m.warmWG.Done()
		defer func() {
			m.mu.Lock()
			delete(m.warming, abs)
			m.mu.Unlock()
		}()
		ctx, cancel := context.WithTimeout(lifeCtx, 2*time.Second)
		defer cancel()
		_, _ = m.syncDocument(ctx, abs)
	}()
}

func (m *Manager) covered(path string) bool {
	ext := filepath.Ext(path)
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, spec := range m.specs {
		if spec.Disabled || len(spec.Command) == 0 {
			continue
		}
		for _, supported := range spec.Extensions {
			if supported == ext {
				return true
			}
		}
	}
	return false
}

// diagsEqual compares two diagnostic sets (order-sensitive: servers push
// ordered lists).
func diagsEqual(a, b []Diagnostic) bool {
	return slices.Equal(a, b)
}

// clientFor resolves a client for the file, spawning on demand. Spawn dedup:
// concurrent touches for the same (server, root) share one spawn via a
// close-to-broadcast channel — losers wait on <-ch, the winner closes it
// after registering clients[key] or broken[key].
func (m *Manager) clientFor(ctx context.Context, abs string) (*clientState, error) {
	ext := filepath.Ext(abs)
	var name string
	var spec ServerSpec
	m.mu.Lock()
	for _, n := range slices.Sorted(maps.Keys(m.specs)) {
		s := m.specs[n]
		if slices.Contains(s.Extensions, ext) {
			name, spec = n, s
			break
		}
	}
	m.mu.Unlock()
	if name == "" || spec.Disabled || len(spec.Command) == 0 {
		return nil, nil
	}
	root := findRoot(filepath.Dir(abs), spec.RootMarkers)
	if m.keyer != nil {
		root = m.keyer(name, abs, spec.RootMarkers)
	}

	m.mu.Lock()
	if m.closed {
		m.mu.Unlock()
		return nil, context.Canceled
	}
	key := name + "\x00" + root
	if cs, ok := m.clients[key]; ok {
		m.mu.Unlock()
		return cs, nil
	}
	if msg, bad := m.broken[key]; bad {
		m.mu.Unlock()
		return nil, errors.New(msg)
	}
	if ch, ok := m.spawning[key]; ok {
		m.mu.Unlock()
		select {
		case <-ch:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
		m.mu.Lock()
		defer m.mu.Unlock()
		if m.closed {
			return nil, context.Canceled
		}
		if cs, ok := m.clients[key]; ok {
			return cs, nil
		}
		if msg, ok := m.broken[key]; ok {
			return nil, errors.New(msg)
		}
		return nil, errors.New("language server failed to start")
	}
	ch := make(chan struct{})
	m.spawning[key] = ch
	m.mu.Unlock()

	cs, err := m.spawn(ctx, key, name, spec, root)

	m.mu.Lock()
	delete(m.spawning, key)
	if m.closed {
		close(ch) // wake deduped callers before killing the late child
		m.mu.Unlock()
		if cs != nil {
			cs.kill()
		}
		return nil, context.Canceled
	}
	if err != nil {
		// A caller-side cancel (ctrl+c mid-spawn) must not poison the server:
		// the process may be fine. Only genuine spawn/handshake failures are
		// remembered as broken.
		if !errors.Is(err, context.Canceled) {
			m.broken[key] = err.Error()
		}
	} else {
		m.clients[key] = cs
	}
	close(ch) // wake all deduped waiters
	m.mu.Unlock()
	return cs, err
}

// spawn starts the server process and runs the initialize handshake.
func (m *Manager) spawn(ctx context.Context, key, name string, spec ServerSpec, root string) (*clientState, error) {
	if _, err := exec.LookPath(spec.Command[0]); err != nil {
		return nil, fmt.Errorf("%s not on PATH", spec.Command[0])
	}
	m.mu.Lock()
	runtime := m.runtime
	m.mu.Unlock()
	var cmd *exec.Cmd
	if runtime != nil && runtime.Policy != nil {
		wrapped, err := runtime.WrapCommand(sandbox.CommandSpec{
			Program: spec.Command[0], Args: spec.Command[1:], Dir: root,
			Env: runtime.ChildEnv(runtime.SafeExplicitEnv(spec.Env)),
		})
		if err != nil {
			return nil, err
		}
		cmd = exec.Command(wrapped.Program, wrapped.Args...)
		cmd.Dir = wrapped.Dir
		cmd.Env = wrapped.Env
	} else {
		cmd = exec.Command(spec.Command[0], spec.Command[1:]...)
		cmd.Dir = root
		cmd.Env = os.Environ()
		for k, v := range spec.Env {
			cmd.Env = append(cmd.Env, k+"="+v)
		}
	}
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true} // own group: kills take the tree
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, err
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	cmd.Stderr = nil // server diagnostics noise; /dev/null
	if err := cmd.Start(); err != nil {
		return nil, err
	}

	cs := &clientState{cmd: cmd, root: root, docs: map[string]int{}, hashes: map[string][32]byte{}}
	cs.cli = newClient(stdin, stdout, func(uri string, version int, diags []Diagnostic) {
		m.publish(key, uri, version, diags)
	})

	initCtx, cancel := context.WithTimeout(ctx, initTimeout)
	defer cancel()
	// ponytail: didChange always sends full text, which gopls (and every
	// server worth configuring) accepts; if a stricter server ever rejects
	// it, parse capabilities.textDocumentSync.change from the result.
	var initResult struct {
		Capabilities struct {
			PositionEncoding string `json:"positionEncoding"`
		} `json:"capabilities"`
	}
	err = cs.cli.request(initCtx, "initialize", map[string]any{
		"processId": os.Getpid(),
		"rootUri":   fileURI(root),
		"workspaceFolders": []map[string]any{
			{"name": "workspace", "uri": fileURI(root)},
		},
		"capabilities": map[string]any{
			"textDocument": map[string]any{
				"synchronization":    map[string]any{"didOpen": true, "didChange": true},
				"publishDiagnostics": map[string]any{"versionSupport": true},
			},
			"general": map[string]any{"positionEncodings": []string{"utf-16"}},
		},
	}, &initResult)
	if err != nil {
		cs.kill()
		return nil, fmt.Errorf("initialize: %w", err)
	}
	encoding := strings.ToLower(strings.TrimSpace(initResult.Capabilities.PositionEncoding))
	if encoding == "" {
		encoding = "utf-16"
	}
	if encoding != "utf-16" {
		cs.kill()
		return nil, fmt.Errorf("initialize: unsupported LSP position encoding %q (only utf-16 is supported)", encoding)
	}
	cs.positionEncoding = encoding
	cs.cli.notify("initialized", map[string]any{})
	return cs, nil
}

// publish handles one publishDiagnostics push: swap the cache entry and wake
// waiters whose (path, version) is at-or-before this push, or whose server
// omitted the version. Runs on the client's read goroutine — no I/O here.
func (m *Manager) publish(key, uri string, version int, diags []Diagnostic) {
	path := uriPath(uri)
	if path == "" {
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.closed {
		return
	}
	m.diags[path] = diags
	m.diagSeq[path]++
	// Wake everyone waiting on this file regardless of push version: a stale
	// push is harmless (the waiter re-checks the cache and re-registers), a
	// missed one costs the full timeout. (ponytail: version matching could
	// skip stale wakes; the re-check already covers it.)
	for _, ch := range m.waiters[path] {
		close(ch)
	}
	delete(m.waiters, path)
}

// Statuses renders the /lsp rows: every configured server plus state.
func (m *Manager) Statuses() []Status {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []Status
	for _, n := range slices.Sorted(maps.Keys(m.specs)) {
		st := Status{Name: n, State: "not started"}
		for key, cs := range m.clients {
			if strings.HasPrefix(key, n+"\x00") {
				st.State = "connected"
				st.Root = cs.root
			}
		}
		for key, msg := range m.broken {
			if strings.HasPrefix(key, n+"\x00") {
				st.State = "failed"
				st.Err = msg
			}
		}
		out = append(out, st)
	}
	return out
}

// Close shuts every server down (shutdown/exit then kill) and wakes all
// waiters. Called on ghg exit before bashrun.KillAll, mirroring mcpMgr.
func (m *Manager) Close() {
	if m == nil {
		return
	}
	if m.cancel != nil {
		m.cancel()
	}
	m.mu.Lock()
	if m.closed {
		m.mu.Unlock()
		return
	}
	m.closed = true
	m.renamesMu.Lock()
	m.renames = map[string]*renameEntry{}
	m.renameOrder = map[string][]string{}
	m.renamesMu.Unlock()
	clients := make([]*clientState, 0, len(m.clients))
	for _, cs := range m.clients {
		clients = append(clients, cs)
	}
	for wk, chans := range m.waiters {
		for _, ch := range chans {
			close(ch)
		}
		delete(m.waiters, wk)
	}
	m.mu.Unlock()
	for _, cs := range clients {
		cs.kill()
	}
	m.warmWG.Wait()
}

// kill tries the polite LSP shutdown then SIGKILLs the process group.
func (cs *clientState) kill() {
	if cs.cmd == nil { // pipe-attached test client: no process to kill
		cs.cli.shutdown()
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	_ = cs.cli.request(ctx, "shutdown", nil, nil) // best effort
	cs.cli.notify("exit", nil)
	cs.cli.shutdown()
	if c, ok := cs.cli.stdin.(io.Closer); ok {
		_ = c.Close()
	}
	if cs.cmd.Process != nil {
		_ = syscall.Kill(-cs.cmd.Process.Pid, syscall.SIGKILL)
	}
	_ = cs.cmd.Wait()
}

// findRoot walks up from dir looking for any marker, falling back to dir
// itself (opencode's NearestRoot falls back to the project dir the same way,
// server.ts:32-79).
func findRoot(dir string, markers []string) string {
	for d := dir; ; d = filepath.Dir(d) {
		for _, mkr := range markers {
			if _, err := os.Stat(filepath.Join(d, mkr)); err == nil {
				return d
			}
		}
		parent := filepath.Dir(d)
		if parent == d {
			return dir
		}
	}
}

// fileURI renders an absolute path as a file:// URI.
func fileURI(path string) string {
	return "file://" + (&url.URL{Path: path}).String()
}

// uriPath parses a file:// URI back to a path; "" for non-file URIs.
// url.Parse already percent-decodes u.Path — do NOT unescape again (a path
// containing a literal % would corrupt or drop).
func uriPath(uri string) string {
	u, err := url.Parse(uri)
	if err != nil || u.Scheme != "file" || (u.Host != "" && u.Host != "localhost") {
		return ""
	}
	return u.Path
}

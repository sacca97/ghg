package mcp

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/sacca97/ghg/internal/config"
)

func TestParseClaudeStdio(t *testing.T) {
	t.Setenv("MCP_TEST_KEY", "sekret")
	cfgs, err := ParseClaude([]byte(`{
		"mcpServers": {
			"docs": {
				"type": "stdio",
				"command": "npx",
				"args": ["-y", "@docs/mcp"],
				"env": {"API_KEY": "${MCP_TEST_KEY}"}
			}
		}
	}`))
	if err != nil {
		t.Fatal(err)
	}
	c := cfgs["docs"]
	want := []string{"npx", "-y", "@docs/mcp"}
	if !reflect.DeepEqual(c.Command, want) {
		t.Errorf("command = %v, want %v", c.Command, want)
	}
	if c.Env["API_KEY"] != "sekret" {
		t.Errorf("env expansion failed: %q", c.Env["API_KEY"])
	}
	if c.Remote() || c.Disabled() {
		t.Errorf("unexpected remote/disabled: %+v", c)
	}
	if c.Valid() != "" {
		t.Errorf("Valid() = %q", c.Valid())
	}
}

func TestParseClaudeRemoteAndSSE(t *testing.T) {
	cfgs, err := ParseClaude([]byte(`{
		"mcpServers": {
			"web": {"type": "http", "url": "https://mcp.example.com/x", "headers": {"Authorization": "Bearer tok"}},
			"old": {"type": "sse", "url": "https://old.example.com/sse"}
		}
	}`))
	if err != nil {
		t.Fatal(err)
	}
	web := cfgs["web"]
	if !web.Remote() || web.URL != "https://mcp.example.com/x" || web.Headers["Authorization"] != "Bearer tok" {
		t.Errorf("bad remote: %+v", web)
	}
	old := cfgs["old"]
	if !old.Disabled() {
		t.Error("sse entry should import as disabled")
	}
	if old.Note == "" {
		t.Error("sse entry should carry an explanatory note")
	}
}

func TestParseClaudeInfersTypeAndMissingVar(t *testing.T) {
	os.Unsetenv("NO_SUCH_VAR_GHG_TEST")
	cfgs, err := ParseClaude([]byte(`{
		"mcpServers": {
			"a": {"command": "srv", "env": {"X": "$NO_SUCH_VAR_GHG_TEST"}},
			"b": {"url": "http://localhost:9000/mcp"}
		}
	}`))
	if err != nil {
		t.Fatal(err)
	}
	if cfgs["a"].Remote() {
		t.Error("command-only entry should be stdio")
	}
	if cfgs["a"].Env["X"] != "" {
		t.Errorf("missing env var should expand to empty, got %q", cfgs["a"].Env["X"])
	}
	if !cfgs["b"].Remote() {
		t.Error("url-only entry should be remote")
	}
}

func TestMergePrecedence(t *testing.T) {
	disabled := false
	ghg := map[string]ServerConfig{"a": {Command: []string{"ghg-a"}}, "b": {Enabled: &disabled, Command: []string{"ghg-b"}}}
	claude := map[string]ServerConfig{"a": {Command: []string{"claude-a"}}, "c": {Command: []string{"claude-c"}}}
	m := Merge(ghg, nil, claude)
	if m["a"].Command[0] != "ghg-a" {
		t.Error("ghg config must win over claude")
	}
	if m["c"].Command[0] != "claude-c" {
		t.Error("claude entry should survive")
	}
	if !m["b"].Disabled() {
		t.Error("ghg-only entry should survive with enabled=false")
	}
}

func TestLoadMergedDiscovery(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, ".mcp.json"), []byte(`{"mcpServers": {"proj": {"command": "proj-srv"}}}`), 0o600); err != nil {
		t.Fatal(err)
	}

	merged, errs := LoadMerged(dir, map[string]ServerConfig{"mine": {Command: []string{"my-srv"}}})
	if len(errs) != 0 {
		t.Fatalf("unexpected discovery errors: %v", errs)
	}
	for _, name := range []string{"proj", "mine"} {
		if _, ok := merged[name]; !ok {
			t.Errorf("missing server %q in merged config", name)
		}
	}

	// A malformed .mcp.json reports an error but does not kill the merge.
	if err := os.WriteFile(filepath.Join(dir, ".mcp.json"), []byte(`{broken`), 0o600); err != nil {
		t.Fatal(err)
	}
	merged, errs = LoadMerged(dir, nil)
	if _, ok := errs[".mcp.json"]; !ok {
		t.Error("expected a parse error for .mcp.json")
	}
}

// TestLoadMergedFilteredPolicy pins the mcpImport gating: a policy-filtered
// import lands in Blocked as disabled+noted — never connected, never silently
// dropped — while admitted imports and ghg's own entries merge normally.
func TestLoadMergedFilteredPolicy(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, ".mcp.json"), []byte(
		`{"mcpServers": {"proj": {"command": "proj-srv"}, "ghost": {"command": "ghost-srv"}, "paper": {"url": "http://127.0.0.1:29979/mcp"}}}`), 0o600); err != nil {
		t.Fatal(err)
	}

	// Exclude ghost.
	policy := ImportPolicy{Claude: ImportSourcePolicy{
		Enabled: true,
		Exclude: map[string]bool{"ghost": true},
	}}
	f := LoadMergedFiltered(dir, nil, policy)
	if _, ok := f.Merged["paper"]; !ok {
		t.Error("admitted server should merge")
	}
	if _, ok := f.Merged["ghost"]; ok {
		t.Error("excluded server must not merge")
	}
	b, ok := f.Blocked["ghost"]
	if !ok {
		t.Fatal("excluded server must appear in Blocked, not vanish")
	}
	if !b.Disabled() || !strings.HasPrefix(b.Note, "blocked by mcpImport config") {
		t.Errorf("blocked entry should be disabled with a note, got %+v", b)
	}
	if f.Sources["proj"] != ".mcp.json" || f.Sources["paper"] != ".mcp.json" {
		t.Errorf("source attribution wrong: %v", f.Sources)
	}

	// Source off: everything from claude drops into Blocked.
	f = LoadMergedFiltered(dir, nil, ImportPolicy{
		Claude: ImportSourcePolicy{Enabled: false},
	})
	if len(f.Blocked) != 3 {
		t.Errorf("all claude servers should be blocked, got %v", f.Blocked)
	}
	if _, ok := f.Merged["proj"]; ok {
		t.Error("disabled source must not merge")
	}

	// Only-allowlist, and exclude beating only when both are set.
	f = LoadMergedFiltered(dir, nil, ImportPolicy{
		Claude: ImportSourcePolicy{Enabled: true, Only: map[string]bool{"proj": true, "ghost": true},
			Exclude: map[string]bool{"ghost": true}},
	})
	if _, ok := f.Merged["proj"]; !ok {
		t.Error("allowlisted server should merge")
	}
	if _, ok := f.Blocked["ghost"]; !ok {
		t.Error("exclude must win over only")
	}

	// A ghg entry of the same name is never shadowed by a ghost row.
	ghgOver := LoadMergedFiltered(dir, map[string]ServerConfig{"ghost": {Command: []string{"mine"}}}, policy)
	if ghgOver.Merged["ghost"].Command[0] != "mine" {
		t.Error("ghg config must still win over a blocked import")
	}
	if _, ok := ghgOver.Blocked["ghost"]; ok {
		t.Error("no ghost row when ghg owns the name")
	}

	// Zero policy == LoadMerged (import everything).
	def := LoadMergedFiltered(dir, nil, ImportPolicyFrom(nil))
	if len(def.Merged) != 3 || len(def.Blocked) != 0 {
		t.Errorf("nil policy must import everything, got merged=%v blocked=%v", def.Merged, def.Blocked)
	}

	// Every discovered entry carries the file it came from.
	if got := def.Merged["proj"].Source; got != filepath.Join(dir, ".mcp.json") {
		t.Errorf("Merged[proj].Source = %q", got)
	}
	if got := f.Blocked["ghost"].Source; got != filepath.Join(dir, ".mcp.json") {
		t.Errorf("Blocked[ghost].Source = %q", got)
	}
}

// TestManagerFromBlockedDiscovery pins the scenario end to end at the manager level:
// a .mcp.json carrying an excluded server, gated by policy, produces a manager with
// no excluded server in the live set and one visible blocked row.
func TestManagerFromBlockedDiscovery(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, ".mcp.json"), []byte(
		`{"mcpServers": {"node_repl": {"command": "/app/bin/node_repl"}}}`), 0o600); err != nil {
		t.Fatal(err)
	}

	f := LoadMergedFiltered(dir, nil, ImportPolicyFrom(&config.MCPImport{
		Claude: &config.MCPImportSource{Exclude: []string{"node_repl"}},
	}))
	mgr := NewManager(f.Merged)
	mgr.SetBlocked(f.Blocked)
	if _, ok := mgr.Config("node_repl"); ok {
		t.Error("a blocked server must never reach the manager's live set")
	}
	if !mgr.BlockedByPolicy("node_repl") {
		t.Error("the manager must remember node_repl as policy-blocked")
	}
	blocked := mgr.Blocked()
	if len(blocked) != 1 || blocked[0].Status != StatusDisabled || blocked[0].Note == "" {
		t.Errorf("blocked snapshot: %+v", blocked)
	}
	if blocked[0].Source != filepath.Join(dir, ".mcp.json") {
		t.Errorf("blocked row should point at its source file, got %q", blocked[0].Source)
	}
}

// TestManagerStatusSource pins the end-to-end handoff: the file a server was
// discovered from survives NewManager and shows up in the Statuses snapshot.
func TestManagerStatusSource(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, ".mcp.json"), []byte(
		`{"mcpServers": {"proj": {"command": "proj-srv"}}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	f := LoadMergedFiltered(dir, nil, ImportPolicyFrom(nil))
	mgr := NewManager(f.Merged)
	sts := mgr.Statuses()
	if len(sts) != 1 || sts[0].Source != filepath.Join(dir, ".mcp.json") {
		t.Errorf("status source: %+v", sts)
	}
}

// TestImportPolicyFrom covers the config-block → policy conversion.
func TestImportPolicyFrom(t *testing.T) {
	off := false
	p := ImportPolicyFrom(&config.MCPImport{
		Claude: &config.MCPImportSource{Enabled: &off},
		Codex:  &config.MCPImportSource{Only: []string{"a"}, Exclude: []string{"b"}},
	})
	if p.Claude.Admits("anything") {
		t.Error("enabled=false must block the whole source")
	}
	if !p.Codex.Admits("a") || p.Codex.Admits("c") {
		t.Error("only must act as an allowlist")
	}
	if p.Codex.Admits("b") {
		t.Error("exclude must beat only")
	}
	// nil block and nil source both mean "on, unfiltered".
	for _, p := range []ImportPolicy{ImportPolicyFrom(nil), ImportPolicyFrom(&config.MCPImport{})} {
		if !p.Claude.Admits("x") || !p.Codex.Admits("x") {
			t.Error("nil policy must admit everything")
		}
	}
}

func TestToolNameRoundTrip(t *testing.T) {
	// Safe names pass through unchanged with claude-style double underscores.
	name := ToolName("my-server", "get_doc.v2")
	if name != "mcp__my-server__get_doc_v2" {
		t.Errorf("ToolName = %q", name)
	}
	srv, tool, ok := ParseToolName(name)
	if !ok || srv != "my-server" || tool != "get_doc_v2" {
		t.Fatalf("ParseToolName(%q) = %q %q %v", name, srv, tool, ok)
	}
	// Underscores inside both names stay recoverable.
	name = ToolName("my_server", "do_thing_now")
	srv, tool, ok = ParseToolName(name)
	if !ok || srv != "my_server" || tool != "do_thing_now" {
		t.Fatalf("ParseToolName(%q) = %q %q %v", name, srv, tool, ok)
	}
	// Unsafe server chars get a hash suffix; the opencode collision class
	// ("a.b" vs "a b") stays distinct.
	if ToolName("a.b", "t") == ToolName("a b", "t") {
		t.Error("sanitized names must not collide")
	}
	srv, _, ok = ParseToolName(ToolName("a.b", "t"))
	if !ok || !strings.HasPrefix(srv, "a-b_") {
		t.Errorf("hashed server key should be unambiguous, got %q", srv)
	}
	if _, _, ok := ParseToolName("bash"); ok {
		t.Error("bash is not an MCP tool")
	}
	if _, _, ok := ParseToolName("mcp__broken"); ok {
		t.Error("mcp__ without server__tool split is invalid")
	}
}

func TestValidAndDefaults(t *testing.T) {
	if (ServerConfig{}).Valid() == "" {
		t.Error("empty config should be invalid")
	}
	c := ServerConfig{Command: []string{"x"}}
	if c.StartupTimeoutDuration() != 30*time.Second || c.ToolTimeoutDuration() != 60*time.Second {
		t.Error("default timeouts wrong")
	}
	if (ServerConfig{URL: "ftp://x"}).Valid() == "" {
		t.Error("non-http url should be invalid")
	}
	if (ServerConfig{URL: "http://x", Command: []string{"y"}}).Valid() == "" {
		t.Error("command+url should be invalid")
	}
}

func TestFromConfigMap(t *testing.T) {
	t.Setenv("FROMCFG_KEY", "v1")
	in := map[string]config.MCPServer{
		"docs": {Command: []string{"npx", "-y"}, Env: map[string]string{"K": "$FROMCFG_KEY"}, StartupTimeout: 3},
		"web":  {URL: "https://x", Headers: map[string]string{"A": "b"}},
	}
	out := FromConfigMap(in)
	if got := out["docs"]; len(got.Command) != 2 || got.Env["K"] != "v1" || got.StartupTimeout != 3 {
		t.Errorf("docs = %+v", got)
	}
	if !out["web"].Remote() || out["web"].Headers["A"] != "b" {
		t.Errorf("web = %+v", out["web"])
	}
	if FromConfigMap(nil) != nil {
		t.Error("nil in, nil out")
	}
}

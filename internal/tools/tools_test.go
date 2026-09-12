package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/sacca97/ghg/internal/models"
)

func run(t *testing.T, name, args string) string {
	t.Helper()
	return Execute(context.Background(), All(), name, json.RawMessage(args))
}

func TestToolRoundTrip(t *testing.T) {
	dir := t.TempDir()
	f := filepath.Join(dir, "sub", "a.txt")

	out := run(t, "write", fmt.Sprintf(`{"path":%q,"content":"one\ntwo\nthree\n"}`, f))
	if strings.HasPrefix(out, "Error") {
		t.Fatal(out)
	}
	out = run(t, "read", fmt.Sprintf(`{"path":%q}`, f))
	if !strings.Contains(out, "2\ttwo") {
		t.Fatalf("read missing line numbers: %q", out)
	}
	out = run(t, "edit", fmt.Sprintf(`{"mode":"exact","path":%q,"old_string":"two","new_string":"2"}`, f))
	if strings.HasPrefix(out, "Error") {
		t.Fatal(out)
	}
	out = run(t, "read", fmt.Sprintf(`{"path":%q,"offset":2,"limit":1}`, f))
	if !strings.Contains(out, "2\t2") {
		t.Fatalf("edit not applied: %q", out)
	}
	readResult := ExecuteResult(context.Background(), All(), "read", json.RawMessage(fmt.Sprintf(`{"path":%q}`, f)))
	if readResult.Source != "read" || !IsUntrusted(readResult) {
		t.Fatalf("read result should carry its untrusted source: %+v", readResult)
	}
	aliasResult := ExecuteResult(context.Background(), All(), "read_file", json.RawMessage(fmt.Sprintf(`{"path":%q}`, f)))
	if aliasResult.ExitCode != 0 || !strings.Contains(aliasResult.Preview, "1\tone") {
		t.Fatalf("read_file alias = %+v", aliasResult)
	}
	// ambiguous edit must fail without replace_all
	run(t, "write", fmt.Sprintf(`{"path":%q,"content":"x x"}`, f))
	out = run(t, "edit", fmt.Sprintf(`{"mode":"exact","path":%q,"old_string":"x","new_string":"y"}`, f))
	if !strings.HasPrefix(out, "Error") {
		t.Fatalf("expected ambiguity error, got %q", out)
	}
	out = run(t, "bash", `{"command":"echo hi; echo err >&2; exit 3"}`)
	if !strings.Contains(out, "hi") || !strings.Contains(out, "err") || !strings.Contains(out, "exit") {
		t.Fatalf("bash output wrong: %q", out)
	}
	out = run(t, "nope", `{}`)
	if !strings.Contains(out, "unknown tool") {
		t.Fatalf("expected unknown tool error, got %q", out)
	}
}

func TestBashGrepNoMatchIsNotFailure(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "runtime.go")
	if err := os.WriteFile(path, []byte("package worker\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	command := fmt.Sprintf("cd %q && grep -n -A 12 %q %q | head -40", dir, "missing symbol", path)
	result := ExecuteResult(context.Background(), All(), "bash", json.RawMessage(fmt.Sprintf(`{"command":%q}`, command)))
	if result.ExitCode != 0 || !strings.Contains(result.Preview, "grep: (no matches)") {
		t.Fatalf("grep no-match result = %+v", result)
	}

	failure := ExecuteResult(context.Background(), All(), "bash", json.RawMessage(`{"command":"false"}`))
	if failure.ExitCode == 0 {
		t.Fatalf("real bash failure was normalized: %+v", failure)
	}
}

func TestReadConsumesAndBoundsAnOversizedSingleLine(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "long.txt")
	content := strings.Repeat("x", int(maxOutputBytes)+4096)
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}

	result := ExecuteResult(context.Background(), All(), "read", json.RawMessage(fmt.Sprintf(`{"path":%q}`, path)))
	if !strings.HasPrefix(result.Preview, "Error:") || !strings.Contains(result.Preview, "line exceeds") {
		t.Fatalf("oversized read should fail without issuing a partial line: %+v", result)
	}
}

func TestReadUsesBoundedDefaultLimit(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "many-lines.txt")
	lines := make([]string, 300)
	for i := range lines {
		lines[i] = fmt.Sprintf("line %d", i+1)
	}
	if err := os.WriteFile(path, []byte(strings.Join(lines, "\n")+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	result := ExecuteResult(context.Background(), All(), "read", json.RawMessage(fmt.Sprintf(`{"path":%q}`, path)))
	if result.ExitCode != 0 {
		t.Fatalf("default read = %+v", result)
	}
	if result.Metadata["observation_end"] != "250" || result.Metadata["observation_next_offset"] != "251" {
		t.Fatalf("default read metadata = %+v", result.Metadata)
	}
}

func TestIntegerStringArguments(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "lines.go")
	if err := os.WriteFile(path, []byte("one\ntwo\nthree\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	read := ExecuteResult(context.Background(), All(), "read", json.RawMessage(fmt.Sprintf(`{"ranges":[{"path":%q,"offset":"2","limit":"1"}]}`, path)))
	if read.ExitCode != 0 || !strings.Contains(read.Preview, "2\ttwo") {
		t.Fatalf("numeric read arguments = %+v", read)
	}
	grep := ExecuteResult(context.Background(), All(), "grep", json.RawMessage(fmt.Sprintf(`{"pattern":"two","path":%q,"max_results":"1"}`, path)))
	if grep.ExitCode != 0 || !strings.Contains(grep.Preview, "two") {
		t.Fatalf("numeric grep arguments = %+v", grep)
	}
	invalid := ExecuteResult(context.Background(), All(), "read", json.RawMessage(fmt.Sprintf(`{"path":%q,"limit":"many"}`, path)))
	if invalid.ExitCode == 0 {
		t.Fatalf("invalid numeric string unexpectedly succeeded: %+v", invalid)
	}
}

func TestReadBatchesIndependentRanges(t *testing.T) {
	dir := t.TempDir()
	first := filepath.Join(dir, "first.go")
	second := filepath.Join(dir, "second.go")
	if err := os.WriteFile(first, []byte("package first\nfirst body\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(second, []byte("package second\nsecond body\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	args := map[string]any{"ranges": []map[string]any{
		{"path": first, "offset": 1, "limit": 2},
		{"path": second, "offset": 1, "limit": 2},
	}}
	raw, err := json.Marshal(args)
	if err != nil {
		t.Fatal(err)
	}
	result := ExecuteResult(context.Background(), All(), "read", raw)
	if result.ExitCode != 0 || result.Metadata["observation_count"] != "2" {
		t.Fatalf("batched read = %+v", result)
	}
	var observations []struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal([]byte(result.Metadata["observations"]), &observations); err != nil {
		t.Fatal(err)
	}
	if len(observations) != 2 || observations[0].ID == "" || observations[1].ID == "" || observations[0].ID == observations[1].ID {
		t.Fatalf("batched observations = %+v", observations)
	}
	if firstAt, secondAt := strings.Index(result.Preview, filepath.ToSlash(first)), strings.Index(result.Preview, filepath.ToSlash(second)); firstAt < 0 || secondAt < 0 || firstAt >= secondAt {
		t.Fatalf("batched read order = %q", result.Preview)
	}
}

func TestReadRangesWinOverLegacyFields(t *testing.T) {
	dir := t.TempDir()
	first := filepath.Join(dir, "first.go")
	second := filepath.Join(dir, "second.go")
	if err := os.WriteFile(first, []byte("package first\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(second, []byte("package second\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	args := fmt.Sprintf(`{"path":%q,"offset":1,"limit":1,"ranges":[{"path":%q},{"path":%q}]}`, first, first, second)
	result := ExecuteResult(context.Background(), All(), "read", json.RawMessage(args))
	if result.ExitCode != 0 || result.Metadata["observation_count"] != "2" {
		t.Fatalf("mixed legacy and batched read = %+v", result)
	}
}

func TestReadBatchKeepsSuccessfulSiblingsWhenOneRangeFails(t *testing.T) {
	dir := t.TempDir()
	first := writeSearchFile(t, dir, "first.go", "package first\n")
	second := writeSearchFile(t, dir, "second.go", "package second\n")
	missing := filepath.Join(dir, "missing.go")
	args := fmt.Sprintf(`{"ranges":[{"path":%q},{"path":%q},{"path":%q}]}`, first, missing, second)
	result := ExecuteResult(context.Background(), All(), "read", json.RawMessage(args))
	if result.ExitCode != 0 || result.Metadata["observation_count"] != "2" {
		t.Fatalf("mixed batched read = %+v", result)
	}
	if !strings.Contains(result.Preview, first) || !strings.Contains(result.Preview, second) || !strings.Contains(result.Preview, "range 2") {
		t.Fatalf("mixed batched read hid a sibling or failure: %q", result.Preview)
	}
}

func TestReadBatchReportsUnprocessedRanges(t *testing.T) {
	dir := t.TempDir()
	paths := make([]string, 3)
	content := strings.Repeat(strings.Repeat("x", 100)+"\n", 300)
	for i := range paths {
		paths[i] = filepath.Join(dir, fmt.Sprintf("large-%d.go", i))
		if err := os.WriteFile(paths[i], []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	args := map[string]any{"ranges": []map[string]any{
		{"path": paths[0], "offset": 1, "limit": 300},
		{"path": paths[1], "offset": 41, "limit": 300},
		{"path": paths[2], "offset": 81, "limit": 300},
	}}
	raw, err := json.Marshal(args)
	if err != nil {
		t.Fatal(err)
	}
	result := ExecuteResult(context.Background(), All(), "read", raw)
	if !strings.Contains(result.Preview, "unprocessed ranges:") ||
		!strings.Contains(result.Preview, "2:"+paths[1]+":41-340") ||
		!strings.Contains(result.Preview, "3:"+paths[2]+":81-380") {
		t.Fatalf("batched omission report = %q", result.Preview)
	}
}

func TestWritePreservesExistingMode(t *testing.T) {
	dir := t.TempDir()
	existing := filepath.Join(dir, "existing.txt")
	if err := os.WriteFile(existing, []byte("old"), 0o600); err != nil {
		t.Fatal(err)
	}
	if out := run(t, "write", fmt.Sprintf(`{"path":%q,"content":"new"}`, existing)); strings.HasPrefix(out, "Error") {
		t.Fatal(out)
	}
	if got, err := os.ReadFile(existing); err != nil || string(got) != "new" {
		t.Fatalf("existing write = %q, %v", got, err)
	}
	if mode := fileMode(t, existing); mode != 0o600 {
		t.Fatalf("existing write changed mode to %o", mode)
	}

	created := filepath.Join(dir, "nested", "created.txt")
	if out := run(t, "write", fmt.Sprintf(`{"path":%q,"content":"new"}`, created)); strings.HasPrefix(out, "Error") {
		t.Fatal(out)
	}
	if mode := fileMode(t, created); mode != 0o644 {
		t.Fatalf("new write mode = %o, want 644", mode)
	}
}

func TestHelpersAndEdgeCases(t *testing.T) {
	if len(Defs(All())) != 10 {
		t.Fatal("expected 10 tool defs")
	}
	long := strings.Repeat("x", maxOutput+10)
	if out := Truncate(long); len(out) > maxOutput || !strings.Contains(out, "truncated") {
		t.Fatalf("truncate: %q", out[len(out)-40:])
	}
	if out := TruncateTail(long); len(out) > maxOutput || !strings.HasPrefix(out, "[... first ") || !strings.Contains(out, "bytes truncated]") {
		t.Fatalf("truncateTail: %q", out[:40])
	}
	// short strings pass through untouched
	if Truncate("ok") != "ok" || TruncateTail("ok") != "ok" {
		t.Fatal("short strings must not be modified")
	}

	// bad args json hits every tool's unmarshal error branch
	for _, name := range []string{"bash", "read", "write", "edit"} {
		if out := run(t, name, `{bad`); !strings.HasPrefix(out, "Error") {
			t.Fatalf("%s: expected error, got %q", name, out)
		}
	}

	// empty output branch
	if out := run(t, "bash", `{"command":"true"}`); out != "(no output)" {
		t.Fatalf("empty output: %q", out)
	}
	// timeout branch
	if out := run(t, "bash", `{"command":"sleep 5","timeout":0.1}`); !strings.Contains(out, "timed out") {
		t.Fatalf("timeout: %q", out)
	}

	dir := t.TempDir()
	f := filepath.Join(dir, "f.txt")
	// read: missing file, offset past EOF, default limit
	if out := run(t, "read", fmt.Sprintf(`{"path":%q}`, f)); !strings.HasPrefix(out, "Error") {
		t.Fatalf("missing file: %q", out)
	}
	run(t, "write", fmt.Sprintf(`{"path":%q,"content":"a\nb"}`, f))
	if out := run(t, "read", fmt.Sprintf(`{"path":%q,"offset":99}`, f)); !strings.Contains(out, "past end") {
		t.Fatalf("offset past EOF: %q", out)
	}
	// write: MkdirAll fails when a parent is a file
	if out := run(t, "write", fmt.Sprintf(`{"path":%q,"content":"x"}`, f+"/child.txt")); !strings.HasPrefix(out, "Error") {
		t.Fatalf("bad parent: %q", out)
	}
	// edit: missing file, not-found old_string, replace_all
	if out := run(t, "edit", fmt.Sprintf(`{"mode":"exact","path":%q,"old_string":"x","new_string":"y"}`, filepath.Join(dir, "nope"))); !strings.HasPrefix(out, "Error") {
		t.Fatalf("edit missing file: %q", out)
	}
	if out := run(t, "edit", fmt.Sprintf(`{"mode":"exact","path":%q,"old_string":"zzz","new_string":"y"}`, f)); !strings.Contains(out, "not found") {
		t.Fatalf("edit not found: %q", out)
	}
	run(t, "write", fmt.Sprintf(`{"path":%q,"content":"x x x"}`, f))
	if out := run(t, "edit", fmt.Sprintf(`{"mode":"exact","path":%q,"old_string":"x","new_string":"y","replace_all":true}`, f)); !strings.Contains(out, "3 occurrence") {
		t.Fatalf("replace_all: %q", out)
	}

	// Regression: a command that reads from /dev/tty (as sudo does for a
	// password) must NOT hang the tool. pre-fix the tool used CombinedOutput
	// with the child sharing ghg's controlling terminal, so the read
	// blocked until the 120s bash timeout. post-fix the child runs in a new
	// session with no controlling tty and stdin tied to /dev/null, so the
	// read fails immediately. We assert it returns well under the cap and
	// surfaces the tty failure rather than silently succeeding.
	start := time.Now()
	out := run(t, "bash", `{"command":"read -r p < /dev/tty; echo got $p","timeout":5}`)
	if elapsed := time.Since(start); elapsed > 3*time.Second {
		t.Fatalf("bash tool hung %s on /dev/tty read — fast-fail regressed: %q", elapsed, out)
	}
	if strings.Contains(out, "timed out") {
		t.Fatalf("bash tool timed out on /dev/tty read — fast-fail regressed: %q", out)
	}
	// The /dev/tty open must fail (no controlling terminal under Setsid);
	// bash reports "No such device or address" or similar. The crucial bit is
	// that $p is EMPTY — no password was read — and we did not hang.
	if !strings.Contains(out, "/dev/tty") {
		t.Fatalf("expected a /dev/tty error in output: %q", out)
	}
}

func TestReadObservedContentFromReader(t *testing.T) {
	data := "first line\nsecond line\nthird line\n"
	res, err := readObservedContent(context.Background(), "/canonical/path.go", "path.go", strings.NewReader(data), 2, 1)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(res.Preview, "2\tsecond line") {
		t.Fatalf("expected second line in preview, got %q", res.Preview)
	}
	if strings.Contains(res.Preview, "first line") || strings.Contains(res.Preview, "third line") {
		t.Fatalf("unexpected line in bounded preview: %q", res.Preview)
	}
	if res.Metadata["observation_start"] != "2" || res.Metadata["observation_end"] != "2" {
		t.Fatalf("unexpected observation line range in metadata: %+v", res.Metadata)
	}

	// Past EOF
	_, err = readObservedContent(context.Background(), "/canonical/path.go", "path.go", strings.NewReader(data), 10, 1)
	if err == nil || !strings.Contains(err.Error(), "offset 10 past end of file") {
		t.Fatalf("expected past EOF error, got: %v", err)
	}

	_, err = readObservedContent(context.Background(), "/canonical/empty.go", "empty.go", strings.NewReader(""), 1, 1)
	if err == nil || !strings.Contains(err.Error(), "empty.go is empty") {
		t.Fatalf("expected explicit empty-file error, got: %v", err)
	}
}

func TestSuggestTool(t *testing.T) {
	cands := []string{"bash", "read", "edit", "mcp__docs__greet", "mcp__docs__fail", "mcp__github__create_issue"}
	tests := []struct {
		name  string
		cands []string
		want  []string
	}{
		{"mcp__docs__gret", nil, []string{"mcp__docs__greet"}},
		{"mcp__doc__greet", nil, []string{"mcp__docs__greet"}},
		{"mcp__docs__greet2", nil, []string{"mcp__docs__greet"}},
		{"mcp__docs__", nil, []string{"mcp__docs__greet", "mcp__docs__fail"}},
		{"mcp__github__create_iss", nil, []string{"mcp__github__create_issue"}},
		{"completely_unrelated_xyz", nil, nil},
		{"bsh", nil, []string{"bash"}},
		{"bash", []string{"read", "edit", "lsp"}, nil},
	}
	for _, tt := range tests {
		candidates := tt.cands
		if candidates == nil {
			candidates = cands
		}
		got := SuggestTool(tt.name, candidates)
		if len(got) != len(tt.want) {
			t.Errorf("SuggestTool(%q) = %v, want %v", tt.name, got, tt.want)
			continue
		}
		for i := range got {
			if got[i] != tt.want[i] {
				t.Errorf("SuggestTool(%q) = %v, want %v", tt.name, got, tt.want)
				break
			}
		}
	}
}

func TestSuggestToolCapsAtTwo(t *testing.T) {
	cands := []string{"mcp__s__aaa", "mcp__s__aab", "mcp__s__aac"}
	if got := SuggestTool("mcp__s__aa", cands); len(got) > 2 {
		t.Errorf("got %v, want at most 2", got)
	}
}

func TestLevenshteinCap(t *testing.T) {
	if d := levenshtein("abc", "abc", 2); d != 0 {
		t.Errorf("identical = %d", d)
	}
	if d := levenshtein("abc", "abd", 2); d != 1 {
		t.Errorf("1 edit = %d", d)
	}
	if d := levenshtein("short", "a-much-longer-string", 3); d <= 3 {
		t.Errorf("should exceed cap, got %d", d)
	}
	if d := levenshtein("", "abcdef", 2); d <= 2 {
		t.Errorf("length gap beyond cap = %d", d)
	}
}

func TestExecuteSuggestsOnUnknownTool(t *testing.T) {
	docs := Tool{Def: models.NewTool("mcp__docs__greet", "", `{}`)}
	out := Execute(context.Background(), []Tool{docs}, "mcp__doc__greet", nil)
	if !strings.Contains(out, `unknown tool "mcp__doc__greet"`) || !strings.Contains(out, "did you mean mcp__docs__greet") {
		t.Errorf("got %q", out)
	}
	other := Tool{Def: models.NewTool("mcp__other__greet", "", `{}`)}
	out = Execute(context.Background(), []Tool{other}, "mcp__other__grete", nil)
	if !strings.Contains(out, "did you mean mcp__other__greet") {
		t.Errorf("current tool set was not used: %q", out)
	}
	out = Execute(context.Background(), []Tool{docs}, "nope", nil)
	if strings.Contains(out, "did you mean") {
		t.Errorf("unrelated tool should have no suggestion, got %q", out)
	}
}

func TestSandboxNetworkDeniedClassifier(t *testing.T) {
	tests := []struct {
		name   string
		output string
		want   bool
	}{
		{
			name:   "listen tcp failure",
			output: "panic: listen tcp 127.0.0.1:8080: bind: operation not permitted",
			want:   true,
		},
		{
			name:   "generic test failure",
			output: "--- FAIL: TestFoo (0.01s)\n    foo_test.go:42: expected 1, got 2\nFAIL",
			want:   false,
		},
		{
			name:   "generic permission denied on file",
			output: "cat: /etc/shadow: Permission denied",
			want:   false,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := isSandboxNetworkDenied(tc.output)
			if got != tc.want {
				t.Fatalf("isSandboxNetworkDenied(%q) = %v, want %v", tc.output, got, tc.want)
			}
		})
	}
}

// Every built-in tool's JSON schema must parse — a malformed schema
// (trailing comma, stray quote) silently corrupts the provider request
// body for ALL tools, surfacing as cryptic marshal errors deep in the
// loop. This ratchet pins parseability at the source.
func TestBuiltinToolSchemasParse(t *testing.T) {
	for _, tool := range All() {
		var v any
		if err := json.Unmarshal(tool.Def.Function.Parameters, &v); err != nil {
			t.Errorf("%s: schema does not parse: %v", tool.Def.Function.Name, err)
		}
	}
}

func TestGrepSchemaAcceptsEitherPatternForm(t *testing.T) {
	var schema struct {
		Required []string `json:"required"`
		AnyOf    []struct {
			Required []string `json:"required"`
		} `json:"anyOf"`
	}
	if err := json.Unmarshal(grepTool().Def.Function.Parameters, &schema); err != nil {
		t.Fatal(err)
	}
	if len(schema.Required) != 0 || len(schema.AnyOf) != 3 {
		t.Fatalf("grep schema requirements = %+v", schema)
	}
	got := map[string]bool{}
	for _, branch := range schema.AnyOf {
		if len(branch.Required) != 1 {
			t.Fatalf("grep schema branch = %+v", branch.Required)
		}
		got[branch.Required[0]] = true
	}
	if !got["pattern"] || !got["patterns"] || !got["cursor"] || len(got) != 3 {
		t.Fatalf("grep schema does not require pattern, patterns, or cursor: %+v", got)
	}
}

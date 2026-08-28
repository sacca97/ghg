package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/sacca97/ghg/internal/observation"
)

func TestExplorationSchemasDescribeTheBoundedToolSurface(t *testing.T) {
	want := map[string][]string{
		"bash":       {"Prefer grep", "glob", "find_files"},
		"read":       {"observation", "offset", "limit"},
		"edit":       {"observed", "exact", "edits"},
		"grep":       {"patterns", "cursor", "default 25"},
		"glob":       {"cursor", "default 25"},
		"find_files": {"fuzzy", "default 25"},
	}
	for _, tool := range All() {
		checks, ok := want[tool.Def.Function.Name]
		if !ok {
			continue
		}
		definition := tool.Def.Function.Description + "\n" + string(tool.Def.Function.Parameters)
		for _, fragment := range checks {
			if !strings.Contains(definition, fragment) {
				t.Errorf("%s definition lacks %q: %s", tool.Def.Function.Name, fragment, definition)
			}
		}
	}
}

func TestBashExplorationRedirectsAndEscapes(t *testing.T) {
	redirectCases := []struct {
		command string
		tool    string
		want    string
	}{
		{"grep -R TODO .", "grep", "dedicated `grep`"},
		{"find .", "glob", "dedicated `glob`"},
		{"ls -R", "glob", "Recursive listing"},
		{"cat internal/agent/agent.go", "read", "Inspection-only `cat`"},
		{"sed -n '1,20p' internal/agent/agent.go", "read", "Inspection-only `sed`"},
	}
	for _, tc := range redirectCases {
		t.Run(tc.command, func(t *testing.T) {
			got, ok := redirectBashSearch(tc.command)
			if !ok || got.Tool != tc.tool || !strings.Contains(got.Message, tc.want) {
				t.Fatalf("redirect = %#v, %v", got, ok)
			}
		})
	}
	for _, command := range []string{
		"rg TODO internal | head",
		"git grep TODO",
		"find . -type f -name '*.go'",
		"grep -R TODO /tmp/outside",
		"cat /tmp/outside.txt",
	} {
		if _, ok := redirectBashSearch(command); ok {
			t.Fatalf("advanced or outside command was redirected: %q", command)
		}
	}

	result := ExecuteResult(context.Background(), All(), "bash", json.RawMessage(`{"command":"find ."}`))
	if result.Metadata["bash_redirect"] != "true" || !strings.Contains(result.Preview, "was not run") {
		t.Fatalf("redirect result = %+v", result)
	}
	if got := bashPreviewLimit("rg TODO ."); got != 8<<10 {
		t.Fatalf("search preview limit = %d, want %d", got, 8<<10)
	}
	if got := bashPreviewLimit("git status"); got != 14<<10 {
		t.Fatalf("ordinary preview limit = %d, want %d", got, 14<<10)
	}
}

func TestEditDiffIsBounded(t *testing.T) {
	oldLines := make([]string, 200)
	newLines := make([]string, 200)
	for i := range oldLines {
		oldLines[i] = "old"
		newLines[i] = "new"
	}
	diff := editDiff(strings.Join(oldLines, "\n"), strings.Join(newLines, "\n"))
	if !strings.Contains(diff, "lines omitted") {
		t.Fatalf("bounded diff lacks omission marker: %q", diff)
	}
	if lines := strings.Count(diff, "\n") + 1; lines > 2*maxEditDiffLines+2 {
		t.Fatalf("diff has %d lines, expected compact output: %q", lines, diff)
	}
}

func TestObservedEditUsesExactReadBytesAndSupportsShift(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "main.go")
	if err := os.WriteFile(path, []byte("one\ntarget\nthree\n"), 0o640); err != nil {
		t.Fatal(err)
	}
	registry := observation.NewRegistry()
	ctx := WithObservationStore(context.Background(), "session-1", registry)
	read := ExecuteResult(ctx, All(), "read", json.RawMessage(`{"path":"`+path+`","offset":2,"limit":1}`))
	if read.ExitCode != 0 || read.Metadata["observation_id"] == "" {
		t.Fatalf("read = %+v", read)
	}
	id := read.Metadata["observation_id"]
	if err := os.WriteFile(path, []byte("zero\none\ntarget\nthree\n"), 0o640); err != nil {
		t.Fatal(err)
	}
	args := map[string]any{
		"mode": "observed",
		"edits": []any{map[string]any{
			"observation": id, "path": path, "start_line": 2, "end_line": 2,
			"operation": "replace", "content": "changed",
		}},
	}
	data, _ := json.Marshal(args)
	result := ExecuteResult(ctx, All(), "edit", data)
	if result.ExitCode != 0 || !strings.Contains(result.Preview, "readback:") {
		t.Fatalf("observed edit = %+v", result)
	}
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "zero\none\nchanged\nthree\n" {
		t.Fatalf("shifted edit wrote %q", got)
	}
	if mode := fileMode(t, path); mode != 0o640 {
		t.Fatalf("edit changed mode to %o", mode)
	}
}

func TestObservedEditSupportsEveryOperation(t *testing.T) {
	tests := []struct {
		name      string
		operation string
		content   string
		want      string
	}{
		{name: "replace", operation: "replace", content: "changed", want: "one\nchanged\nthree\n"},
		{name: "delete", operation: "delete", content: "", want: "one\nthree\n"},
		{name: "insert before", operation: "insert_before", content: "before", want: "one\nbefore\ntwo\nthree\n"},
		{name: "insert after", operation: "insert_after", content: "after", want: "one\ntwo\nafter\nthree\n"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dir := t.TempDir()
			path := filepath.Join(dir, "main.go")
			if err := os.WriteFile(path, []byte("one\ntwo\nthree\n"), 0o640); err != nil {
				t.Fatal(err)
			}
			ctx := WithObservationStore(context.Background(), "session-1", observation.NewRegistry())
			read := ExecuteResult(ctx, All(), "read", json.RawMessage(fmt.Sprintf(`{"path":%q,"offset":2,"limit":1}`, path)))
			if read.ExitCode != 0 {
				t.Fatalf("read = %+v", read)
			}
			args := map[string]any{
				"mode": "observed",
				"edits": []any{map[string]any{
					"observation": read.Metadata["observation_id"], "path": path,
					"start_line": 2, "end_line": 2, "operation": tt.operation, "content": tt.content,
				}},
			}
			data, _ := json.Marshal(args)
			result := ExecuteResult(ctx, All(), "edit", data)
			if result.ExitCode != 0 {
				t.Fatalf("%s edit = %+v", tt.operation, result)
			}
			got, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			if string(got) != tt.want {
				t.Fatalf("%s result = %q, want %q", tt.operation, got, tt.want)
			}
		})
	}
}

func TestObservedEditRejectsOverlappingRangesWithoutWriting(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "main.go")
	original := "one\ntwo\nthree\n"
	if err := os.WriteFile(path, []byte(original), 0o644); err != nil {
		t.Fatal(err)
	}
	ctx := WithObservationStore(context.Background(), "session-1", observation.NewRegistry())
	read := ExecuteResult(ctx, All(), "read", json.RawMessage(fmt.Sprintf(`{"path":%q}`, path)))
	args := map[string]any{
		"mode": "observed",
		"edits": []any{
			map[string]any{"observation": read.Metadata["observation_id"], "path": path, "start_line": 1, "end_line": 1, "operation": "replace", "content": "first"},
			map[string]any{"observation": read.Metadata["observation_id"], "path": path, "start_line": 1, "end_line": 1, "operation": "delete", "content": ""},
		},
	}
	data, _ := json.Marshal(args)
	result := ExecuteResult(ctx, All(), "edit", data)
	if !strings.Contains(result.Preview, "intersect") {
		t.Fatalf("overlapping edit should fail: %q", result.Preview)
	}
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != original {
		t.Fatalf("overlapping edit changed file: %q", got)
	}
}

func TestByteLimitedReadAuthorizesReturnedWholeLines(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "large.go")
	content := strings.Repeat(strings.Repeat("x", 500)+"\n", 100)
	if err := os.WriteFile(path, []byte(content), 0o640); err != nil {
		t.Fatal(err)
	}
	ctx := WithObservationStore(context.Background(), "session-1", observation.NewRegistry())
	read := ExecuteResult(ctx, All(), "read", json.RawMessage(fmt.Sprintf(`{"path":%q,"limit":1000}`, path)))
	if read.ExitCode != 0 || read.Metadata["observation_complete"] != "false" {
		t.Fatalf("read should stop at the byte ceiling: %+v", read)
	}
	id := read.Metadata["observation_id"]
	result := ExecuteResult(ctx, All(), "edit", observedReplaceArgs(id, path, 1, 1, "changed"))
	if result.ExitCode != 0 {
		t.Fatalf("returned whole line should authorize edit: %+v", result)
	}
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(string(got), "changed\n") {
		t.Fatalf("byte-limited observation edit result = %q", got[:min(len(got), 40)])
	}
}

func TestObservedEditPreservesLineEndingsAndFinalNewline(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want string
	}{
		{name: "crlf", in: "one\r\ntwo\r\nthree\r\n", want: "one\r\nchanged\r\nthree\r\n"},
		{name: "no final newline", in: "one\ntwo", want: "one\nchanged"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dir := t.TempDir()
			path := filepath.Join(dir, "main.txt")
			if err := os.WriteFile(path, []byte(tt.in), 0o644); err != nil {
				t.Fatal(err)
			}
			ctx := WithObservationStore(context.Background(), "session-1", observation.NewRegistry())
			read := ExecuteResult(ctx, All(), "read", json.RawMessage(fmt.Sprintf(`{"path":%q,"offset":2,"limit":1}`, path)))
			result := ExecuteResult(ctx, All(), "edit", observedReplaceArgs(read.Metadata["observation_id"], path, 2, 2, "changed"))
			if result.ExitCode != 0 {
				t.Fatalf("edit = %+v", result)
			}
			got, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			if string(got) != tt.want {
				t.Fatalf("line endings = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestObservedEditPermissionAndPreflightFailuresDoNotWrite(t *testing.T) {
	dir := t.TempDir()
	first := filepath.Join(dir, "a.txt")
	second := filepath.Join(dir, "b.txt")
	firstOriginal, secondOriginal := "one\n", "two\n"
	if err := os.WriteFile(first, []byte(firstOriginal), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(second, []byte(secondOriginal), 0o644); err != nil {
		t.Fatal(err)
	}
	ctx := WithObservationStore(context.Background(), "session-1", observation.NewRegistry())
	firstRead := ExecuteResult(ctx, All(), "read", json.RawMessage(fmt.Sprintf(`{"path":%q}`, first)))
	secondRead := ExecuteResult(ctx, All(), "read", json.RawMessage(fmt.Sprintf(`{"path":%q}`, second)))

	savedGate := Gate
	defer func() { Gate = savedGate }()
	Gate = func(GateRequest) (GateDecision, string) { return GateReject, "not now" }
	denied := ExecuteResult(ctx, All(), "edit", observedReplaceArgs(firstRead.Metadata["observation_id"], first, 1, 1, "changed"))
	if !strings.Contains(denied.Preview, "Permission denied") {
		t.Fatalf("permission rejection = %q", denied.Preview)
	}

	Gate = func(req GateRequest) (GateDecision, string) {
		if filepath.Base(req.Command) == "b.txt" {
			return GateReject, "second file rejected"
		}
		return GateAllowOnce, ""
	}
	args := map[string]any{
		"mode": "observed",
		"edits": []any{
			map[string]any{"observation": firstRead.Metadata["observation_id"], "path": first, "start_line": 1, "end_line": 1, "operation": "replace", "content": "changed"},
			map[string]any{"observation": secondRead.Metadata["observation_id"], "path": second, "start_line": 1, "end_line": 1, "operation": "replace", "content": "changed"},
		},
	}
	data, _ := json.Marshal(args)
	preflight := ExecuteResult(ctx, All(), "edit", data)
	if !strings.Contains(preflight.Preview, "Permission denied") {
		t.Fatalf("preflight rejection = %q", preflight.Preview)
	}
	for path, want := range map[string]string{first: firstOriginal, second: secondOriginal} {
		got, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		if string(got) != want {
			t.Fatalf("preflight wrote %s: %q", path, got)
		}
	}
}

func TestObservedEditRollsBackPartialPublication(t *testing.T) {
	dir := t.TempDir()
	first := filepath.Join(dir, "a.txt")
	second := filepath.Join(dir, "b.txt")
	if err := os.WriteFile(first, []byte("one\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(second, []byte("two\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	ctx := WithObservationStore(context.Background(), "session-1", observation.NewRegistry())
	firstRead := ExecuteResult(ctx, All(), "read", json.RawMessage(fmt.Sprintf(`{"path":%q}`, first)))
	secondRead := ExecuteResult(ctx, All(), "read", json.RawMessage(fmt.Sprintf(`{"path":%q}`, second)))
	args := map[string]any{
		"mode": "observed",
		"edits": []any{
			map[string]any{"observation": firstRead.Metadata["observation_id"], "path": first, "start_line": 1, "end_line": 1, "operation": "replace", "content": "changed one"},
			map[string]any{"observation": secondRead.Metadata["observation_id"], "path": second, "start_line": 1, "end_line": 1, "operation": "replace", "content": "changed two"},
		},
	}
	data, _ := json.Marshal(args)
	savedRename := renameEditFile
	defer func() { renameEditFile = savedRename }()
	renameEditFile = func(source, target string) error {
		if filepath.Base(target) == "b.txt" {
			return fmt.Errorf("forced publication failure")
		}
		return os.Rename(source, target)
	}
	result := ExecuteResult(ctx, All(), "edit", data)
	if !strings.Contains(result.Preview, "published files were rolled back") {
		t.Fatalf("partial publication result = %q", result.Preview)
	}
	for path, want := range map[string]string{first: "one\n", second: "two\n"} {
		got, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		if string(got) != want {
			t.Fatalf("rollback left %s as %q", path, got)
		}
	}
}

func TestObservedEditReadbackIsBounded(t *testing.T) {
	got := editReadback([]byte(strings.Repeat("x", maxEditReadbackLineBytes+1000)), []observedEdit{{start: 0}})
	if len(got) > maxEditReadbackLineBytes+16 || !strings.Contains(got, "…") {
		t.Fatalf("readback was not bounded: %d bytes %q", len(got), got)
	}
}

func TestObservedEditRejectsChangedAndAmbiguousBytes(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "main.go")
	if err := os.WriteFile(path, []byte("one\ntarget\nthree\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	registry := observation.NewRegistry()
	ctx := WithObservationStore(context.Background(), "session-1", registry)
	read := ExecuteResult(ctx, All(), "read", json.RawMessage(`{"path":"`+path+`","offset":2,"limit":1}`))
	id := read.Metadata["observation_id"]
	if err := os.WriteFile(path, []byte("one\nchanged\nthree\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	args := observedReplaceArgs(id, path, 2, 2, "new")
	result := ExecuteResult(ctx, All(), "edit", args)
	if !strings.Contains(result.Preview, "issued bytes") {
		t.Fatalf("changed bytes should fail: %q", result.Preview)
	}

	if err := os.WriteFile(path, []byte("one\ntarget\nthree\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	read = ExecuteResult(ctx, All(), "read", json.RawMessage(`{"path":"`+path+`","offset":2,"limit":1}`))
	id = read.Metadata["observation_id"]
	if err := os.WriteFile(path, []byte("zero\nother\ntarget\ntarget\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	result = ExecuteResult(ctx, All(), "edit", observedReplaceArgs(id, path, 2, 2, "new"))
	if !strings.Contains(result.Preview, "more than once") {
		t.Fatalf("ambiguous shifted bytes should fail: %q", result.Preview)
	}
}

func TestObservedEditIgnoresUnrelatedObservedLineChange(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "main.go")
	if err := os.WriteFile(path, []byte("one\ntarget\nthree\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	ctx := WithObservationStore(context.Background(), "session-1", observation.NewRegistry())
	read := ExecuteResult(ctx, All(), "read", json.RawMessage(fmt.Sprintf(`{"path":%q}`, path)))
	if read.ExitCode != 0 {
		t.Fatalf("read = %+v", read)
	}
	if err := os.WriteFile(path, []byte("one\ntarget\nchanged elsewhere\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	result := ExecuteResult(ctx, All(), "edit", observedReplaceArgs(read.Metadata["observation_id"], path, 2, 2, "updated"))
	if result.ExitCode != 0 {
		t.Fatalf("unrelated observed-line change should not reject target: %+v", result)
	}
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "one\nupdated\nchanged elsewhere\n" {
		t.Fatalf("target edit result = %q", got)
	}
}

func TestObservedEditRejectsCrossSessionObservation(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "main.go")
	if err := os.WriteFile(path, []byte("one\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	registry := observation.NewRegistry()
	firstCtx := WithObservationStore(context.Background(), "session-1", registry)
	read := ExecuteResult(firstCtx, All(), "read", json.RawMessage(fmt.Sprintf(`{"path":%q}`, path)))
	record, err := registry.Load(context.Background(), "session-1", read.Metadata["observation_id"])
	if err != nil {
		t.Fatal(err)
	}
	foreignCtx := WithObservationStore(context.Background(), "session-2", foreignObservationStore{record: record})
	result := ExecuteResult(foreignCtx, All(), "edit", observedReplaceArgs(record.ID, path, 1, 1, "changed"))
	if !strings.Contains(result.Preview, "another session") {
		t.Fatalf("cross-session observation should be rejected: %q", result.Preview)
	}
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "one\n" {
		t.Fatalf("cross-session edit changed file: %q", got)
	}
}

func TestObservedEditAppliesMultipleFilesAtomically(t *testing.T) {
	dir := t.TempDir()
	one := filepath.Join(dir, "one.txt")
	two := filepath.Join(dir, "two.txt")
	if err := os.WriteFile(one, []byte("a\nb\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(two, []byte("c\nd\n"), 0o640); err != nil {
		t.Fatal(err)
	}
	registry := observation.NewRegistry()
	ctx := WithObservationStore(context.Background(), "session-1", registry)
	first := ExecuteResult(ctx, All(), "read", json.RawMessage(`{"path":"`+one+`"}`))
	second := ExecuteResult(ctx, All(), "read", json.RawMessage(`{"path":"`+two+`"}`))
	args := map[string]any{
		"mode": "observed",
		"edits": []any{
			map[string]any{"observation": first.Metadata["observation_id"], "path": one, "start_line": 2, "end_line": 2, "operation": "delete", "content": ""},
			map[string]any{"observation": second.Metadata["observation_id"], "path": two, "start_line": 1, "end_line": 1, "operation": "insert_after", "content": "inserted"},
		},
	}
	data, _ := json.Marshal(args)
	result := ExecuteResult(ctx, All(), "edit", data)
	if result.ExitCode != 0 {
		t.Fatalf("multi-file edit = %+v", result)
	}
	oneData, _ := os.ReadFile(one)
	twoData, _ := os.ReadFile(two)
	if string(oneData) != "a\n" || string(twoData) != "c\ninserted\nd\n" {
		t.Fatalf("multi-file result = %q / %q", oneData, twoData)
	}
	if fileMode(t, one) != 0o600 || fileMode(t, two) != 0o640 {
		t.Fatal("multi-file edit did not preserve modes")
	}
}

func observedReplaceArgs(id, path string, start, end int, content string) []byte {
	args := map[string]any{
		"mode": "observed",
		"edits": []any{map[string]any{
			"observation": id, "path": path, "start_line": start, "end_line": end,
			"operation": "replace", "content": content,
		}},
	}
	data, _ := json.Marshal(args)
	return data
}

func fileMode(t *testing.T, path string) os.FileMode {
	t.Helper()
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	return info.Mode().Perm()
}

type foreignObservationStore struct {
	record observation.Record
}

func (s foreignObservationStore) Save(context.Context, string, observation.Record) error {
	return nil
}

func (s foreignObservationStore) Load(context.Context, string, string) (observation.Record, error) {
	return s.record, nil
}

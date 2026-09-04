package tools

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/sacca97/ghg/internal/observation"
	"github.com/sacca97/ghg/internal/sandbox"
	"github.com/sacca97/ghg/internal/search"
)

func TestStructuralSearchPaginatesAndAuthorizesVisibleEdit(t *testing.T) {
	dir := t.TempDir()
	file := filepath.Join(dir, "first.go")
	secondFile := filepath.Join(dir, "second.go")
	if err := os.WriteFile(file, []byte("package p\n\nfunc first() { return 1 }\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(secondFile, []byte("package p\n\nfunc second() { return 2 }\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	ctx := WithObservationStore(context.Background(), "session-1", observation.NewRegistry())
	ctx = WithSearchStore(ctx, "session-1", search.NewRegistry())
	args := func(extra map[string]any) []byte {
		values := map[string]any{
			"patterns":    []string{"func $NAME() { $$$BODY }"},
			"language":    "go",
			"path":        dir,
			"max_results": 1,
			"observe":     true,
		}
		for key, value := range extra {
			values[key] = value
		}
		data, err := json.Marshal(values)
		if err != nil {
			t.Fatal(err)
		}
		return data
	}

	first := ExecuteResult(ctx, All(), "structural_search", args(nil))
	if first.ExitCode != 0 || !strings.Contains(first.Preview, "structural_search: showing 1/2") {
		t.Fatalf("first structural page = %+v", first)
	}
	observationID := structuralObservationID(first.Preview)
	if observationID == "" {
		t.Fatalf("visible structural result lacks an observation id: %q", first.Preview)
	}
	editArgs, err := json.Marshal(map[string]any{
		"mode": "observed",
		"edits": []any{map[string]any{
			"observation": observationID,
			"path":        file,
			"start_line":  3,
			"end_line":    3,
			"operation":   "replace",
			"content":     "func changed() { return 1 }",
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	edited := ExecuteResult(ctx, All(), "edit", editArgs)
	if edited.ExitCode != 0 {
		t.Fatalf("observed edit = %+v", edited)
	}
	data, err := os.ReadFile(file)
	if err != nil || !strings.Contains(string(data), "func changed()") {
		t.Fatalf("edited source = %q, err=%v", data, err)
	}

	if err := os.Remove(file); err != nil {
		t.Fatal(err)
	}
	next := ExecuteResult(ctx, All(), "structural_search", args(map[string]any{
		"cursor":  first.Metadata["search_cursor"],
		"observe": false,
	}))
	if next.ExitCode != 0 || !strings.Contains(next.Preview, "func second()") {
		t.Fatalf("cursor page after source removal = %+v", next)
	}
}

func TestStructuralSearchDoesNotIssueStaleObservation(t *testing.T) {
	dir := t.TempDir()
	file := filepath.Join(dir, "sample.go")
	if err := os.WriteFile(file, []byte("package p\n\nfunc first() { return 1 }\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	ctx := WithObservationStore(context.Background(), "session-1", observation.NewRegistry())
	ctx = WithSearchStore(ctx, "session-1", search.NewRegistry())
	args := map[string]any{
		"patterns":    []string{"func $NAME() { $$$BODY }"},
		"language":    "go",
		"path":        dir,
		"max_results": 1,
	}
	data, _ := json.Marshal(args)
	result := ExecuteResult(ctx, All(), "structural_search", data)
	if result.ExitCode != 0 {
		t.Fatalf("structural search = %+v", result)
	}
	if err := os.WriteFile(file, []byte("package p\n\nfunc changed() { return 1 }\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	args["cursor"] = "structural_search/" + result.Metadata["search_id"] + "/0"
	args["observe"] = true
	data, _ = json.Marshal(args)
	stale := ExecuteResult(ctx, All(), "structural_search", data)
	if stale.ExitCode != 0 || strings.Contains(stale.Preview, "[observation ") {
		t.Fatalf("stale structural result issued observation: %+v", stale)
	}
	restrictedDir := t.TempDir()
	policy, err := sandbox.NewPolicy(sandbox.PolicyConfig{Workspace: restrictedDir, Mode: sandbox.ModeWorkspaceWrite})
	if err != nil {
		t.Fatal(err)
	}
	runtime, err := NewToolRuntime(policy, ApprovalNever, true)
	if err != nil {
		t.Fatal(err)
	}
	unauthorized := ExecuteResult(WithRuntime(ctx, runtime), All(), "structural_search", data)
	if unauthorized.ExitCode != 0 || strings.Contains(unauthorized.Preview, "[observation ") {
		t.Fatalf("unauthorized structural result issued observation: %+v", unauthorized)
	}
}

func structuralObservationID(preview string) string {
	const marker = "[observation "
	start := strings.Index(preview, marker)
	if start < 0 {
		return ""
	}
	rest := preview[start+len(marker):]
	if end := strings.IndexByte(rest, ']'); end >= 0 {
		rest = rest[:end]
	}
	return strings.TrimSpace(strings.SplitN(rest, " ", 2)[0])
}

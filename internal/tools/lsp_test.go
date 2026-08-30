package tools

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// stubLSP records calls and returns a canned diagnostics block.
type stubLSP struct {
	calls []string
	block string
}

func (s *stubLSP) WaitDiagnostics(ctx context.Context, path string) string {
	s.calls = append(s.calls, path)
	return s.block
}

func (*stubLSP) Warm(context.Context, string) {}
func (*stubLSP) Navigate(context.Context, NavigationRequest) (NavigationResult, error) {
	return NavigationResult{}, errors.New("not implemented")
}
func (*stubLSP) PreviewRename(context.Context, RenameRequest) (RenamePreview, error) {
	return RenamePreview{}, errors.New("not implemented")
}
func (*stubLSP) LookupRename(context.Context, string, string) (RenamePlan, error) {
	return RenamePlan{}, errors.New("not implemented")
}
func (*stubLSP) ValidateRename(context.Context, RenamePlan) error {
	return errors.New("not implemented")
}
func (*stubLSP) ConsumeRename(context.Context, string, string) error { return nil }

func TestWriteEditAppendLSPDiagnostics(t *testing.T) {
	stub := &stubLSP{block: "\n\n<diagnostics file=\"x.go\">\nERROR [1:1] boom\n</diagnostics>"}
	ctx := WithRuntime(context.Background(), &ToolRuntime{LanguageService: stub})

	dir := t.TempDir()
	p := filepath.Join(dir, "x.go")

	args, _ := json.Marshal(map[string]any{"path": p, "content": "package main\n"})
	out := Execute(ctx, All(), "write", args)
	if !strings.Contains(out, "<diagnostics") || !strings.Contains(out, "ERROR [1:1] boom") {
		t.Fatalf("write output missing diagnostics: %q", out)
	}
	if len(stub.calls) != 1 || stub.calls[0] != p {
		t.Fatalf("hook calls: %v", stub.calls)
	}

	args, _ = json.Marshal(map[string]any{"mode": "exact", "path": p, "old_string": "main", "new_string": "main2"})
	out = Execute(ctx, All(), "edit", args)
	if !strings.Contains(out, "<diagnostics") {
		t.Fatalf("edit output missing diagnostics: %q", out)
	}
	if len(stub.calls) != 2 {
		t.Fatalf("hook calls: %v", stub.calls)
	}
}

func TestLSPNilUnchangedOutput(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "x.go")
	args, _ := json.Marshal(map[string]any{"path": p, "content": "hi"})
	out := Execute(context.Background(), All(), "write", args)
	if strings.Contains(out, "<diagnostics") {
		t.Fatalf("nil hook must not alter output: %q", out)
	}
}

func TestLSPFailureNeverFailsTool(t *testing.T) {
	ctx := WithRuntime(context.Background(), &ToolRuntime{LanguageService: &stubLSP{block: ""}}) // server slow/absent: empty block
	dir := t.TempDir()
	p := filepath.Join(dir, "x.go")
	args, _ := json.Marshal(map[string]any{"path": p, "content": "hi"})
	out := Execute(ctx, All(), "write", args)
	if !strings.HasPrefix(out, "Wrote") {
		t.Fatalf("tool result should be the success message, got %q", out)
	}
	if _, err := os.Stat(p); err != nil {
		t.Fatal("write still happened")
	}
}

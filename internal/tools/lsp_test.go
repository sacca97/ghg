package tools

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/sacca97/ghg/internal/observation"
)

// stubLSP records calls and returns a canned diagnostics block.
type stubLSP struct {
	calls         []string
	block         string
	navigateCalls []NavigationRequest
	navigate      func(NavigationRequest) (NavigationResult, error)
}

func (s *stubLSP) WaitDiagnostics(ctx context.Context, path string) string {
	s.calls = append(s.calls, path)
	return s.block
}

func (*stubLSP) Warm(context.Context, string) {}
func (s *stubLSP) Navigate(_ context.Context, request NavigationRequest) (NavigationResult, error) {
	s.navigateCalls = append(s.navigateCalls, request)
	if s.navigate != nil {
		return s.navigate(request)
	}
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

func TestLSPCompositeSymbolOperations(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "main.go")
	source := "package p\n\nfunc target() {\n\treturn\n}\n\nfunc caller() {\n\ttarget()\n}\n"
	if err := os.WriteFile(path, []byte(source), 0o644); err != nil {
		t.Fatal(err)
	}
	symbol := LSPSymbol{
		Name: "target", Kind: "function", Path: path,
		Range:          LSPRange{Start: LSPPosition{Line: 3, Column: 1}, End: LSPPosition{Line: 5, Column: 2}},
		SelectionRange: LSPRange{Start: LSPPosition{Line: 3, Column: 6}, End: LSPPosition{Line: 3, Column: 12}},
	}
	stub := &stubLSP{navigate: func(request NavigationRequest) (NavigationResult, error) {
		switch request.Operation {
		case "document_symbol":
			return NavigationResult{Operation: request.Operation, Symbols: []LSPSymbol{symbol}}, nil
		case "references":
			return NavigationResult{Operation: request.Operation, Locations: []LSPLocation{{Path: path, Range: symbol.SelectionRange}}}, nil
		default:
			return NavigationResult{}, errors.New("unexpected navigation operation")
		}
	}}
	registry := observation.NewRegistry()
	ctx := WithObservationStore(WithRuntime(context.Background(), &ToolRuntime{LanguageService: stub}), "session-1", registry)

	references := ExecuteResult(ctx, All(), "lsp", json.RawMessage(`{"operation":"symbol_references","path":"`+path+`","symbol":"target","include_declaration":true}`))
	if references.ExitCode != 0 || !strings.Contains(references.Preview, "references:") {
		t.Fatalf("symbol references = %+v", references)
	}
	if len(stub.navigateCalls) != 2 || stub.navigateCalls[1].Line != 3 || stub.navigateCalls[1].Column != 6 || !stub.navigateCalls[1].IncludeDeclaration {
		t.Fatalf("navigation calls = %+v", stub.navigateCalls)
	}

	contextResult := ExecuteResult(ctx, All(), "lsp", json.RawMessage(`{"operation":"symbol_context","path":"`+path+`","symbol":"target"}`))
	if contextResult.ExitCode != 0 || contextResult.Metadata["observation_id"] == "" || !strings.Contains(contextResult.Preview, "func target()") {
		t.Fatalf("symbol context = %+v", contextResult)
	}
	record, err := registry.Load(context.Background(), "session-1", contextResult.Metadata["observation_id"])
	if err != nil {
		t.Fatal(err)
	}
	if record.StartLine != 3 || record.EndLine != 5 || record.Content != "func target() {\n\treturn\n}\n" {
		t.Fatalf("context observation = %+v", record)
	}
}

func TestLSPCompositeSymbolRequiresPositionDisambiguation(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "main.go")
	if err := os.WriteFile(path, []byte("package p\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	symbols := []LSPSymbol{
		{Name: "target", Kind: "function", Path: path, Range: LSPRange{Start: LSPPosition{Line: 2, Column: 1}, End: LSPPosition{Line: 2, Column: 10}}},
		{Name: "target", Kind: "method", Path: path, Range: LSPRange{Start: LSPPosition{Line: 5, Column: 1}, End: LSPPosition{Line: 5, Column: 10}}},
	}
	stub := &stubLSP{navigate: func(request NavigationRequest) (NavigationResult, error) {
		if request.Operation != "document_symbol" {
			return NavigationResult{}, errors.New("ambiguous symbol must not navigate by position")
		}
		return NavigationResult{Operation: request.Operation, Symbols: symbols}, nil
	}}
	ctx := WithRuntime(context.Background(), &ToolRuntime{LanguageService: stub})
	result := ExecuteResult(ctx, All(), "lsp", json.RawMessage(`{"operation":"symbol_references","path":"`+path+`","symbol":"target"}`))
	if result.ExitCode != 0 || !strings.Contains(result.Preview, "ambiguous") || !strings.Contains(result.Preview, "function") || !strings.Contains(result.Preview, "method") {
		t.Fatalf("ambiguous symbol = %+v", result)
	}
	if len(stub.navigateCalls) != 1 {
		t.Fatalf("ambiguous navigation calls = %+v", stub.navigateCalls)
	}
}

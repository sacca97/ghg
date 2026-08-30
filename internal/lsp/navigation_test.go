package lsp

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/sacca97/ghg/internal/tools"
)

func TestNavigationNormalizesUTF16AndAllReadOnlyOperations(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "main.go")
	writeFile(t, path, "package p\nvar café = 1\nvar face = café\n")
	onRequest := func(method string, _ json.RawMessage) json.RawMessage {
		uri := fileURI(path)
		switch method {
		case "textDocument/definition":
			return json.RawMessage(`[{"targetUri":"` + uri + `","targetRange":{"start":{"line":1,"character":4},"end":{"line":1,"character":8}},"targetSelectionRange":{"start":{"line":1,"character":4},"end":{"line":1,"character":8}}}]`)
		case "textDocument/references":
			return json.RawMessage(`[{"uri":"` + uri + `","range":{"start":{"line":1,"character":4},"end":{"line":1,"character":8}}},{"uri":"` + uri + `","range":{"start":{"line":2,"character":11},"end":{"line":2,"character":15}}},{"uri":"` + uri + `","range":{"start":{"line":1,"character":4},"end":{"line":1,"character":8}}}]`)
		case "textDocument/documentSymbol":
			return json.RawMessage(`[{"name":"café","kind":13,"range":{"start":{"line":1,"character":4},"end":{"line":1,"character":8}},"selectionRange":{"start":{"line":1,"character":4},"end":{"line":1,"character":8}},"children":[{"name":"child","kind":6,"range":{"start":{"line":2,"character":4},"end":{"line":2,"character":8}},"selectionRange":{"start":{"line":2,"character":4},"end":{"line":2,"character":8}}}]}]`)
		case "textDocument/hover":
			return json.RawMessage(`{"contents":{"language":"go","value":"var café int"},"range":{"start":{"line":1,"character":4},"end":{"line":1,"character":8}}}`)
		default:
			return nil
		}
	}
	f := startFakeServerWithRequest(t, nil, onRequest)
	m := pipeManager(f)
	defer m.Close()

	for _, operation := range []string{"definition", "references", "document_symbol", "hover"} {
		result, err := m.Navigate(context.Background(), tools.NavigationRequest{
			Operation: operation, Path: path, Line: 2, Column: 5,
			IncludeDeclaration: operation == "references",
		})
		if err != nil {
			t.Fatalf("%s: %v", operation, err)
		}
		if result.Operation != operation {
			t.Fatalf("operation = %q, want %q", result.Operation, operation)
		}
		switch operation {
		case "definition":
			if len(result.Locations) != 1 || result.Locations[0].Range.Start.Column != 5 {
				t.Fatalf("definition normalization: %+v", result.Locations)
			}
		case "references":
			if len(result.Locations) != 2 || result.Omitted != 0 {
				t.Fatalf("reference normalization: %+v omitted=%d", result.Locations, result.Omitted)
			}
		case "document_symbol":
			if len(result.Symbols) != 2 || result.Symbols[0].Name != "café" {
				t.Fatalf("symbol flattening: %+v", result.Symbols)
			}
		case "hover":
			if !strings.Contains(result.Hover, "var café int") || result.HoverRange == nil {
				t.Fatalf("hover normalization: %+v", result)
			}
		}
	}
}

func TestRenamePreviewApplyIsSessionBoundAndExact(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "main.go")
	writeFile(t, path, "package p\nvar café = 1\n")
	onRequest := func(method string, _ json.RawMessage) json.RawMessage {
		if method != "textDocument/rename" {
			return nil
		}
		return json.RawMessage(`{"changes":{"` + fileURI(path) + `":[{"range":{"start":{"line":1,"character":4},"end":{"line":1,"character":8}},"newText":"name"}]}}`)
	}
	f := startFakeServerWithRequest(t, nil, onRequest)
	m := pipeManager(f)
	defer m.Close()
	if err := m.SetWorkspace(dir); err != nil {
		t.Fatal(err)
	}

	preview, err := m.PreviewRename(context.Background(), tools.RenameRequest{SessionID: "session-1", Path: path, Line: 2, Column: 5, NewName: "name"})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(preview.ID, "rn_") || len(preview.Files) != 1 {
		t.Fatalf("preview = %+v", preview)
	}
	if _, err := m.LookupRename(context.Background(), "other-session", preview.ID); err == nil {
		t.Fatal("foreign session inspected rename preview")
	}

	ctx := tools.WithSessionID(tools.WithRuntime(context.Background(), &tools.ToolRuntime{LanguageService: m}), "session-1")
	result := tools.ExecuteResult(ctx, tools.All(), "lsp_rename", json.RawMessage(`{"operation":"apply","rename_id":"`+preview.ID+`"}`))
	if result.ExitCode != 0 || strings.HasPrefix(result.Preview, "Error:") {
		t.Fatalf("apply result = %+v", result)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != "package p\nvar name = 1\n" {
		t.Fatalf("content after rename = %q", data)
	}
	if _, err := m.LookupRename(context.Background(), "session-1", preview.ID); err == nil {
		t.Fatal("successful rename preview was not consumed")
	}
}

func TestLSPRejectsUTF16SurrogateSplitAndNonFileLocation(t *testing.T) {
	if _, err := utf16PositionToModel([]byte("😀"), wirePosition{Line: 0, Character: 1}); err == nil {
		t.Fatal("surrogate split was accepted")
	}
	dir := t.TempDir()
	path := filepath.Join(dir, "main.go")
	writeFile(t, path, "package p\n")
	f := startFakeServerWithRequest(t, nil, func(method string, _ json.RawMessage) json.RawMessage {
		if method == "textDocument/definition" {
			return json.RawMessage(`[{"uri":"https://example.invalid/main.go","range":{"start":{"line":0,"character":0},"end":{"line":0,"character":1}}}]`)
		}
		return nil
	})
	m := pipeManager(f)
	defer m.Close()
	if _, err := m.Navigate(context.Background(), tools.NavigationRequest{Operation: "definition", Path: path, Line: 1, Column: 1}); err == nil {
		t.Fatal("non-file location was accepted")
	}
}

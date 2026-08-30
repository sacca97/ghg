package tools

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"

	"github.com/sacca97/ghg/internal/llm"
	"github.com/sacca97/ghg/internal/sandbox"
)

// LanguageService is the per-run language-server boundary. Keeping the
// protocol-facing implementation in internal/lsp and these small value types
// here avoids a tools <-> lsp import cycle and makes the tool surface easy to
// stub in focused tests.
type LanguageService interface {
	WaitDiagnostics(context.Context, string) string
	Warm(context.Context, string)
	Navigate(context.Context, NavigationRequest) (NavigationResult, error)
	PreviewRename(context.Context, RenameRequest) (RenamePreview, error)
	LookupRename(context.Context, string, string) (RenamePlan, error)
	ValidateRename(context.Context, RenamePlan) error
	ConsumeRename(context.Context, string, string) error
}

type LSPPosition struct {
	Line   int `json:"line"`
	Column int `json:"column"`
}

type LSPRange struct {
	Start LSPPosition `json:"start"`
	End   LSPPosition `json:"end"`
}

type LSPLocation struct {
	Path  string   `json:"path"`
	Range LSPRange `json:"range"`
}

type LSPSymbol struct {
	Name           string   `json:"name"`
	Kind           string   `json:"kind,omitempty"`
	Path           string   `json:"path"`
	Range          LSPRange `json:"range"`
	SelectionRange LSPRange `json:"selection_range"`
}

type NavigationRequest struct {
	Operation          string
	Path               string
	Line               int
	Column             int
	IncludeDeclaration bool
}

type NavigationResult struct {
	Operation  string
	Locations  []LSPLocation
	Symbols    []LSPSymbol
	Hover      string
	HoverRange *LSPRange
	Omitted    int
}

type RenameRequest struct {
	SessionID string
	Path      string
	Line      int
	Column    int
	NewName   string
}

type RenameFilePreview struct {
	Path string
	Diff string
}

type RenamePreview struct {
	ID      string
	Files   []RenameFilePreview
	Omitted int
}

// RenameFile contains the exact bytes authorized by a preview. It is never
// serialized to the model; RenamePlan is an in-memory session-bound value.
type RenameFile struct {
	Path     string
	Original []byte
	Updated  []byte
	Mode     os.FileMode
	Version  int
}

type RenamePlan struct {
	ID        string
	SessionID string
	Files     []RenameFile
}

type languageSessionKey struct{}

// WithSessionID binds a tool call to its owning conversation. Normal sessions
// carry a non-empty value; a --no-session headless run may use the manager's
// process-local empty scope.
func WithSessionID(ctx context.Context, sessionID string) context.Context {
	if sessionID == "" {
		return ctx
	}
	return context.WithValue(ctx, languageSessionKey{}, sessionID)
}

func SessionIDFromContext(ctx context.Context) string {
	if ctx == nil {
		return ""
	}
	id, _ := ctx.Value(languageSessionKey{}).(string)
	return id
}

func lspTool() Tool {
	return resultTool(llm.NewTool("lsp", "Use the language server for bounded definitions, references, document symbols, or hover information. Paths and positions are workspace-authorized; columns are one-based Unicode-rune columns.", `{"type":"object","properties":{"operation":{"type":"string","enum":["definition","references","document_symbol","hover"]},"path":{"type":"string"},"line":{"type":"integer","description":"One-based line; required except document_symbol"},"column":{"type":"integer","description":"One-based Unicode-rune column; required except document_symbol"},"include_declaration":{"type":"boolean","description":"References only"}},"required":["operation","path"]}`), runLSPResult)
}

func lspRenameTool() Tool {
	return resultTool(llm.NewTool("lsp_rename", "Preview or apply a safe, session-bound language-server rename. Preview before apply; apply accepts only the exact stored preview.", `{"type":"object","properties":{"operation":{"type":"string","enum":["preview","apply"]},"path":{"type":"string"},"line":{"type":"integer"},"column":{"type":"integer"},"new_name":{"type":"string"},"rename_id":{"type":"string"}},"required":["operation"]}`), runLSPRenameResult)
}

func runLSPResult(ctx context.Context, args json.RawMessage) (ToolResult, error) {
	var request struct {
		Operation          string `json:"operation"`
		Path               string `json:"path"`
		Line               int    `json:"line"`
		Column             int    `json:"column"`
		IncludeDeclaration bool   `json:"include_declaration"`
	}
	if err := json.Unmarshal(args, &request); err != nil {
		return ToolResult{}, err
	}
	if strings.TrimSpace(request.Path) == "" {
		return ToolResult{}, errors.New("lsp requires path")
	}
	operation := strings.ToLower(strings.TrimSpace(request.Operation))
	switch operation {
	case "definition", "references", "hover":
		if request.Line <= 0 || request.Column <= 0 {
			return ToolResult{}, fmt.Errorf("lsp %s requires one-based line and column", operation)
		}
	case "document_symbol":
		if request.IncludeDeclaration {
			return ToolResult{}, errors.New("include_declaration applies only to references")
		}
	default:
		return ToolResult{}, fmt.Errorf("unsupported lsp operation %q", request.Operation)
	}
	if operation != "references" && request.IncludeDeclaration {
		return ToolResult{}, errors.New("include_declaration applies only to references")
	}
	runtime := RuntimeFromContext(ctx)
	if runtime == nil || runtime.LanguageService == nil {
		return ToolResult{}, errors.New("lsp is unavailable")
	}
	result, err := runtime.LanguageService.Navigate(ctx, NavigationRequest{
		Operation: operation, Path: request.Path, Line: request.Line, Column: request.Column,
		IncludeDeclaration: request.IncludeDeclaration,
	})
	if err != nil {
		return ToolResult{}, err
	}
	raw := renderNavigation(result)
	return MarkUntrusted(textResult(raw, Truncate(raw), 0), "lsp"), nil
}

func runLSPRenameResult(ctx context.Context, args json.RawMessage) (ToolResult, error) {
	var request struct {
		Operation string `json:"operation"`
		Path      string `json:"path"`
		Line      int    `json:"line"`
		Column    int    `json:"column"`
		NewName   string `json:"new_name"`
		RenameID  string `json:"rename_id"`
	}
	if err := json.Unmarshal(args, &request); err != nil {
		return ToolResult{}, err
	}
	runtime := RuntimeFromContext(ctx)
	if runtime == nil || runtime.LanguageService == nil {
		return ToolResult{}, errors.New("lsp rename is unavailable")
	}
	operation := strings.ToLower(strings.TrimSpace(request.Operation))
	sessionID := SessionIDFromContext(ctx)
	switch operation {
	case "preview":
		if strings.TrimSpace(request.Path) == "" || request.Line <= 0 || request.Column <= 0 || request.NewName == "" {
			return ToolResult{}, errors.New("rename preview requires path, one-based line and column, and new_name")
		}
		preview, err := runtime.LanguageService.PreviewRename(ctx, RenameRequest{
			SessionID: sessionID, Path: request.Path, Line: request.Line,
			Column: request.Column, NewName: request.NewName,
		})
		if err != nil {
			return ToolResult{}, err
		}
		raw := renderRenamePreview(preview)
		return MarkUntrusted(textResult(raw, Truncate(raw), 0), "lsp_rename"), nil
	case "apply":
		if strings.TrimSpace(request.RenameID) == "" {
			return ToolResult{}, errors.New("rename apply requires rename_id from a preview")
		}
		plan, err := runtime.LanguageService.LookupRename(ctx, sessionID, request.RenameID)
		if err != nil {
			return ToolResult{}, err
		}
		if err := authorizeRenamePlan(ctx, plan); err != nil {
			return ToolResult{}, err
		}
		if err := runtime.LanguageService.ValidateRename(ctx, plan); err != nil {
			return ToolResult{}, err
		}
		result, err := applyRenamePlan(ctx, runtime, plan)
		if err != nil {
			return ToolResult{}, err
		}
		if err := runtime.LanguageService.ConsumeRename(ctx, sessionID, request.RenameID); err != nil {
			return ToolResult{}, err
		}
		return MarkUntrusted(textResult(result, Truncate(result), 0), "lsp_rename"), nil
	default:
		return ToolResult{}, fmt.Errorf("unsupported lsp_rename operation %q", request.Operation)
	}
}

func authorizeRenamePlan(ctx context.Context, plan RenamePlan) error {
	if len(plan.Files) == 0 {
		return errors.New("rename preview contains no files")
	}
	for _, file := range plan.Files {
		if file.Path == "" {
			return errors.New("rename preview contains an empty path")
		}
		if _, err := AuthorizePath(ctx, file.Path, sandbox.AccessWrite, false); err != nil {
			return err
		}
		if deny := checkGate("lsp_rename", file.Path); deny != "" {
			return errors.New(deny)
		}
	}
	return nil
}

func renderNavigation(result NavigationResult) string {
	var b strings.Builder
	switch result.Operation {
	case "hover":
		b.WriteString("hover:\n")
		if result.Hover == "" {
			b.WriteString("(no hover information)\n")
		} else {
			b.WriteString(result.Hover)
			if !strings.HasSuffix(result.Hover, "\n") {
				b.WriteByte('\n')
			}
		}
	case "document_symbol":
		b.WriteString("symbols:\n")
		for _, symbol := range result.Symbols {
			fmt.Fprintf(&b, "- %s", symbol.Name)
			if symbol.Kind != "" {
				fmt.Fprintf(&b, " (%s)", symbol.Kind)
			}
			fmt.Fprintf(&b, " %s [%s]\n", symbol.Path, formatLSPRange(symbol.Range))
		}
		if len(result.Symbols) == 0 {
			b.WriteString("(no symbols)\n")
		}
	default:
		label := result.Operation
		if label == "" {
			label = "locations"
		}
		b.WriteString(label + ":\n")
		for _, location := range result.Locations {
			fmt.Fprintf(&b, "- %s [%s]\n", location.Path, formatLSPRange(location.Range))
		}
		if len(result.Locations) == 0 {
			b.WriteString("(no locations)\n")
		}
	}
	if result.Omitted > 0 {
		fmt.Fprintf(&b, "... [%d entries omitted]\n", result.Omitted)
	}
	return strings.TrimSuffix(b.String(), "\n")
}

func renderRenamePreview(preview RenamePreview) string {
	var b strings.Builder
	fmt.Fprintf(&b, "rename preview %s:\n", preview.ID)
	for _, file := range preview.Files {
		fmt.Fprintf(&b, "--- %s\n", file.Path)
		if file.Diff != "" {
			b.WriteString(file.Diff)
			if !strings.HasSuffix(file.Diff, "\n") {
				b.WriteByte('\n')
			}
		}
	}
	if len(preview.Files) == 0 {
		b.WriteString("(no changes)\n")
	}
	if preview.Omitted > 0 {
		fmt.Fprintf(&b, "... [%d files omitted]\n", preview.Omitted)
	}
	return strings.TrimSuffix(b.String(), "\n")
}

func formatLSPRange(r LSPRange) string {
	return fmt.Sprintf("%d:%d-%d:%d", r.Start.Line, r.Start.Column, r.End.Line, r.End.Column)
}

package tools

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/sacca97/ghg/internal/models"
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

func lspTool() Tool {
	return resultTool(models.NewTool("lsp", "Use the language server for bounded definitions, references, document symbols, hover information, or exact-symbol references/context. Paths and positions are workspace-authorized; columns are one-based Unicode-rune columns.", `{"type":"object","properties":{"operation":{"type":"string","enum":["definition","references","document_symbol","hover","symbol_references","symbol_context"]},"path":{"type":"string"},"symbol":{"type":"string","description":"Exact symbol name; required for symbol_references and symbol_context"},"line":{"type":"integer","description":"One-based line; required for position-based operations"},"column":{"type":"integer","description":"One-based Unicode-rune column; required for position-based operations"},"include_declaration":{"type":"boolean","description":"References and symbol_references only"}},"required":["operation","path"]}`), runLSPResult)
}

func runLSPResult(ctx context.Context, args json.RawMessage) (ToolResult, error) {
	var request struct {
		Operation          string `json:"operation"`
		Path               string `json:"path"`
		Symbol             string `json:"symbol"`
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
	case "symbol_references", "symbol_context":
		if strings.TrimSpace(request.Symbol) == "" {
			return ToolResult{}, fmt.Errorf("lsp %s requires symbol", operation)
		}
	default:
		return ToolResult{}, fmt.Errorf("unsupported lsp operation %q", request.Operation)
	}
	if operation != "references" && operation != "symbol_references" && request.IncludeDeclaration {
		return ToolResult{}, errors.New("include_declaration applies only to references")
	}
	runtime := RuntimeFromContext(ctx)
	if runtime == nil || runtime.LanguageService == nil {
		return ToolResult{}, errors.New("lsp is unavailable")
	}
	if operation == "symbol_references" || operation == "symbol_context" {
		return runSymbolLSPResult(ctx, runtime.LanguageService, operation, request.Path, request.Symbol, request.IncludeDeclaration)
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

const maxSymbolCandidates = 20

func runSymbolLSPResult(ctx context.Context, service LanguageService, operation, path, name string, includeDeclaration bool) (ToolResult, error) {
	authorizedPath, err := AuthorizePath(ctx, path, sandbox.AccessRead, false)
	if err != nil {
		return ToolResult{}, err
	}
	symbols, err := service.Navigate(ctx, NavigationRequest{Operation: "document_symbol", Path: authorizedPath})
	if err != nil {
		return ToolResult{}, err
	}
	exact := make([]LSPSymbol, 0, len(symbols.Symbols))
	for _, symbol := range symbols.Symbols {
		if symbol.Name == strings.TrimSpace(name) {
			exact = append(exact, symbol)
		}
	}
	if len(exact) == 0 {
		raw := fmt.Sprintf("symbol %q not found in %s", strings.TrimSpace(name), authorizedPath)
		if symbols.Omitted > 0 {
			raw += fmt.Sprintf(" (%d other symbols omitted)", symbols.Omitted)
		}
		return MarkUntrusted(textResult(raw, Truncate(raw), 0), "lsp"), nil
	}
	if len(exact) > 1 {
		raw := renderAmbiguousSymbols(strings.TrimSpace(name), exact)
		return MarkUntrusted(textResult(raw, Truncate(raw), 0), "lsp"), nil
	}
	symbol := exact[0]
	if operation == "symbol_context" {
		result, err := runObservedRead(ctx, struct {
			Path   string `json:"path"`
			Offset int    `json:"offset"`
			Limit  int    `json:"limit"`
		}{
			Path:   symbol.Path,
			Offset: symbol.Range.Start.Line,
			Limit:  symbol.Range.End.Line - symbol.Range.Start.Line + 1,
		})
		if err != nil {
			return ToolResult{}, err
		}
		return MarkUntrusted(result, "lsp"), nil
	}
	references, err := service.Navigate(ctx, NavigationRequest{
		Operation:          "references",
		Path:               symbol.Path,
		Line:               symbol.SelectionRange.Start.Line,
		Column:             symbol.SelectionRange.Start.Column,
		IncludeDeclaration: includeDeclaration,
	})
	if err != nil {
		return ToolResult{}, err
	}
	raw := renderNavigation(references)
	return MarkUntrusted(textResult(raw, Truncate(raw), 0), "lsp"), nil
}

func renderAmbiguousSymbols(name string, symbols []LSPSymbol) string {
	var b strings.Builder
	fmt.Fprintf(&b, "symbol %q is ambiguous; use a position-based lsp operation:\n", name)
	limit := min(len(symbols), maxSymbolCandidates)
	for _, symbol := range symbols[:limit] {
		fmt.Fprintf(&b, "- %s", symbol.Name)
		if symbol.Kind != "" {
			fmt.Fprintf(&b, " (%s)", symbol.Kind)
		}
		fmt.Fprintf(&b, " %s [%s]\n", symbol.Path, formatLSPRange(symbol.Range))
	}
	if omitted := len(symbols) - limit; omitted > 0 {
		fmt.Fprintf(&b, "... [%d candidates omitted]\n", omitted)
	}
	return strings.TrimSuffix(b.String(), "\n")
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

func formatLSPRange(r LSPRange) string {
	return fmt.Sprintf("%d:%d-%d:%d", r.Start.Line, r.Start.Column, r.End.Line, r.End.Column)
}

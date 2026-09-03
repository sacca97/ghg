package tools

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/sacca97/ghg/internal/models"
	"github.com/sacca97/ghg/internal/sandbox"
)

func writeTool() Tool {
	return resultTool(models.NewTool("write",
		"Write content to a file, creating it (and parent directories) or overwriting it.",
		`{"type":"object","properties":{"path":{"type":"string","description":"Path to the file"},"content":{"type":"string","description":"Full file content"}},"required":["path","content"]}`),
		runWriteResult)
}

func runWriteResult(ctx context.Context, args json.RawMessage) (ToolResult, error) {
	var a struct {
		Path    string `json:"path"`
		Content string `json:"content"`
	}
	if err := json.Unmarshal(args, &a); err != nil {
		return ToolResult{}, err
	}
	path, err := AuthorizePath(ctx, a.Path, sandbox.AccessWrite, true)
	if err != nil {
		return ToolResult{}, err
	}
	if deny := checkGate(ctx, "write", path); deny != "" {
		return ToolResult{}, errors.New(deny)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return ToolResult{}, err
	}
	mode := os.FileMode(0o644)
	if info, statErr := os.Stat(path); statErr == nil {
		mode = info.Mode()
	} else if !os.IsNotExist(statErr) {
		return ToolResult{}, statErr
	}
	if err := atomicWriteFile(path, []byte(a.Content), mode); err != nil {
		return ToolResult{}, err
	}
	hookReports := runtimePostEditReports(ctx, []string{path})
	raw := fmt.Sprintf("Wrote %d bytes to %s", len(a.Content), path)
	if final, readErr := os.ReadFile(path); readErr == nil {
		if string(final) != a.Content {
			raw += fmt.Sprintf("\npostEdit final bytes: %d", len(final))
		}
		raw += lspDiagnostics(ctx, path)
	} else if os.IsNotExist(readErr) {
		raw += "\npostEdit removed the file"
	}
	for _, report := range hookReports {
		raw += "\n\n" + report.note(RuntimeFromContext(ctx))
	}
	return textResult(raw, raw, 0), nil
}

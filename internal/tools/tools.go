// Package tools implements the agent's built-in tools.
package tools

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/sacca97/ghg/internal/models"
	"github.com/sacca97/ghg/internal/tools/bashrun"
)

// Tool is a named executable tool with a JSON schema.
type Tool struct {
	Def       models.Tool
	Run       func(ctx context.Context, args json.RawMessage) (string, error)
	RunResult func(ctx context.Context, args json.RawMessage) (ToolResult, error)
}

func resultTool(def models.Tool, run func(context.Context, json.RawMessage) (ToolResult, error)) Tool {
	return Tool{
		Def: def,
		Run: func(ctx context.Context, args json.RawMessage) (string, error) {
			result, err := run(ctx, args)
			return result.Preview, err
		},
		RunResult: run,
	}
}

// All returns the built-in tool set.
func All() []Tool {
	return []Tool{bashTool(), readTool(), writeTool(), editTool(), grepTool(), structuralSearchTool(), globTool(), findFilesTool(), lspTool(), lspRenameTool()}
}

// CapabilityReporter lets an optional runtime service report deterministic
// preflight failures without making the tools package depend on its concrete
// implementation.
type CapabilityReporter interface {
	CapabilityStatus() (bool, []string)
}

// FilterAvailable removes tools whose deterministic runtime prerequisites are
// known to be missing before a model request is built. A nil runtime keeps
// lightweight/unit callers compatible; configured production agents always
// attach a runtime before their first turn.
func FilterAvailable(ts []Tool, runtime *ToolRuntime) ([]Tool, []string) {
	if runtime == nil {
		return ts, nil
	}

	needBash := false
	needLSP := false
	for _, tool := range ts {
		switch tool.Def.Function.Name {
		case "bash":
			needBash = true
		case "lsp", "lsp_rename":
			needLSP = true
		}
	}

	missing := make(map[string]string)
	var notices []string
	if needBash && !bashrun.Available() {
		missing["bash"] = "bash unavailable: the selected shell is not on PATH"
	}
	if needLSP {
		lspAvailable := runtime.LanguageService != nil
		lspNotices := []string(nil)
		if reporter, ok := runtime.LanguageService.(CapabilityReporter); ok {
			lspAvailable, lspNotices = reporter.CapabilityStatus()
		}
		if len(lspNotices) > 0 {
			notices = append(notices, lspNotices...)
		}
		if !lspAvailable {
			missing["lsp"] = "lsp unavailable: no configured language server is runnable"
			missing["lsp_rename"] = missing["lsp"]
			if len(lspNotices) == 0 {
				notices = append(notices, missing["lsp"])
			}
		}
	}
	if len(missing) == 0 {
		return ts, notices
	}
	out := make([]Tool, 0, len(ts)-len(missing))
	for _, tool := range ts {
		if _, unavailable := missing[tool.Def.Function.Name]; unavailable {
			continue
		}
		out = append(out, tool)
	}
	return out, notices
}

// Defs returns the models.Tool definitions for a tool set.
func Defs(ts []Tool) []models.Tool {
	defs := make([]models.Tool, len(ts))
	for i, t := range ts {
		defs[i] = t.Def
	}
	return defs
}

// Execute runs the named tool. Errors are returned as strings so they can be
// fed back to the model rather than aborting the loop.
func Execute(ctx context.Context, ts []Tool, name string, args json.RawMessage) string {
	return ExecuteResult(ctx, ts, name, args).Preview
}

// ExecuteResult runs the named tool and returns its structured result. The
// string Execute wrapper above remains the compatibility surface for MCP and
// existing callers; agent turns use this form so retained evidence is still
// available after the model preview is bounded.
func ExecuteResult(ctx context.Context, ts []Tool, name string, args json.RawMessage) ToolResult {
	name = canonicalToolName(name)
	for _, t := range ts {
		if t.Def.Function.Name == name {
			var result ToolResult
			var err error
			if t.RunResult != nil {
				result, err = t.RunResult(ctx, args)
			} else if t.Run != nil {
				var out string
				out, err = t.Run(ctx, args)
				if err == nil {
					result = textResult(out, out, 0)
				}
			} else {
				err = errors.New("tool has no implementation")
			}
			if err != nil {
				result := errorToolResult(err)
				result.Source = name
				return result
			}
			result = normalizeResult(result)
			if result.Source == "" {
				result.Source = name
			}
			if result.Metadata == nil {
				result.Metadata = map[string]string{}
			}
			if _, ok := result.Metadata["source"]; !ok {
				result.Metadata["source"] = result.Source
			}
			return result
		}
	}
	msg := fmt.Sprintf("Error: unknown tool %q", name)
	names := make([]string, len(ts))
	for i, tool := range ts {
		names[i] = tool.Def.Function.Name
	}
	if hints := SuggestTool(name, names); len(hints) > 0 {
		msg += " — did you mean " + strings.Join(hints, " or ") + "?"
	}
	result := errorToolResult(errors.New(strings.TrimPrefix(msg, "Error: ")))
	result.Source = name
	return result
}

func canonicalToolName(name string) string {
	switch name {
	case "artifact_read":
		return "output_read"
	case "artifact_list":
		return "output_list"
	default:
		return name
	}
}

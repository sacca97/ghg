// Package tools implements the agent's built-in tools.
package tools

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"sort"
	"strconv"
	"strings"

	"github.com/sacca97/ghg/internal/models"
)

// Tool is a named executable tool with a JSON schema.
type Tool struct {
	Def       models.Tool
	Run       func(ctx context.Context, args json.RawMessage) (string, error)
	RunResult func(ctx context.Context, args json.RawMessage) (ToolResult, error)
	Available func(*ToolRuntime) (bool, string)
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

func withAvailability(tool Tool, check func(*ToolRuntime) (bool, string)) Tool {
	tool.Available = check
	return tool
}

// All returns the built-in tool set.
func All() []Tool {
	return []Tool{bashTool(), readTool(), writeTool(), editTool(), grepTool(), globTool(), findFilesTool(), lspTool(), webFetchTool(), webSearchTool()}
}

// FilterAvailable removes tools whose deterministic runtime prerequisites are
// known to be missing before a model request is built. A nil runtime keeps
// lightweight/unit callers compatible; configured production agents always
// attach a runtime before their first turn.
func FilterAvailable(ts []Tool, runtime *ToolRuntime) ([]Tool, []string) {
	if runtime == nil {
		return ts, nil
	}

	out := make([]Tool, 0, len(ts))
	var notices []string
	for _, tool := range ts {
		if tool.Available == nil {
			out = append(out, tool)
			continue
		}
		available, reason := tool.Available(runtime)
		if !available {
			if reason != "" && !slices.Contains(notices, reason) {
				notices = append(notices, reason)
			}
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
	args = normalizeIntegerArgs(name, args)
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

var integerToolArgs = map[string]map[string]struct{}{
	"read":       {"offset": {}, "limit": {}},
	"grep":       {"max_results": {}},
	"glob":       {"max_results": {}},
	"find_files": {"max_results": {}},
	"web_search": {"count": {}},
	"edit":       {"start_line": {}, "end_line": {}},
}

func normalizeIntegerArgs(name string, args json.RawMessage) json.RawMessage {
	keys, ok := integerToolArgs[name]
	if !ok {
		return args
	}
	normalized, changed := normalizeIntegerJSON(args, keys)
	if !changed {
		return args
	}
	return normalized
}

func normalizeIntegerJSON(raw json.RawMessage, keys map[string]struct{}) (json.RawMessage, bool) {
	var object map[string]json.RawMessage
	if err := json.Unmarshal(raw, &object); err == nil && object != nil {
		changed := false
		for key, value := range object {
			if _, ok := keys[key]; ok {
				if number, ok := quotedInteger(value); ok {
					object[key] = number
					changed = true
					continue
				}
			}
			if nested, ok := normalizeIntegerJSON(value, keys); ok {
				object[key] = nested
				changed = true
			}
		}
		if !changed {
			return raw, false
		}
		normalized, err := json.Marshal(object)
		if err != nil {
			return raw, false
		}
		return normalized, true
	}

	var array []json.RawMessage
	if err := json.Unmarshal(raw, &array); err != nil || array == nil {
		return raw, false
	}
	changed := false
	for i, value := range array {
		if nested, ok := normalizeIntegerJSON(value, keys); ok {
			array[i] = nested
			changed = true
		}
	}
	if !changed {
		return raw, false
	}
	normalized, err := json.Marshal(array)
	if err != nil {
		return raw, false
	}
	return normalized, true
}

func quotedInteger(raw json.RawMessage) (json.RawMessage, bool) {
	var value string
	if err := json.Unmarshal(raw, &value); err != nil {
		return nil, false
	}
	number, err := strconv.Atoi(strings.TrimSpace(value))
	if err != nil {
		return nil, false
	}
	return json.RawMessage(strconv.Itoa(number)), true
}

func canonicalToolName(name string) string {
	switch name {
	case "artifact_read":
		return "output_read"
	case "artifact_list":
		return "output_list"
	case "read_file":
		return "read"
	case "search", "search_text":
		return "grep"
	case "find", "find_file":
		return "find_files"
	default:
		return name
	}
}

func SuggestTool(name string, candidates []string) []string {
	type scored struct {
		name string
		dist int
	}
	var hits []scored
	for _, candidate := range candidates {
		prefix := strings.HasPrefix(candidate, name) || strings.HasPrefix(name, candidate)
		distance := levenshtein(name, candidate, 4)
		maxDistance := 3
		if min(len(name), len(candidate)) <= 4 {
			maxDistance = 1
		} else if min(len(name), len(candidate)) <= 6 {
			maxDistance = 2
		}
		if prefix {
			distance = -len(candidate)
		} else if distance > maxDistance {
			continue
		}
		hits = append(hits, scored{candidate, distance})
	}
	sort.Slice(hits, func(i, j int) bool {
		if hits[i].dist != hits[j].dist {
			return hits[i].dist < hits[j].dist
		}
		return hits[i].name < hits[j].name
	})
	out := make([]string, 0, min(len(hits), 2))
	for _, hit := range hits {
		if len(out) == 2 {
			break
		}
		out = append(out, hit.name)
	}
	return out
}

func levenshtein(a, b string, cap int) int {
	if a == b {
		return 0
	}
	if d := len(a) - len(b); d > cap || -d > cap {
		return cap + 1
	}
	prev := make([]int, len(b)+1)
	for j := range prev {
		prev[j] = j
	}
	for i := 1; i <= len(a); i++ {
		cur := make([]int, len(b)+1)
		cur[0] = i
		rowMin := cur[0]
		for j := 1; j <= len(b); j++ {
			cost := 0
			if a[i-1] != b[j-1] {
				cost = 1
			}
			cur[j] = min3(cur[j-1]+1, prev[j]+1, prev[j-1]+cost)
			rowMin = min(rowMin, cur[j])
		}
		if rowMin > cap {
			return cap + 1
		}
		prev = cur
	}
	return prev[len(b)]
}

func min3(a, b, c int) int { return min(min(a, b), c) }

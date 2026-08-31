package agent

import (
	"encoding/json"
	"path/filepath"
	"sort"
	"strings"

	"github.com/sacca97/ghg/internal/llm"
	"github.com/sacca97/ghg/internal/tools"
)

// RebuildTouched rehydrates the ranking hints from a resumed conversation.
// The hints never grant access or change search results; they only improve the
// first-page order for files the session already inspected.
func (a *Agent) RebuildTouched(msgs []llm.Message) {
	if a == nil {
		return
	}
	for _, msg := range msgs {
		if msg.Role != "assistant" {
			continue
		}
		for _, call := range msg.ToolCalls {
			a.recordTouched(call.Function.Name, call.Function.Arguments)
		}
	}
}

func (a *Agent) recordTouched(toolName, args string) {
	paths := toolMutationPaths(toolName, args)
	if toolName == "read" {
		var in struct {
			Path string `json:"path"`
		}
		if json.Unmarshal([]byte(args), &in) == nil && in.Path != "" {
			paths = append(paths, in.Path)
		}
	}
	if len(paths) == 0 {
		return
	}
	a.touchedMu.Lock()
	defer a.touchedMu.Unlock()
	for _, name := range paths {
		if key := canonicalPathHint(name); key != "" {
			a.touched[key] = struct{}{}
		}
	}
}

func (a *Agent) searchHints() tools.SearchHints {
	a.touchedMu.Lock()
	paths := make([]string, 0, len(a.touched))
	for path := range a.touched {
		paths = append(paths, path)
	}
	a.touchedMu.Unlock()
	sort.Strings(paths)
	return tools.SearchHints{Touched: paths}
}

func canonicalPathHint(name string) string {
	if strings.TrimSpace(name) == "" {
		return ""
	}
	abs, err := filepath.Abs(name)
	if err != nil {
		return ""
	}
	if resolved, err := filepath.EvalSymlinks(abs); err == nil {
		abs = resolved
	}
	return filepath.Clean(abs)
}

package agent

import (
	"encoding/json"
	"path/filepath"
	"sort"
	"strings"
	"sync"

	"github.com/sacca97/ghg/internal/models"
	"github.com/sacca97/ghg/internal/tools"
)

// fileLocks serializes write operations to identical canonical paths using 1-slot channel semaphores.
// Path writes serialize concurrently with bash commands via world/global locks.
type fileLocks struct {
	mu     sync.Mutex
	locks  map[string]chan struct{}
	global chan struct{} // serializes bash (unknown side effects) with mutations
	world  sync.RWMutex  // bash is the writer; path-scoped mutations are readers
}

func newFileLocks() *fileLocks {
	return &fileLocks{
		locks:  map[string]chan struct{}{},
		global: make(chan struct{}, 1),
	}
}

// acquirePaths acquires path locks in lexical order to prevent deadlocks across multi-file edits.
func (f *fileLocks) acquirePaths(paths []string) func() {
	keys := make([]string, 0, len(paths))
	seen := make(map[string]struct{}, len(paths))
	for _, path := range paths {
		key := canonicalPath(path)
		if key == "" {
			continue
		}
		if _, ok := seen[key]; ok {
			continue
		}
		seen[key] = struct{}{}
		keys = append(keys, key)
	}
	sort.Strings(keys)
	f.world.RLock()
	channels := make([]chan struct{}, 0, len(keys))
	for _, key := range keys {
		f.mu.Lock()
		ch, ok := f.locks[key]
		if !ok {
			ch = make(chan struct{}, 1)
			f.locks[key] = ch
		}
		f.mu.Unlock()
		ch <- struct{}{}
		channels = append(channels, ch)
	}
	return func() {
		for i := len(channels) - 1; i >= 0; i-- {
			<-channels[i]
		}
		f.world.RUnlock()
	}
}

// acquireGlobal serializes a tool call against every other mutation — used by
// bash, whose side effects can't be attributed to one path.
func (f *fileLocks) acquireGlobal() func() {
	f.world.Lock()
	f.global <- struct{}{}
	return func() {
		<-f.global
		f.world.Unlock()
	}
}

// canonicalPath normalizes a path so two spellings of the same file share
// one lock (pi resolves through the FS; we settle for absolute + clean).
func canonicalPath(path string) string {
	if strings.TrimSpace(path) == "" {
		return ""
	}
	if abs, err := filepath.Abs(path); err == nil {
		path = abs
	}
	if resolved, err := filepath.EvalSymlinks(path); err == nil {
		path = resolved
	}
	return filepath.Clean(path)
}

// toolMutationPaths extracts every file an explicit mutation can replace.
// Bash remains global because its side effects are intentionally opaque.
func toolMutationPaths(toolName, args string) []string {
	if toolName != "write" && toolName != "edit" {
		return nil
	}
	var envelope struct {
		Path  string `json:"path"`
		Edits []struct {
			Path string `json:"path"`
		} `json:"edits"`
	}
	if err := json.Unmarshal([]byte(args), &envelope); err != nil {
		return nil
	}
	paths := make([]string, 0, len(envelope.Edits)+1)
	if envelope.Path != "" {
		paths = append(paths, envelope.Path)
	}
	for _, edit := range envelope.Edits {
		if edit.Path != "" {
			paths = append(paths, edit.Path)
		}
	}
	return paths
}

func toolRequiresGlobalMutation(toolName string) bool {
	return toolName == "bash"
}

// RebuildTouched rehydrates the ranking hints from a resumed conversation.
// The hints never grant access or change search results; they only improve the
// first-page order for files the session already inspected.
func (a *Agent) RebuildTouched(msgs []models.Message) {
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
		if requests, ok := normalizeReadRequests(toolName, args); ok {
			for _, request := range requests {
				paths = append(paths, request.path)
			}
		}
	}
	if len(paths) == 0 {
		return
	}
	a.touchedMu.Lock()
	defer a.touchedMu.Unlock()
	for _, name := range paths {
		if key := canonicalPath(name); key != "" {
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

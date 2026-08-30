package agent

import (
	"encoding/json"
	"path/filepath"
	"sort"
	"strings"
	"sync"
)

// fileLocks serializes mutations to the same canonical path across parallel
// tool calls. Each path owns a 1-capacity channel used as a semaphore: a tool
// acquires by sending (blocks until free) and releases by receiving. A channel
// per path is the idiomatic Go form of pi's per-path promise-chain queue — no
// explicit unlock bookkeeping.
//
// Only write/edit take a per-path lock; reads don't. Bash and lsp_rename apply
// take the global lock because their side effects cannot be attributed to one
// path before the tool validates the complete request.
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

// acquirePaths takes every path lock in canonical lexical order. The sorted
// order is important for a multi-file edit: two overlapping calls cannot
// deadlock by taking their files in opposite orders.
func (f *fileLocks) acquirePaths(paths []string) func() {
	keys := make([]string, 0, len(paths))
	seen := make(map[string]struct{}, len(paths))
	for _, path := range paths {
		key := canonicalPathKey(path)
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

// canonicalPathKey normalizes a path so two spellings of the same file share
// one lock (pi resolves through the FS; we settle for absolute + clean).
func canonicalPathKey(path string) string {
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

func toolRequiresGlobalMutation(toolName, args string) bool {
	if toolName == "bash" {
		return true
	}
	if toolName != "lsp_rename" {
		return false
	}
	var request struct {
		Operation string `json:"operation"`
	}
	if err := json.Unmarshal([]byte(args), &request); err != nil {
		return false
	}
	return strings.EqualFold(strings.TrimSpace(request.Operation), "apply")
}

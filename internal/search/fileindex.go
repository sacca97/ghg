package search

import (
	"cmp"
	"container/heap"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"
	"unicode/utf8"
)

// fileIndexTTL keeps completion responsive without turning every keystroke
// into a full recursive walk.
const fileIndexTTL = 30 * time.Second

// ponytail: keep at most 16 workspace indexes; use an LRU only if this cap
// becomes a measured memory or hit-rate problem.
const maxFileIndexRoots = 16

type fileIndexEntry struct {
	builtAt time.Time
	files   []string
}

var fileIndexes struct {
	sync.Mutex
	entries map[string]fileIndexEntry
}

// InvalidateFileIndex forces the next FuzzyFiles call for root to rescan it.
// The TUI uses this when its short-lived completion cache notices a new tree.
func InvalidateFileIndex(root string) {
	root = cleanRoot(root)
	fileIndexes.Lock()
	if fileIndexes.entries != nil {
		delete(fileIndexes.entries, root)
	}
	fileIndexes.Unlock()
}

func pruneFileIndexes(now time.Time) {
	for root, entry := range fileIndexes.entries {
		if now.Sub(entry.builtAt) >= fileIndexTTL {
			delete(fileIndexes.entries, root)
		}
	}
}

func evictOldestFileIndex() {
	if len(fileIndexes.entries) < maxFileIndexRoots {
		return
	}
	oldestRoot := oldestKey(fileIndexes.entries, func(entry fileIndexEntry) time.Time {
		return entry.builtAt
	}, true)
	if oldestRoot != "" {
		delete(fileIndexes.entries, oldestRoot)
	}
}

type fuzzyHit struct {
	path       string
	tier       int
	start      int
	depth      int
	pathLength int
}

func compareHits(a, b fuzzyHit) int {
	if a.tier != b.tier {
		return cmp.Compare(a.tier, b.tier)
	}
	if a.start != b.start {
		return cmp.Compare(a.start, b.start)
	}
	if a.depth != b.depth {
		return cmp.Compare(a.depth, b.depth)
	}
	if a.pathLength != b.pathLength {
		return cmp.Compare(a.pathLength, b.pathLength)
	}
	return cmp.Compare(a.path, b.path)
}

type hitMaxHeap []fuzzyHit

func (h hitMaxHeap) Len() int           { return len(h) }
func (h hitMaxHeap) Less(i, j int) bool { return compareHits(h[i], h[j]) > 0 } // worst match at root
func (h hitMaxHeap) Swap(i, j int)      { h[i], h[j] = h[j], h[i] }
func (h *hitMaxHeap) Push(x any)        { *h = append(*h, x.(fuzzyHit)) }
func (h *hitMaxHeap) Pop() any {
	old := *h
	n := len(old)
	x := old[n-1]
	*h = old[0 : n-1]
	return x
}

// FuzzyFiles returns up to limit paths relative to root, ranked after every
// candidate has been scored. In particular, limit never cuts traversal short:
// a strong match late in a large tree can still displace an early weak one.
func FuzzyFiles(root, query string, limit int) []string {
	root = cleanRoot(root)
	if root == "" {
		return nil
	}
	files := indexedFiles(root)
	q := strings.ToLower(strings.TrimSpace(query))
	if limit <= 0 {
		hits := make([]fuzzyHit, 0, len(files))
		for _, name := range files {
			pathLower := strings.ToLower(name)
			base := pathLower[strings.LastIndexByte(pathLower, '/')+1:]
			tier, start := fuzzyTier(base, pathLower, q)
			if tier < 0 {
				continue
			}
			hits = append(hits, fuzzyHit{
				path: name, tier: tier, start: start,
				depth: strings.Count(name, "/"), pathLength: len(name),
			})
		}
		slices.SortFunc(hits, compareHits)
		out := make([]string, len(hits))
		for i, h := range hits {
			out[i] = h.path
		}
		return out
	}

	h := make(hitMaxHeap, 0, limit)
	for _, name := range files {
		pathLower := strings.ToLower(name)
		base := pathLower[strings.LastIndexByte(pathLower, '/')+1:]
		tier, start := fuzzyTier(base, pathLower, q)
		if tier < 0 {
			continue
		}
		cand := fuzzyHit{
			path: name, tier: tier, start: start,
			depth: strings.Count(name, "/"), pathLength: len(name),
		}
		if len(h) < limit {
			heap.Push(&h, cand)
		} else if compareHits(cand, h[0]) < 0 {
			h[0] = cand
			heap.Fix(&h, 0)
		}
	}
	slices.SortFunc(h, compareHits)
	out := make([]string, len(h))
	for i, item := range h {
		out[i] = item.path
	}
	return out
}

func fuzzyTier(base, full, query string) (tier, start int) {
	if query == "" {
		return 0, 0
	}
	if i := strings.Index(base, query); i >= 0 {
		return 0, i
	}
	if i := strings.Index(full, query); i >= 0 {
		return 1, i
	}
	if ok, i := fuzzySubsequence(base, query); ok {
		return 2, i
	}
	if ok, i := fuzzySubsequence(full, query); ok {
		return 3, i
	}
	return -1, 0
}

func fuzzySubsequence(s, query string) (bool, int) {
	if query == "" {
		return true, 0
	}
	start := -1
	for _, r := range query {
		i := strings.IndexRune(s, r)
		if i < 0 {
			return false, 0
		}
		if start < 0 {
			start = i
		}
		_, size := utf8.DecodeRuneInString(s[i:])
		s = s[i+size:]
	}
	return true, start
}

func indexedFiles(root string) []string {
	fileIndexes.Lock()
	if fileIndexes.entries == nil {
		fileIndexes.entries = make(map[string]fileIndexEntry)
	}
	now := time.Now()
	pruneFileIndexes(now)
	if entry, ok := fileIndexes.entries[root]; ok {
		files := slices.Clone(entry.files)
		fileIndexes.Unlock()
		return files
	}
	fileIndexes.Unlock()

	var files []string
	_ = filepath.WalkDir(root, func(name string, entry fs.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if name == root {
			return nil
		}
		base := entry.Name()
		if entry.IsDir() {
			if base == ".git" || strings.HasPrefix(base, ".") || base == "vendor" || base == "node_modules" {
				return filepath.SkipDir
			}
			return nil
		}
		if entry.Type()&fs.ModeSymlink != 0 || !entry.Type().IsRegular() {
			return nil
		}
		rel, relErr := filepath.Rel(root, name)
		if relErr == nil {
			files = append(files, filepath.ToSlash(rel))
		}
		return nil
	})
	slices.Sort(files)

	fileIndexes.Lock()
	pruneFileIndexes(time.Now())
	evictOldestFileIndex()
	fileIndexes.entries[root] = fileIndexEntry{builtAt: time.Now(), files: slices.Clone(files)}
	fileIndexes.Unlock()
	return files
}

func cleanRoot(root string) string {
	if strings.TrimSpace(root) == "" {
		return ""
	}
	abs, err := filepath.Abs(root)
	if err != nil {
		return ""
	}
	if info, err := os.Lstat(abs); err != nil || info.Mode()&fs.ModeSymlink != 0 || !info.IsDir() {
		return ""
	}
	if resolved, err := filepath.EvalSymlinks(abs); err == nil {
		return resolved
	}
	return abs
}

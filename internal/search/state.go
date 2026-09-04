// Package search contains the small pieces of search state shared by the
// native tools and the TUI. Filesystem traversal remains in internal/tools;
// this package owns stable snapshots and the fuzzy file index.
package search

import (
	"cmp"
	"container/heap"
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"sync"
	"time"
	"unicode/utf8"
)

// Item is one stable search result. Line is zero for path-only results such as
// glob and find_files.
type Item struct {
	Path          string `json:"path"`
	Line          int    `json:"line,omitempty"`
	Text          string `json:"text,omitempty"`
	StartColumn   int    `json:"start_column,omitempty"`
	EndLine       int    `json:"end_line,omitempty"`
	EndColumn     int    `json:"end_column,omitempty"`
	StartByte     int    `json:"start_byte,omitempty"`
	EndByte       int    `json:"end_byte,omitempty"`
	Pattern       int    `json:"pattern,omitempty"`
	ObservationID string `json:"-"`
}

// Snapshot is the bounded result set behind a pagination cursor. It is
// immutable after Save; keeping the full bounded set here makes later pages
// independent of worktree changes.
type Snapshot struct {
	ID        string    `json:"id"`
	Kind      string    `json:"kind"`
	Items     []Item    `json:"items"`
	Complete  bool      `json:"complete"`
	Reason    string    `json:"reason,omitempty"`
	CreatedAt time.Time `json:"created_at"`
}

// Store persists snapshots under a session boundary. The session package
// implements this interface; Registry supplies an in-memory copy so a new
// session can paginate before its first database save.
type Store interface {
	SaveSearchSnapshot(ctx context.Context, sessionID string, snapshot Snapshot) error
	LoadSearchSnapshot(ctx context.Context, sessionID, id string) (Snapshot, error)
}

// Registry keeps live snapshots and optionally mirrors them into a durable
// session store. A session id is passed per operation so background and
// foreground agents can safely share the registry.
type Registry struct {
	mu         sync.Mutex
	snapshots  map[string]Snapshot
	persistent Store
}

// NewRegistry creates an empty snapshot registry.
func NewRegistry() *Registry {
	return &Registry{snapshots: make(map[string]Snapshot)}
}

// SetPersistent installs the durable session store used by future saves.
func (r *Registry) SetPersistent(store Store) {
	if r == nil {
		return
	}
	r.mu.Lock()
	r.persistent = store
	r.mu.Unlock()
}

// BindSession persists snapshots made before a session row existed. This is
// what lets an interactive first turn use a cursor and still recover it after
// the TUI creates the session at the end of that turn.
func (r *Registry) BindSession(ctx context.Context, sessionID string) error {
	if r == nil || strings.TrimSpace(sessionID) == "" {
		return nil
	}
	r.mu.Lock()
	store := r.persistent
	var pending []Snapshot
	for key, snapshot := range r.snapshots {
		if strings.HasPrefix(key, "\x00") {
			copySnapshot := cloneSnapshot(snapshot)
			r.snapshots[sessionKey(sessionID, snapshot.ID)] = copySnapshot
			delete(r.snapshots, key)
			pending = append(pending, copySnapshot)
		}
	}
	r.mu.Unlock()
	if store == nil {
		return nil
	}
	for _, snapshot := range pending {
		if err := store.SaveSearchSnapshot(ctx, sessionID, snapshot); err != nil {
			return err
		}
	}
	return nil
}

const maxLiveSnapshotsPerSession = 16

// Save stores a snapshot in memory and mirrors it when sessionID is set.
func (r *Registry) Save(ctx context.Context, sessionID string, snapshot Snapshot) error {
	if r == nil {
		return errors.New("search snapshot registry is nil")
	}
	if strings.TrimSpace(snapshot.ID) == "" {
		return errors.New("search snapshot id is required")
	}
	if snapshot.CreatedAt.IsZero() {
		snapshot.CreatedAt = time.Now().UTC()
	}
	snapshot = cloneSnapshot(snapshot)
	r.mu.Lock()
	r.snapshots[sessionKey(sessionID, snapshot.ID)] = snapshot
	r.evictOldest(sessionID)
	store := r.persistent
	r.mu.Unlock()
	if store != nil && strings.TrimSpace(sessionID) != "" {
		return store.SaveSearchSnapshot(ctx, sessionID, snapshot)
	}
	return nil
}

// ponytail: O(n) eviction is intentional while the per-session limit is 16;
// replace with an LRU only if this bound grows materially.
func (r *Registry) evictOldest(sessionID string) {
	prefix := sessionID + "\x00"
	count := 0
	var oldestKey string
	var oldestTime time.Time
	for key, snap := range r.snapshots {
		if strings.HasPrefix(key, prefix) {
			count++
			if oldestKey == "" || snap.CreatedAt.Before(oldestTime) {
				oldestKey = key
				oldestTime = snap.CreatedAt
			}
		}
	}
	if count > maxLiveSnapshotsPerSession && oldestKey != "" {
		delete(r.snapshots, oldestKey)
	}
}

// Load returns a live snapshot first, then asks the durable store for it.
func (r *Registry) Load(ctx context.Context, sessionID, id string) (Snapshot, error) {
	if r == nil {
		return Snapshot{}, errors.New("search snapshot registry is nil")
	}
	r.mu.Lock()
	snapshot, ok := r.snapshots[sessionKey(sessionID, id)]
	store := r.persistent
	r.mu.Unlock()
	if ok {
		return cloneSnapshot(snapshot), nil
	}
	if store == nil || strings.TrimSpace(sessionID) == "" {
		return Snapshot{}, os.ErrNotExist
	}
	snapshot, err := store.LoadSearchSnapshot(ctx, sessionID, id)
	if err != nil {
		return Snapshot{}, err
	}
	r.mu.Lock()
	r.snapshots[sessionKey(sessionID, id)] = cloneSnapshot(snapshot)
	r.mu.Unlock()
	return snapshot, nil
}

func sessionKey(sessionID, id string) string { return sessionID + "\x00" + id }

func cloneSnapshot(in Snapshot) Snapshot {
	in.Items = slices.Clone(in.Items)
	return in
}

// NewID returns a short opaque id suitable for a model-facing cursor. It is
// intentionally not a content hash: two searches of the same file can carry
// different bounded result sets.
func NewID(prefix string) string {
	var raw [8]byte
	if _, err := rand.Read(raw[:]); err == nil {
		return prefix + "-" + hex.EncodeToString(raw[:])
	}
	return prefix + "-" + hex.EncodeToString([]byte(time.Now().UTC().Format("150405.000000000")))
}

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
	oldestRoot := ""
	var oldest time.Time
	for root, entry := range fileIndexes.entries {
		if oldestRoot == "" || entry.builtAt.Before(oldest) || (entry.builtAt.Equal(oldest) && root < oldestRoot) {
			oldestRoot, oldest = root, entry.builtAt
		}
	}
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
	sort.Strings(files)

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

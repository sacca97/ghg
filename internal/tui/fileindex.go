package tui

import (
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/sacca97/ghg/internal/search"
)

// fileIndexTTL is how long a cached recursive file listing is reused.
// Rebuilding walks the tree, so completion must not do it per keystroke.
const fileIndexTTL = 2 * time.Second

// fileIndex is a small compatibility/invalidation wrapper around the shared
// search index. Keeping the fields lets completion tests reset the cache
// without making the TUI own a second traversal implementation.
var fileIndex struct {
	sync.Mutex
	builtAt time.Time
	root    string
}

// currentRoot reports the directory fuzzy @mentions search. The TUI runs from
// the repo root, but tests chdir into fixture trees, so the model carries the
// effective root; the fallback keeps the bare completion helpers testable.
var currentRoot = os.Getwd

// refreshFileIndex invalidates the shared index when the TUI's root changes or
// its short completion cache expires.
func refreshFileIndex() {
	wd, err := currentRoot()
	if err != nil {
		return
	}
	fileIndex.Lock()
	if wd == fileIndex.root && time.Since(fileIndex.builtAt) < fileIndexTTL {
		fileIndex.Unlock()
		return
	}
	fileIndex.root, fileIndex.builtAt = wd, time.Now()
	fileIndex.Unlock()
	search.InvalidateFileIndex(wd)
	_ = search.FuzzyFiles(wd, "", 0) // warm the shared index
}

// fuzzyFiles returns up to limit files from the index matching query, best
// first. An empty query lists every indexed file (sorted, so results are
// stable). Match quality prefers the query as a contiguous substring (base
// name first, then path), then subsequence matches (base name, then path).
// The result cap keeps per-keystroke scoring bounded; it only binds on
// pathological queries in huge trees, where the top-ranked matches win anyway.
func fuzzyFiles(query string, limit int) []string {
	refreshFileIndex()
	root, err := currentRoot()
	if err != nil {
		return nil
	}
	return search.FuzzyFiles(root, query, limit)
}

// matchTier grades how well q matches file f (both compared lowercase):
//
//	0: q is a substring of the base name
//	1: q is a substring of the full path
//	2: q is a subsequence of the base name
//	3: q is a subsequence of the full path
//	-1: no match
func matchTier(f, q string) int {
	if q == "" {
		return 0
	}
	lf := strings.ToLower(f)
	base := lf[strings.LastIndexByte(lf, '/')+1:]
	if strings.Contains(base, q) {
		return 0
	}
	if strings.Contains(lf, q) {
		return 1
	}
	if subseq(base, q) {
		return 2
	}
	if subseq(lf, q) {
		return 3
	}
	return -1
}

// subseq reports whether every rune of q appears in s, in order.
func subseq(s, q string) bool {
	for _, r := range q {
		i := strings.IndexRune(s, r)
		if i < 0 {
			return false
		}
		s = s[i+1:]
	}
	return true
}

// resolveMentionPath turns an @mention token into an absolute path. Real
// paths (relative, absolute, ~) stat as-is; a bare word like "roadmap"
// falls back to the fuzzy index when exactly one file matches.
func resolveMentionPath(p string) (string, bool) {
	abs := p
	if abs == "~" || strings.HasPrefix(abs, "~/") {
		if home, err := os.UserHomeDir(); err == nil {
			abs = home + abs[1:]
		}
	}
	if !filepath.IsAbs(abs) {
		if wd, err := os.Getwd(); err == nil {
			abs = filepath.Join(wd, abs)
		}
	}
	if _, err := os.Stat(abs); err == nil {
		return abs, true
	}
	// Not a literal path: try a unique fuzzy match against the index, but
	// only for bare words (no separators) so partial paths stay untouched.
	if !strings.ContainsAny(p, "/\\") {
		if hits := fuzzyFiles(p, 2); len(hits) == 1 {
			if wd, err := currentRoot(); err == nil {
				return filepath.Join(wd, filepath.FromSlash(hits[0])), true
			}
		}
	}
	return "", false
}

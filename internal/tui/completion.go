package tui

import (
	tea "github.com/charmbracelet/bubbletea"
	"github.com/sacca97/ghg/internal/search"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
)

// cand is one completion candidate with an optional description.
type cand struct {
	Text string
	Desc string
}

// commands is the tab-completion table, built from the registry so a command
// can never be dispatchable but uncompletable (or vice versa).
var commands = completionTable()

// completionTable derives the tab-completion table from the registry, so a
// command can never be dispatchable but uncompletable (or vice versa).
func completionTable() []cand {
	var out []cand
	for _, e := range slashRegistry() {
		out = append(out, cand{e.Name, e.Hint})
	}
	return out

}

var exportKindCands = []cand{
	{"chat", "export the full conversation"},
	{"last", "export the latest assistant message"},
	{"plan", "export the latest plan"},
	{"review", "export the latest review"},
}

// completions splits val into an untouched head and candidates for its last
// token. nil efforts uses the default /effort candidates.
func completions(val string, models, providers, authProviders, skillCands, efforts []cand) (head string, cands []cand) {
	return completionsWithMention(val, models, providers, authProviders, skillCands, efforts, nil)
}

// completionsWithMention is the production variant of completions. A nil
// mention list keeps the old synchronous behavior for pure completion tests;
// a non-nil list means the recursive search already ran off the UI loop.
func completionsWithMention(val string, models, providers, authProviders, skillCands, efforts, mention []cand) (head string, cands []cand) {
	if efforts == nil {
		efforts = effortCands
	}
	i := strings.LastIndexByte(val, ' ')
	head, token := val[:i+1], val[i+1:]
	fields := strings.Fields(head)
	switch {
	case strings.HasPrefix(val, "/") && len(fields) == 0:
		cands = filterPrefix(commands, token)
	case len(fields) == 1 && fields[0] == "/auth":
		cands = filterPrefix(authProviders, token)
	case len(fields) == 1 && fields[0] == "/model":
		cands = filterFuzzy(append([]cand{{"refresh", "refetch provider model catalogs"}}, models...), token)
	case len(fields) == 2 && fields[0] == "/model" && fields[1] != "refresh":
		cands = filterFuzzy(providers, token)
	case len(fields) == 1 && fields[0] == "/effort":
		cands = filterPrefix(efforts, token)
	case len(fields) == 1 && fields[0] == "/compact":
		cands = filterPrefix([]cand{
			{"retry", "undo the latest compaction and retry"},
			{"log", "list recorded compactions"},
		}, token)
	case len(fields) == 1 && fields[0] == "/notify":
		cands = filterPrefix([]cand{
			{"config", "configure and verify Telegram"},
			{"on", "enable Telegram for this session"},
			{"off", "disable Telegram for this session"},
		}, token)
	case len(fields) == 1 && (fields[0] == "/export" || fields[0] == "/export-result"):
		cands = filterPrefix(exportKindCands, token)
	case strings.HasPrefix(token, "$"): // codex-style skill invocation
		cands = filterPrefix(skillCands, token)
	case strings.HasPrefix(token, "@"):
		// Literal paths stay instant; recursive matches arrive through the
		// background index supplied by the model.
		if q := token[1:]; isPathQuery(q) {
			for _, c := range mentionPathMatches(q) {
				cands = append(cands, cand{"@" + c.Text, c.Desc})
			}
		}
		if mention != nil {
			for _, c := range mention {
				cands = append(cands, cand{"@" + c.Text, c.Desc})
			}
		} else if !isPathQuery(token[1:]) {
			for _, f := range fuzzyFiles(token[1:], menuRows) {
				cands = append(cands, cand{"@" + f, ""})
			}
		}
	case strings.HasPrefix(val, "/"): // other slash-command args: nothing to complete
	default:
		cands = pathMatches(token)
	}
	// A recursive result can overlap a literal directory lookup. Keep the
	// dropdown stable instead of showing the same path twice.
	seen := make(map[string]bool, len(cands))
	unique := cands[:0]
	for _, c := range cands {
		if !seen[c.Text] {
			seen[c.Text] = true
			unique = append(unique, c)
		}
	}
	sort.Slice(unique, func(a, b int) bool { return unique[a].Text < unique[b].Text })
	return head, unique
}

func filterPrefix(all []cand, prefix string) []cand {
	var out []cand
	for _, c := range all {
		if strings.HasPrefix(c.Text, prefix) {
			out = append(out, c)
		}
	}
	return out
}

// filterFuzzy matches candidates tiered like fuzzyFiles (substring, then
// subsequence), best tier first. An empty prefix keeps the original order.
func filterFuzzy(all []cand, q string) []cand {
	if q == "" {
		return slices.Clone(all)
	}
	type hit struct {
		c    cand
		tier int
	}
	var hits []hit
	for _, c := range all {
		if tier := matchTier(c.Text, q); tier >= 0 {
			hits = append(hits, hit{c, tier})
		}
	}
	sort.SliceStable(hits, func(a, b int) bool { return hits[a].tier < hits[b].tier })
	out := make([]cand, 0, len(hits))
	for _, h := range hits {
		out = append(out, h.c)
	}
	return out
}

// isPathQuery reports whether an @mention query looks like a path (has a
// separator, ~, or leading dot) and should use plain glob completion rather
// than the recursive fuzzy index.
func isPathQuery(q string) bool {
	return q == "" || strings.ContainsAny(q, "/\\") || strings.HasPrefix(q, "~") || strings.HasPrefix(q, ".")
}

// mentionPathMatches globs an @mention path query against the mention root
// (not the process cwd): absolute and ~ queries glob as-is; relative ones are
// joined to the root and returned root-relative, with dirs keeping their
// trailing slash.
func mentionPathMatches(q string) []cand {
	if filepath.IsAbs(q) || q == "~" || strings.HasPrefix(q, "~/") {
		return pathMatches(q)
	}
	root, err := os.Getwd()
	if err != nil {
		return nil
	}
	var out []cand
	for _, c := range pathMatches(filepath.Join(root, q)) {
		dir := strings.HasSuffix(c.Text, "/")
		if rel, err := filepath.Rel(root, strings.TrimSuffix(c.Text, "/")); err == nil {
			c.Text = filepath.ToSlash(rel)
			if dir {
				c.Text += "/"
			}
			out = append(out, c)
		}
	}
	return out
}

func pathMatches(prefix string) []cand {
	p := prefix
	if p == "~" || strings.HasPrefix(p, "~/") {
		if home, err := os.UserHomeDir(); err == nil {
			p = home + p[1:]
		}
	}
	matches, _ := filepath.Glob(p + "*")
	var out []cand
	for _, m := range matches {
		if fi, err := os.Stat(m); err == nil && fi.IsDir() {
			out = append(out, cand{m + "/", "dir"})
		} else {
			out = append(out, cand{m, ""})
		}
	}
	return out
}

// fuzzyFiles returns up to limit files from the index matching query, best
// first. An empty query lists every indexed file (sorted, so results are
// stable). Match quality prefers the query as a contiguous substring (base
// name first, then path), then subsequence matches (base name, then path).
// The result cap keeps per-keystroke scoring bounded; it only binds on
// pathological queries in huge trees, where the top-ranked matches win anyway.
func fuzzyFiles(query string, limit int) []string {
	root, err := os.Getwd()
	if err != nil {
		return nil
	}
	return search.FuzzyFiles(root, query, limit)
}

// mentionCandidates turns recursive file hits into file and useful parent
// directory completions. Empty directories remain available through literal
// path completion, which avoids a second recursive walker.
func mentionCandidates(paths []string, query string) []cand {
	seen := make(map[string]bool, len(paths)*2)
	out := make([]cand, 0, len(paths)*2)
	add := func(path string, folder bool) {
		if path == "" || seen[path] {
			return
		}
		seen[path] = true
		desc := ""
		if folder {
			desc = "dir"
		}
		out = append(out, cand{path, desc})
	}
	for _, path := range paths {
		add(path, false)
		parts := strings.Split(path, "/")
		for i := 1; i < len(parts); i++ {
			dir := strings.Join(parts[:i], "/") + "/"
			if matchTier(strings.TrimSuffix(dir, "/"), query) >= 0 {
				add(dir, true)
			}
		}
	}
	return out
}

func mentionQuery(value string) (string, bool) {
	token := value[strings.LastIndexByte(value, ' ')+1:]
	if !strings.HasPrefix(token, "@") {
		return "", false
	}
	return token[1:], true
}

type mentionSearchMsg struct {
	root       string
	query      string
	generation uint64
	candidates []cand
}

// mentionSearchCmd runs the existing cached walk in a Bubble Tea command so
// the first scan cannot block typing or rendering.
func (m *model) mentionSearchCmd() tea.Cmd {
	query, ok := mentionQuery(m.input.Value())
	if !ok {
		return nil
	}
	root := m.workingDirectory()
	if filepath.IsAbs(query) || query == "~" || strings.HasPrefix(query, "~/") {
		return nil
	}
	if m.mentionSearchPending {
		return nil
	}
	if m.mentionSearchRoot == root && m.mentionSearchQuery == query && m.mentionSearchResults != nil {
		return nil
	}
	m.mentionSearchPending = true
	m.mentionSearchRoot = root
	m.mentionSearchQuery = query
	m.mentionSearchResults = []cand{}
	generation := m.mentionSearchGeneration
	return func() tea.Msg {
		paths := search.FuzzyFiles(root, query, menuRows)
		return mentionSearchMsg{
			root: root, query: query, generation: generation,
			candidates: mentionCandidates(paths, query),
		}
	}
}

func (m *model) invalidateMentionSearch(root string) {
	search.InvalidateFileIndex(root)
	m.mentionSearchGeneration++
	m.mentionSearchPending = false
	m.mentionSearchRoot = ""
	m.mentionSearchQuery = ""
	m.mentionSearchResults = nil
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
	q = strings.ToLower(q)
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
			if wd, err := os.Getwd(); err == nil {
				return filepath.Join(wd, filepath.FromSlash(hits[0])), true
			}
		}
	}
	return "", false
}

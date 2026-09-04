package tui

import (
	"github.com/charmbracelet/glamour"
	glamouransi "github.com/charmbracelet/glamour/ansi"
	"github.com/charmbracelet/glamour/styles"
	"github.com/charmbracelet/x/ansi"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
)

// Clickable links (OSC 8 hyperlinks): the terminal owns the click — no mouse
// plumbing here. Two post-render passes over glamour's output:
//
//  1. hyperlinkGlamourLinks rewires rendered markdown links: glamour prints
//     "[label](url)" as underlined `label url` with no hyperlink; the label
//     atoms become one OSC 8 hyperlink to the href and the href atoms vanish
//     (no doubled "label url", at any width). Bare autolinks become clickable
//     in place.
//  2. linkifyRenderedFilePaths wraps bare path/to/file[:N] tokens in file://
//     hyperlinks, gated on the file existing on disk.
//
// linkifyFilePaths applies the same file-ref linkification to raw user text
// (submit/resume/steer echoes), which never goes through glamour.
//
// Unsupported terminals ignore OSC 8 and show the underlined text as before;
// ansi.Strip (copy/selection) drops the sequences. Width-safe: verified
// against ansi.Wrap/Hardwrap/StringWidth.

// fileRefRE matches a file reference with an optional :line suffix: a path
// with at least one slash and a dotted extension. The leading run of
// path-ish characters spans the full reference; matches inside [text](target)
// or a URL are rejected in linkifyFilePaths via the preceding byte. Bare
// filenames without a slash are not matched — too common as prose.
var fileRefRE = regexp.MustCompile(
	`/?[\w@+~-][\w@+~.-]*(?:/[\w@+~-][\w@+~.-]*)+(?::\d+)?` + // path with slashes
		`|/?[\w@+~-][\w@+~.-]*\.[A-Za-z]{2,10}(?::\d+)?` + // bare multi-letter ext
		`|\.{1,2}/[\w@+~-][\w@+~.-]*(?:/[\w@+~-][\w@+~.-]*)*(?::\d+)?`) // ./ ../

// linkifyFilePaths wraps mentions of existing local files in OSC 8 file://
// hyperlinks. exists decides whether a candidate path names a real file
// (injectable for tests). Markdown link internals are skipped: a match
// immediately preceded by '(' or ']' is part of [text](target) and gets
// handled after rendering instead. Extensionless and bare single-letter-ext
// matches are ignored — prose, not file refs.
func linkifyFilePaths(s string, exists func(string) bool) string {
	return replaceMatches(s, fileRefRE, func(m string, before byte) string {
		// Skip markdown link internals, URL tails, code spans, quotes. A
		// parenthesized path in prose — "(see tui.go)" — is not linkified:
		// '(' also opens a markdown [text](target), which the one-byte
		// lookahead can't distinguish. The markdown path is covered after
		// rendering instead.
		if strings.ContainsRune("([]/:;\"`", rune(before)) {
			return m
		}
		path, line := splitLineRef(m)
		if !isFileRef(path) || !exists(path) {
			return m
		}
		return hyperlink(absFileURI(path, line), m)
	})
}

// isFileRef rejects extension dots that belong to a directory segment. The
// regex handles the multi-letter minimum for bare filenames.
func isFileRef(path string) bool {
	dot := strings.LastIndexByte(path, '.')
	if dot < 0 {
		// no extension: only a slashed path counts (internal/tui, /etc/hostname)
		return strings.Contains(path, "/")
	}
	ext := path[dot+1:]
	if strings.ContainsRune(ext, '/') {
		return false // dot was in a directory segment
	}
	return len(ext) >= 1
}

// hyperlink wraps text in an OSC 8 hyperlink to uri.
func hyperlink(uri, text string) string {
	return ansi.SetHyperlink(uri) + text + ansi.ResetHyperlink()
}

// replaceMatches applies fn to each regex match; fn also receives the byte
// preceding the match (0 at string start) for cheap context checks.
func replaceMatches(s string, re *regexp.Regexp, fn func(m string, before byte) string) string {
	var b strings.Builder
	b.Grow(len(s) + len(s)/4)
	last := 0
	for _, loc := range re.FindAllStringIndex(s, -1) {
		var before byte
		if loc[0] > 0 {
			before = s[loc[0]-1]
		}
		b.WriteString(s[last:loc[0]])
		b.WriteString(fn(s[loc[0]:loc[1]], before))
		last = loc[1]
	}
	b.WriteString(s[last:])
	return b.String()
}

// realFileExists stats path relative to the process working directory (ghg
// runs at the project root) and reports whether it is a regular file.
func realFileExists(path string) bool {
	if !filepath.IsAbs(path) {
		wd, err := os.Getwd()
		if err != nil {
			return false
		}
		path = filepath.Join(wd, path)
	}
	info, err := os.Stat(path)
	return err == nil && info.Mode().IsRegular()
}

// splitLineRef separates a trailing :N line number and any absorbed trailing
// sentence punctuation from a file reference.
func splitLineRef(ref string) (path, line string) {
	i := strings.LastIndexByte(ref, ':')
	if i > 0 && i < len(ref)-1 && isDigits(ref[i+1:]) {
		return strings.TrimRight(ref[:i], ".,;:!?"), ref[i+1:]
	}
	return strings.TrimRight(ref, ".,;:!?"), ""
}

func isDigits(s string) bool {
	for i := 0; i < len(s); i++ {
		if s[i] < '0' || s[i] > '9' {
			return false
		}
	}
	return len(s) > 0
}

// absFileURI builds an absolute file:// URI. The :line suffix stays in the
// URI path: handlers that understand it jump to the line, the rest still
// open the file or its directory. Returns "" only when the CWD is unknown.
func absFileURI(path, line string) string {
	if !filepath.IsAbs(path) {
		wd, err := os.Getwd()
		if err != nil {
			return ""
		}
		path = filepath.Join(wd, path)
	}
	if line != "" {
		path += ":" + line
	}
	return "file://" + (&url.URL{Path: path}).String()
}

// --- glamour link rewiring -------------------------------------------------

// Rendered-link shape, verified against glamour v1.0.0: text is emitted as
// word atoms, each an SGR span closed by \x1b[0m.
// A "[label](url)" link renders as label atoms + doc-colored space atoms +
// href atoms; glamour's wrap may split any atom across lines (a newline
// lands inside the atom). A bare autolink renders as href atoms alone.
//
// hyperlinkGlamourLinks therefore works in two passes over the atoms: first
// it groups consecutive label/href atoms into links (across newlines and
// padding), recording each link's href and label spans; then it rewrites —
// the label atoms become one OSC 8 hyperlink to the href, the href atoms and
// their gap vanish, and a standalone href (autolink) becomes clickable in
// place. Width-independent: the merge is structural, not adjacency-at-one-
// width.
const (
	linkTextSGRDark  = "\x1b[38;5;35;1m"
	linkTextSGRLight = "\x1b[38;5;29;1m"
	linkSGRDark      = "\x1b[38;5;30;4m"
	linkSGRLight     = "\x1b[38;5;36;4m"
	linkTextSGRANSI  = "\x1b[35;1m"
	linkSGRANSI      = "\x1b[36;4m"
	sgrReset         = "\x1b[0m"
)

// linkAtom is one parsed glamour word atom: its SGR span, visible text, and
// byte range in the source. text ends at an embedded newline (word wrap).
type linkAtom struct {
	start, end int // byte range of the whole atom (span + text + reset)
	sgr        string
	kind       byte // 't' label (LinkText), 'h' href (Link)
	text       string
}

// parseLinkAtoms extracts every link atom (label or href) in order.
func parseLinkAtoms(s string) []linkAtom {
	var atoms []linkAtom
	for i := 0; i < len(s); {
		sgr, kind := linkAtomAt(s[i:])
		if kind == 0 {
			i++
			continue
		}
		end, text := scanAtom(s, i, sgr)
		if end < 0 {
			i++
			continue
		}
		atoms = append(atoms, linkAtom{start: i, end: end, sgr: sgr, kind: kind, text: text})
		i = end
	}
	return atoms
}

// linkGroup is one logical link: the label atom(s) and the href atom(s) that
// follow them, with the byte range covering the whole group (so the gap can
// be dropped when the link is rewired).
type linkGroup struct {
	labels     []linkAtom
	hrefs      []linkAtom
	start, end int // from the first label atom to the last href atom
	hasLabel   bool
}

// groupLinkAtoms merges consecutive label atoms + following href atoms into
// logical links. Only whitespace/newlines/styling may separate the label from
// its href; any other visible text ends the group. A run of href atoms with
// no preceding label is one autolink (glamour splits a wrapped URL into
// several fragments and emits empty placeholder atoms around the break).
func groupLinkAtoms(s string, atoms []linkAtom) []linkGroup {
	var groups []linkGroup
	i := 0
	for i < len(atoms) {
		a := atoms[i]
		if a.kind == 'h' {
			// href run (autolink or wrap-split fragments): accumulate while
			// the gap stays whitespace/styling
			g := linkGroup{start: a.start}
			for i < len(atoms) && atoms[i].kind == 'h' &&
				(len(g.hrefs) == 0 || gapOK(s[g.hrefs[len(g.hrefs)-1].end:atoms[i].start])) {
				g.hrefs = append(g.hrefs, atoms[i])
				g.end = atoms[i].end
				i++
			}
			groups = append(groups, g)
			continue
		}
		// label run
		g := linkGroup{hasLabel: true, start: a.start}
		for i < len(atoms) && atoms[i].kind == 't' {
			g.labels = append(g.labels, atoms[i])
			i++
		}
		// href run after the label: same whitespace/styling gap rule
		prevEnd := g.labels[len(g.labels)-1].end
		for i < len(atoms) && atoms[i].kind == 'h' && gapOK(s[prevEnd:atoms[i].start]) {
			g.hrefs = append(g.hrefs, atoms[i])
			prevEnd = atoms[i].end
			i++
		}
		if len(g.hrefs) > 0 {
			g.end = g.hrefs[len(g.hrefs)-1].end
		} else {
			g.end = g.labels[len(g.labels)-1].end
		}
		groups = append(groups, g)
	}
	return groups
}

// gapOK reports whether the bytes between two atoms are only whitespace,
// newlines, and SGR spans — the gap glamour leaves inside one link.
func gapOK(gap string) bool {
	for i := 0; i < len(gap); {
		c := gap[i]
		if c == ' ' || c == '\n' || c == '\t' {
			i++
			continue
		}
		if c == 0x1b {
			// skip one SGR span
			j := i + 1
			for j < len(gap) && gap[j] != 'm' {
				j++
			}
			if j >= len(gap) {
				return false
			}
			i = j + 1
			continue
		}
		return false // visible text in the gap
	}
	return true
}

// hyperlinkGlamourLinks rewrites glamour's rendered links into OSC 8
// hyperlinks. A [label](href) group collapses to the clickable label (the
// href stops printing); a standalone href (autolink) becomes clickable in
// place. Hrefs that don't map to a clickable target (anchors, missing files)
// keep glamour's plain output.
func hyperlinkGlamourLinks(s string, exists func(string) bool) string {
	atoms := parseLinkAtoms(s)
	if len(atoms) == 0 {
		return s
	}
	groups := groupLinkAtoms(s, atoms)

	// Decide each group's replacement and target before splicing.
	type repl struct {
		start, end int
		out        string
	}
	var repls []repl
	for _, g := range groups {
		if len(g.hrefs) == 0 {
			continue // label with no href (anchor-only link): glamour already
			// prints just the label; nothing to rewire
		}
		var hrefText string
		for _, h := range g.hrefs {
			hrefText += strings.ReplaceAll(h.text, "\n", "")
		}
		uri := targetURI(hrefText, exists)
		if uri == "" {
			continue // leave glamour's output untouched
		}
		if !g.hasLabel {
			// autolink: every fragment stays visible, wrapped in one target.
			// Glamour may have split the URL across lines; rejoin it so the
			// click target is the whole URL, not a fragment.
			var out strings.Builder
			for _, h := range g.hrefs {
				out.WriteString(hyperlink(uri, h.sgr+h.text+sgrReset))
			}
			repls = append(repls, repl{g.start, g.end, out.String()})
			continue
		}
		// label+href: clickable label, href and gap dropped
		var label strings.Builder
		for _, l := range g.labels {
			label.WriteString(l.sgr + l.text + sgrReset)
		}
		repls = append(repls, repl{g.start, g.end, hyperlink(uri, label.String())})
	}

	// Splice replacements back, copying untouched regions verbatim.
	var b strings.Builder
	b.Grow(len(s) + len(s)/4)
	last := 0
	for _, r := range repls {
		b.WriteString(s[last:r.start])
		b.WriteString(r.out)
		last = r.end
	}
	b.WriteString(s[last:])
	return b.String()
}

// linkAtomAt reports whether s starts with a link SGR span, returning the
// span and 't' (LinkText/label) or 'h' (Link/href).
func linkAtomAt(s string) (string, byte) {
	for _, cand := range []struct {
		sgr  string
		kind byte
	}{
		{linkTextSGRDark, 't'}, {linkTextSGRLight, 't'},
		{linkTextSGRANSI, 't'},
		{linkSGRDark, 'h'}, {linkSGRLight, 'h'}, {linkSGRANSI, 'h'},
	} {
		if strings.HasPrefix(s, cand.sgr) {
			return cand.sgr, cand.kind
		}
	}
	return "", 0
}

// scanAtom returns the end offset (exclusive) and visible text of the atom
// opened at s[start] with the given SGR span: a newline inside the atom ends
// the visible text (glamour's word wrap), and the atom closes at the first
// following reset.
func scanAtom(s string, start int, sgr string) (end int, text string) {
	body := start + len(sgr)
	if body > len(s) {
		return -1, ""
	}
	rest := s[body:]
	nl := strings.IndexByte(rest, '\n')
	rs := strings.Index(rest, sgrReset)
	if rs < 0 {
		if nl >= 0 {
			return body + nl + 1, rest[:nl+1]
		}
		return len(s), rest
	}
	if nl >= 0 && nl < rs {
		// Glamour can leave a link span open across a wrapped line and emit
		// the reset after the next line's placeholder span. Keep this atom at
		// the newline so the following span remains independently parseable.
		return body + nl + 1, rest[:nl+1]
	}
	return body + rs + len(sgrReset), rest[:rs]
}

// linkifyRenderedFilePaths is linkifyFilePaths for glamour's output: the
// renderer splits text into word atoms separated by SGR sequences, so the
// pre-render injection would wrap mid-sequence. Runs on the rendered string
// where every file ref appears contiguous inside one word atom. The same
// preceding-byte skips apply (ESC from styling, hrefs already handled by
// hyperlinkGlamourLinks).
func linkifyRenderedFilePaths(s string, exists func(string) bool) string {
	return replaceMatches(s, fileRefRE, func(m string, before byte) string {
		if before == 0x1b || strings.ContainsRune("([]/:;\"`m", rune(before)) {
			// ESC or 'm': inside an SGR/OSC 8 sequence (an atom's text starts
			// right after its span's closing 'm'). Others: markdown internals,
			// code spans, quotes.
			return m
		}
		path, line := splitLineRef(m)
		if !exists(path) {
			return m
		}
		return hyperlink(absFileURI(path, line), m)
	})
}

// targetURI maps a link destination to a clickable URI: absolute URLs pass
// through; existing local files become file://; anything else (anchors,
// missing files) returns "" so the caller keeps the unlinked rendering.
func targetURI(dest string, exists func(string) bool) string {
	low := strings.ToLower(dest)
	if strings.HasPrefix(low, "http://") || strings.HasPrefix(low, "https://") ||
		strings.HasPrefix(low, "mailto:") || strings.HasPrefix(low, "file://") {
		return dest
	}
	if strings.HasPrefix(dest, "#") {
		return ""
	}
	path, line := splitLineRef(dest)
	// Resolve in order: as written (absolute or CWD-relative), then — for a
	// leading "/" — glamour's normalization of "./x" as CWD-relative. The
	// model writes "./docs/features.md"; glamour renders the href "/docs/…",
	// so the dot-relative reading is what makes those clickable.
	candidates := []string{path}
	if strings.HasPrefix(path, "/") && !strings.HasPrefix(path, "//") {
		candidates = append(candidates, "."+path)
	}
	for _, c := range candidates {
		if exists(c) {
			return absFileURI(c, line)
		}
	}
	return ""
}

// renderMarkdown renders assistant message text as rich terminal markdown
// (glamour): headings, bold/italic, lists, fenced code blocks, tables.
// Falls back to the raw input when parsing fails — a degraded transcript is
// never worth a broken one.
//
// The style is a hardcoded dark variant (never WithEnvironmentConfig): an
// OSC background query mid-session can hang over mosh/tmux, and the TUI
// already commits to plain ANSI colors everywhere else.
func renderMarkdown(s string, width int) string {
	if strings.TrimSpace(s) == "" {
		return s
	}
	width = max(width, 8) // glamour treats width<=0 as its ~80-col default
	out, err := mdRenderer(width).Render(s)
	if err != nil {
		return s
	}
	rendered := stripLinePadding(strings.Trim(out, "\n"))
	linked := hyperlinkGlamourLinks(rendered, realFileExists)
	linked = linkifyRenderedFilePaths(linked, realFileExists)
	return wrapWideLines(linked, width)
}

// wrapWideLines hard-wraps any rendered line still wider than width.
// Glamour never breaks code-fence or table content, so a long line overflows
// the terminal; ansi.Hardwrap is cell- and escape-aware (styles stay intact).
func wrapWideLines(s string, width int) string {
	lines := strings.Split(s, "\n")
	for i, l := range lines {
		if ansi.StringWidth(l) > width {
			lines[i] = ansi.Hardwrap(l, width, true) // ANSI-aware, breaks mid-word
		}
	}
	return strings.Join(lines, "\n")
}

// padStripRE matches glamour's right-padding at end of line: runs of (SGR
// sequence [empty params allowed — bare \x1b[m], spaces), optionally closed
// by a final SGR reset. The reset is kept (captured group) so a line's
// styling never bleeds into the next block.
var padStripRE = regexp.MustCompile(`(?:\x1b\[[0-9;]*m[ \t]*)+(\x1b\[[0-9;]*m)?$`)

// stripLinePadding removes glamour's right-padding: it pads every line to
// the full render width with individually styled spaces, which bloats the
// transcript 10-20x and breaks terminal select/copy. Lines whose visible
// content is empty (blank separators) become truly empty — no styled blank
// rows. Leading indentation and styled content are untouched.
func stripLinePadding(s string) string {
	lines := strings.Split(s, "\n")
	for i, l := range lines {
		l = padStripRE.ReplaceAllString(l, "$1")
		l = strings.TrimRight(l, " \t")
		if ansi.StringWidth(l) == 0 || strings.TrimSpace(ansi.Strip(l)) == "" {
			l = "" // blank separator line: drop any leftover styling entirely
		}
		lines[i] = l
	}
	return strings.Join(lines, "\n")
}

var (
	mdMu          sync.Mutex
	mdRenderers   = make(map[int]*glamour.TermRenderer)
	mdRendererErr bool
)

// mdStyle keeps body text and backgrounds at terminal defaults. Only semantic
// accents use the standard ANSI palette.
func mdStyle() glamouransi.StyleConfig {
	st := styles.DarkStyleConfig
	for _, primitive := range []*glamouransi.StylePrimitive{
		&st.Document.StylePrimitive, &st.BlockQuote.StylePrimitive, &st.Paragraph.StylePrimitive,
		&st.List.StylePrimitive, &st.Heading.StylePrimitive,
		&st.H1.StylePrimitive, &st.H2.StylePrimitive, &st.H3.StylePrimitive,
		&st.H4.StylePrimitive, &st.H5.StylePrimitive, &st.H6.StylePrimitive,
		&st.Text, &st.Strikethrough, &st.Emph, &st.Strong, &st.HorizontalRule,
		&st.Item, &st.Enumeration, &st.Task.StylePrimitive, &st.Link, &st.LinkText,
		&st.Image, &st.ImageText, &st.Code.StylePrimitive, &st.CodeBlock.StylePrimitive,
		&st.Table.StylePrimitive, &st.DefinitionList.StylePrimitive,
		&st.DefinitionTerm, &st.DefinitionDescription, &st.HTMLBlock.StylePrimitive,
		&st.HTMLSpan.StylePrimitive,
	} {
		primitive.Color = nil
		primitive.BackgroundColor = nil
	}
	st.CodeBlock.Chroma = nil
	st.CodeBlock.Theme = ""
	bold, italic, underline := true, true, true
	cyan := "6"
	magenta := "5"
	st.Heading.Bold = &bold
	st.Link.Color = &cyan
	st.Link.Underline = &underline
	st.LinkText.Color = &magenta
	st.LinkText.Bold = &bold
	st.Strong.Bold = &bold
	st.Emph.Italic = &italic
	st.Table.ColumnSeparator = strPtr("│")
	st.Table.CenterSeparator = strPtr("┼")
	st.Table.RowSeparator = strPtr("─")
	zero := uint(0)
	st.Table.Margin = &zero
	return st
}

func strPtr(s string) *string { return &s }

// mdRenderer returns a cached renderer per width (glamour builds a
// style-traversed renderer per Render call otherwise).
func mdRenderer(width int) *glamour.TermRenderer {
	mdMu.Lock()
	defer mdMu.Unlock()
	if mdRendererErr {
		return nil
	}
	if r, ok := mdRenderers[width]; ok {
		return r
	}
	st := mdStyle()
	margin := uint(2)
	st.Document.Margin = &margin
	r, err := glamour.NewTermRenderer(
		glamour.WithStyles(st),
		glamour.WithWordWrap(width),
		glamour.WithPreservedNewLines(), // streamed text keeps its line breaks verbatim
	)
	if err != nil { // style is built-in; only reachable on a broken build
		mdRendererErr = true
		return nil
	}
	if len(mdRenderers) >= 8 {
		clear(mdRenderers)
	}
	mdRenderers[width] = r
	return r
}

// bareSGR is the empty SGR escape (\x1b[m) lipgloss' Width().Render appends
// before its right-padding; some terminals render the empty parameter list
// inconsistently, and the styled pad shows up as visual smear. Normalize it
// to a proper reset.
var bareSGR = strings.NewReplacer("\x1b[m", "\x1b[0m")

// sanitizeView cleans one rendered screen: bare SGR escapes become real
// resets and trailing style+space tails (lipgloss/viewport padding) are
// trimmed from each line.
func sanitizeView(s string) string {
	if !strings.Contains(s, "\x1b[") {
		return s
	}
	s = bareSGR.Replace(s)
	lines := strings.Split(s, "\n")
	for i, l := range lines {
		if strings.Contains(l, "\x1b[") {
			lines[i] = padStripRE.ReplaceAllString(l, "$1")
		}
	}
	return strings.Join(lines, "\n")
}

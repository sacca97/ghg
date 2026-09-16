package tui

import (
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"

	"github.com/charmbracelet/x/ansi"
)

var fileRefRE = regexp.MustCompile(
	`/?[\w@+~-][\w@+~.-]*(?:/[\w@+~-][\w@+~.-]*)+(?::\d+)?` +
		`|/?[\w@+~-][\w@+~.-]*\.[A-Za-z]{2,10}(?::\d+)?` +
		`|\.{1,2}/[\w@+~-][\w@+~.-]*(?:/[\w@+~-][\w@+~.-]*)*(?::\d+)?`)

var markdownURLRE = regexp.MustCompile(`https?://[^\s<>()]+`)

var fileExistsCache struct {
	sync.Mutex
	paths map[string]bool
}

// linkifyFilePaths wraps existing local files in OSC 8 hyperlinks.
func linkifyFilePaths(s string, exists func(string) bool) string {
	return replaceMatches(s, fileRefRE, func(m string, before byte) string {
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

func isFileRef(path string) bool {
	dot := strings.LastIndexByte(path, '.')
	if dot < 0 {
		return strings.Contains(path, "/")
	}
	ext := path[dot+1:]
	if strings.ContainsRune(ext, '/') {
		return false
	}
	return len(ext) >= 1
}

func hyperlink(uri, text string) string {
	return ansi.SetHyperlink(uri) + text + ansi.ResetHyperlink()
}

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

func realFileExists(path string) bool {
	if !filepath.IsAbs(path) {
		wd, err := os.Getwd()
		if err != nil {
			return false
		}
		path = filepath.Join(wd, path)
	}
	path = filepath.Clean(path)
	fileExistsCache.Lock()
	defer fileExistsCache.Unlock()
	if fileExistsCache.paths == nil {
		fileExistsCache.paths = make(map[string]bool)
	}
	if found, ok := fileExistsCache.paths[path]; ok {
		return found
	}
	info, err := os.Stat(path)
	found := err == nil && info.Mode().IsRegular()
	if len(fileExistsCache.paths) >= 1024 {
		clear(fileExistsCache.paths)
	}
	fileExistsCache.paths[path] = found
	return found
}

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

// targetURI maps web URLs and existing local paths to clickable URIs.
func targetURI(dest string, exists func(string) bool) string {
	dest = strings.TrimSpace(dest)
	low := strings.ToLower(dest)
	if strings.HasPrefix(low, "http://") || strings.HasPrefix(low, "https://") ||
		strings.HasPrefix(low, "mailto:") || strings.HasPrefix(low, "file://") {
		return dest
	}
	if strings.HasPrefix(dest, "#") {
		return ""
	}
	path, line := splitLineRef(dest)
	candidates := []string{path}
	if strings.HasPrefix(path, "/") && !strings.HasPrefix(path, "//") {
		candidates = append(candidates, "."+path)
	}
	for _, candidate := range candidates {
		if exists(candidate) {
			return absFileURI(candidate, line)
		}
	}
	return ""
}

const (
	markdownHeadingSGR = "\x1b[1;35m"
	markdownBoldSGR    = "\x1b[1m"
	markdownItalicSGR  = "\x1b[3m"
	markdownCodeSGR    = "\x1b[2m"
	markdownResetSGR   = "\x1b[0m"
)

func markdownStyle(sgr, text string) string {
	return sgr + text + markdownResetSGR
}

// renderMarkdown implements the small markdown subset used in the transcript.
func renderMarkdown(s string, width int) string {
	if strings.TrimSpace(s) == "" {
		return s
	}
	width = max(width, 8)
	lines := strings.Split(s, "\n")
	rendered := make([]string, 0, len(lines))
	inFence := false
	for i := 0; i < len(lines); i++ {
		line := lines[i]
		trimmed := strings.TrimSpace(line)
		if isFence(trimmed) {
			inFence = !inFence
			continue
		}
		if inFence {
			rendered = append(rendered, markdownStyle(markdownCodeSGR, "│ "+line))
			continue
		}
		if cells, ok := tableCells(line); ok && i+1 < len(lines) {
			separatorCells, separator := tableCells(lines[i+1])
			if separator && tableSeparator(separatorCells) {
				end := i + 2
				for end < len(lines) {
					if _, ok := tableCells(lines[end]); !ok {
						break
					}
					end++
				}
				rendered = append(rendered, renderTable(cells, lines[i+2:end], width)...)
				i = end - 1
				continue
			}
		}
		if heading, ok := headingText(line); ok {
			rendered = append(rendered, markdownStyle(markdownHeadingSGR, renderInline(heading)))
			continue
		}
		if prefix, item, ok := listItem(line); ok {
			rendered = append(rendered, prefix+"• "+renderInline(item))
			continue
		}
		rendered = append(rendered, renderInline(line))
	}
	return wrapWideLines(strings.Trim(strings.Join(rendered, "\n"), "\n"), width)
}

func isFence(line string) bool {
	return strings.HasPrefix(line, "```") || strings.HasPrefix(line, "~~~")
}

func headingText(line string) (string, bool) {
	trimmed := strings.TrimLeft(line, " \t")
	space := strings.IndexByte(trimmed, ' ')
	if space < 1 || space > 6 || strings.Trim(trimmed[:space], "#") != "" {
		return "", false
	}
	return strings.TrimSpace(trimmed[space+1:]), true
}

func listItem(line string) (prefix, item string, ok bool) {
	trimmed := strings.TrimLeft(line, " \t")
	prefix = line[:len(line)-len(trimmed)]
	for _, marker := range []string{"- ", "* ", "+ "} {
		if strings.HasPrefix(trimmed, marker) {
			return prefix, trimmed[len(marker):], true
		}
	}
	for i := 0; i < len(trimmed); i++ {
		if trimmed[i] < '0' || trimmed[i] > '9' {
			break
		}
		if i+2 < len(trimmed) && (trimmed[i+1] == '.' || trimmed[i+1] == ')') && trimmed[i+2] == ' ' {
			return prefix, trimmed[i+3:], true
		}
	}
	return "", "", false
}

func tableCells(line string) ([]string, bool) {
	trimmed := strings.TrimSpace(line)
	if !strings.HasPrefix(trimmed, "|") || !strings.HasSuffix(trimmed, "|") {
		return nil, false
	}
	body := strings.Trim(trimmed, "|")
	if body == "" {
		return nil, false
	}
	parts := strings.Split(body, "|")
	for i := range parts {
		parts[i] = strings.TrimSpace(parts[i])
	}
	return parts, true
}

func tableSeparator(cells []string) bool {
	if len(cells) == 0 {
		return false
	}
	for _, cell := range cells {
		cell = strings.Trim(strings.TrimSpace(cell), ":")
		if len(cell) < 3 || strings.Trim(cell, "-") != "" {
			return false
		}
	}
	return true
}

func renderTable(header []string, rows []string, width int) []string {
	out := []string{renderInline(strings.Join(header, " │ "))}
	ruleWidth := min(width, max(1, ansi.StringWidth(out[0])))
	out = append(out, markdownStyle(markdownCodeSGR, strings.Repeat("─", ruleWidth)))
	for _, row := range rows {
		cells, ok := tableCells(row)
		if !ok || tableSeparator(cells) {
			continue
		}
		out = append(out, renderInline(strings.Join(cells, " │ ")))
	}
	return out
}

func renderInline(s string) string {
	var b strings.Builder
	plainStart := 0
	flushPlain := func(end int) {
		if end > plainStart {
			b.WriteString(linkifyURLs(linkifyFilePaths(s[plainStart:end], realFileExists)))
		}
	}
	for i := 0; i < len(s); {
		if s[i] == '`' {
			end := strings.IndexByte(s[i+1:], '`')
			if end >= 0 {
				end += i + 1
				flushPlain(i)
				b.WriteString(markdownStyle(markdownCodeSGR, s[i+1:end]))
				i = end + 1
				plainStart = i
				continue
			}
		}
		if strings.HasPrefix(s[i:], "**") || strings.HasPrefix(s[i:], "__") {
			marker := s[i : i+2]
			if end := strings.Index(s[i+2:], marker); end >= 0 {
				end += i + 2
				flushPlain(i)
				b.WriteString(markdownStyle(markdownBoldSGR, renderInline(s[i+2:end])))
				i = end + 2
				plainStart = i
				continue
			}
		}
		if s[i] == '*' || s[i] == '_' {
			marker := s[i]
			if end := strings.IndexByte(s[i+1:], marker); end >= 0 {
				end += i + 1
				flushPlain(i)
				b.WriteString(markdownStyle(markdownItalicSGR, renderInline(s[i+1:end])))
				i = end + 1
				plainStart = i
				continue
			}
		}
		if s[i] == '[' {
			label, dest, end, ok := markdownLinkAt(s, i)
			if ok {
				flushPlain(i)
				label = renderInline(label)
				if uri := targetURI(dest, realFileExists); uri != "" {
					b.WriteString(hyperlink(uri, label))
				} else {
					b.WriteString(label)
				}
				i = end
				plainStart = i
				continue
			}
		}
		i++
	}
	flushPlain(len(s))
	return b.String()
}

func markdownLinkAt(s string, start int) (label, dest string, end int, ok bool) {
	closeLabel := strings.IndexByte(s[start+1:], ']')
	if closeLabel < 0 {
		return "", "", 0, false
	}
	closeLabel += start + 1
	if closeLabel+1 >= len(s) || s[closeLabel+1] != '(' {
		return "", "", 0, false
	}
	closeDest := strings.IndexByte(s[closeLabel+2:], ')')
	if closeDest < 0 {
		return "", "", 0, false
	}
	closeDest += closeLabel + 2
	return s[start+1 : closeLabel], s[closeLabel+2 : closeDest], closeDest + 1, true
}

func linkifyURLs(s string) string {
	return replaceMatches(s, markdownURLRE, func(m string, _ byte) string {
		urlText := strings.TrimRight(m, ".,;:!?")
		return hyperlink(urlText, urlText) + m[len(urlText):]
	})
}

func wrapWideLines(s string, width int) string {
	lines := strings.Split(s, "\n")
	for i, line := range lines {
		lines[i] = ansi.Wordwrap(line, width, " \t")
		parts := strings.Split(lines[i], "\n")
		for j, part := range parts {
			if ansi.StringWidth(part) > width {
				parts[j] = ansi.Hardwrap(part, width, true)
			}
		}
		lines[i] = strings.Join(parts, "\n")
	}
	return strings.Join(lines, "\n")
}

var trailingSGRRE = regexp.MustCompile(`(?:\x1b\[[0-9;]*m[ \t]*)+(\x1b\[[0-9;]*m)?$`)

var bareSGR = strings.NewReplacer("\x1b[m", "\x1b[0m")

func sanitizeView(s string) string {
	if !strings.Contains(s, "\x1b[") {
		return s
	}
	s = bareSGR.Replace(s)
	lines := strings.Split(s, "\n")
	for i, line := range lines {
		if strings.Contains(line, "\x1b[") {
			lines[i] = trailingSGRRE.ReplaceAllString(line, "$1")
		}
	}
	return strings.Join(lines, "\n")
}

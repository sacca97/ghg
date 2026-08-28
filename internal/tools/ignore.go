package tools

import (
	"bufio"
	"bytes"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"path"
	"regexp"
	"strings"
	"unicode/utf8"
)

const maxIgnoreFileBytes = 1 << 20

// ignoreRule is one .gitignore pattern. base is the directory containing the
// ignore file, relative to the search root, and is always a slash-separated
// fs path. Gitignore patterns without a slash match any basename below base;
// patterns with a slash are relative to base.
type ignoreRule struct {
	base          string
	regex         *regexp.Regexp
	basename      bool
	directoryOnly bool
	negated       bool
}

type ignoreTree struct {
	fsys  fs.FS
	rules map[string][]ignoreRule
}

func newIgnoreTree(fsys fs.FS) *ignoreTree {
	return &ignoreTree{
		fsys:  fsys,
		rules: make(map[string][]ignoreRule),
	}
}

// ensureDir loads the rules that apply below dir. Rules are cached as the
// accumulated ancestor list, so matching a path never has to reread a file.
func (t *ignoreTree) ensureDir(dir string) error {
	dir = cleanFSPath(dir)
	if _, ok := t.rules[dir]; ok {
		return nil
	}
	if dir == "." {
		local, err := readIgnoreFile(t.fsys, dir)
		if err != nil {
			return err
		}
		t.rules[dir] = local
		return nil
	}
	parent := path.Dir(dir)
	if err := t.ensureDir(parent); err != nil {
		return err
	}

	local, err := readIgnoreFile(t.fsys, dir)
	if err != nil {
		return err
	}
	combined := append([]ignoreRule(nil), t.rules[parent]...)
	combined = append(combined, local...)
	t.rules[dir] = combined
	return nil
}

// ignored applies rules in ancestor order. An ignored directory blocks
// negations below it, matching Git's rule that a file cannot be re-included if
// one of its parent directories remains excluded.
func (t *ignoreTree) ignored(name string, isDir bool) (bool, error) {
	name = cleanFSPath(name)
	if name == "." {
		return false, nil
	}
	if err := t.ensureDir(path.Dir(name)); err != nil {
		return false, err
	}

	rules := t.rules[path.Dir(name)]
	parts := strings.Split(name, "/")
	ignored := false
	for i := range parts {
		if ignored {
			return true, nil
		}
		current := strings.Join(parts[:i+1], "/")
		currentIsDir := i < len(parts)-1 || isDir
		for _, rule := range rules {
			if !rule.matches(current, currentIsDir) {
				continue
			}
			ignored = !rule.negated
		}
	}
	return ignored, nil
}

func (r ignoreRule) matches(name string, isDir bool) bool {
	if r.directoryOnly && !isDir {
		return false
	}
	rel, ok := relativeFSPath(r.base, name)
	if !ok {
		return false
	}
	if r.basename {
		rel = path.Base(rel)
	}
	return r.regex.MatchString(rel)
}

func readIgnoreFile(fsys fs.FS, dir string) ([]ignoreRule, error) {
	name := ".gitignore"
	if dir != "." {
		name = path.Join(dir, name)
	}
	f, err := fsys.Open(name)
	if err != nil {
		if fsysErrNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("read %s: %w", name, err)
	}
	defer func() { _ = f.Close() }()

	data, err := io.ReadAll(io.LimitReader(f, maxIgnoreFileBytes+1))
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", name, err)
	}
	if len(data) > maxIgnoreFileBytes {
		return nil, fmt.Errorf("read %s: file exceeds %d-byte limit", name, maxIgnoreFileBytes)
	}
	return parseIgnoreFile(name, dir, data)
}

func parseIgnoreFile(source, base string, data []byte) ([]ignoreRule, error) {
	scanner := bufio.NewScanner(bytes.NewReader(data))
	scanner.Buffer(make([]byte, 64<<10), maxIgnoreFileBytes)
	var rules []ignoreRule
	lineNo := 0
	for scanner.Scan() {
		lineNo++
		line := strings.TrimSuffix(scanner.Text(), "\r")
		line = trimIgnoreWhitespace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}

		negated := false
		if strings.HasPrefix(line, `\#`) {
			line = line[1:]
		} else if strings.HasPrefix(line, "!") {
			negated = true
			line = line[1:]
		} else if strings.HasPrefix(line, `\!`) {
			line = line[1:]
		}
		if line == "" {
			continue
		}

		directoryOnly := strings.HasSuffix(line, "/")
		line = strings.TrimSuffix(line, "/")
		anchored := strings.HasPrefix(line, "/") || strings.HasPrefix(line, "./")
		line = strings.TrimPrefix(line, "/")
		line = strings.TrimPrefix(line, "./")
		if line == "" {
			continue
		}

		regex, err := compileGlobPattern(line)
		if err != nil {
			return nil, fmt.Errorf("%s:%d: invalid ignore pattern %q: %w", source, lineNo, line, err)
		}
		rules = append(rules, ignoreRule{
			base:          base,
			regex:         regex,
			basename:      !anchored && !strings.Contains(line, "/"),
			directoryOnly: directoryOnly,
			negated:       negated,
		})
	}
	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("%s: %w", source, err)
	}
	return rules, nil
}

func trimIgnoreWhitespace(line string) string {
	line = strings.TrimLeft(line, " \t")
	for len(line) > 0 {
		last := line[len(line)-1]
		if last != ' ' && last != '\t' {
			break
		}
		backslashes := 0
		for i := len(line) - 2; i >= 0 && line[i] == '\\'; i-- {
			backslashes++
		}
		if backslashes%2 == 1 {
			break
		}
		line = line[:len(line)-1]
	}
	return line
}

func fsysErrNotExist(err error) bool {
	return errors.Is(err, fs.ErrNotExist)
}

func cleanFSPath(name string) string {
	name = strings.ReplaceAll(name, "\\", "/")
	name = path.Clean(name)
	if name == "" {
		return "."
	}
	return name
}

func relativeFSPath(base, name string) (string, bool) {
	base = cleanFSPath(base)
	name = cleanFSPath(name)
	if base == "." {
		return name, true
	}
	if name == base {
		return ".", true
	}
	prefix := base + "/"
	if !strings.HasPrefix(name, prefix) {
		return "", false
	}
	return strings.TrimPrefix(name, prefix), true
}

// compileGlobPattern compiles the useful path subset shared by glob and
// .gitignore: *, ?, character classes, and ** across path separators. It
// returns an anchored expression so callers cannot accidentally match a
// substring of a path.
func compileGlobPattern(pattern string) (*regexp.Regexp, error) {
	var b strings.Builder
	b.WriteByte('^')
	for i := 0; i < len(pattern); {
		switch pattern[i] {
		case '*':
			if i+1 < len(pattern) && pattern[i+1] == '*' {
				i += 2
				for i < len(pattern) && pattern[i] == '*' {
					i++
				}
				if i < len(pattern) && pattern[i] == '/' {
					b.WriteString(`(?:.*/)?`)
					i++
				} else {
					b.WriteString(`.*`)
				}
				continue
			}
			b.WriteString(`[^/]*`)
			i++
		case '?':
			b.WriteString(`[^/]`)
			i++
		case '[':
			end, ok := globClassEnd(pattern, i+1)
			if !ok {
				return nil, fmt.Errorf("unterminated character class")
			}
			class := pattern[i : end+1]
			if len(class) > 1 && class[1] == '!' {
				class = "[^" + class[2:]
			}
			b.WriteString(class)
			i = end + 1
		case '\\':
			if i+1 >= len(pattern) {
				b.WriteString(`\\`)
				i++
				continue
			}
			r, size := utf8.DecodeRuneInString(pattern[i+1:])
			b.WriteString(regexp.QuoteMeta(string(r)))
			i += 1 + size
		default:
			r, size := utf8.DecodeRuneInString(pattern[i:])
			b.WriteString(regexp.QuoteMeta(string(r)))
			i += size
		}
	}
	b.WriteByte('$')
	return regexp.Compile(b.String())
}

func globClassEnd(pattern string, start int) (int, bool) {
	for i := start; i < len(pattern); i++ {
		if pattern[i] == '\\' {
			i++
			continue
		}
		if pattern[i] == ']' && i > start {
			return i, true
		}
	}
	return 0, false
}

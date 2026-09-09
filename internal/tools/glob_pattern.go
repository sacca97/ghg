package tools

import (
	"fmt"
	"path"
	"regexp"
	"strings"
	"unicode/utf8"
)

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

// compileGlobPattern covers the path subset used by glob validation.
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

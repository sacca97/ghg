package tools

import (
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
)

// bashRedirect is a suggestion for a command that is unambiguously an
// inspection operation. It is deliberately not a shell parser: anything with
// operators, substitutions, predicates, or an unfamiliar path stays a real
// bash command.
type bashRedirect struct {
	Message string
	Tool    string
}

var simpleSedRange = regexp.MustCompile(`^'?([0-9]+),([0-9]+)p'?$`)

func redirectBashSearch(command string) (bashRedirect, bool) {
	if strings.TrimSpace(command) == "" || strings.ContainsAny(command, ";&|<>`$\n\r") {
		return bashRedirect{}, false
	}
	tokens := strings.Fields(command)
	if len(tokens) == 0 {
		return bashRedirect{}, false
	}
	for _, token := range tokens {
		if token == "" || strings.HasPrefix(token, "~") {
			return bashRedirect{}, false
		}
	}
	if !redirectPathsStayInWorkspace(tokens[1:]) {
		return bashRedirect{}, false
	}

	switch tokens[0] {
	case "grep":
		if !hasRecursiveFlag(tokens[1:]) || hasAdvancedSearchFlag(tokens[1:]) {
			return bashRedirect{}, false
		}
		return bashRedirect{
			Tool:    "grep",
			Message: "Bash search was not run. Use the dedicated `grep` tool for recursive text search; provide `pattern` (or `patterns`) and optionally `path`/`include`, then use its cursor for more results.",
		}, true
	case "find":
		if len(tokens) > 2 {
			return bashRedirect{}, false
		}
		return bashRedirect{
			Tool:    "glob",
			Message: "Bash exploration was not run. Use the dedicated `glob` tool for exact paths (for example pattern `**/*`) or `find_files` for a fuzzy path search.",
		}, true
	case "ls":
		if !hasRecursiveFlag(tokens[1:]) || hasAdvancedSearchFlag(tokens[1:]) {
			return bashRedirect{}, false
		}
		return bashRedirect{
			Tool:    "glob",
			Message: "Recursive listing was not run. Use the dedicated `glob` tool for exact paths or `find_files` for a fuzzy path search.",
		}, true
	case "cat":
		if len(tokens) != 2 || strings.HasPrefix(tokens[1], "-") {
			return bashRedirect{}, false
		}
		return bashRedirect{
			Tool:    "read",
			Message: "Inspection-only `cat` was not run. Use the `read` tool with `path`, `offset`, and `limit` so the file stays bounded.",
		}, true
	case "sed":
		if len(tokens) != 4 || tokens[1] != "-n" || !simpleSedRange.MatchString(tokens[2]) || strings.HasPrefix(tokens[3], "-") {
			return bashRedirect{}, false
		}
		match := simpleSedRange.FindStringSubmatch(tokens[2])
		start, _ := strconv.Atoi(match[1])
		end, _ := strconv.Atoi(match[2])
		if start <= 0 || end < start {
			return bashRedirect{}, false
		}
		return bashRedirect{
			Tool:    "read",
			Message: "Inspection-only `sed` was not run. Use the `read` tool with `path`, `offset`, and `limit` instead.",
		}, true
	}
	return bashRedirect{}, false
}

func hasRecursiveFlag(tokens []string) bool {
	for _, token := range tokens {
		if token == "-r" || token == "-R" || token == "--recursive" || strings.Contains(token, "R") && strings.HasPrefix(token, "-") {
			return true
		}
	}
	return false
}

func hasAdvancedSearchFlag(tokens []string) bool {
	for _, token := range tokens {
		if strings.HasPrefix(token, "--exclude") || strings.HasPrefix(token, "--include") || strings.HasPrefix(token, "--color") {
			return true
		}
	}
	return false
}

func redirectPathsStayInWorkspace(tokens []string) bool {
	cwd, err := filepath.Abs(".")
	if err != nil {
		return false
	}
	for _, token := range tokens {
		trimmed := strings.Trim(token, "'\"")
		if trimmed == "" || strings.HasPrefix(trimmed, "-") || strings.ContainsAny(trimmed, "*?[]{}") {
			continue
		}
		if filepath.IsAbs(trimmed) {
			abs, err := filepath.Abs(trimmed)
			if err != nil {
				return false
			}
			rel, err := filepath.Rel(cwd, abs)
			if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
				return false
			}
		} else if trimmed == ".." || strings.HasPrefix(trimmed, ".."+string(filepath.Separator)) || strings.HasPrefix(trimmed, "../") {
			return false
		}
	}
	return true
}

func bashPreviewLimit(command string) int {
	if isBashExploration(command) {
		return 8 << 10
	}
	return 14 << 10
}

// isBashExploration identifies the small set of commands whose output is
// normally file/search inspection. It intentionally does not parse a shell
// expression: commands containing operators keep the ordinary preview cap
// because they are an explicit escape hatch.
func isBashExploration(command string) bool {
	if strings.TrimSpace(command) == "" || strings.ContainsAny(command, ";|&<>`$\n\r") {
		return false
	}
	tokens := strings.Fields(command)
	if len(tokens) == 0 {
		return false
	}
	switch filepath.Base(tokens[0]) {
	case "grep", "rg", "find", "ls", "cat", "sed":
		return true
	default:
		return false
	}
}

func boundedTailPreview(s string, limit int) string {
	if limit <= 0 || len(s) <= limit {
		return s
	}
	marker := []byte("[... first " + strconv.Itoa(len(s)-limit) + " bytes truncated]\n")
	if len(marker) >= limit {
		return string([]byte(s)[len(s)-limit:])
	}
	return string(marker) + s[len(s)-(limit-len(marker)):]
}

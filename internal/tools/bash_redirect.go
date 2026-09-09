package tools

import (
	"encoding/json"
	"strconv"
	"strings"
)

type bashRedirect struct {
	Tool    string
	Args    json.RawMessage
	Command string
}

func simpleCommandTokens(command string) ([]string, bool) {
	if strings.TrimSpace(command) == "" || strings.ContainsAny(command, ";&|<>`$\n\r") {
		return nil, false
	}
	tokens := strings.Fields(command)
	return tokens, len(tokens) > 0
}

func redirectBashInspection(command string) (bashRedirect, bool) {
	tokens, ok := simpleInspectionTokens(command)
	if !ok {
		return bashRedirect{}, false
	}
	switch tokens[0] {
	case "cat":
		if len(tokens) == 2 || len(tokens) == 3 && tokens[1] == "--" {
			path := tokens[len(tokens)-1]
			if literalInspectionPath(path) {
				return bashReadRedirect(tokens[0], path, 1, 1)
			}
		}
	case "head":
		if len(tokens) == 2 && literalInspectionPath(tokens[1]) {
			return bashReadRedirect(tokens[0], tokens[1], 1, defaultReadLines)
		}
		if len(tokens) == 4 && tokens[1] == "-n" {
			if limit, ok := positiveInt(tokens[2]); ok {
				if literalInspectionPath(tokens[3]) {
					return bashReadRedirect(tokens[0], tokens[3], 1, limit)
				}
			}
		}
		if len(tokens) == 3 && strings.HasPrefix(tokens[1], "-") {
			if limit, ok := positiveInt(strings.TrimPrefix(tokens[1], "-")); ok {
				if literalInspectionPath(tokens[2]) {
					return bashReadRedirect(tokens[0], tokens[2], 1, limit)
				}
			}
		}
	case "sed":
		if len(tokens) == 4 && tokens[1] == "-n" {
			if start, end, ok := sedRange(tokens[2]); ok {
				if literalInspectionPath(tokens[3]) {
					return bashReadRedirect(tokens[0], tokens[3], start, end-start+1)
				}
			}
		}
	case "grep", "rg":
		if redirect, ok := searchRedirect(tokens); ok {
			return redirect, true
		}
	case "find":
		if len(tokens) == 4 && tokens[2] == "-name" && literalInspectionPath(tokens[1]) && tokens[3] != "" {
			pattern := tokens[3]
			if !strings.Contains(pattern, "/") {
				pattern = "**/" + pattern
			}
			return bashGlobRedirect(tokens[0], tokens[1], pattern)
		}
	}
	return bashRedirect{}, false
}

func simpleInspectionTokens(command string) ([]string, bool) {
	if strings.TrimSpace(command) == "" || strings.ContainsAny(command, ";&|<>`$\\\n\r()") {
		return nil, false
	}
	var tokens []string
	var word strings.Builder
	var quote byte
	flush := func() {
		if word.Len() > 0 {
			tokens = append(tokens, word.String())
			word.Reset()
		}
	}
	for i := 0; i < len(command); i++ {
		ch := command[i]
		if quote != 0 {
			if ch == quote {
				quote = 0
				continue
			}
			word.WriteByte(ch)
			continue
		}
		switch ch {
		case '\'', '"':
			quote = ch
		case ' ', '\t':
			flush()
		default:
			word.WriteByte(ch)
		}
	}
	if quote != 0 {
		return nil, false
	}
	flush()
	return tokens, len(tokens) > 0
}

func bashReadRedirect(command, path string, offset, limit int) (bashRedirect, bool) {
	args, err := json.Marshal(map[string]any{"path": path, "offset": offset, "limit": limit})
	if err != nil {
		return bashRedirect{}, false
	}
	return bashRedirect{
		Tool:    "read",
		Args:    args,
		Command: command,
	}, true
}

func bashGlobRedirect(command, path, pattern string) (bashRedirect, bool) {
	args, err := json.Marshal(map[string]string{"path": path, "pattern": pattern})
	if err != nil {
		return bashRedirect{}, false
	}
	return bashRedirect{
		Tool:    "glob",
		Args:    args,
		Command: command,
	}, true
}

func searchRedirect(tokens []string) (bashRedirect, bool) {
	index := 1
	if index < len(tokens) && tokens[index] == "--" {
		index++
	}
	if index < len(tokens) && (tokens[index] == "-r" || tokens[index] == "-R" || tokens[index] == "-rn") {
		index++
	}
	if len(tokens)-index < 1 || len(tokens)-index > 2 || strings.HasPrefix(tokens[index], "-") {
		return bashRedirect{}, false
	}
	args := map[string]string{"pattern": tokens[index]}
	if len(tokens)-index == 2 {
		if !literalInspectionPath(tokens[index+1]) {
			return bashRedirect{}, false
		}
		args["path"] = tokens[index+1]
	}
	data, err := json.Marshal(args)
	if err != nil {
		return bashRedirect{}, false
	}
	return bashRedirect{
		Tool:    "grep",
		Args:    data,
		Command: tokens[0],
	}, true
}

func positiveInt(value string) (int, bool) {
	parsed, err := strconv.Atoi(value)
	return parsed, err == nil && parsed > 0
}

func literalInspectionPath(value string) bool {
	return value != "" && !strings.HasPrefix(value, "-") && !strings.ContainsAny(value, "*?[")
}

func sedRange(value string) (start, end int, ok bool) {
	if !strings.HasSuffix(value, "p") {
		return 0, 0, false
	}
	value = strings.TrimSuffix(value, "p")
	parts := strings.Split(value, ",")
	if len(parts) != 2 {
		return 0, 0, false
	}
	start, ok = positiveInt(parts[0])
	if !ok {
		return 0, 0, false
	}
	end, ok = positiveInt(parts[1])
	if !ok || end < start {
		return 0, 0, false
	}
	return start, end, true
}

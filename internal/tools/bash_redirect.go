package tools

import (
	"slices"
	"strings"
)

type bashRedirect struct {
	Message string
	Tool    string
}

func simpleCommandTokens(command string) ([]string, bool) {
	if strings.TrimSpace(command) == "" || strings.ContainsAny(command, ";&|<>`$\n\r") {
		return nil, false
	}
	tokens := strings.Fields(command)
	return tokens, len(tokens) > 0
}

func redirectBashInspection(command string) (bashRedirect, bool) {
	tokens := strings.Fields(strings.TrimSpace(command))
	if len(tokens) == 0 || strings.ContainsAny(command, ";&|<>`$\n\r") {
		return bashRedirect{}, false
	}
	switch tokens[0] {
	case "grep":
		if slices.Contains(tokens[1:], "-r") || slices.Contains(tokens[1:], "-rn") {
			return bashRedirect{Tool: "grep", Message: "Use the dedicated `grep` tool for recursive search."}, true
		}
	case "cat", "head":
		if len(tokens) == 2 && !strings.HasPrefix(tokens[1], "-") {
			return bashRedirect{Tool: "read", Message: "Use the dedicated `read` tool to inspect files."}, true
		}
	}
	return bashRedirect{}, false
}

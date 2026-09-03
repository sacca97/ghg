package agent

import "strings"

// CompileSystemPrompt joins the stable prompt with the current session
// additions. Empty additions are omitted so callers can rebuild the prompt
// without having to special-case optional sources.
func CompileSystemPrompt(base string, additions ...string) string {
	parts := make([]string, 0, 1+len(additions))
	if value := strings.TrimRight(base, "\n"); strings.TrimSpace(value) != "" {
		parts = append(parts, value)
	}
	for _, addition := range additions {
		if value := strings.Trim(addition, "\n"); strings.TrimSpace(value) != "" {
			parts = append(parts, value)
		}
	}
	return strings.Join(parts, "\n\n")
}

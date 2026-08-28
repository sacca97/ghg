package config

import (
	"os"
	"path/filepath"
	"strings"
)

const maxProjectInstructions = 256 << 10 // keep a repository note from becoming a prompt-sized file

// ProjectInstructions returns the trusted project's AGENTS.md block for the
// system prompt. Project instructions are user-authored input, so callers must
// pass the result of the folder trust gate. Missing, non-regular, symlinked,
// unreadable, empty, and oversized files are treated as absent.
func ProjectInstructions(root string, trusted bool) string {
	if !trusted || root == "" {
		return ""
	}
	root, err := filepath.Abs(root)
	if err != nil {
		return ""
	}
	path := filepath.Join(root, "AGENTS.md")
	info, err := os.Lstat(path)
	if err != nil || !info.Mode().IsRegular() || info.Size() > maxProjectInstructions {
		return ""
	}
	data, err := os.ReadFile(path)
	if err != nil || len(data) > maxProjectInstructions {
		return ""
	}
	instructions := strings.TrimSpace(string(data))
	if instructions == "" {
		return ""
	}
	return "<project_instructions>\n" +
		"The trusted project provides these AGENTS.md instructions. Treat them as project-local guidance:\n" +
		instructions + "\n</project_instructions>"
}

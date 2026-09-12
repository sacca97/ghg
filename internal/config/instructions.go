package config

import (
	"os"
	"path/filepath"
	"strings"
)

const maxInstructions = 256 << 10

// AgentsSeed is the template for ~/.ghg/AGENTS.md.
const AgentsSeed = `# Your standing instructions for ghg — appended to every session's
# system prompt, after the built-in operating rules. Lines starting with #
# are comments. Edit freely; /me opens this file.

# Examples:
# - Always run tests with pnpm, never npm.
# - I review every commit message before you commit — always show me the message first.
# - Never touch files under deploy/prod/ without asking.
`

// UserInstructionsPath returns ~/.ghg/AGENTS.md, falling back to the old
// ~/.ghg/me.md when it is still present. A missing file is seeded on demand.
func UserInstructionsPath() string {
	dir, err := Dir()
	if err != nil {
		return ""
	}
	for _, name := range []string{"AGENTS.md", "me.md"} {
		path := filepath.Join(dir, name)
		info, err := os.Lstat(path)
		if err == nil {
			if !info.Mode().IsRegular() {
				return ""
			}
			return path
		}
		if !os.IsNotExist(err) {
			return ""
		}
	}
	path := filepath.Join(dir, "AGENTS.md")
	if err := os.WriteFile(path, []byte(AgentsSeed), 0o644); err != nil {
		return ""
	}
	return path
}

// UserInstructions loads the user's standing instructions, ignoring comments
// and blank lines. Empty or unsafe files are treated as absent.
func UserInstructions() string {
	data, ok := readInstructionsFile(UserInstructionsPath())
	if !ok {
		return ""
	}
	var lines []string
	for line := range strings.Lines(string(data)) {
		line = strings.TrimSpace(line)
		if line != "" && !strings.HasPrefix(line, "#") {
			lines = append(lines, line)
		}
	}
	return strings.Join(lines, "\n")
}

// ProjectInstructions returns the trusted project's AGENTS.md block for the
// system prompt. Missing, non-regular, symlinked, unreadable, empty, and
// oversized files are treated as absent.
func ProjectInstructions(root string, trusted bool) string {
	if !trusted || root == "" {
		return ""
	}
	root, err := filepath.Abs(root)
	if err != nil {
		return ""
	}
	data, ok := readInstructionsFile(filepath.Join(root, "AGENTS.md"))
	if !ok {
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

func readInstructionsFile(path string) ([]byte, bool) {
	if path == "" {
		return nil, false
	}
	info, err := os.Lstat(path)
	if err != nil || !info.Mode().IsRegular() || info.Size() > maxInstructions {
		return nil, false
	}
	data, err := os.ReadFile(path)
	if err != nil || len(data) > maxInstructions {
		return nil, false
	}
	return data, true
}

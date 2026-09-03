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

// projects.json holds per-project overrides keyed by absolute folder path.
const DefaultGoalMaxRounds = 100

type projectsFile struct {
	GoalMaxRounds map[string]int `json:"goalMaxRounds,omitempty"`
}

func loadProjects() projectsFile {
	var f projectsFile
	_ = ReadJSON("projects.json", &f)
	return f
}

func (f projectsFile) save() error {
	return WriteJSON("projects.json", f)
}

// ProjectGoalMaxRounds returns the goal-round cap overridden for dir, or 0
// when the project has no override.
func ProjectGoalMaxRounds(dir string) int {
	return loadProjects().GoalMaxRounds[dir]
}

// SetProjectGoalMaxRounds records or clears the goal-round cap override for dir.
func SetProjectGoalMaxRounds(dir string, n int) error {
	f := loadProjects()
	if f.GoalMaxRounds == nil {
		f.GoalMaxRounds = map[string]int{}
	}
	if n > 0 {
		f.GoalMaxRounds[dir] = n
	} else {
		delete(f.GoalMaxRounds, dir)
	}
	return f.save()
}

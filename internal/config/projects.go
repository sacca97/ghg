package config

// projects.json holds per-project overrides keyed by absolute folder path —
// settings that should apply only when working in a given repo, on top of the
// global defaults in config.json.

// DefaultGoalMaxRounds caps goal-loop continuation turns when nothing
// overrides it.
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

// ProjectGoalMaxRounds returns the goal-round cap overridden for dir
// (absolute path), or 0 when the project has no override.
func ProjectGoalMaxRounds(dir string) int {
	return loadProjects().GoalMaxRounds[dir]
}

// SetProjectGoalMaxRounds records (n > 0) or clears (n <= 0) the goal-round
// cap override for dir (absolute path).
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

package skills

import (
	"strings"
	"testing"
)

// The ponytail skill (https://ponytail.dev) ships with the repo and must
// index from the project skills dir. Tests run with the package dir as CWD,
// so point Scan at the repo root explicitly.
func TestPonytailSkillIndexed(t *testing.T) {
	idx := Scan("../../.agents/skills")
	var found, reviewFound bool
	for _, s := range idx {
		switch s.Name {
		case "ponytail":
			found = true
			if !strings.Contains(s.Description, "lazy senior dev") {
				t.Errorf("description: %q", s.Description)
			}
		case "ponytail-review":
			reviewFound = true
			if !s.DisableModelInvocation {
				t.Error("ponytail-review must remain explicit-only")
			}
			if !strings.Contains(s.Description, "over-engineering") {
				t.Errorf("ponytail-review description: %q", s.Description)
			}
		}
	}
	if !found || !reviewFound {
		var names []string
		for _, s := range idx {
			names = append(names, s.Name)
		}
		t.Fatalf("ponytail skills not indexed; got %v", names)
	}
	if strings.Contains(PromptBlock(idx), "ponytail-review") {
		t.Error("explicit-only ponytail-review leaked into the automatic catalog")
	}
}

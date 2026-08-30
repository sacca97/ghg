package skills

import (
	"fmt"
	"testing"
)

// Every skill that ships with the repo must be Agent Skills spec-clean:
// valid name, description ≤1024, parseable frontmatter. This test is the
// ratchet — the startup report warns on violations, and this fails CI.
func TestRepoSkillsSpecClean(t *testing.T) {
	sk, problems := ScanDetailed("../../.agents/skills")
	for _, p := range problems {
		t.Errorf("unparseable skill: %s: %s", p.Path, p.Err)
	}
	for _, s := range sk {
		if s.Warning != "" {
			t.Errorf("%s: %s", s.Name, s.Warning)
		}
	}
}

// The block total should stay sane: it lands in EVERY session's system
// prompt, so growth here is a per-turn tax. The repo catalog contains only
// guidance that encodes ghg-specific risks or workflows; generic Go references
// belong in user-installed skills. The budget is a ratchet, so a new project
// skill has to justify its permanent prompt cost.
func TestSkillBlockBudget(t *testing.T) {
	sk := Scan("../../.agents/skills")
	block := PromptBlock(sk)
	const budget = 8_000 // ≈2k tokens
	if len(block) > budget {
		t.Errorf("skills block = %d chars (budget %d)", len(block), budget)
	}
	fmt.Printf("skills block: %d chars across %d skills\n", len(block), len(sk))
}

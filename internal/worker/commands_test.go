package worker

import "testing"

// The catalogue is the single source of command metadata, so every entry must
// be resolvable and no name or alias may be claimed twice — the drift class
// that used to let the TUI and the extension disagree about usage text.
func TestCommandCatalogueIntegrity(t *testing.T) {
	seen := make(map[string]string)
	for _, spec := range Commands() {
		if spec.Name == "" || spec.Hint == "" {
			t.Fatalf("catalogue entry %+v needs a name and hint", spec)
		}
		if spec.Owner != OwnerWorker && spec.Owner != OwnerClient && spec.Owner != OwnerSupervisor {
			t.Errorf("%s has unknown owner %q", spec.Name, spec.Owner)
		}
		for _, name := range append([]string{spec.Name}, spec.Aliases...) {
			if other, dup := seen[name]; dup {
				t.Errorf("%s is claimed by both %s and %s", name, other, spec.Name)
			}
			seen[name] = spec.Name
			if got := CommandName(name); got != spec.Name {
				t.Errorf("CommandName(%q) = %q, want %q", name, got, spec.Name)
			}
		}
	}
	if len(seen) < 30 {
		t.Fatalf("catalogue looks truncated: %d names", len(seen))
	}
	// Supervisor commands must stay out of the ordinary worker path.
	if spec := FindCommand("/resume"); spec == nil || spec.Owner != OwnerSupervisor {
		t.Fatalf("/resume must be supervisor-owned, got %+v", spec)
	}
	if FindCommand("/nope") != nil || CommandName("/nope") != "/nope" {
		t.Fatal("unknown commands must not resolve")
	}
}

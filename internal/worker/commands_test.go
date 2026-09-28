package worker

import "testing"

// The catalogue is the single source of command metadata, so every entry must
// be resolvable and names must be unique — the drift class
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
		if other, dup := seen[spec.Name]; dup {
			t.Errorf("%s is claimed by both %s and %s", spec.Name, other, spec.Name)
		}
		seen[spec.Name] = spec.Name
		if got := FindCommand(spec.Name); got == nil || got.Name != spec.Name {
			t.Errorf("FindCommand(%q) = %+v", spec.Name, got)
		}
	}
	if len(seen) < 30 {
		t.Fatalf("catalogue looks truncated: %d names", len(seen))
	}
	// Supervisor commands must stay out of the ordinary worker path.
	if spec := FindCommand("/resume"); spec == nil || spec.Owner != OwnerSupervisor {
		t.Fatalf("/resume must be supervisor-owned, got %+v", spec)
	}
	for _, name := range []string{"/export-result", "/export-chat", "/export-log", "/commands", "/agents", "/exit", "/q"} {
		if FindCommand(name) != nil {
			t.Errorf("removed command alias %q still resolves", name)
		}
	}
	if FindCommand("/nope") != nil {
		t.Fatal("unknown commands must not resolve")
	}
}

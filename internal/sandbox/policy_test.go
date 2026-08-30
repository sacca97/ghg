package sandbox

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestPolicyAuthorizesCanonicalWorkspaceAndRejectsEscapes(t *testing.T) {
	workspace := t.TempDir()
	outside := t.TempDir()
	inside := filepath.Join(workspace, "src", "main.go")
	if err := os.MkdirAll(filepath.Dir(inside), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(inside, []byte("package main\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	policy, err := NewPolicy(PolicyConfig{Workspace: workspace, Mode: ModeWorkspaceWrite})
	if err != nil {
		t.Fatal(err)
	}
	got, err := policy.Authorize(filepath.Join(workspace, "src", "..", "src", "main.go"), AccessRead, false)
	want, _ := filepath.EvalSymlinks(inside)
	if err != nil || got != want {
		t.Fatalf("workspace read = %q, %v; want %q", got, err, want)
	}
	if _, err := policy.Authorize(outside, AccessRead, false); !errors.Is(err, ErrOutsideRoot) {
		t.Fatalf("outside read error = %v, want ErrOutsideRoot", err)
	}

	link := filepath.Join(workspace, "link")
	if err := os.Symlink(outside, link); err != nil {
		t.Skipf("symlink unavailable: %v", err)
	}
	if _, err := policy.Authorize(filepath.Join(link, "new.txt"), AccessWrite, true); !errors.Is(err, ErrOutsideRoot) {
		t.Fatalf("symlink escape error = %v, want ErrOutsideRoot", err)
	}
}

func TestPolicyModesAndOneShotGrant(t *testing.T) {
	workspace := t.TempDir()
	outside := t.TempDir()
	cache := filepath.Join(workspace, "cache")
	temp := filepath.Join(workspace, "temp")
	if err := os.MkdirAll(cache, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(temp, 0o700); err != nil {
		t.Fatal(err)
	}
	protected := filepath.Join(workspace, "protected")
	immutable := filepath.Join(workspace, "immutable")
	if err := os.MkdirAll(protected, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(immutable, 0o700); err != nil {
		t.Fatal(err)
	}
	readOnly, err := NewPolicy(PolicyConfig{
		Workspace: workspace, Mode: ModeReadOnly,
		WriteRoots: []string{outside}, CacheRoots: []string{cache}, TempRoots: []string{temp},
		ProtectedRoots: []string{protected}, ImmutableRoots: []string{immutable},
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := readOnly.Authorize(filepath.Join(workspace, "new.txt"), AccessWrite, true); !errors.Is(err, ErrWriteDenied) {
		t.Fatalf("read-only write error = %v, want ErrWriteDenied", err)
	}
	if _, err := readOnly.Authorize(filepath.Join(workspace, "new.txt"), AccessRead, true); err != nil {
		t.Fatalf("read-only read of future path = %v", err)
	}
	for _, root := range []string{cache, temp} {
		if _, err := readOnly.Authorize(filepath.Join(root, "new.txt"), AccessWrite, true); err != nil {
			t.Fatalf("read-only ephemeral write in %q = %v", root, err)
		}
	}
	for _, root := range []string{workspace, outside, protected, immutable} {
		if _, err := readOnly.Authorize(filepath.Join(root, "blocked.txt"), AccessWrite, true); err == nil {
			t.Fatalf("read-only write in %q was authorized", root)
		}
	}
	if _, err := readOnly.Grant(nil, []string{outside}, false); err == nil {
		t.Fatal("read-only policy accepted an ordinary write grant")
	}
	if _, err := readOnly.GrantProtected([]string{protected}); err == nil {
		t.Fatal("read-only policy accepted a protected write grant")
	}
	if granted, err := readOnly.Grant(nil, nil, true); err != nil || granted.Network() != NetworkHost {
		t.Fatalf("read-only network grant = %v, %v", granted, err)
	}

	policy, err := NewPolicy(PolicyConfig{Workspace: workspace, Mode: ModeWorkspaceWrite})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := policy.Authorize(filepath.Join(outside, "file.txt"), AccessWrite, true); !errors.Is(err, ErrOutsideRoot) {
		t.Fatalf("outside write before grant = %v", err)
	}
	granted, err := policy.Grant(nil, []string{outside}, false)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := granted.Authorize(filepath.Join(outside, "file.txt"), AccessWrite, true); err != nil {
		t.Fatalf("granted write = %v", err)
	}
	if _, err := policy.Authorize(filepath.Join(outside, "file.txt"), AccessWrite, true); !errors.Is(err, ErrOutsideRoot) {
		t.Fatalf("parent policy widened by grant: %v", err)
	}
	if granted.Network() != NetworkDeny {
		t.Fatalf("grant without network changed network mode to %q", granted.Network())
	}
	if network, err := policy.Grant(nil, nil, true); err != nil || network.Network() != NetworkHost {
		t.Fatalf("network grant = %v, %v", network, err)
	}
}

func TestImmutableRootsStayReadableAndNeverWritable(t *testing.T) {
	workspace := t.TempDir()
	toolchain := filepath.Join(workspace, "toolchain")
	cache := filepath.Join(workspace, "cache")
	if err := os.MkdirAll(toolchain, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(cache, 0o700); err != nil {
		t.Fatal(err)
	}
	policy, err := NewPolicy(PolicyConfig{
		Workspace:      workspace,
		Mode:           ModeWorkspaceWrite,
		CacheRoots:     []string{cache},
		ImmutableRoots: []string{toolchain},
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := policy.Authorize(filepath.Join(toolchain, "new-file"), AccessRead, true); err != nil {
		t.Fatalf("immutable read = %v", err)
	}
	if _, err := policy.Authorize(filepath.Join(toolchain, "new-file"), AccessWrite, true); err == nil {
		t.Fatal("immutable write was authorized")
	}
	if _, err := policy.Authorize(filepath.Join(cache, "new-file"), AccessWrite, true); err != nil {
		t.Fatalf("cache write = %v", err)
	}
	for _, root := range policy.WriteRoots() {
		if root == policy.CacheRoots()[0] {
			t.Fatal("cache root was folded into ordinary write roots")
		}
	}
	granted, err := policy.Grant(nil, []string{toolchain}, false)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := granted.Authorize(filepath.Join(toolchain, "still-read-only"), AccessWrite, true); err == nil {
		t.Fatal("ordinary grant made immutable root writable")
	}
	status := policy.Status()
	if len(status.ImmutableRoots) != 1 || status.ImmutableRoots[0] != policy.ImmutableRoots()[0] {
		t.Fatalf("status immutable roots=%v policy=%v", status.ImmutableRoots, policy.ImmutableRoots())
	}
}

func TestDangerFullAccessRequiresExplicitMode(t *testing.T) {
	workspace := t.TempDir()
	policy, err := NewPolicy(PolicyConfig{Workspace: workspace, Mode: ModeDangerFull, Network: NetworkHost})
	if err != nil {
		t.Fatal(err)
	}
	outside := filepath.Join(t.TempDir(), "file.txt")
	if _, err := policy.Authorize(outside, AccessWrite, true); err != nil {
		t.Fatalf("explicit full access write = %v", err)
	}
	wrapped, err := policy.WrapCommand(CommandSpec{Program: "echo", Args: []string{"ok"}, Dir: workspace})
	if err != nil {
		t.Fatal(err)
	}
	if wrapped.Program != "echo" || wrapped.Backend == "" {
		t.Fatalf("full access wrapper = %+v", wrapped)
	}
}

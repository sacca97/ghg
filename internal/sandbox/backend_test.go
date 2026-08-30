package sandbox

import (
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func backendTestPolicy(t *testing.T, network NetworkMode) (*Policy, string, string) {
	t.Helper()
	workspace := t.TempDir()
	gitRoot := filepath.Join(workspace, ".git")
	if err := os.Mkdir(gitRoot, 0o755); err != nil {
		t.Fatal(err)
	}
	policy, err := NewPolicy(PolicyConfig{Workspace: workspace, Mode: ModeWorkspaceWrite, Network: network})
	if err != nil {
		t.Fatal(err)
	}
	canonicalGitRoot, ok, err := policy.ProtectedRootFor(gitRoot, true)
	if err != nil || !ok {
		t.Fatalf("protected git root = %q, %v, ok=%v", canonicalGitRoot, err, ok)
	}
	return policy, policy.Workspace(), canonicalGitRoot
}

func TestSeatbeltProfileIsDenyByDefaultAndHonorsNetworkAndProtectedRoots(t *testing.T) {
	policy, workspace, gitRoot := backendTestPolicy(t, NetworkDeny)
	profile := seatbeltProfile(policy)
	if !strings.Contains(profile, "(deny default)") {
		t.Fatal("seatbelt profile is not deny-by-default")
	}
	if strings.Contains(profile, "(allow network*)") {
		t.Fatal("network deny policy emitted a network allow")
	}
	if !strings.Contains(profile, filepath.Clean(workspace)) || !strings.Contains(profile, filepath.Clean(gitRoot)) {
		t.Fatalf("profile omitted workspace or protected metadata:\n%s", profile)
	}
	if !strings.Contains(profile, "(deny file-write*") {
		t.Fatal("profile omitted protected write denial")
	}
	if !strings.Contains(profile, "(allow file-write* (literal \"/dev/null\"))") {
		t.Fatal("profile omitted /dev/null file-write* allow")
	}
	if !strings.Contains(profile, "(allow file-write* (literal \"/dev/ptmx\"))") {
		t.Fatal("profile omitted /dev/ptmx file-write* allow")
	}
	if !strings.Contains(profile, "(allow network* (local unix-socket))") {
		t.Fatal("profile omitted unix-socket allow")
	}

	granted, err := policy.GrantProtected([]string{gitRoot})
	if err != nil {
		t.Fatal(err)
	}
	grantedProfile := seatbeltProfile(granted)
	if strings.Contains(grantedProfile, "(deny file-write* (subpath \""+gitRoot+"\"))") {
		t.Fatalf("human-approved protected root remains denied:\n%s", grantedProfile)
	}
	if !strings.Contains(grantedProfile, "(allow file-write* (subpath \""+gitRoot+"\"))") {
		t.Fatalf("human-approved protected root is not writable:\n%s", grantedProfile)
	}
}

func TestSeatbeltAncestorRootsAreLiteralDeduplicatedAndOrdered(t *testing.T) {
	workspace := filepath.Join(string(filepath.Separator), "Users", "sacca", "workspace", "deep")
	cache := filepath.Join(string(filepath.Separator), "Users", "sacca", "workspace", "cache")
	temp := filepath.Join(string(filepath.Separator), "private", "tmp", "ghg")
	got := seatbeltAncestorRoots([]string{workspace, cache, workspace, temp, "relative"})
	want := []string{
		string(filepath.Separator),
		filepath.Join(string(filepath.Separator), "Users"),
		filepath.Join(string(filepath.Separator), "Users", "sacca"),
		filepath.Join(string(filepath.Separator), "Users", "sacca", "workspace"),
		filepath.Join(string(filepath.Separator), "private"),
		filepath.Join(string(filepath.Separator), "private", "tmp"),
	}
	if strings.Join(got, "\x00") != strings.Join(want, "\x00") {
		t.Fatalf("ancestor roots = %v, want %v", got, want)
	}
}

func TestImmutableRootsAreDeniedAfterWritableBackendGrants(t *testing.T) {
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
	workspace = policy.Workspace()
	toolchain, err = filepath.EvalSymlinks(toolchain)
	if err != nil {
		t.Fatal(err)
	}
	cache, err = filepath.EvalSymlinks(cache)
	if err != nil {
		t.Fatal(err)
	}
	profile := seatbeltProfile(policy)
	allowWorkspace := strings.Index(profile, `(allow file-write* (subpath "`+workspace+`"))`)
	denyToolchain := strings.Index(profile, `(deny file-write* (subpath "`+toolchain+`"))`)
	if allowWorkspace < 0 || denyToolchain < 0 || denyToolchain < allowWorkspace {
		t.Fatalf("immutable denial did not follow writable grant:\n%s", profile)
	}
	if strings.Contains(profile, `(allow file-write* (subpath "`+toolchain+`"))`) {
		t.Fatalf("immutable root received a direct write allow:\n%s", profile)
	}
	args := strings.Join(bubblewrapArgs(policy, CommandSpec{Dir: workspace}), " ")
	bindWorkspace := strings.Index(args, "--bind "+workspace+" "+workspace)
	roToolchain := strings.LastIndex(args, "--ro-bind "+toolchain+" "+toolchain)
	if bindWorkspace < 0 || roToolchain < 0 || roToolchain < bindWorkspace {
		t.Fatalf("immutable read-only rebind did not follow writable mount: %s", args)
	}
	if !strings.Contains(args, "--bind "+cache+" "+cache) {
		t.Fatalf("cache root was not mounted writable: %s", args)
	}
}

func TestBubblewrapArgsContainIsolationAndProtectedRebind(t *testing.T) {
	policy, workspace, gitRoot := backendTestPolicy(t, NetworkDeny)
	args := bubblewrapArgs(policy, CommandSpec{Dir: workspace})
	joined := strings.Join(args, " ")
	for _, required := range []string{"--tmpfs /", "--ro-bind /usr /usr", "--unshare-net", "--no-new-privs", "--bind " + workspace + " " + workspace, "--ro-bind " + gitRoot + " " + gitRoot} {
		if !strings.Contains(joined, required) {
			t.Fatalf("bubblewrap args missing %q: %s", required, joined)
		}
	}
	if strings.Contains(joined, "--ro-bind / /") {
		t.Fatalf("bubblewrap args expose the host root: %s", joined)
	}

	granted, err := policy.GrantProtected([]string{gitRoot})
	if err != nil {
		t.Fatal(err)
	}
	grantedArgs := strings.Join(bubblewrapArgs(granted, CommandSpec{Dir: workspace}), " ")
	if strings.Contains(grantedArgs, "--ro-bind "+gitRoot+" "+gitRoot) {
		t.Fatalf("human-approved protected root is still rebound read-only: %s", grantedArgs)
	}
}

func TestUntrustedBubblewrapPathIsRejected(t *testing.T) {
	dir := t.TempDir()
	if err := os.Chmod(dir, 0o777); err != nil {
		t.Fatal(err)
	}
	fake := filepath.Join(dir, "bwrap")
	if err := os.WriteFile(fake, []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := lookupBubblewrap(fake); err == nil {
		t.Fatal("user-writable bubblewrap path was accepted")
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	if got, err := lookupBubblewrap(""); err == nil && got == fake {
		t.Fatal("PATH-prepended fake bubblewrap was selected")
	}
}

func TestConfiguredBubblewrapPathFailsClosedConsistently(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("bubblewrap configuration is Linux-specific")
	}
	workspace := t.TempDir()
	configured := filepath.Join(t.TempDir(), "missing-bwrap")
	policy, err := NewPolicy(PolicyConfig{Workspace: workspace, Mode: ModeWorkspaceWrite, BubblewrapPath: configured})
	if err != nil {
		t.Fatal(err)
	}
	status := policy.Status()
	if !status.Degraded || status.Backend != "" {
		t.Fatalf("unusable configured backend status = %+v", status)
	}
	if _, err := policy.WrapCommand(CommandSpec{Program: "/bin/echo", Dir: workspace}); !errors.Is(err, ErrSandboxUnavailable) {
		t.Fatalf("wrap error = %v, want ErrSandboxUnavailable", err)
	}
}

func TestRestrictedWrapReportsBackendAvailability(t *testing.T) {
	policy, workspace, _ := backendTestPolicy(t, NetworkDeny)
	status := policy.Status()
	wrapped, err := policy.WrapCommand(CommandSpec{Program: "echo", Args: []string{"ok"}, Dir: workspace})
	if err != nil {
		if !status.Degraded || !errors.Is(err, ErrSandboxUnavailable) {
			t.Fatalf("wrap error=%v status=%+v", err, status)
		}
		return
	}
	if status.Degraded || wrapped.Backend == "" {
		t.Fatalf("wrap succeeded with degraded status: wrapped=%+v status=%+v", wrapped, status)
	}
	switch runtime.GOOS {
	case "darwin":
		if wrapped.Program != "/usr/bin/sandbox-exec" {
			t.Fatalf("macOS wrapper program = %q", wrapped.Program)
		}
	case "linux":
		if wrapped.Backend != "bubblewrap" {
			t.Fatalf("Linux wrapper backend = %q", wrapped.Backend)
		}
	}
}

//go:build darwin

package sandbox

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

func TestSeatbeltDeepWorkspaceExecutableAndBoundary(t *testing.T) {
	root := t.TempDir()
	workspace := filepath.Join(root, "one", "two", "three", "four", "workspace")
	if err := os.MkdirAll(workspace, 0o700); err != nil {
		t.Fatal(err)
	}
	workspace, err := filepath.EvalSymlinks(workspace)
	if err != nil {
		t.Fatal(err)
	}
	parent := filepath.Dir(workspace)
	adjacent := filepath.Join(parent, "adjacent")
	if err := os.Mkdir(adjacent, 0o700); err != nil {
		t.Fatal(err)
	}

	source := filepath.Join(workspace, "source.txt")
	target := filepath.Join(workspace, "written.txt")
	adjacentSecret := filepath.Join(adjacent, "secret.txt")
	siblingSecret := filepath.Join(parent, "sibling-secret.txt")
	gitRoot := filepath.Join(workspace, ".git")
	ghgRoot := filepath.Join(workspace, ".ghg")
	gitTarget := filepath.Join(gitRoot, "index")
	ghgTarget := filepath.Join(ghgRoot, "state")
	for _, dir := range []string{gitRoot, ghgRoot} {
		if err := os.Mkdir(dir, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	for name, contents := range map[string]string{
		source:         "workspace-secret\n",
		adjacentSecret: "adjacent-secret\n",
		siblingSecret:  "sibling-secret\n",
	} {
		if err := os.WriteFile(name, []byte(contents), 0o600); err != nil {
			t.Fatal(err)
		}
	}

	policy, err := NewPolicy(PolicyConfig{Workspace: workspace, Mode: ModeWorkspaceWrite})
	if err != nil {
		t.Fatal(err)
	}
	requireNativeBackend(t, policy)
	env := []string{"PATH=/usr/bin:/bin"}

	run := func(policy *Policy, spec CommandSpec) ([]byte, error) {
		t.Helper()
		wrapped, wrapErr := policy.WrapCommand(spec)
		if wrapErr != nil {
			t.Fatal(wrapErr)
		}
		cmd := exec.Command(wrapped.Program, wrapped.Args...)
		cmd.Dir = wrapped.Dir
		cmd.Env = wrapped.Env
		return cmd.CombinedOutput()
	}

	echoOutput, err := run(policy, CommandSpec{
		Program: "/bin/echo",
		Args:    []string{"seatbelt-ready"},
		Dir:     workspace,
		Env:     env,
	})
	if err != nil || string(echoOutput) != "seatbelt-ready\n" {
		t.Fatalf("wrapped echo output=%q err=%v", echoOutput, err)
	}

	script := `set -eu
[ "$(/bin/cat "$1")" = "workspace-secret" ]
/usr/bin/printf written > "$2"
if /bin/cat "$3" >/dev/null 2>&1; then exit 10; fi
if /bin/cat "$4" >/dev/null 2>&1; then exit 11; fi
if /usr/bin/printf denied > "$5" 2>/dev/null; then exit 12; fi
if /usr/bin/printf denied > "$6" 2>/dev/null; then exit 13; fi
`
	output, err := run(policy, CommandSpec{
		Program: "/bin/sh",
		Args:    []string{"-c", script, "sh", source, target, adjacentSecret, siblingSecret, gitTarget, ghgTarget},
		Dir:     workspace,
		Env:     env,
	})
	if err != nil {
		t.Fatalf("boundary probe output=%q err=%v", output, err)
	}
	if got, err := os.ReadFile(target); err != nil || string(got) != "written" {
		t.Fatalf("workspace write = %q, %v", got, err)
	}
	for _, path := range []string{gitTarget, ghgTarget} {
		if _, err := os.Stat(path); !os.IsNotExist(err) {
			t.Fatalf("protected target %s exists or returned unexpected error: %v", path, err)
		}
	}

	granted, err := policy.GrantProtected([]string{gitRoot, ghgRoot})
	if err != nil {
		t.Fatal(err)
	}
	approvedGit := filepath.Join(gitRoot, "approved")
	approvedGhg := filepath.Join(ghgRoot, "approved")
	output, err = run(granted, CommandSpec{
		Program: "/bin/sh",
		Args:    []string{"-c", `/usr/bin/printf approved > "$1" && /usr/bin/printf approved > "$2"`, "sh", approvedGit, approvedGhg},
		Dir:     workspace,
		Env:     env,
	})
	if err != nil {
		t.Fatalf("approved protected write output=%q err=%v", output, err)
	}
}

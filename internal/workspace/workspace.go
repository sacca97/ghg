// Package workspace runs the Git operations ghg uses to snapshot and restore a
// working tree around a turn. It is deliberately separate from session storage:
// session owns the per-turn snapshot index, this package owns the Git refs.
package workspace

import (
	"bytes"
	"context"
	"fmt"
	"os/exec"
	"strings"
	"time"
)

// Snapshot pins the tracked working tree before a turn and returns the commit
// it recorded ("" when the tree cannot be snapshotted). Untracked files are
// deliberately outside this snapshot contract.
func Snapshot(cwd string) string {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	commit, err := gitOut(ctx, cwd, "stash", "create")
	if err != nil {
		return ""
	}
	if commit == "" {
		commit, err = gitOut(ctx, cwd, "commit-tree", "HEAD^{tree}", "-m", "ghg turn snapshot")
		if err != nil {
			return ""
		}
	}
	if _, err := gitOut(ctx, cwd, "update-ref", "refs/ghg/snapshots/"+commit, commit); err != nil {
		return ""
	}
	return commit
}

// Clean reports whether tracked files match HEAD.
func Clean(cwd string) bool {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	out, err := gitOut(ctx, cwd, "status", "--porcelain", "--untracked-files=no")
	return err == nil && out == ""
}

// Drop removes a pinned snapshot ref.
func Drop(cwd, ref string) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	_, _ = gitOut(ctx, cwd, "update-ref", "-d", "refs/ghg/snapshots/"+ref)
}

// Restore restores tracked files from a snapshot and returns the number of
// dirty tracked files replaced. Untracked files are not removed.
func Restore(cwd, ref string) (int, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	dirty, err := gitOut(ctx, cwd, "status", "--porcelain", "--untracked-files=no")
	if err != nil {
		return 0, err
	}
	if _, err := gitOut(ctx, cwd, "checkout", ref, "--", "."); err != nil {
		return 0, err
	}
	if dirty == "" {
		return 0, nil
	}
	return len(strings.Split(dirty, "\n")), nil
}

func gitOut(ctx context.Context, cwd string, args ...string) (string, error) {
	c := exec.CommandContext(ctx, "git", args...)
	c.Dir = cwd
	var out, errb bytes.Buffer
	c.Stdout, c.Stderr = &out, &errb
	if err := c.Run(); err != nil {
		line, _, _ := strings.Cut(strings.TrimSpace(errb.String()), "\n")
		if line == "" {
			line = err.Error()
		}
		return "", fmt.Errorf("%s", line)
	}
	return strings.TrimSpace(out.String()), nil
}

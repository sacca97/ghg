package session

import (
	"bytes"
	"context"
	"fmt"
	"os/exec"
	"strings"
	"time"
)

// SetSnapshot records the workspace snapshot ref for a turn.
func (s *Store) SetSnapshot(id string, seq int, ref string) error {
	if ref == "" {
		_, err := s.db.Exec(`DELETE FROM snapshots WHERE session_id=? AND seq=?`, id, seq)
		return err
	}
	_, err := s.db.Exec(`INSERT OR REPLACE INTO snapshots (session_id, seq, ref, created_at) VALUES (?,?,?,?)`,
		id, seq, ref, now())
	return err
}

// Snapshots returns workspace snapshot refs keyed by turn.
func (s *Store) Snapshots(id string) map[int]string {
	rows, err := s.db.Query(`SELECT seq, ref FROM snapshots WHERE session_id=?`, id)
	if err != nil {
		return nil
	}
	defer func() { _ = rows.Close() }()
	out := map[int]string{}
	for rows.Next() {
		var seq int
		var ref string
		if rows.Scan(&seq, &ref) == nil {
			out[seq] = ref
		}
	}
	return out
}

// ClearSnapshots drops all snapshot refs for a session.
func (s *Store) ClearSnapshots(id string) error {
	_, err := s.db.Exec(`DELETE FROM snapshots WHERE session_id=?`, id)
	return err
}

// SnapshotWorkspace pins the tracked working tree before a turn.
func SnapshotWorkspace(cwd string) string {
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

// WorkspaceClean reports whether tracked files match HEAD.
func WorkspaceClean(cwd string) bool {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	out, err := gitOut(ctx, cwd, "status", "--porcelain", "--untracked-files=no")
	return err == nil && out == ""
}

// DropSnapshot removes a pinned snapshot ref.
func DropSnapshot(cwd, ref string) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	_, _ = gitOut(ctx, cwd, "update-ref", "-d", "refs/ghg/snapshots/"+ref)
}

// RestoreWorkspace restores tracked files from a snapshot and returns the
// number of dirty tracked files replaced.
func RestoreWorkspace(cwd, ref string) (int, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	dirty, err := gitOut(ctx, cwd, "status", "--porcelain", "--untracked-files=no")
	if err != nil {
		return 0, err
	}
	if _, err := gitOut(ctx, cwd, "checkout", ref, "--", "."); err != nil {
		return 0, err
	}
	DropSnapshot(cwd, ref)
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

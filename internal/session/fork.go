package session

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"strings"

	"github.com/sacca97/ghg/internal/models"
)

// Fork copies stored rows through uptoSeq into a new session.
func (s *Store) Fork(srcID string, uptoSeq int, title string) (string, error) {
	newID := NewSessionID()
	tx, err := s.db.Begin()
	if err != nil {
		return "", err
	}
	defer func() { _ = tx.Rollback() }()
	if _, err := tx.Exec(`INSERT INTO sessions (id, created_at, updated_at, cwd, model, provider, title, goal, forked_from, fork_seq, effort)
		SELECT ?, ?, ?, cwd, model, provider, ?, goal, ?, ?, effort FROM sessions WHERE id=?`,
		newID, now(), now(), title, srcID, uptoSeq, srcID); err != nil {
		return "", err
	}
	if _, err := tx.Exec(`INSERT INTO goals
		(session_id, goal_id, objective, status, rounds, usage_in, usage_cached,
		 usage_out, progress, blocker, created_at, updated_at)
		SELECT ?, goal_id, objective, status, rounds, usage_in, usage_cached,
		 usage_out, progress, blocker, created_at, updated_at
		FROM goals WHERE session_id=?`, newID, srcID); err != nil {
		return "", err
	}
	if _, err := tx.Exec(`INSERT INTO goal_checkpoints
		(session_id, goal_id, seq, status, rounds, usage_in, usage_cached,
		 usage_out, progress, blocker, created_at)
		SELECT ?, goal_id, seq, status, rounds, usage_in, usage_cached,
		 usage_out, progress, blocker, created_at
		FROM goal_checkpoints WHERE session_id=?`, newID, srcID); err != nil {
		return "", err
	}
	if uptoSeq > 0 {
		if _, err := tx.Exec(`INSERT INTO messages (session_id, seq, role, content)
			SELECT ?, seq, role, content FROM messages WHERE session_id=? AND seq <= ?`,
			newID, srcID, uptoSeq); err != nil {
			return "", err
		}
		if _, err := tx.Exec(`INSERT INTO history_fts (session_id, seq, role, content)
			SELECT ?, seq, role, content FROM history_fts WHERE session_id=? AND seq <= ?`,
			newID, srcID, uptoSeq); err != nil {
			return "", err
		}
		outputIDs, err := outputIDsInMessages(tx, srcID, uptoSeq)
		if err != nil {
			return "", err
		}
		query := `INSERT INTO artifacts
			(session_id, message_seq, id, tool_call_id, tool_name, media_type,
			 original_bytes, stored_bytes, hash, path, complete, metadata, created_at)
			SELECT ?, message_seq, id, tool_call_id, tool_name, media_type,
			 original_bytes, stored_bytes, hash, path, complete, metadata, created_at
			FROM artifacts WHERE session_id=? AND (message_seq <= ?`
		args := []any{newID, srcID, uptoSeq}
		if len(outputIDs) > 0 {
			placeholders := make([]string, len(outputIDs))
			for i, id := range outputIDs {
				placeholders[i] = "?"
				args = append(args, id)
			}
			query += ` OR id IN (` + strings.Join(placeholders, ",") + ")"
		}
		query += ")"
		if _, err := tx.Exec(query, args...); err != nil {
			return "", err
		}
		if _, err := tx.Exec(`INSERT INTO compactions (session_id, seq, cutoff, summary, created_at)
			SELECT ?, seq, cutoff, summary, created_at FROM compactions
			WHERE session_id=? AND cutoff<=?`, newID, srcID, uptoSeq); err != nil {
			return "", err
		}
		if _, err := tx.Exec(`INSERT INTO workflow_results
			(session_id, result_id, kind, version, payload, role, provider, model, message_seq, created_at)
			SELECT ?, result_id, kind, version, payload, role, provider, model, message_seq, created_at
			FROM workflow_results WHERE session_id=? AND message_seq<=?`, newID, srcID, uptoSeq); err != nil {
			return "", err
		}
	}
	return newID, tx.Commit()
}

func outputIDsInMessages(tx *sql.Tx, sessionID string, uptoSeq int) ([]string, error) {
	rows, err := tx.Query(`SELECT content FROM messages WHERE session_id=? AND seq<=?`, sessionID, uptoSeq)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	seen := map[string]bool{}
	var ids []string
	for rows.Next() {
		var data string
		if err := rows.Scan(&data); err != nil {
			return nil, err
		}
		var msg models.Message
		if err := json.Unmarshal([]byte(data), &msg); err != nil || msg.Output == nil {
			continue
		}
		if !seen[msg.Output.ID] {
			seen[msg.Output.ID] = true
			ids = append(ids, msg.Output.ID)
		}
	}
	return ids, rows.Err()
}

func (s *Store) ForksOf(id string) ([]Meta, error) {
	rows, err := s.db.Query(`SELECT `+sessionMetaColumns+`
		FROM sessions WHERE forked_from=? ORDER BY updated_at DESC`, id)
	if err != nil {
		return nil, err
	}
	return scanMetas(rows)
}

// ForkTitle derives the next non-nested fork title.
func (s *Store) ForkTitle(base string) (string, error) {
	if base == "" {
		base = "session"
	}
	base = strings.TrimSpace(base)
	if i := strings.LastIndex(base, " (fork #"); i > 0 {
		var n0 int
		var rest string
		n, err := fmt.Sscanf(base[i:], " (fork #%d)%s", &n0, &rest)
		if n0 > 0 && rest == "" && (err == nil || err == io.EOF) && n >= 1 {
			base = base[:i]
		}
	}
	rows, err := s.db.Query(`SELECT title FROM sessions WHERE title = ? OR title LIKE ? ESCAPE '\'`,
		base, likeEscape(base)+` (fork #%)`)
	if err != nil {
		return "", err
	}
	defer func() { _ = rows.Close() }()
	n := 0
	for rows.Next() {
		var t string
		if err := rows.Scan(&t); err != nil {
			return "", err
		}
		var num int
		var rest string
		if nf, err := fmt.Sscanf(t, base+" (fork #%d)%s", &num, &rest); num > n && rest == "" && nf >= 1 && (err == nil || err == io.EOF) {
			n = num
		}
	}
	return fmt.Sprintf("%s (fork #%d)", base, n+1), rows.Err()
}

func likeEscape(s string) string {
	r := strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`)
	return r.Replace(s)
}

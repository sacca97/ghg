package session

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/sacca97/ghg/internal/artifact"
)

const (
	defaultArtifactListLimit = 100
	maxArtifactListLimit     = 1000
)

// LookupArtifact returns one artifact reference only when it belongs to the
// supplied session. The caller still reads the payload through artifact.Store,
// which derives its path from the validated hash rather than this row's path.
func (s *Store) LookupArtifact(ctx context.Context, sessionID, id string) (artifact.Metadata, error) {
	if strings.TrimSpace(sessionID) == "" {
		return artifact.Metadata{}, fmt.Errorf("session id is required")
	}
	if strings.TrimSpace(id) == "" {
		return artifact.Metadata{}, fmt.Errorf("artifact id is required")
	}
	row := s.db.QueryRowContext(ctx, `SELECT session_id, message_seq, id, tool_call_id,
		tool_name, media_type, original_bytes, stored_bytes, hash, path, complete, metadata, created_at
		FROM artifacts WHERE session_id=? AND id=?
		ORDER BY created_at DESC, message_seq DESC, tool_call_id LIMIT 1`, sessionID, id)
	return scanArtifact(row)
}

// ListArtifacts returns a bounded, deterministic artifact catalog for one
// session. Query matches stable text metadata only; it never searches payload
// contents and never accepts a filesystem path.
func (s *Store) ListArtifacts(ctx context.Context, sessionID string, filter artifact.Filter, limit int) ([]artifact.Metadata, error) {
	if strings.TrimSpace(sessionID) == "" {
		return nil, fmt.Errorf("session id is required")
	}
	if limit <= 0 {
		limit = defaultArtifactListLimit
	}
	if limit > maxArtifactListLimit {
		limit = maxArtifactListLimit
	}
	query := `SELECT session_id, message_seq, id, tool_call_id, tool_name,
		media_type, original_bytes, stored_bytes, hash, path, complete, metadata, created_at
		FROM artifacts WHERE session_id=?`
	args := []any{sessionID}
	if filter.ToolName != "" {
		query += ` AND tool_name=?`
		args = append(args, filter.ToolName)
	}
	if filter.ToolCallID != "" {
		query += ` AND tool_call_id=?`
		args = append(args, filter.ToolCallID)
	}
	if filter.Query != "" {
		like := "%" + filter.Query + "%"
		query += ` AND (id LIKE ? OR tool_call_id LIKE ? OR tool_name LIKE ? OR media_type LIKE ? OR metadata LIKE ?)`
		args = append(args, like, like, like, like, like)
	}
	if !filter.Since.IsZero() {
		query += ` AND created_at>=?`
		args = append(args, filter.Since.UTC().Format(time.RFC3339))
	}
	if !filter.Until.IsZero() {
		query += ` AND created_at<=?`
		args = append(args, filter.Until.UTC().Format(time.RFC3339))
	}
	query += ` ORDER BY created_at DESC, message_seq DESC, id ASC, tool_call_id ASC LIMIT ?`
	args = append(args, limit)
	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []artifact.Metadata
	for rows.Next() {
		meta, err := scanArtifact(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, meta)
	}
	return out, rows.Err()
}

type scanner interface {
	Scan(dest ...any) error
}

func scanArtifact(row scanner) (artifact.Metadata, error) {
	var (
		meta     artifact.Metadata
		complete int
		metadata string
		created  string
	)
	if err := row.Scan(&meta.SessionID, &meta.MessageSeq, &meta.ID, &meta.ToolCallID,
		&meta.ToolName, &meta.MediaType, &meta.OriginalBytes, &meta.StoredBytes,
		&meta.Hash, &meta.Path, &complete, &metadata, &created); err != nil {
		return artifact.Metadata{}, err
	}
	meta.Complete = complete != 0
	if metadata != "" {
		if err := json.Unmarshal([]byte(metadata), &meta.Metadata); err != nil {
			return artifact.Metadata{}, fmt.Errorf("parse artifact metadata: %w", err)
		}
	}
	var err error
	meta.CreatedAt, err = time.Parse(time.RFC3339, created)
	if err != nil {
		return artifact.Metadata{}, fmt.Errorf("parse artifact timestamp: %w", err)
	}
	return meta, nil
}

// ReferencedArtifactHashes returns the payload hashes still referenced by any
// session row. Callers can pass the result to artifact.Store.GarbageCollect
// after removing sessions; payload files are intentionally outside SQLite.
func (s *Store) ReferencedArtifactHashes(ctx context.Context) (map[string]bool, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT DISTINCT hash FROM artifacts`)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	refs := map[string]bool{}
	for rows.Next() {
		var hash string
		if err := rows.Scan(&hash); err != nil {
			return nil, err
		}
		refs[hash] = true
	}
	return refs, rows.Err()
}

// GarbageCollectArtifacts removes unreferenced payloads after consulting this
// session database for the complete set of live references. Keeping the
// reference query and payload deletion in one helper makes the safe cleanup
// sequence hard to accidentally reverse at a call site.
func (s *Store) GarbageCollectArtifacts(ctx context.Context, payloads *artifact.Store, maxAge time.Duration, maxBytes int64) (int, error) {
	if payloads == nil {
		return 0, fmt.Errorf("artifact store is required")
	}
	referenced, err := s.ReferencedArtifactHashes(ctx)
	if err != nil {
		return 0, err
	}
	return payloads.GarbageCollect(ctx, referenced, maxAge, maxBytes)
}

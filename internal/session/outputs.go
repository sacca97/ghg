package session

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/sacca97/ghg/internal/models"
)

const (
	defaultOutputListLimit = 100
	maxOutputListLimit     = 1000
)

// OutputMetadata is the session-owned index entry for one retained output.
type OutputMetadata struct {
	models.OutputRef
	SessionID  string
	MessageSeq int
	ToolCallID string
	ToolName   string
	Path       string
	CreatedAt  time.Time
}

// OutputFilter selects output metadata for one session.
type OutputFilter struct {
	ToolName   string
	ToolCallID string
	Query      string
	Since      time.Time
	Until      time.Time
}

// OutputCatalog resolves session-owned output metadata.
type OutputCatalog interface {
	LookupOutput(context.Context, string, string) (OutputMetadata, error)
	ListOutputs(context.Context, string, OutputFilter, int) ([]OutputMetadata, error)
}

func (s *Store) LookupOutput(ctx context.Context, sessionID, id string) (OutputMetadata, error) {
	if strings.TrimSpace(sessionID) == "" {
		return OutputMetadata{}, fmt.Errorf("session id is required")
	}
	if strings.TrimSpace(id) == "" {
		return OutputMetadata{}, fmt.Errorf("output id is required")
	}
	row := s.db.QueryRowContext(ctx, `SELECT session_id, message_seq, id, tool_call_id,
		tool_name, media_type, original_bytes, stored_bytes, hash, path, complete, metadata, created_at
		FROM artifacts WHERE session_id=? AND id=?
		ORDER BY created_at DESC, message_seq DESC, tool_call_id LIMIT 1`, sessionID, id)
	return scanOutput(row)
}

func (s *Store) ListOutputs(ctx context.Context, sessionID string, filter OutputFilter, limit int) ([]OutputMetadata, error) {
	if strings.TrimSpace(sessionID) == "" {
		return nil, fmt.Errorf("session id is required")
	}
	if limit <= 0 {
		limit = defaultOutputListLimit
	}
	if limit > maxOutputListLimit {
		limit = maxOutputListLimit
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
	var out []OutputMetadata
	for rows.Next() {
		meta, err := scanOutput(rows)
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

func scanOutput(row scanner) (OutputMetadata, error) {
	var (
		meta     OutputMetadata
		complete int
		metadata string
		created  string
	)
	if err := row.Scan(&meta.SessionID, &meta.MessageSeq, &meta.ID, &meta.ToolCallID,
		&meta.ToolName, &meta.MediaType, &meta.OriginalBytes, &meta.StoredBytes,
		&meta.Hash, &meta.Path, &complete, &metadata, &created); err != nil {
		return OutputMetadata{}, err
	}
	meta.Complete = complete != 0
	if metadata != "" {
		if err := json.Unmarshal([]byte(metadata), &meta.Metadata); err != nil {
			return OutputMetadata{}, fmt.Errorf("parse output metadata: %w", err)
		}
	}
	var err error
	meta.CreatedAt, err = time.Parse(time.RFC3339, created)
	if err != nil {
		return OutputMetadata{}, fmt.Errorf("parse output timestamp: %w", err)
	}
	return meta, nil
}

// ReferencedOutputHashes returns payload hashes still referenced by sessions.
func (s *Store) ReferencedOutputHashes(ctx context.Context) (map[string]bool, error) {
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

// GarbageCollectOutputs removes unreferenced payloads from the configured store.
func (s *Store) GarbageCollectOutputs(ctx context.Context, maxAge time.Duration, maxBytes int64) (int, error) {
	if s.Outputs == nil {
		return 0, fmt.Errorf("output store is required")
	}
	referenced, err := s.ReferencedOutputHashes(ctx)
	if err != nil {
		return 0, err
	}
	return s.Outputs.GarbageCollect(ctx, referenced, maxAge, maxBytes)
}

package session

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"
)

const (
	workflowTimeLayout    = "2006-01-02T15:04:05.000000000Z"
	workflowResultColumns = `session_id, result_id, kind, version, payload,
		role, provider, model, message_seq, created_at`
)

// WorkflowResultRecord is a typed workflow output (plan, review) persisted with
// a session. Payloads are immutable JSON documents; renderers generate
// exported Markdown on-demand.
type WorkflowResultRecord struct {
	ResultID   string    `json:"result_id"`
	SessionID  string    `json:"session_id"`
	Kind       string    `json:"kind"`
	Version    int       `json:"version"`
	Payload    string    `json:"payload"`
	Role       string    `json:"role,omitempty"`
	Provider   string    `json:"provider,omitempty"`
	Model      string    `json:"model,omitempty"`
	MessageSeq int       `json:"message_seq,omitempty"`
	CreatedAt  time.Time `json:"created_at"`
}

func scanWorkflowResult(row scanner) (WorkflowResultRecord, error) {
	var (
		r       WorkflowResultRecord
		created string
	)
	if err := row.Scan(
		&r.SessionID, &r.ResultID, &r.Kind, &r.Version, &r.Payload,
		&r.Role, &r.Provider, &r.Model, &r.MessageSeq, &created,
	); err != nil {
		return WorkflowResultRecord{}, err
	}
	var err error
	r.CreatedAt, err = time.Parse(time.RFC3339Nano, created)
	if err != nil {
		return WorkflowResultRecord{}, fmt.Errorf("parse workflow result timestamp: %w", err)
	}
	return r, nil
}

// SaveWorkflowResult persists a structured workflow result for a session.
func (s *Store) SaveWorkflowResult(ctx context.Context, record WorkflowResultRecord) error {
	if strings.TrimSpace(record.SessionID) == "" {
		return errors.New("session id is required")
	}
	if strings.TrimSpace(record.ResultID) == "" {
		return errors.New("result id is required")
	}
	if strings.TrimSpace(record.Kind) == "" {
		return errors.New("result kind is required")
	}
	if record.Version <= 0 {
		record.Version = 1
	}
	if record.CreatedAt.IsZero() {
		record.CreatedAt = time.Now().UTC()
	}
	query := `INSERT OR REPLACE INTO workflow_results
		(session_id, result_id, kind, version, payload, role, provider, model, message_seq, created_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`
	_, err := s.db.ExecContext(ctx, query,
		record.SessionID,
		record.ResultID,
		record.Kind,
		record.Version,
		record.Payload,
		record.Role,
		record.Provider,
		record.Model,
		record.MessageSeq,
		record.CreatedAt.UTC().Format(workflowTimeLayout),
	)
	if err != nil {
		return fmt.Errorf("save workflow result: %w", err)
	}
	return nil
}

// LoadWorkflowResult loads one workflow result by ID within a session.
func (s *Store) LoadWorkflowResult(ctx context.Context, sessionID, resultID string) (WorkflowResultRecord, error) {
	if strings.TrimSpace(sessionID) == "" {
		return WorkflowResultRecord{}, errors.New("session id is required")
	}
	if strings.TrimSpace(resultID) == "" {
		return WorkflowResultRecord{}, errors.New("result id is required")
	}
	query := `SELECT ` + workflowResultColumns + `
		FROM workflow_results WHERE session_id=? AND result_id=?`
	r, err := scanWorkflowResult(s.db.QueryRowContext(ctx, query, sessionID, resultID))
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return WorkflowResultRecord{}, fmt.Errorf("workflow result %q not found in session %s", resultID, sessionID)
		}
		return WorkflowResultRecord{}, err
	}
	return r, nil
}

// ListWorkflowResults lists workflow results for a session, optionally filtered by kind.
func (s *Store) ListWorkflowResults(ctx context.Context, sessionID string, kind string) ([]WorkflowResultRecord, error) {
	if strings.TrimSpace(sessionID) == "" {
		return nil, errors.New("session id is required")
	}
	query := `SELECT ` + workflowResultColumns + `
		FROM workflow_results WHERE session_id=?`
	args := []any{sessionID}
	if kind != "" {
		query += " AND kind=?"
		args = append(args, kind)
	}
	query += " ORDER BY created_at DESC, message_seq DESC"
	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()

	var results []WorkflowResultRecord
	for rows.Next() {
		r, err := scanWorkflowResult(rows)
		if err != nil {
			return nil, err
		}
		results = append(results, r)
	}
	return results, rows.Err()
}

// LatestWorkflowResult returns the most recent workflow result for a session and optional kind.
func (s *Store) LatestWorkflowResult(ctx context.Context, sessionID string, kind string) (WorkflowResultRecord, bool, error) {
	if strings.TrimSpace(sessionID) == "" {
		return WorkflowResultRecord{}, false, errors.New("session id is required")
	}
	query := `SELECT ` + workflowResultColumns + `
		FROM workflow_results WHERE session_id=?`
	args := []any{sessionID}
	if kind != "" {
		query += " AND kind=?"
		args = append(args, kind)
	}
	query += " ORDER BY created_at DESC, message_seq DESC LIMIT 1"
	r, err := scanWorkflowResult(s.db.QueryRowContext(ctx, query, args...))
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return WorkflowResultRecord{}, false, nil
		}
		return WorkflowResultRecord{}, false, err
	}
	return r, true, nil
}

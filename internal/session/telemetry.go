package session

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
)

// TelemetryEvent is an ordered, session-owned execution diagnostic. Payload is
// kept as JSON so exports retain the original event shape without coupling the
// session package to agent event types.
type TelemetryEvent struct {
	Seq       int             `json:"seq"`
	Kind      string          `json:"kind"`
	Payload   json.RawMessage `json:"payload"`
	CreatedAt time.Time       `json:"created_at"`
}

// AppendTelemetry appends one event after the current session sequence.
func (s *Store) AppendTelemetry(ctx context.Context, sessionID, kind string, payload any) error {
	if strings.TrimSpace(sessionID) == "" {
		return errors.New("session id is required")
	}
	if strings.TrimSpace(kind) == "" {
		return errors.New("telemetry kind is required")
	}
	raw, err := json.Marshal(payload)
	if err != nil {
		return fmt.Errorf("marshal telemetry: %w", err)
	}
	_, err = s.db.ExecContext(ctx, `INSERT INTO telemetry_events
		(session_id, seq, kind, payload, created_at)
		SELECT ?, COALESCE(MAX(seq), 0) + 1, ?, ?, ?
		FROM telemetry_events WHERE session_id=?`,
		sessionID, kind, string(raw), time.Now().UTC().Format(time.RFC3339Nano), sessionID)
	if err != nil {
		return fmt.Errorf("append telemetry: %w", err)
	}
	return nil
}

// ListTelemetry returns all persisted events in append order.
func (s *Store) ListTelemetry(ctx context.Context, sessionID string) ([]TelemetryEvent, error) {
	if strings.TrimSpace(sessionID) == "" {
		return nil, errors.New("session id is required")
	}
	rows, err := s.db.QueryContext(ctx, `SELECT seq, kind, payload, created_at
		FROM telemetry_events WHERE session_id=? ORDER BY seq`, sessionID)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	return scanTelemetry(rows)
}

func (s *Store) ListTelemetryKind(ctx context.Context, sessionID, kind string) ([]TelemetryEvent, error) {
	if strings.TrimSpace(sessionID) == "" {
		return nil, errors.New("session id is required")
	}
	if strings.TrimSpace(kind) == "" {
		return nil, errors.New("telemetry kind is required")
	}
	rows, err := s.db.QueryContext(ctx, `SELECT seq, kind, payload, created_at
		FROM telemetry_events WHERE session_id=? AND kind=? ORDER BY seq`, sessionID, kind)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	return scanTelemetry(rows)
}

func (s *Store) LatestTelemetry(ctx context.Context, sessionID, kind string) (TelemetryEvent, bool, error) {
	if strings.TrimSpace(sessionID) == "" {
		return TelemetryEvent{}, false, errors.New("session id is required")
	}
	if strings.TrimSpace(kind) == "" {
		return TelemetryEvent{}, false, errors.New("telemetry kind is required")
	}
	var event TelemetryEvent
	var payload, created string
	err := s.db.QueryRowContext(ctx, `SELECT seq, kind, payload, created_at
		FROM telemetry_events WHERE session_id=? AND kind=? ORDER BY seq DESC LIMIT 1`, sessionID, kind).Scan(
		&event.Seq, &event.Kind, &payload, &created)
	if errors.Is(err, sql.ErrNoRows) {
		return TelemetryEvent{}, false, nil
	}
	if err != nil {
		return TelemetryEvent{}, false, err
	}
	event.Payload = json.RawMessage(payload)
	event.CreatedAt, err = time.Parse(time.RFC3339Nano, created)
	if err != nil {
		return TelemetryEvent{}, false, fmt.Errorf("parse telemetry timestamp: %w", err)
	}
	return event, true, nil
}

func scanTelemetry(rows *sql.Rows) ([]TelemetryEvent, error) {
	var events []TelemetryEvent
	for rows.Next() {
		var event TelemetryEvent
		var payload, created string
		if err := rows.Scan(&event.Seq, &event.Kind, &payload, &created); err != nil {
			return nil, err
		}
		event.Payload = json.RawMessage(payload)
		var err error
		event.CreatedAt, err = time.Parse(time.RFC3339Nano, created)
		if err != nil {
			return nil, fmt.Errorf("parse telemetry timestamp: %w", err)
		}
		events = append(events, event)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return events, nil
}

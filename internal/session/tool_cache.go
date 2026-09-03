package session

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/sacca97/ghg/internal/observation"
	"github.com/sacca97/ghg/internal/search"
)

// SaveObservation persists the exact bounded bytes issued by a read.
func (s *Store) SaveObservation(ctx context.Context, sessionID string, record observation.Record) error {
	if strings.TrimSpace(sessionID) == "" {
		return fmt.Errorf("session id is required")
	}
	if record.ID == "" || record.Path == "" {
		return fmt.Errorf("observation id and path are required")
	}
	complete := 0
	if record.Complete {
		complete = 1
	}
	_, err := s.db.ExecContext(ctx, `INSERT OR REPLACE INTO observations
		(session_id, observation_id, path, start_line, end_line, next_offset,
		 issued_bytes, content, complete, created_at)
		VALUES (?,?,?,?,?,?,?,?,?,?)`,
		sessionID, record.ID, record.Path, record.StartLine, record.EndLine,
		record.NextOffset, record.IssuedBytes, record.Content, complete,
		record.CreatedAt.UTC().Format(time.RFC3339))
	return err
}

func (s *Store) LoadObservation(ctx context.Context, sessionID, id string) (observation.Record, error) {
	if strings.TrimSpace(sessionID) == "" {
		return observation.Record{}, fmt.Errorf("session id is required")
	}
	var record observation.Record
	var complete int
	var created string
	err := s.db.QueryRowContext(ctx, `SELECT observation_id, path, start_line,
		end_line, next_offset, issued_bytes, content, complete, created_at
		FROM observations WHERE session_id=? AND observation_id=?`, sessionID, id).
		Scan(&record.ID, &record.Path, &record.StartLine, &record.EndLine,
			&record.NextOffset, &record.IssuedBytes, &record.Content, &complete, &created)
	if err != nil {
		return observation.Record{}, err
	}
	record.SessionID = sessionID
	record.Complete = complete != 0
	record.CreatedAt, _ = time.Parse(time.RFC3339, created)
	return record, nil
}

// SaveSearchSnapshot persists an immutable bounded search result set.
func (s *Store) SaveSearchSnapshot(ctx context.Context, sessionID string, snapshot search.Snapshot) error {
	if strings.TrimSpace(sessionID) == "" {
		return fmt.Errorf("session id is required")
	}
	data, err := json.Marshal(snapshot)
	if err != nil {
		return fmt.Errorf("marshal search snapshot: %w", err)
	}
	_, err = s.db.ExecContext(ctx, `INSERT OR REPLACE INTO search_snapshots
		(session_id, snapshot_id, kind, snapshot, created_at) VALUES (?,?,?,?,?)`,
		sessionID, snapshot.ID, snapshot.Kind, string(data), snapshot.CreatedAt.UTC().Format(time.RFC3339))
	return err
}

func (s *Store) LoadSearchSnapshot(ctx context.Context, sessionID, id string) (search.Snapshot, error) {
	if strings.TrimSpace(sessionID) == "" {
		return search.Snapshot{}, fmt.Errorf("session id is required")
	}
	var data string
	err := s.db.QueryRowContext(ctx, `SELECT snapshot FROM search_snapshots WHERE session_id=? AND snapshot_id=?`, sessionID, id).Scan(&data)
	if err != nil {
		return search.Snapshot{}, err
	}
	var snapshot search.Snapshot
	if err := json.Unmarshal([]byte(data), &snapshot); err != nil {
		return search.Snapshot{}, fmt.Errorf("decode search snapshot %s: %w", id, err)
	}
	return snapshot, nil
}

// ObservationRegistryStore adapts the durable session store to the live registry.
func (s *Store) ObservationRegistryStore() observation.Store {
	return s
}

// SearchRegistryStore adapts the durable session store to the live search registry.
func (s *Store) SearchRegistryStore() search.Store {
	return s
}

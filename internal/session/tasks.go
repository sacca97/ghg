package session

import "time"

type Task struct {
	ID          string
	Description string
	Prompt      string
	Status      string
	Report      string
	StartedAt   time.Time
	EndedAt     time.Time
}

func (s *Store) SaveTask(sessionID string, t Task) error {
	ended := ""
	if !t.EndedAt.IsZero() {
		ended = t.EndedAt.UTC().Format(time.RFC3339)
	}
	_, err := s.db.Exec(`INSERT OR REPLACE INTO tasks
		(session_id, task_id, description, prompt, status, report, started_at, ended_at)
		VALUES (?,?,?,?,?,?,?,?)`,
		sessionID, t.ID, t.Description, t.Prompt, t.Status, t.Report,
		t.StartedAt.UTC().Format(time.RFC3339), ended)
	return err
}

func (s *Store) LoadTasks(sessionID string) ([]Task, error) {
	rows, err := s.db.Query(`SELECT task_id, description, prompt, status, report, started_at, ended_at
		FROM tasks WHERE session_id=? ORDER BY started_at`, sessionID)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []Task
	for rows.Next() {
		var t Task
		var started, ended string
		if err := rows.Scan(&t.ID, &t.Description, &t.Prompt, &t.Status, &t.Report, &started, &ended); err != nil {
			return nil, err
		}
		t.StartedAt, _ = time.Parse(time.RFC3339, started)
		if ended != "" {
			t.EndedAt, _ = time.Parse(time.RFC3339, ended)
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

type Schedule struct {
	ID       int
	Schedule string
	Prompt   string
	Anchor   time.Time
	LastFire time.Time
}

func (s *Store) AddSchedule(sessionID, schedule, prompt string, anchor time.Time) (int, error) {
	var id int
	err := s.db.QueryRow(`INSERT INTO schedules (session_id, id, schedule, prompt, anchor, created_at)
		SELECT ?, COALESCE(MAX(id),0)+1, ?, ?, ?, ? FROM schedules WHERE session_id=? RETURNING id`,
		sessionID, schedule, prompt, anchor.UTC().Format(time.RFC3339), now(), sessionID).Scan(&id)
	return id, err
}

func (s *Store) Schedules(sessionID string) []Schedule {
	rows, err := s.db.Query(`SELECT id, schedule, prompt, anchor, last_fire FROM schedules WHERE session_id=? ORDER BY id`, sessionID)
	if err != nil {
		return nil
	}
	defer func() { _ = rows.Close() }()
	var out []Schedule
	for rows.Next() {
		var sc Schedule
		var anchor, lastFire string
		if rows.Scan(&sc.ID, &sc.Schedule, &sc.Prompt, &anchor, &lastFire) != nil {
			continue
		}
		sc.Anchor, _ = time.Parse(time.RFC3339, anchor)
		sc.LastFire, _ = time.Parse(time.RFC3339, lastFire)
		out = append(out, sc)
	}
	return out
}

func (s *Store) MarkFired(sessionID string, id int, at time.Time) error {
	_, err := s.db.Exec(`UPDATE schedules SET last_fire=? WHERE session_id=? AND id=?`,
		at.UTC().Format(time.RFC3339), sessionID, id)
	return err
}

func (s *Store) DeleteSchedule(sessionID string, id int) error {
	_, err := s.db.Exec(`DELETE FROM schedules WHERE session_id=? AND id=?`, sessionID, id)
	return err
}

package session

import (
	"database/sql"
	"encoding/json"

	"github.com/sacca97/ghg/internal/models"
)

type storedMessage struct {
	seq int
	msg models.Message
}

// applyCompactionRows derives the current prompt view from raw messages.
func applyCompactionRows(db *sql.DB, sessionID string, rows []storedMessage) []models.Message {
	var cutoff int
	var summary string
	err := db.QueryRow(`SELECT cutoff, summary FROM compactions WHERE session_id=? ORDER BY seq DESC LIMIT 1`,
		sessionID).Scan(&cutoff, &summary)
	if err != nil || cutoff <= 0 {
		return storedMessages(rows)
	}
	fold := len(rows)
	for i, row := range rows {
		if row.seq >= cutoff {
			fold = i
			break
		}
	}
	if fold == len(rows) && (len(rows) == 0 || rows[len(rows)-1].seq+1 < cutoff) {
		return storedMessages(rows)
	}
	out := make([]models.Message, 0, len(rows)+1)
	start := 0
	if len(rows) > 0 && rows[0].seq == 0 && rows[0].msg.Role == "system" {
		out = append(out, rows[0].msg)
		start = 1
	}
	out = append(out, models.Message{Role: "system", Content: "Summary of the conversation so far:\n\n" + summary})
	var prior []models.Message
	for i := start; i < fold; i++ {
		if rows[i].msg.Role == "system" {
			prior = append(prior, rows[i].msg)
		}
	}
	if len(prior) > 0 {
		out = append(out, prior[len(prior)-1])
	}
	for _, row := range rows[fold:] {
		out = append(out, row.msg)
	}
	return out
}

func storedMessages(rows []storedMessage) []models.Message {
	msgs := make([]models.Message, 0, len(rows))
	for _, row := range rows {
		msgs = append(msgs, row.msg)
	}
	return msgs
}

// answerDanglingToolCalls repairs a history interrupted after tool calls.
func answerDanglingToolCalls(msgs []models.Message) []models.Message {
	answered := make(map[string]bool, len(msgs))
	dangling := false
	for _, m := range msgs {
		if m.Role == "tool" {
			answered[m.ToolCallID] = true
		}
	}
	for _, m := range msgs {
		if m.Role == "assistant" {
			for _, tc := range m.ToolCalls {
				dangling = dangling || !answered[tc.ID]
			}
		}
	}
	if !dangling {
		return msgs
	}
	out := make([]models.Message, 0, len(msgs)+4)
	for _, m := range msgs {
		out = append(out, m)
		if m.Role != "assistant" {
			continue
		}
		for _, tc := range m.ToolCalls {
			if !answered[tc.ID] {
				out = append(out, models.Message{
					Role:       "tool",
					Content:    "Error: tool call interrupted — the session ended before a result was recorded",
					ToolCallID: tc.ID,
					Name:       tc.Function.Name,
				})
			}
		}
	}
	return out
}

type Compaction struct {
	Seq     int
	Cutoff  int
	Summary string
}

// PersistCompaction saves an unsaved tail and records a compaction event.
func (s *Store) PersistCompaction(id string, saved int, msgs []models.Message, model, provider, summary string, cutoff int) error {
	if s == nil || id == "" {
		return nil
	}
	if len(msgs) > saved {
		if err := s.Save(id, saved, msgs, model, provider); err != nil {
			return err
		}
	}
	return s.RecordCompaction(id, s.RawCutoff(id, cutoff, msgs), summary)
}

// RecordCompaction appends an event without rewriting raw messages.
func (s *Store) RecordCompaction(id string, cutoff int, summary string) error {
	_, err := s.db.Exec(`INSERT INTO compactions (session_id, seq, cutoff, summary, created_at)
		SELECT ?, COALESCE(MAX(seq),0)+1, ?, ?, ? FROM compactions WHERE session_id=?`,
		id, cutoff, summary, now(), id)
	return err
}

func (s *Store) Compactions(id string) []Compaction {
	rows, err := s.db.Query(`SELECT seq, cutoff, summary FROM compactions WHERE session_id=? ORDER BY seq`, id)
	if err != nil {
		return nil
	}
	defer func() { _ = rows.Close() }()
	var out []Compaction
	for rows.Next() {
		var c Compaction
		if rows.Scan(&c.Seq, &c.Cutoff, &c.Summary) == nil {
			out = append(out, c)
		}
	}
	return out
}

// RawCutoff maps a prompt-view cutoff to raw-log coordinates.
func (s *Store) RawCutoff(id string, cutoff int, before []models.Message) int {
	events := s.Compactions(id)
	if len(events) == 0 {
		return cutoff
	}
	firstRaw := 1
	for firstRaw < len(before) && before[firstRaw].Role == "system" {
		firstRaw++
	}
	return events[len(events)-1].Cutoff + cutoff - firstRaw
}

func (s *Store) DeleteCompaction(id string, seq int) error {
	_, err := s.db.Exec(`DELETE FROM compactions WHERE session_id=? AND seq=?`, id, seq)
	return err
}

// RawMessages returns the un-folded stored log.
func (s *Store) RawMessages(id string) []models.Message {
	rows, err := s.db.Query(`SELECT content FROM messages WHERE session_id=? ORDER BY seq`, id)
	if err != nil {
		return nil
	}
	defer func() { _ = rows.Close() }()
	var msgs []models.Message
	for rows.Next() {
		var data string
		if rows.Scan(&data) != nil {
			continue
		}
		var m models.Message
		if json.Unmarshal([]byte(data), &m) == nil {
			msgs = append(msgs, m)
		}
	}
	return msgs
}

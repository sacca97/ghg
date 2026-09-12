package session

import (
	"bytes"
	"database/sql"
	"encoding/json"
	"fmt"

	"github.com/sacca97/ghg/internal/models"
)

const interruptedToolResultSource = "ghg:interrupted"

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
	events, err := s.latestCompaction(id)
	if err != nil {
		return err
	}
	rawCutoff, err := s.translatedRawCutoff(id, cutoff, msgs, events)
	if err != nil {
		return err
	}
	return s.RecordCompaction(id, rawCutoff, summary)
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

// latestCompaction returns only the cutoff needed to translate a view
// boundary. Summary-list callers should use Compactions instead.
func (s *Store) latestCompaction(id string) ([]Compaction, error) {
	var cutoff int
	err := s.db.QueryRow(`SELECT cutoff FROM compactions WHERE session_id=? ORDER BY seq DESC LIMIT 1`, id).Scan(&cutoff)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return []Compaction{{Cutoff: cutoff}}, nil
}

// RawCutoff maps a prompt-view cutoff to raw-log coordinates. derivedPrefixLen
// is the number of prompt-view entries before the raw tail; it is supplied by
// the caller because message roles do not identify derived rows reliably.
func (s *Store) RawCutoff(cutoff, derivedPrefixLen int, events []Compaction) int {
	if len(events) == 0 {
		return cutoff
	}
	return events[len(events)-1].Cutoff + cutoff - derivedPrefixLen
}

// translatedRawCutoff obtains the prompt-view prefix by locating the persisted
// raw tail in the view. Derived rows and interruption repairs are not raw
// history, and newly appended in-memory rows may follow the raw tail.
func (s *Store) translatedRawCutoff(id string, cutoff int, view []models.Message, events []Compaction) (int, error) {
	if len(events) == 0 {
		return cutoff, nil
	}
	rows, err := s.db.Query(`SELECT content FROM messages WHERE session_id=? AND seq>=? ORDER BY seq`,
		id, events[len(events)-1].Cutoff)
	if err != nil {
		return 0, err
	}
	var rawTail []models.Message
	for rows.Next() {
		var data string
		if err := rows.Scan(&data); err != nil {
			_ = rows.Close()
			return 0, err
		}
		var msg models.Message
		if err := json.Unmarshal([]byte(data), &msg); err != nil {
			_ = rows.Close()
			return 0, fmt.Errorf("decode raw tail: %w", err)
		}
		rawTail = append(rawTail, msg)
	}
	if err := rows.Close(); err != nil {
		return 0, err
	}
	if err := rows.Err(); err != nil {
		return 0, err
	}
	derivedPrefixLen, ok := rawTailPrefix(view, rawTail)
	if !ok {
		return 0, fmt.Errorf("compacted view does not contain the persisted raw tail")
	}
	return s.RawCutoff(cutoff, derivedPrefixLen, events), nil
}

func rawTailPrefix(view, rawTail []models.Message) (int, bool) {
	if len(rawTail) == 0 {
		prefix := 0
		for _, msg := range view {
			if !isInterruptedToolResult(msg) {
				prefix++
			}
		}
		return prefix, true
	}
	for start := range view {
		if isInterruptedToolResult(view[start]) {
			continue
		}
		matched := 0
		for i := start; i < len(view) && matched < len(rawTail); i++ {
			if isInterruptedToolResult(view[i]) {
				continue
			}
			if !sameStoredMessage(view[i], rawTail[matched]) {
				matched = -1
				break
			}
			matched++
		}
		if matched == len(rawTail) {
			return start, true
		}
	}
	return 0, false
}

func isInterruptedToolResult(msg models.Message) bool {
	return msg.Role == "tool" && msg.Source == interruptedToolResultSource
}

func sameStoredMessage(a, b models.Message) bool {
	left, err := json.Marshal(a)
	if err != nil {
		return false
	}
	right, err := json.Marshal(b)
	return err == nil && bytes.Equal(left, right)
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

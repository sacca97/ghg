package session

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"unicode/utf8"

	"github.com/sacca97/ghg/internal/models"
)

// ErrInvalidHistoryQuery hides SQLite parser details from model-facing tools.
var ErrInvalidHistoryQuery = errors.New("invalid history query")

type HistoryHit struct {
	Seq     int
	Role    string
	Epoch   int
	Snippet string
}

type HistoryMessage struct {
	Seq     int
	Epoch   int
	Message models.Message
}

const (
	historyIndexTextLimit = 16 << 10
	historyQueryLimit     = 200
	historyReadLimit      = 4000
	maxHistoryQueryBytes  = 512
)

// historyIndexText extracts searchable text without indexing system policy,
// image data, provider blocks, or output payload files. Tool-call names and
// bounded arguments remain useful for finding a past operation.
func historyIndexText(msg models.Message) string {
	var parts []string
	switch msg.Role {
	case "user":
		parts = append(parts, msg.TextContent())
	case "assistant":
		parts = append(parts, msg.TextContent())
		for _, call := range msg.ToolCalls {
			parts = append(parts, call.Function.Name, boundedHistoryText(call.Function.Arguments, 2048))
		}
	case "tool":
		parts = append(parts, msg.Source, msg.Name, msg.TextContent())
	default:
		return ""
	}
	return boundedHistoryText(strings.Join(parts, "\n"), historyIndexTextLimit)
}

func boundedHistoryText(value string, limit int) string {
	value = strings.TrimSpace(value)
	return truncateHistory(value, limit)
}

func truncateHistory(value string, limit int) string {
	if limit <= 0 {
		return ""
	}
	if len(value) <= limit {
		return value
	}
	const suffix = "…"
	if limit < len(suffix) {
		return value[:utf8Prefix(value, limit)]
	}
	return value[:utf8Prefix(value, limit-len(suffix))] + suffix
}

func utf8Prefix(value string, limit int) int {
	end := 0
	for end < len(value) {
		_, size := utf8.DecodeRuneInString(value[end:])
		if end+size > limit {
			break
		}
		end += size
	}
	return end
}

// backfillHistoryFTS rebuilds the derived index once for databases created
// before history recall existed. Save maintains it transactionally afterward.
func backfillHistoryFTS(db *sql.DB) error {
	var version int
	if err := db.QueryRow(`PRAGMA user_version`).Scan(&version); err != nil {
		return err
	}
	if version >= 1 {
		return nil
	}
	if err := rebuildHistoryFTS(db); err != nil {
		return err
	}
	_, err := db.Exec(`PRAGMA user_version = 1`)
	return err
}

// rebuildHistoryFTS is an idempotent recovery/test helper. JSON rows that no
// longer decode are skipped, matching the existing history-load behavior.
func rebuildHistoryFTS(db *sql.DB) error {
	rows, err := db.Query(`SELECT session_id, seq, content FROM messages ORDER BY session_id, seq`)
	if err != nil {
		return err
	}
	type rawMessage struct {
		sessionID string
		seq       int
		message   models.Message
	}
	var messages []rawMessage
	for rows.Next() {
		var sessionID, data string
		var seq int
		if err := rows.Scan(&sessionID, &seq, &data); err != nil {
			_ = rows.Close()
			return err
		}
		var msg models.Message
		if json.Unmarshal([]byte(data), &msg) == nil {
			messages = append(messages, rawMessage{sessionID: sessionID, seq: seq, message: msg})
		}
	}
	if err := rows.Close(); err != nil {
		return err
	}
	if err := rows.Err(); err != nil {
		return err
	}
	tx, err := db.Begin()
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	if _, err := tx.Exec(`DELETE FROM history_fts`); err != nil {
		return err
	}
	for _, row := range messages {
		if err := insertHistoryFTS(tx, row.sessionID, row.seq, row.message); err != nil {
			return err
		}
	}
	return tx.Commit()
}

func replaceHistoryFTS(tx *sql.Tx, sessionID string, seq int, msg models.Message) error {
	if _, err := tx.Exec(`DELETE FROM history_fts WHERE session_id=? AND seq=?`, sessionID, seq); err != nil {
		return err
	}
	return insertHistoryFTS(tx, sessionID, seq, msg)
}

func insertHistoryFTS(tx *sql.Tx, sessionID string, seq int, msg models.Message) error {
	content := historyIndexText(msg)
	if content == "" {
		return nil
	}
	_, err := tx.Exec(`INSERT INTO history_fts (session_id, seq, role, content) VALUES (?,?,?,?)`,
		sessionID, seq, msg.Role, content)
	return err
}

func (s *Store) SearchHistory(ctx context.Context, sessionID, query, role string, epoch *int, limit int) ([]HistoryHit, error) {
	if strings.TrimSpace(sessionID) == "" {
		return nil, errors.New("session id is required")
	}
	query = strings.TrimSpace(query)
	if query == "" {
		return nil, errors.New("history query is required")
	}
	if len(query) > maxHistoryQueryBytes {
		return nil, fmt.Errorf("history query exceeds %d-byte limit", maxHistoryQueryBytes)
	}
	role = strings.ToLower(strings.TrimSpace(role))
	switch role {
	case "", "user", "assistant", "tool":
	default:
		return nil, errors.New("history role must be user, assistant, or tool")
	}
	if limit <= 0 || limit > historyQueryLimit {
		limit = historyQueryLimit
	}
	cutoffs, err := s.historyCutoffs(ctx, sessionID)
	if err != nil {
		return nil, err
	}
	start, end, err := historyEpochRange(cutoffs, epoch)
	if err != nil {
		return nil, err
	}
	rows, err := s.db.QueryContext(ctx, `SELECT seq, role,
		snippet(history_fts, 3, '[', ']', '...', 40), rank
		FROM history_fts
		WHERE history_fts MATCH ? AND session_id=?
		  AND (? = '' OR role = ?)
		  AND seq >= ? AND seq < ?
		ORDER BY rank ASC, seq ASC LIMIT ?`,
		query, sessionID, role, role, start, end, limit)
	if err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		message := strings.ToLower(err.Error())
		if strings.Contains(message, "fts5: syntax error") ||
			strings.Contains(message, "unterminated string") ||
			strings.Contains(message, "fts5: parser stack overflow") {
			return nil, ErrInvalidHistoryQuery
		}
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var hits []HistoryHit
	for rows.Next() {
		var hit HistoryHit
		var rank float64
		if err := rows.Scan(&hit.Seq, &hit.Role, &hit.Snippet, &rank); err != nil {
			return nil, err
		}
		hit.Epoch = sort.Search(len(cutoffs), func(i int) bool { return cutoffs[i] > hit.Seq })
		hits = append(hits, hit)
	}
	return hits, rows.Err()
}

func (s *Store) ReadHistory(ctx context.Context, sessionID string, start, end int, epoch *int, limit int) ([]HistoryMessage, []string, error) {
	if strings.TrimSpace(sessionID) == "" {
		return nil, nil, errors.New("session id is required")
	}
	if start < 0 || end < start {
		return nil, nil, errors.New("history range is invalid")
	}
	if limit <= 0 || limit > historyReadLimit {
		limit = historyReadLimit
	}
	cutoffs, err := s.historyCutoffs(ctx, sessionID)
	if err != nil {
		return nil, nil, err
	}
	rangeStart, rangeEnd, err := historyEpochRange(cutoffs, epoch)
	if err != nil {
		return nil, nil, err
	}
	if epoch != nil && (start < rangeStart || (rangeEnd != maxHistorySeq && end >= rangeEnd)) {
		return nil, nil, errors.New("history range is outside the requested epoch")
	}
	rows, err := s.db.QueryContext(ctx, `SELECT seq, content FROM messages
		WHERE session_id=? AND seq>=? AND seq<=? ORDER BY seq LIMIT ?`,
		sessionID, start, end, limit)
	if err != nil {
		return nil, nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []HistoryMessage
	var diagnostics []string
	for rows.Next() {
		var seq int
		var data string
		if err := rows.Scan(&seq, &data); err != nil {
			return nil, nil, err
		}
		var msg models.Message
		if err := json.Unmarshal([]byte(data), &msg); err != nil {
			if len(diagnostics) < 4 {
				diagnostics = append(diagnostics, fmt.Sprintf("skipped malformed message at seq %d", seq))
			}
			continue
		}
		if msg.Role == "system" {
			continue
		}
		out = append(out, HistoryMessage{Seq: seq, Epoch: sort.Search(len(cutoffs), func(i int) bool { return cutoffs[i] > seq }), Message: msg})
	}
	return out, diagnostics, rows.Err()
}

const maxHistorySeq = int(^uint(0) >> 1)

func (s *Store) historyCutoffs(ctx context.Context, sessionID string) ([]int, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT cutoff FROM compactions WHERE session_id=? ORDER BY seq`, sessionID)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var cutoffs []int
	for rows.Next() {
		var cutoff int
		if err := rows.Scan(&cutoff); err != nil {
			return nil, err
		}
		cutoffs = append(cutoffs, cutoff)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return cutoffs, nil
}

func historyEpochRange(cutoffs []int, epoch *int) (int, int, error) {
	if epoch == nil {
		return 0, maxHistorySeq, nil
	}
	if *epoch < 0 {
		return 0, 0, errors.New("history epoch must be non-negative")
	}
	if *epoch > len(cutoffs) {
		return 0, 0, fmt.Errorf("history epoch %d does not exist", *epoch)
	}
	start, end := 0, maxHistorySeq
	if *epoch > 0 {
		start = cutoffs[*epoch-1]
	}
	if *epoch < len(cutoffs) {
		end = cutoffs[*epoch]
	}
	return start, end, nil
}

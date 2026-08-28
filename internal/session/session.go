// Package session persists chat histories in ~/.ghg/sessions.db (SQLite).
package session

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"time"

	_ "modernc.org/sqlite"

	"github.com/sacca97/ghg/internal/artifact"
	"github.com/sacca97/ghg/internal/llm"
)

const schema = `
CREATE TABLE IF NOT EXISTS sessions (
	id         TEXT PRIMARY KEY,
	created_at TEXT NOT NULL,
	updated_at TEXT NOT NULL,
	cwd        TEXT NOT NULL,
	model      TEXT NOT NULL,
	provider   TEXT NOT NULL,
	title      TEXT NOT NULL DEFAULT '',
	goal       TEXT NOT NULL DEFAULT ''
);
CREATE TABLE IF NOT EXISTS messages (
	session_id TEXT NOT NULL REFERENCES sessions(id),
	seq        INTEGER NOT NULL,
	role       TEXT NOT NULL,
	content    TEXT NOT NULL, -- llm.Message JSON
	PRIMARY KEY (session_id, seq)
);
-- Artifact metadata is session-scoped. Payloads are immutable and shared by
-- content hash; the message sequence is only the ownership/fork/rewind index.
CREATE TABLE IF NOT EXISTS artifacts (
	session_id    TEXT NOT NULL REFERENCES sessions(id),
	message_seq   INTEGER NOT NULL,
	id            TEXT NOT NULL,
	tool_call_id  TEXT NOT NULL,
	tool_name     TEXT NOT NULL,
	media_type    TEXT NOT NULL DEFAULT '',
	original_bytes INTEGER NOT NULL,
	stored_bytes   INTEGER NOT NULL,
	hash          TEXT NOT NULL,
	path          TEXT NOT NULL,
	complete      INTEGER NOT NULL,
	metadata      TEXT NOT NULL DEFAULT '',
	created_at    TEXT NOT NULL,
	PRIMARY KEY (session_id, id, tool_call_id)
);
CREATE INDEX IF NOT EXISTS artifacts_session_seq ON artifacts(session_id, message_seq);
CREATE INDEX IF NOT EXISTS artifacts_session_created ON artifacts(session_id, created_at);
CREATE TABLE IF NOT EXISTS tasks (
	session_id  TEXT NOT NULL REFERENCES sessions(id),
	task_id     TEXT NOT NULL,
	description TEXT NOT NULL,
	prompt      TEXT NOT NULL,
	status      TEXT NOT NULL,
	report      TEXT NOT NULL DEFAULT '',
	started_at  TEXT NOT NULL,
	ended_at    TEXT NOT NULL DEFAULT '',
	PRIMARY KEY (session_id, task_id)
);
-- Workspace snapshots: one git stash ref per turn (keyed by the conversation
-- index the turn started at), so a conversation rewind can also restore the
-- files that turn changed. Same seq semantics as messages, so DeleteFrom
-- trims both together.
CREATE TABLE IF NOT EXISTS snapshots (
	session_id TEXT NOT NULL REFERENCES sessions(id),
	seq        INTEGER NOT NULL,
	ref        TEXT NOT NULL,
	created_at TEXT NOT NULL,
	PRIMARY KEY (session_id, seq)
);
-- Scheduled tasks: the wakeup channel's durable records. One row per task,
-- keyed by the session it fires into. The store keeps the schedule
-- expression, anchor, and last fire; the TUI's ticker evaluates due tasks.
CREATE TABLE IF NOT EXISTS schedules (
	session_id TEXT NOT NULL REFERENCES sessions(id),
	id         INTEGER NOT NULL,
	schedule   TEXT NOT NULL,      -- '@every 10m' | '@at <rfc3339>'
	prompt     TEXT NOT NULL,      -- the machine-authored turn to submit on fire
	anchor     TEXT NOT NULL,      -- grid origin (RFC3339)
	last_fire  TEXT NOT NULL DEFAULT '', -- last fire time ("" = never); one-shots complete here
	created_at TEXT NOT NULL,
	PRIMARY KEY (session_id, id)
);
-- Compaction events: append-only. Each row records a compaction as summary +
-- cutoff (the raw-log seq it folded). The messages table is never rewritten
-- by a compaction — Load derives the compacted view from the latest event,
-- so a bad compaction is inspectable and retryable.
CREATE TABLE IF NOT EXISTS compactions (
	session_id TEXT NOT NULL REFERENCES sessions(id),
	seq        INTEGER NOT NULL, -- compaction generation, 1-based
	cutoff     INTEGER NOT NULL, -- raw-log seq the summary replaces (1..cutoff-1)
	summary    TEXT NOT NULL,
	created_at TEXT NOT NULL,
	PRIMARY KEY (session_id, seq)
);`

// extraColumns are added idempotently after the base schema: SQLite's
// ADD COLUMN errors if the column already exists, so each is guarded by an
// information check in migrate(). New per-session bookkeeping lands here, not
// in the CREATE above (which only runs on a fresh DB).
var extraColumns = []struct{ name, def string }{
	{"forked_from", "forked_from TEXT NOT NULL DEFAULT ''"},     // source session id
	{"fork_seq", "fork_seq INTEGER NOT NULL DEFAULT 0"},         // branch point in the source
	{"tags", "tags TEXT NOT NULL DEFAULT ''"},                   // comma-separated labels
	{"pinned", "pinned INTEGER NOT NULL DEFAULT 0"},             // 1 = keep / sort first
	{"effort", "effort TEXT NOT NULL DEFAULT ''"},               // reasoning effort in effect ("" = global default)
	{"usage_in", "usage_in INTEGER NOT NULL DEFAULT 0"},         // cumulative input tokens (provider-reported)
	{"usage_cached", "usage_cached INTEGER NOT NULL DEFAULT 0"}, // of usage_in, tokens served from the prompt cache
	{"usage_out", "usage_out INTEGER NOT NULL DEFAULT 0"},       // cumulative output tokens
	{"todos", "todos TEXT NOT NULL DEFAULT ''"},                 // todowrite plan JSON ([]agent.Todo)
}

// Meta is a session's bookkeeping row.
type Meta struct {
	ID          string
	Title       string
	Model       string
	Provider    string
	CWD         string
	Goal        string
	ForkedFrom  string   // source session id when created by /fork ("" = root)
	ForkSeq     int      // conversation index the fork branched at
	Tags        []string // freeform labels, for filtering /resume
	Pinned      bool     // pinned sessions sort first and survive cleanup
	Effort      string   // reasoning effort for this session ("" = use the global default)
	UsageIn     int      // cumulative input tokens across the session's API calls
	UsageCached int      // of UsageIn, tokens served from the provider's prompt cache
	UsageOut    int      // cumulative output tokens
	UpdatedAt   time.Time
}

type Store struct{ db *sql.DB }

// Open opens (creating if needed) the sessions database at path.
func Open(path string) (*Store, error) {
	db, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, err
	}
	for _, pragma := range []string{
		"PRAGMA busy_timeout=5000",
		"PRAGMA journal_mode=WAL",   // faster commits, no read/write blocking
		"PRAGMA synchronous=NORMAL", // safe in WAL; skips per-commit fsync
		"PRAGMA temp_store=MEMORY",
	} {
		if _, err := db.Exec(pragma); err != nil {
			return nil, err
		}
	}
	if _, err := db.Exec(schema); err != nil {
		return nil, err
	}
	// migrate pre-goal databases; duplicate-column errors are expected
	_, _ = db.Exec(`ALTER TABLE sessions ADD COLUMN goal TEXT NOT NULL DEFAULT ''`)
	// later per-session bookkeeping (fork linkage, tags, pinned); the same
	// duplicate-column-tolerant migration as goal
	for _, c := range extraColumns {
		_, _ = db.Exec(`ALTER TABLE sessions ADD COLUMN ` + c.def)
	}
	// The artifact table was introduced after the initial Phase 1 slice; keep
	// databases created by that slice readable when metadata is added.
	_, _ = db.Exec(`ALTER TABLE artifacts ADD COLUMN metadata TEXT NOT NULL DEFAULT ''`)
	return &Store{db: db}, nil
}

// SetGoal stores the session's active goal ("" clears it).
func (s *Store) SetGoal(id, goal string) error {
	_, err := s.db.Exec(`UPDATE sessions SET goal=? WHERE id=?`, goal, id)
	return err
}

// SetTodos stores the session's todowrite plan as JSON ("" clears it). The
// plan is a whole-list snapshot: the model rewrites it in full each call, so
// this is a plain overwrite, not a merge.
func (s *Store) SetTodos(id, todosJSON string) error {
	_, err := s.db.Exec(`UPDATE sessions SET todos=? WHERE id=?`, todosJSON, id)
	return err
}

// Todos returns the session's stored todowrite plan JSON ("" when unset or
// the session is unknown). The agent package owns the schema.
func (s *Store) Todos(id string) string {
	var v string
	_ = s.db.QueryRow(`SELECT todos FROM sessions WHERE id=?`, id).Scan(&v)
	return v
}

// SetEffort stores the session's reasoning effort. "" means the row pre-dates
// per-session effort or never set one: resume falls back to the current global
// default and stamps it on the next save.
func (s *Store) SetEffort(id, effort string) error {
	_, err := s.db.Exec(`UPDATE sessions SET effort=? WHERE id=?`, effort, id)
	return err
}

// SetUsage stores the session's cumulative token totals (absolute values, not
// deltas) so a resumed session keeps its spend across restarts and
// compactions. Rows from before this column existed read as zero and get
// stamped with real totals on the next save.
func (s *Store) SetUsage(id string, in, cached, out int) error {
	_, err := s.db.Exec(`UPDATE sessions SET usage_in=?, usage_cached=?, usage_out=? WHERE id=?`, in, cached, out, id)
	return err
}

// Task is one background subagent's persisted record. It deliberately
// mirrors agent.BackgroundTask's exported fields without importing agent
// (session is a leaf; the TUI converts between them).
type Task struct {
	ID          string
	Description string
	Prompt      string
	Status      string // "running", "done", "error", "cancelled"
	Report      string
	StartedAt   time.Time
	EndedAt     time.Time
}

// SaveTask upserts a background subagent's record for a session. Called on
// start and on settle, so the final row holds the settled status/report.
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

// LoadTasks returns a session's persisted background subagents, oldest first.
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

func (s *Store) Close() error { return s.db.Close() }

func now() string { return time.Now().UTC().Format(time.RFC3339) }

// Create inserts a new session and returns its id.
func (s *Store) Create(cwd, model, provider string) (string, error) {
	b := make([]byte, 4)
	rand.Read(b)
	id := hex.EncodeToString(b)
	_, err := s.db.Exec(`INSERT INTO sessions (id, created_at, updated_at, cwd, model, provider) VALUES (?,?,?,?,?,?)`,
		id, now(), now(), cwd, model, provider)
	return id, err
}

// Save persists msgs[from:] (the conversation without the system prompt) and
// refreshes the session's metadata.
func (s *Store) Save(id string, from int, msgs []llm.Message, model, provider string) error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	// Once a compaction event exists, Agent.Messages is a derived prompt view:
	// its indexes no longer match the raw message sequence in SQLite. New
	// messages must append after the raw tail instead of replacing an older
	// row at the same derived index. The placeholder convention below keeps
	// this compatible with callers that pass the old view as padding.
	var compacted int
	if err := tx.QueryRow(`SELECT EXISTS(SELECT 1 FROM compactions WHERE session_id=?)`, id).Scan(&compacted); err != nil {
		return err
	}
	nextSeq := 0
	if compacted != 0 {
		if err := tx.QueryRow(`SELECT COALESCE(MAX(seq),0)+1 FROM messages WHERE session_id=?`, id).Scan(&nextSeq); err != nil {
			return err
		}
	}
	for i := from; i < len(msgs); i++ {
		// Placeholder rows (zero-value messages the caller never meant to
		// write, e.g. padding before a post-compaction tail) must not
		// clobber the raw log — skip them.
		if msgs[i].Role == "" {
			continue
		}
		seq := i
		if compacted != 0 {
			seq = nextSeq
			nextSeq++
		}
		// A re-save can replace a tool message with a version that no longer
		// carries an artifact. Remove the old index rows before inserting the
		// current message, while leaving payload files untouched.
		if _, err := tx.Exec(`DELETE FROM artifacts WHERE session_id=? AND message_seq=?`, id, seq); err != nil {
			return err
		}
		data, err := json.Marshal(msgs[i])
		if err != nil {
			return err
		}
		if _, err := tx.Exec(`INSERT OR REPLACE INTO messages (session_id, seq, role, content) VALUES (?,?,?,?)`,
			id, seq, msgs[i].Role, string(data)); err != nil {
			return err
		}
		if msgs[i].Artifact != nil {
			ref := *msgs[i].Artifact
			relPath, err := artifact.RelativePath(ref)
			if err != nil {
				return fmt.Errorf("message %d artifact: %w", seq, err)
			}
			complete := 0
			if ref.Complete {
				complete = 1
			}
			metadata := ""
			if len(ref.Metadata) > 0 {
				data, err := json.Marshal(ref.Metadata)
				if err != nil {
					return fmt.Errorf("message %d artifact metadata: %w", seq, err)
				}
				metadata = string(data)
			}
			toolName := msgs[i].Name
			if toolName == "" {
				toolName = msgs[i].Source
			}
			if _, err := tx.Exec(`INSERT OR REPLACE INTO artifacts
				(session_id, message_seq, id, tool_call_id, tool_name, media_type,
				 original_bytes, stored_bytes, hash, path, complete, metadata, created_at)
				VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?)`,
				id, seq, ref.ID, msgs[i].ToolCallID, toolName, ref.MediaType,
				ref.OriginalBytes, ref.StoredBytes, ref.Hash, relPath, complete, metadata, now()); err != nil {
				return err
			}
		}
	}
	title := ""
	for _, m := range msgs {
		if m.Role == "user" {
			title = truncate(strings.Join(strings.Fields(m.TextContent()), " "), 64)
			break
		}
	}
	if _, err := tx.Exec(`UPDATE sessions SET updated_at=?, model=?, provider=?, title=CASE WHEN title='' THEN ? ELSE title END WHERE id=?`,
		now(), model, provider, title, id); err != nil {
		return err
	}
	return tx.Commit()
}

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

// DeleteSession removes a session and its database-owned history. It does not
// remove immutable payload files; run ReferencedArtifactHashes followed by
// artifact.Store.GarbageCollect to reclaim payloads no longer referenced by
// any session.
func (s *Store) DeleteSession(id string) error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	for _, table := range []string{"artifacts", "messages", "tasks", "snapshots", "schedules", "compactions"} {
		if _, err := tx.Exec(`DELETE FROM `+table+` WHERE session_id=?`, id); err != nil {
			return err
		}
	}
	if _, err := tx.Exec(`DELETE FROM sessions WHERE id=?`, id); err != nil {
		return err
	}
	return tx.Commit()
}

// Load resolves idOrPrefix to a session and returns its metadata and messages.
func (s *Store) Load(idOrPrefix string) (Meta, []llm.Message, error) {
	rows, err := s.db.Query(`SELECT id, title, model, provider, cwd, goal, forked_from, fork_seq, tags, pinned, effort, usage_in, usage_cached, usage_out, updated_at FROM sessions WHERE id LIKE ?||'%' LIMIT 3`, idOrPrefix)
	if err != nil {
		return Meta{}, nil, err
	}
	metas, err := scanMetas(rows)
	if err != nil {
		return Meta{}, nil, err
	}
	switch len(metas) {
	case 0:
		return Meta{}, nil, fmt.Errorf("no session matching %q", idOrPrefix)
	case 1:
	default:
		return Meta{}, nil, fmt.Errorf("session id %q is ambiguous", idOrPrefix)
	}
	meta := metas[0]

	// pre-size the slice: a long session is hundreds of rows; the COUNT is
	// one index scan and avoids O(log n) reallocs while scanning
	var count int
	_ = s.db.QueryRow(`SELECT COUNT(*) FROM messages WHERE session_id=?`, meta.ID).Scan(&count)

	mrows, err := s.db.Query(`SELECT seq, content FROM messages WHERE session_id=? ORDER BY seq`, meta.ID)
	if err != nil {
		return Meta{}, nil, err
	}
	defer func() { _ = mrows.Close() }()
	stored := make([]storedMessage, 0, count)
	for mrows.Next() {
		var seq int
		var data string
		if err := mrows.Scan(&seq, &data); err != nil {
			return Meta{}, nil, err
		}
		var m llm.Message
		if err := json.Unmarshal([]byte(data), &m); err != nil {
			return Meta{}, nil, err
		}
		stored = append(stored, storedMessage{seq: seq, msg: m})
	}
	return meta, answerDanglingToolCalls(applyCompactionRows(s.db, meta.ID, stored)), mrows.Err()
}

type storedMessage struct {
	seq int
	msg llm.Message
}

// applyCompaction derives the compacted view from the raw log: the latest
// compaction event's summary replaces raw messages [1, cutoff), keeping the
// system prompt (seq 0) and the raw tail. "Raw" matters: a stored row that is
// itself a summary (system role past index 0) is a *derived* row saved after
// a compaction — folding it again would nest summaries — so the cutoff only
// ever applies to non-system rows. No event → the log loads verbatim. This is
// what makes a compaction non-destructive: the event is metadata, the raw
// rows are the history.
func applyCompactionRows(db *sql.DB, sessionID string, rows []storedMessage) []llm.Message {
	var cutoff int
	var summary string
	err := db.QueryRow(`SELECT cutoff, summary FROM compactions WHERE session_id=? ORDER BY seq DESC LIMIT 1`,
		sessionID).Scan(&cutoff, &summary)
	if err != nil || cutoff <= 0 {
		return storedMessages(rows) // no event
	}
	// The cutoff is a raw SQLite sequence, not an index in the loaded slice.
	// This matters because normal TUI saves omit the system prompt (seq 0),
	// while a few low-level callers persist it for round-trip tests.
	fold := len(rows)
	for i, row := range rows {
		if row.seq >= cutoff {
			fold = i
			break
		}
	}
	if fold == len(rows) && (len(rows) == 0 || rows[len(rows)-1].seq < cutoff) {
		return storedMessages(rows) // the event post-dates the raw log
	}
	out := make([]llm.Message, 0, len(rows)+1)
	start := 0
	if len(rows) > 0 && rows[0].seq == 0 && rows[0].msg.Role == "system" {
		out = append(out, rows[0].msg)
		start = 1
	}
	out = append(out, llm.Message{Role: "system", Content: "Summary of the conversation so far:\n\n" + summary})
	// keep the last derived summary before the fold (a second compaction's
	// saved row — it summarizes history the new summary doesn't reach)
	var prior []llm.Message
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

func storedMessages(rows []storedMessage) []llm.Message {
	msgs := make([]llm.Message, 0, len(rows))
	for _, row := range rows {
		msgs = append(msgs, row.msg)
	}
	return msgs
}

// answerDanglingToolCalls appends a synthetic error result for every
// persisted tool call that has none — a ctrl+c or crash mid-turn interrupts
// between the assistant message and its results, and the API rejects a
// resumed conversation with an unanswered tool_call. Results go right after
// the assistant message (the API wants them before the next non-tool
// message); a fully-answered history is returned unchanged.
func answerDanglingToolCalls(msgs []llm.Message) []llm.Message {
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
	out := make([]llm.Message, 0, len(msgs)+4)
	for _, m := range msgs {
		out = append(out, m)
		if m.Role != "assistant" {
			continue
		}
		for _, tc := range m.ToolCalls {
			if !answered[tc.ID] {
				out = append(out, llm.Message{
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

// Recent returns up to n sessions, newest first.
func (s *Store) Recent(n int) ([]Meta, error) {
	rows, err := s.db.Query(`SELECT id, title, model, provider, cwd, goal, forked_from, fork_seq, tags, pinned, effort, usage_in, usage_cached, usage_out, updated_at FROM sessions
		WHERE EXISTS (SELECT 1 FROM messages WHERE session_id = sessions.id)
		ORDER BY updated_at DESC LIMIT ?`, n)
	if err != nil {
		return nil, err
	}
	return scanMetas(rows)
}

// MostRecentForCWD returns the newest session with persisted messages for cwd.
// Empty sessions are deliberately excluded, matching Recent and the resume
// picker: --continue should return something the user can actually resume.
func (s *Store) MostRecentForCWD(cwd string) (Meta, error) {
	rows, err := s.db.Query(`SELECT id, title, model, provider, cwd, goal, forked_from, fork_seq, tags, pinned, effort, usage_in, usage_cached, usage_out, updated_at FROM sessions
		WHERE cwd=? AND EXISTS (SELECT 1 FROM messages WHERE session_id = sessions.id)
		ORDER BY updated_at DESC, rowid DESC LIMIT 1`, cwd)
	if err != nil {
		return Meta{}, err
	}
	metas, err := scanMetas(rows)
	if err != nil {
		return Meta{}, err
	}
	if len(metas) == 0 {
		return Meta{}, fmt.Errorf("no resumable session for working directory %q", cwd)
	}
	return metas[0], nil
}

// UserHistory returns user-message contents across ALL sessions (every folder),
// newest first and de-duplicated, for up-arrow input recall. Order is by the
// session's last activity then the message's position within it, so the most
// recently typed input comes first. Only messages the human actually typed are
// recalled: steered background-task
// results and goal-continuation prompts are stored as role "user" too, but
// they're injected by ghg, not written by the user. Those carry Authored=false
// and are skipped; only Authored=true messages come back.
func (s *Store) UserHistory(limit int) ([]string, error) {
	rows, err := s.db.Query(`SELECT m.content FROM messages m
		JOIN sessions s ON s.id = m.session_id
		WHERE m.role='user'
		ORDER BY s.updated_at DESC, m.seq DESC`)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	seen := map[string]bool{}
	var out []string
	for rows.Next() {
		var data string
		if err := rows.Scan(&data); err != nil {
			return nil, err
		}
		var msg llm.Message
		if err := json.Unmarshal([]byte(data), &msg); err != nil {
			continue // skip malformed rows rather than fail the whole recall
		}
		if !msg.Authored {
			continue // injected by ghg (steered task result / goal prompt), not typed
		}
		content := strings.TrimSpace(msg.TextContent())
		if content == "" || seen[content] {
			continue
		}
		seen[content] = true
		out = append(out, content)
		if limit > 0 && len(out) >= limit {
			break
		}
	}
	return out, rows.Err()
}

// LastExchange returns the text of the session's last user message and last
// assistant response, for previews.
func (s *Store) LastExchange(id string) (user, assistant string) {
	for _, q := range []struct {
		role string
		dst  *string
	}{{"user", &user}, {"assistant", &assistant}} {
		var data string
		if err := s.db.QueryRow(`SELECT content FROM messages WHERE session_id=? AND role=? ORDER BY seq DESC LIMIT 1`,
			id, q.role).Scan(&data); err == nil {
			var m llm.Message
			if json.Unmarshal([]byte(data), &m) == nil {
				*q.dst = m.TextContent()
			}
		}
	}
	return user, assistant
}

// ClearMessages deletes the stored message rows for a session (the session
// row is kept). Used after compaction rewrites history: the compacted
// messages are smaller and re-seqenced from 0, so the old rows must go first.
func (s *Store) ClearMessages(id string) error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	if _, err := tx.Exec(`DELETE FROM messages WHERE session_id=?`, id); err != nil {
		return err
	}
	if _, err := tx.Exec(`DELETE FROM artifacts WHERE session_id=?`, id); err != nil {
		return err
	}
	return tx.Commit()
}

// DeleteFrom drops every stored message with seq >= from, plus the workspace
// snapshots for those turns (their refs stop being restorable once the
// conversation no longer contains the turn). seq equals the conversation
// index (Save persists msgs[i] at seq i; the system prompt is never
// persisted). Used by rewind: the clipped tail is deleted from disk but kept
// in memory for forward travel.
func (s *Store) DeleteFrom(id string, from int) error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	if _, err := tx.Exec(`DELETE FROM messages WHERE session_id=? AND seq>=?`, id, from); err != nil {
		return err
	}
	if _, err := tx.Exec(`DELETE FROM snapshots WHERE session_id=? AND seq>=?`, id, from); err != nil {
		return err
	}
	if _, err := tx.Exec(`DELETE FROM artifacts WHERE session_id=? AND message_seq>=?`, id, from); err != nil {
		return err
	}
	return tx.Commit()
}

// SetSnapshot records the workspace snapshot ref for the turn starting at
// conversation index seq ("" deletes: the turn's files were restored away).
func (s *Store) SetSnapshot(id string, seq int, ref string) error {
	if ref == "" {
		_, err := s.db.Exec(`DELETE FROM snapshots WHERE session_id=? AND seq=?`, id, seq)
		return err
	}
	_, err := s.db.Exec(`INSERT OR REPLACE INTO snapshots (session_id, seq, ref, created_at) VALUES (?,?,?,?)`,
		id, seq, ref, now())
	return err
}

// Snapshots returns the session's workspace snapshot refs keyed by
// conversation index.
func (s *Store) Snapshots(id string) map[int]string {
	rows, err := s.db.Query(`SELECT seq, ref FROM snapshots WHERE session_id=?`, id)
	if err != nil {
		return nil
	}
	defer func() { _ = rows.Close() }()
	out := map[int]string{}
	for rows.Next() {
		var seq int
		var ref string
		if rows.Scan(&seq, &ref) == nil {
			out[seq] = ref
		}
	}
	return out
}

// Schedule is one scheduled task's durable record.
type Schedule struct {
	ID       int
	Schedule string    // '@every 10m' | '@at <rfc3339>'
	Prompt   string    // the machine-authored turn submitted on fire
	Anchor   time.Time // grid origin
	LastFire time.Time // zero = never fired
}

// AddSchedule records a scheduled task and returns its id.
func (s *Store) AddSchedule(sessionID, schedule, prompt string, anchor time.Time) (int, error) {
	var id int
	err := s.db.QueryRow(`INSERT INTO schedules (session_id, id, schedule, prompt, anchor, created_at)
		SELECT ?, COALESCE(MAX(id),0)+1, ?, ?, ?, ? FROM schedules WHERE session_id=? RETURNING id`,
		sessionID, schedule, prompt, anchor.UTC().Format(time.RFC3339), now(), sessionID).Scan(&id)
	return id, err
}

// Schedules returns a session's scheduled tasks, id order.
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

// MarkFired stamps a task's last fire (a fired one-shot stays listed but
// never fires again).
func (s *Store) MarkFired(sessionID string, id int, at time.Time) error {
	_, err := s.db.Exec(`UPDATE schedules SET last_fire=? WHERE session_id=? AND id=?`,
		at.UTC().Format(time.RFC3339), sessionID, id)
	return err
}

// DeleteSchedule removes a scheduled task.
func (s *Store) DeleteSchedule(sessionID string, id int) error {
	_, err := s.db.Exec(`DELETE FROM schedules WHERE session_id=? AND id=?`, sessionID, id)
	return err
}

// ClearSnapshots drops all of a session's workspace snapshot rows (compaction
// re-seqs messages, so the keys stop mapping to turns).
func (s *Store) ClearSnapshots(id string) error {
	_, err := s.db.Exec(`DELETE FROM snapshots WHERE session_id=?`, id)
	return err
}

// Compaction is one recorded compaction event.
type Compaction struct {
	Seq     int    // generation (1-based)
	Cutoff  int    // raw-log seq the summary replaces
	Summary string // the generated summary text
}

// RecordCompaction appends a compaction event. The raw messages stay.
func (s *Store) RecordCompaction(id string, cutoff int, summary string) error {
	_, err := s.db.Exec(`INSERT INTO compactions (session_id, seq, cutoff, summary, created_at)
		SELECT ?, COALESCE(MAX(seq),0)+1, ?, ?, ? FROM compactions WHERE session_id=?`,
		id, cutoff, summary, now(), id)
	return err
}

// Compactions returns a session's compaction events, oldest first.
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

// DeleteCompaction removes one compaction event by generation (retry drops
// the bad event before re-compacting from the raw log).
func (s *Store) DeleteCompaction(id string, seq int) error {
	_, err := s.db.Exec(`DELETE FROM compactions WHERE session_id=? AND seq=?`, id, seq)
	return err
}

// RawMessages returns the full stored log (no compaction view applied) —
// the inspection/retry surface for compactions.
func (s *Store) RawMessages(id string) []llm.Message {
	rows, err := s.db.Query(`SELECT content FROM messages WHERE session_id=? ORDER BY seq`, id)
	if err != nil {
		return nil
	}
	defer func() { _ = rows.Close() }()
	var msgs []llm.Message
	for rows.Next() {
		var data string
		if rows.Scan(&data) != nil {
			continue
		}
		var m llm.Message
		if json.Unmarshal([]byte(data), &m) == nil {
			msgs = append(msgs, m)
		}
	}
	return msgs
}

// SetTitle retitles a session (/rename).
func (s *Store) SetTitle(id, title string) error {
	_, err := s.db.Exec(`UPDATE sessions SET title=? WHERE id=?`, title, id)
	return err
}

// Fork copies a session's stored rows with seq <= uptoSeq (pass len(msgs)
// for a full copy — one past the last row) into a new session titled title,
// carrying over cwd/model/provider/goal, and returns the new id. seq equals
// the conversation index (the system prompt is never persisted). The source
// session is untouched. The rows are cloned in one INSERT…SELECT, so the DB
// does the copy; nothing round-trips through Go.
func (s *Store) Fork(srcID string, uptoSeq int, title string) (string, error) {
	b := make([]byte, 4)
	rand.Read(b)
	newID := hex.EncodeToString(b)
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
	if uptoSeq > 0 {
		if _, err := tx.Exec(`INSERT INTO messages (session_id, seq, role, content)
			SELECT ?, seq, role, content FROM messages WHERE session_id=? AND seq <= ?`,
			newID, srcID, uptoSeq); err != nil {
			return "", err
		}
		artifactIDs, err := artifactIDsInMessages(tx, srcID, uptoSeq)
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
		if len(artifactIDs) > 0 {
			placeholders := make([]string, len(artifactIDs))
			for i, id := range artifactIDs {
				placeholders[i] = "?"
				args = append(args, id)
			}
			query += ` OR id IN (` + strings.Join(placeholders, ",") + ")"
		}
		query += ")"
		if _, err := tx.Exec(query, args...); err != nil {
			return "", err
		}
		// A full/raw fork should retain the derived prompt view. The payloads
		// and raw messages are already copied above; copying only events whose
		// cutoff is inside the branch keeps a partial fork independently
		// loadable without inventing a new cutoff.
		if _, err := tx.Exec(`INSERT INTO compactions (session_id, seq, cutoff, summary, created_at)
			SELECT ?, seq, cutoff, summary, created_at FROM compactions
			WHERE session_id=? AND cutoff<=?`, newID, srcID, uptoSeq); err != nil {
			return "", err
		}
	}
	return newID, tx.Commit()
}

func artifactIDsInMessages(tx *sql.Tx, sessionID string, uptoSeq int) ([]string, error) {
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
		var msg llm.Message
		if err := json.Unmarshal([]byte(data), &msg); err != nil || msg.Artifact == nil {
			continue
		}
		if !seen[msg.Artifact.ID] {
			seen[msg.Artifact.ID] = true
			ids = append(ids, msg.Artifact.ID)
		}
	}
	return ids, rows.Err()
}

// SetTags replaces a session's label set (comma-separated storage).
func (s *Store) SetTags(id string, tags []string) error {
	_, err := s.db.Exec(`UPDATE sessions SET tags=? WHERE id=?`, strings.Join(tags, ","), id)
	return err
}

// SetPinned marks a session pinned (sorts first in /resume, kept by cleanup).
func (s *Store) SetPinned(id string, pinned bool) error {
	v := 0
	if pinned {
		v = 1
	}
	_, err := s.db.Exec(`UPDATE sessions SET pinned=? WHERE id=?`, v, id)
	return err
}

// ForksOf lists sessions forked from id, newest first — the session tree's
// children of one node.
func (s *Store) ForksOf(id string) ([]Meta, error) {
	rows, err := s.db.Query(`SELECT id, title, model, provider, cwd, goal, forked_from, fork_seq, tags, pinned, effort, usage_in, usage_cached, usage_out, updated_at
		FROM sessions WHERE forked_from=? ORDER BY updated_at DESC`, id)
	if err != nil {
		return nil, err
	}
	return scanMetas(rows)
}

// ForkTitle derives the default fork name: "<title> (fork #N)" with N
// incremented past any existing fork of the same base (opencode's
// getForkedTitle — packages/opencode/src/session/session.ts:162). Falls back
// to "session (fork #1)" for untitled sessions.
func (s *Store) ForkTitle(base string) (string, error) {
	if base == "" {
		base = "session"
	}
	// unwrap an existing "(fork #N)" suffix so forks of forks increment
	// instead of nesting: "x (fork #2)" → "x (fork #3)", not "x (fork #2) (fork #1)"
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
		// exact suffix match only: a manually renamed "x (fork #9) notes"
		// must not inflate the numbering
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

func scanMetas(rows *sql.Rows) ([]Meta, error) {
	defer func() { _ = rows.Close() }()
	var out []Meta
	for rows.Next() {
		var m Meta
		var updated, tags string
		var pinned int
		if err := rows.Scan(&m.ID, &m.Title, &m.Model, &m.Provider, &m.CWD, &m.Goal,
			&m.ForkedFrom, &m.ForkSeq, &tags, &pinned, &m.Effort,
			&m.UsageIn, &m.UsageCached, &m.UsageOut, &updated); err != nil {
			return nil, err
		}
		if tags != "" {
			m.Tags = strings.Split(tags, ",")
		}
		m.Pinned = pinned != 0
		m.UpdatedAt, _ = time.Parse(time.RFC3339, updated)
		out = append(out, m)
	}
	return out, rows.Err()
}

func truncate(s string, n int) string {
	if len(s) > n {
		return s[:n-1] + "…"
	}
	return s
}

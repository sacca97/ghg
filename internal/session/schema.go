package session

import (
	"database/sql"
	"fmt"
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
	content    TEXT NOT NULL, -- models.Message JSON
	PRIMARY KEY (session_id, seq)
);
-- Output metadata is session-scoped. The table name is retained for database
-- compatibility. Payloads are immutable and shared by
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
);
-- Goal state is separate from the legacy sessions.goal mirror so a goal can
-- retain its identity, lifecycle, accounting, and checkpoint history.
CREATE TABLE IF NOT EXISTS goals (
	session_id       TEXT NOT NULL REFERENCES sessions(id),
	goal_id          TEXT NOT NULL,
	objective        TEXT NOT NULL,
	status           TEXT NOT NULL,
	rounds           INTEGER NOT NULL DEFAULT 0,
	usage_in         INTEGER NOT NULL DEFAULT 0,
	usage_cached     INTEGER NOT NULL DEFAULT 0,
	usage_out        INTEGER NOT NULL DEFAULT 0,
	progress         TEXT NOT NULL DEFAULT '',
	blocker          TEXT NOT NULL DEFAULT '',
	created_at       TEXT NOT NULL,
	updated_at       TEXT NOT NULL,
	PRIMARY KEY (session_id, goal_id)
);
CREATE INDEX IF NOT EXISTS goals_session_updated ON goals(session_id, updated_at DESC);
CREATE TABLE IF NOT EXISTS goal_checkpoints (
	session_id       TEXT NOT NULL REFERENCES sessions(id),
	goal_id          TEXT NOT NULL,
	seq              INTEGER NOT NULL,
	status           TEXT NOT NULL,
	rounds           INTEGER NOT NULL DEFAULT 0,
	usage_in         INTEGER NOT NULL DEFAULT 0,
	usage_cached     INTEGER NOT NULL DEFAULT 0,
	usage_out        INTEGER NOT NULL DEFAULT 0,
	progress         TEXT NOT NULL DEFAULT '',
	blocker          TEXT NOT NULL DEFAULT '',
	created_at       TEXT NOT NULL,
	PRIMARY KEY (session_id, goal_id, seq)
);
-- Read observations are exact, bounded byte ranges issued to the model. They
-- are session-owned authorization evidence for stateful edits, not hashes.
CREATE TABLE IF NOT EXISTS observations (
	session_id   TEXT NOT NULL REFERENCES sessions(id),
	observation_id TEXT NOT NULL,
	path         TEXT NOT NULL,
	start_line   INTEGER NOT NULL,
	end_line     INTEGER NOT NULL,
	next_offset  INTEGER NOT NULL,
	issued_bytes INTEGER NOT NULL,
	content      TEXT NOT NULL,
	complete     INTEGER NOT NULL,
	created_at   TEXT NOT NULL,
	PRIMARY KEY (session_id, observation_id)
);
CREATE INDEX IF NOT EXISTS observations_session_path ON observations(session_id, path);
-- Search snapshots back cursor pagination with an immutable, bounded result
-- set. The JSON payload is intentionally opaque to SQL.
CREATE TABLE IF NOT EXISTS search_snapshots (
	session_id TEXT NOT NULL REFERENCES sessions(id),
	snapshot_id TEXT NOT NULL,
	kind       TEXT NOT NULL,
	snapshot   TEXT NOT NULL,
	created_at TEXT NOT NULL,
	PRIMARY KEY (session_id, snapshot_id)
);
CREATE INDEX IF NOT EXISTS search_snapshots_session_created ON search_snapshots(session_id, created_at);
-- Derived, rebuildable full-text index for bounded session history recall.
-- Raw messages remain authoritative; this table stores only extracted text.
CREATE VIRTUAL TABLE IF NOT EXISTS history_fts USING fts5(
	session_id UNINDEXED,
	seq        UNINDEXED,
	role       UNINDEXED,
	content
);
-- Persisted structured workflow results (plans, reviews) produced by terminal tools.
CREATE TABLE IF NOT EXISTS workflow_results (
	session_id   TEXT NOT NULL REFERENCES sessions(id),
	result_id    TEXT NOT NULL,
	kind         TEXT NOT NULL, -- 'plan' | 'review'
	version      INTEGER NOT NULL,
	payload      TEXT NOT NULL, -- JSON
	role         TEXT NOT NULL DEFAULT '',
	provider     TEXT NOT NULL DEFAULT '',
	model        TEXT NOT NULL DEFAULT '',
	message_seq  INTEGER NOT NULL DEFAULT 0,
	created_at   TEXT NOT NULL,
	PRIMARY KEY (session_id, result_id)
);
CREATE INDEX IF NOT EXISTS workflow_results_session_kind ON workflow_results(session_id, kind, created_at DESC);
`

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

func applySchema(db *sql.DB) error {
	if _, err := db.Exec(schema); err != nil {
		return err
	}
	// migrate pre-goal databases; duplicate-column errors are expected
	_, _ = db.Exec(`ALTER TABLE sessions ADD COLUMN goal TEXT NOT NULL DEFAULT ''`)
	// later per-session bookkeeping (fork linkage, tags, pinned); the same
	// duplicate-column-tolerant migration as goal
	for _, c := range extraColumns {
		_, _ = db.Exec(`ALTER TABLE sessions ADD COLUMN ` + c.def)
	}
	// Keep databases created by the initial Phase 1 slice readable when output
	// metadata is added.
	_, _ = db.Exec(`ALTER TABLE artifacts ADD COLUMN metadata TEXT NOT NULL DEFAULT ''`)
	if err := migrateLegacyGoals(db); err != nil {
		return err
	}
	if err := backfillHistoryFTS(db); err != nil {
		return fmt.Errorf("backfill history index: %w", err)
	}
	return nil
}

// Package session persists chat histories in ~/.ghg/sessions.db (SQLite).
package session

import (
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	_ "modernc.org/sqlite"

	"github.com/sacca97/ghg/internal/models"
)

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

const sessionMetaColumns = `id, title, model, provider, cwd, goal, forked_from, fork_seq, tags, pinned, effort, usage_in, usage_cached, usage_out, updated_at`

type Store struct {
	db      *sql.DB
	Outputs *OutputStore
}

// Open opens (creating if needed) the sessions database at path.
func Open(path string) (*Store, error) {
	dsn := path
	if !strings.Contains(dsn, "?") {
		dsn += "?_pragma=busy_timeout(5000)&_pragma=journal_mode(WAL)&_pragma=synchronous(NORMAL)&_pragma=temp_store(MEMORY)"
	}
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
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
	if err := applySchema(db); err != nil {
		_ = db.Close()
		return nil, err
	}
	return &Store{db: db}, nil
}

// SetGoal stores the session's active goal ("" clears it).
func (s *Store) SetGoal(id, objective string) error {
	objective = strings.TrimSpace(objective)
	if objective == "" {
		return s.ClearGoal(id)
	}
	record, ok, err := s.LoadGoal(id)
	if err != nil {
		return err
	}
	if !ok {
		record = NewGoal(objective)
		record.ID = "legacy-" + id
	} else {
		record.Objective = objective
		record.Status = GoalStatusActive
		record.Blocker = ""
		record.UpdatedAt = time.Now().UTC()
	}
	return s.CheckpointGoal(id, record)
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

// SetRoute records the model/provider selected for an existing session even
// when no new message has been saved yet.
func (s *Store) SetRoute(id, model, provider string) error {
	_, err := s.db.Exec(`UPDATE sessions SET model=?, provider=?, updated_at=? WHERE id=?`, model, provider, now(), id)
	return err
}

func (s *Store) Close() error { return s.db.Close() }

func now() string { return time.Now().UTC().Format(time.RFC3339) }

// NewSessionID returns an id suitable for a session and worker runtime.
func NewSessionID() string {
	b := make([]byte, 4)
	if _, err := rand.Read(b); err == nil {
		return hex.EncodeToString(b)
	}
	return fmt.Sprintf("%x", time.Now().UnixNano())
}

// Create inserts a new session and returns its id.
func (s *Store) Create(cwd, model, provider string) (string, error) {
	id := NewSessionID()
	return id, s.CreateWithID(id, cwd, model, provider)
}

// CreateWithID inserts a session with a caller-owned id.
func (s *Store) CreateWithID(id, cwd, model, provider string) error {
	_, err := s.db.Exec(`INSERT INTO sessions (id, created_at, updated_at, cwd, model, provider) VALUES (?,?,?,?,?,?)`,
		id, now(), now(), cwd, model, provider)
	return err
}

// Save persists msgs[from:] (the conversation without the system prompt) and
// refreshes the session's metadata.
func (s *Store) Save(id string, from int, msgs []models.Message, model, provider string) error {
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
		// carries an output. Remove the old index rows before inserting the
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
		if err := replaceHistoryFTS(tx, id, seq, msgs[i]); err != nil {
			return err
		}
		if msgs[i].Output != nil {
			ref := *msgs[i].Output
			relPath, err := RelativePath(ref)
			if err != nil {
				return fmt.Errorf("message %d output: %w", seq, err)
			}
			complete := 0
			if ref.Complete {
				complete = 1
			}
			metadata := ""
			if len(ref.Metadata) > 0 {
				data, err := json.Marshal(ref.Metadata)
				if err != nil {
					return fmt.Errorf("message %d output metadata: %w", seq, err)
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
			title = DeterministicTitle(m.TextContent())
			break
		}
	}
	if _, err := tx.Exec(`UPDATE sessions SET updated_at=?, model=?, provider=?, title=CASE WHEN title='' THEN ? ELSE title END WHERE id=?`,
		now(), model, provider, title, id); err != nil {
		return err
	}
	return tx.Commit()
}

func DeterministicTitle(text string) string {
	text = strings.Map(func(r rune) rune {
		if unicode.IsControl(r) {
			if unicode.IsSpace(r) {
				return ' '
			}
			return -1
		}
		return r
	}, text)
	title := strings.Join(strings.Fields(text), " ")
	if utf8.RuneCountInString(title) > 64 {
		runes := []rune(title)
		title = strings.TrimSpace(string(runes[:64]))
	}
	return title
}

// DeleteSession removes a session, its database-owned history, and its Git
// snapshot refs. It does not remove immutable payload files; run
// ReferencedOutputHashes followed by OutputStore.GarbageCollect to reclaim
// payloads no longer referenced by any session.
func (s *Store) DeleteSession(id string) error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	var cwd string
	if err := tx.QueryRow(`SELECT cwd FROM sessions WHERE id=?`, id).Scan(&cwd); err != nil && err != sql.ErrNoRows {
		return err
	}
	rows, err := tx.Query(`SELECT ref FROM snapshots WHERE session_id=?`, id)
	if err != nil {
		return err
	}
	var refs []string
	for rows.Next() {
		var ref string
		if err := rows.Scan(&ref); err != nil {
			_ = rows.Close()
			return err
		}
		refs = append(refs, ref)
	}
	if err := rows.Close(); err != nil {
		return err
	}
	if err := rows.Err(); err != nil {
		return err
	}
	for _, table := range []string{"artifacts", "messages", "history_fts", "tasks", "snapshots", "schedules", "compactions", "goal_checkpoints", "goals", "observations", "search_snapshots", "workflow_results"} {
		if _, err := tx.Exec(`DELETE FROM `+table+` WHERE session_id=?`, id); err != nil {
			return err
		}
	}
	if _, err := tx.Exec(`DELETE FROM sessions WHERE id=?`, id); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	for _, ref := range refs {
		if cwd != "" {
			DropSnapshot(cwd, ref)
		}
	}
	return nil
}

// Load resolves idOrPrefix to a session and returns its metadata and messages.
func (s *Store) Load(idOrPrefix string) (Meta, []models.Message, error) {
	rows, err := s.db.Query(`SELECT `+sessionMetaColumns+` FROM sessions WHERE id LIKE ?||'%' LIMIT 3`, idOrPrefix)
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
		var m models.Message
		if err := json.Unmarshal([]byte(data), &m); err != nil {
			return Meta{}, nil, err
		}
		stored = append(stored, storedMessage{seq: seq, msg: m})
	}
	return meta, answerDanglingToolCalls(applyCompactionRows(s.db, meta.ID, stored)), mrows.Err()
}

// Recent returns up to n sessions, newest first.
func (s *Store) Recent(n int) ([]Meta, error) {
	rows, err := s.db.Query(`SELECT `+sessionMetaColumns+` FROM sessions
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
	rows, err := s.db.Query(`SELECT `+sessionMetaColumns+` FROM sessions
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
		var msg models.Message
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
			var m models.Message
			if json.Unmarshal([]byte(data), &m) == nil {
				*q.dst = m.TextContent()
			}
		}
	}
	return user, assistant
}

// DeleteFrom drops the stored tail at a conversation-view boundary, plus
// snapshots and workflow results for those turns. Compacted sessions map the
// view boundary back to raw message sequence numbers before deleting.
func (s *Store) DeleteFrom(id string, from int, before []models.Message) error {
	rawFrom := from
	events := s.Compactions(id)
	if len(events) > 0 {
		rawFrom = s.RawCutoff(id, from, before)
	}
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	if _, err := tx.Exec(`DELETE FROM messages WHERE session_id=? AND seq>=?`, id, rawFrom); err != nil {
		return err
	}
	if _, err := tx.Exec(`DELETE FROM history_fts WHERE session_id=? AND seq>=?`, id, rawFrom); err != nil {
		return err
	}
	if _, err := tx.Exec(`DELETE FROM snapshots WHERE session_id=? AND seq>=?`, id, from); err != nil {
		return err
	}
	if _, err := tx.Exec(`DELETE FROM artifacts WHERE session_id=? AND message_seq>=?`, id, rawFrom); err != nil {
		return err
	}
	if _, err := tx.Exec(`DELETE FROM workflow_results WHERE session_id=? AND message_seq>=?`, id, from); err != nil {
		return err
	}
	if len(events) > 0 && rawFrom < events[len(events)-1].Cutoff {
		if _, err := tx.Exec(`DELETE FROM compactions WHERE session_id=?`, id); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// SetTitle retitles a session (/rename).
func (s *Store) SetTitle(id, title string) error {
	_, err := s.db.Exec(`UPDATE sessions SET title=? WHERE id=?`, title, id)
	return err
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

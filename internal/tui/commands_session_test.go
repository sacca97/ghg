package tui

import (
	"path/filepath"
	"testing"

	"github.com/sacca97/ghg/internal/models"
	"github.com/sacca97/ghg/internal/session"
)

// Resume restores the session's own effort; a row that pre-dates per-session
// effort ("") inherits the current default and is stamped on the next save.
func TestResumeRestoresEffort(t *testing.T) {
	m := compactCmdModel()
	m.cfg.DefaultEffort = "medium"
	m.effort = "medium"
	st, err := session.Open(filepath.Join(t.TempDir(), "s.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	m.store = st

	id, err := st.Create("/tmp", m.modelName, m.provName)
	if err != nil {
		t.Fatal(err)
	}
	msgs := []models.Message{{Role: "system", Content: "sys"}, {Role: "user", Content: "q", Authored: true}}
	if err := st.Save(id, 1, msgs, m.modelName, m.provName); err != nil {
		t.Fatal(err)
	}

	// session chose high; the global default drifting to low must not matter
	if err := st.SetEffort(id, "high"); err != nil {
		t.Fatal(err)
	}
	m.effort = "low"
	if err := m.resume(id); err != nil {
		t.Fatal(err)
	}
	if m.effort != "high" {
		t.Fatalf("resume should restore the session effort, got %q", m.effort)
	}

	// a legacy row (no effort) inherits the current default…
	id2, err := st.Create("/tmp", m.modelName, m.provName)
	if err != nil {
		t.Fatal(err)
	}
	if err := st.Save(id2, 1, msgs, m.modelName, m.provName); err != nil {
		t.Fatal(err)
	}
	m.cfg.DefaultEffort = "low"
	m.effort = "low"
	if err := m.resume(id2); err != nil {
		t.Fatal(err)
	}
	if m.effort != "low" {
		t.Fatalf("legacy row should inherit the current default, got %q", m.effort)
	}
}

// Usage totals persist with the session: resume restores them (so the status
// line shows the real spend, not 0/0) and the next save keeps them. Legacy
// rows read zero and get stamped on the first save after resume.
func TestResumeRestoresUsage(t *testing.T) {
	m := compactCmdModel()
	st, err := session.Open(filepath.Join(t.TempDir(), "s.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	m.store = st

	id, err := st.Create("/tmp", m.modelName, m.provName)
	if err != nil {
		t.Fatal(err)
	}
	msgs := []models.Message{{Role: "system", Content: "sys"}, {Role: "user", Content: "q", Authored: true}}
	if err := st.Save(id, 1, msgs, m.modelName, m.provName); err != nil {
		t.Fatal(err)
	}
	if err := st.SetUsage(id, 12000, 8000, 1500); err != nil {
		t.Fatal(err)
	}

	if err := m.resume(id); err != nil {
		t.Fatal(err)
	}
	u := m.usage
	if u.PromptTokens != 12000 || u.Cached() != 8000 || u.CompletionTokens != 1500 {
		t.Fatalf("resume should restore usage, got in=%d cached=%d out=%d", u.PromptTokens, u.Cached(), u.CompletionTokens)
	}

	// new spend accumulates on top of the restored totals…
	m.usage.Add(models.Usage{PromptTokens: 3000, CompletionTokens: 500})

	// legacy row (no usage columns stamped): totals are reconstructed from the
	// per-message usage stored on assistant messages, then stamped on the next
	// persist so reconstruction happens once
	id2, err := st.Create("/tmp", m.modelName, m.provName)
	if err != nil {
		t.Fatal(err)
	}
	cu := models.Usage{PromptTokens: 1000, CompletionTokens: 200,
		PromptTokensDetails: &struct {
			CachedTokens int `json:"cached_tokens"`
		}{CachedTokens: 600}}
	legacy := []models.Message{
		{Role: "system", Content: "sys"},
		{Role: "user", Content: "q1", Authored: true},
		{Role: "assistant", Content: "a1", Usage: &cu},
		{Role: "user", Content: "q2", Authored: true},
		{Role: "assistant", Content: "a2", Usage: &models.Usage{PromptTokens: 500, CompletionTokens: 100}},
	}
	if err := st.Save(id2, 1, legacy, m.modelName, m.provName); err != nil {
		t.Fatal(err)
	}
	if err := m.resume(id2); err != nil {
		t.Fatal(err)
	}
	u2 := m.usage
	if u2.PromptTokens != 1500 || u2.Cached() != 600 || u2.CompletionTokens != 300 {
		t.Fatalf("legacy row should reconstruct usage from messages, got in=%d cached=%d out=%d",
			u2.PromptTokens, u2.Cached(), u2.CompletionTokens)
	}

	// a session with no usage anywhere (pre-usage tracking) stays zero
	id3, err := st.Create("/tmp", m.modelName, m.provName)
	if err != nil {
		t.Fatal(err)
	}
	if err := st.Save(id3, 1, msgs, m.modelName, m.provName); err != nil {
		t.Fatal(err)
	}
	if err := m.resume(id3); err != nil {
		t.Fatal(err)
	}
	if u := m.usage; u.PromptTokens != 0 || u.CompletionTokens != 0 {
		t.Fatalf("usage-free session should start at zero, got %+v", u)
	}
}

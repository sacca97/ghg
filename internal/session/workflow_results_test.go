package session

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/sacca97/ghg/internal/models"
)

func TestWorkflowResultsRoundTripForkAndRewind(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "sessions.db")
	st, err := Open(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = st.Close() }()

	ctx := context.Background()
	sessionID, err := st.Create(dir, "model-a", "prov-a")
	if err != nil {
		t.Fatal(err)
	}

	// Create session with 4 messages
	msgs := []models.Message{
		{Role: "user", Content: "initial prompt"},
		{Role: "assistant", Content: "first answer"},
		{Role: "user", Content: "second prompt"},
		{Role: "assistant", Content: "second answer"},
	}
	if err := st.Save(sessionID, 0, msgs, "model-a", "prov-a"); err != nil {
		t.Fatal(err)
	}

	// Save two workflow results at different message sequences
	planRes := WorkflowResultRecord{
		ResultID:   "res-plan-1",
		SessionID:  sessionID,
		Kind:       "plan",
		Version:    1,
		Payload:    `{"goal":"implement feature","steps":["step 1","step 2"],"acceptance_checks":["check 1"]}`,
		Role:       "smart",
		Provider:   "prov-a",
		Model:      "model-a",
		MessageSeq: 2,
		CreatedAt:  time.Now().UTC().Add(-time.Minute),
	}
	if err := st.SaveWorkflowResult(ctx, planRes); err != nil {
		t.Fatal(err)
	}

	reviewRes := WorkflowResultRecord{
		ResultID:   "res-review-1",
		SessionID:  sessionID,
		Kind:       "review",
		Version:    1,
		Payload:    `{"summary":"looks good","verdict":"approve","findings":[],"checks_performed":["lint","test"]}`,
		Role:       "smart",
		Provider:   "prov-a",
		Model:      "model-a",
		MessageSeq: 4,
		CreatedAt:  time.Now().UTC(),
	}
	if err := st.SaveWorkflowResult(ctx, reviewRes); err != nil {
		t.Fatal(err)
	}

	// Verify LoadWorkflowResult
	loaded, err := st.LoadWorkflowResult(ctx, sessionID, "res-plan-1")
	if err != nil {
		t.Fatalf("load plan: %v", err)
	}
	if loaded.Kind != "plan" || loaded.Payload != planRes.Payload {
		t.Fatalf("loaded plan mismatch: %+v", loaded)
	}

	// Verify LatestWorkflowResult by kind
	latestPlan, ok, err := st.LatestWorkflowResult(ctx, sessionID, "plan")
	if err != nil || !ok || latestPlan.ResultID != "res-plan-1" {
		t.Fatalf("latest plan: ok=%v, err=%v, res=%+v", ok, err, latestPlan)
	}

	latestReview, ok, err := st.LatestWorkflowResult(ctx, sessionID, "review")
	if err != nil || !ok || latestReview.ResultID != "res-review-1" {
		t.Fatalf("latest review: ok=%v, err=%v, res=%+v", ok, err, latestReview)
	}

	latestAny, ok, err := st.LatestWorkflowResult(ctx, sessionID, "")
	if err != nil || !ok || latestAny.ResultID != "res-review-1" {
		t.Fatalf("latest any: ok=%v, err=%v, res=%+v", ok, err, latestAny)
	}

	// Verify ListWorkflowResults
	allList, err := st.ListWorkflowResults(ctx, sessionID, "")
	if err != nil || len(allList) != 2 {
		t.Fatalf("list all: len=%d, err=%v", len(allList), err)
	}

	plansList, err := st.ListWorkflowResults(ctx, sessionID, "plan")
	if err != nil || len(plansList) != 1 || plansList[0].ResultID != "res-plan-1" {
		t.Fatalf("list plans: len=%d, err=%v", len(plansList), err)
	}

	// Subsecond ordering test: two records in the same second with the same message_seq
	t1 := time.Date(2026, 8, 31, 12, 0, 0, 500000000, time.UTC)
	t2 := time.Date(2026, 8, 31, 12, 0, 0, 550000000, time.UTC)
	sub1 := WorkflowResultRecord{
		ResultID:   "res-sub-1",
		SessionID:  sessionID,
		Kind:       "subplan",
		Version:    1,
		Payload:    `{"goal":"sub1"}`,
		MessageSeq: 1,
		CreatedAt:  t1,
	}
	sub2 := WorkflowResultRecord{
		ResultID:   "res-sub-2",
		SessionID:  sessionID,
		Kind:       "subplan",
		Version:    1,
		Payload:    `{"goal":"sub2"}`,
		MessageSeq: 1,
		CreatedAt:  t2,
	}
	if err := st.SaveWorkflowResult(ctx, sub1); err != nil {
		t.Fatal(err)
	}
	if err := st.SaveWorkflowResult(ctx, sub2); err != nil {
		t.Fatal(err)
	}

	latestSub, ok, err := st.LatestWorkflowResult(ctx, sessionID, "subplan")
	if err != nil || !ok || latestSub.ResultID != "res-sub-2" {
		t.Fatalf("latest subsecond: want res-sub-2, got ok=%v, res=%+v, err=%v", ok, latestSub, err)
	}
	subList, err := st.ListWorkflowResults(ctx, sessionID, "subplan")
	if err != nil || len(subList) != 2 || subList[0].ResultID != "res-sub-2" {
		t.Fatalf("list subsecond: want res-sub-2 first, got %+v, err=%v", subList, err)
	}

	// Failure path: invalid timestamp in database produces parse error
	_, err = st.db.Exec(`INSERT INTO workflow_results
		(session_id, result_id, kind, version, payload, role, provider, model, message_seq, created_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		sessionID, "res-bad-time", "corrupt", 1, "{}", "", "", "", 99, "invalid-timestamp")
	if err != nil {
		t.Fatal(err)
	}
	_, err = st.LoadWorkflowResult(ctx, sessionID, "res-bad-time")
	if err == nil || !strings.Contains(err.Error(), "parse workflow result timestamp") {
		t.Fatalf("expected timestamp parse error for bad timestamp, got: %v", err)
	}

	// Fork at seq 2: should copy planRes (seq 2 <= 2), sub1/sub2 (seq 1 <= 2), bad-time (seq 1 <= 2) but NOT reviewRes (seq 4 > 2)
	forkID, err := st.Fork(sessionID, 2, "forked-session")
	if err != nil {
		t.Fatalf("fork: %v", err)
	}
	forkedPlans, err := st.ListWorkflowResults(ctx, forkID, "plan")
	if err != nil {
		t.Fatalf("list forked plans: %v", err)
	}
	if len(forkedPlans) != 1 || forkedPlans[0].ResultID != "res-plan-1" {
		t.Fatalf("forked results: want 1 plan result, got %+v", forkedPlans)
	}

	// Rewind original session to seq 3: should remove reviewRes (seq 4 >= 3) and keep planRes (seq 2 < 3)
	if err := st.DeleteFrom(sessionID, 3, nil); err != nil {
		t.Fatalf("delete from: %v", err)
	}
	afterRewind, err := st.ListWorkflowResults(ctx, sessionID, "review")
	if err != nil {
		t.Fatalf("list after rewind: %v", err)
	}
	if len(afterRewind) != 0 {
		t.Fatalf("after rewind: want 0 review results, got %+v", afterRewind)
	}

	// Delete original session completely
	if err := st.DeleteSession(sessionID); err != nil {
		t.Fatalf("delete session: %v", err)
	}
	afterDelete, err := st.ListWorkflowResults(ctx, sessionID, "")
	if err != nil {
		t.Fatalf("list after delete: %v", err)
	}
	if len(afterDelete) != 0 {
		t.Fatalf("expected 0 results after delete session, got %d", len(afterDelete))
	}
}

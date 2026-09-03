package session

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/sacca97/ghg/internal/observation"
	"github.com/sacca97/ghg/internal/search"
)

func TestSearchAndObservationStateRoundTrip(t *testing.T) {
	store, err := Open(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	sessionID, err := store.Create(t.TempDir(), "model", "provider")
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	record := observation.Record{ID: "obs-1", Path: "/tmp/main.go", StartLine: 2, EndLine: 3, NextOffset: 4, IssuedBytes: 10, Content: "b\nc\n", Complete: true}
	if err := store.SaveObservation(ctx, sessionID, record); err != nil {
		t.Fatal(err)
	}
	gotRecord, err := store.LoadObservation(ctx, sessionID, record.ID)
	if err != nil || gotRecord.Content != record.Content || gotRecord.SessionID != sessionID || !gotRecord.Complete {
		t.Fatalf("observation = %+v, %v", gotRecord, err)
	}
	snapshot := search.Snapshot{ID: "grep-1", Kind: "grep", Items: []search.Item{{Path: "main.go", Line: 2, Text: "b"}}, Complete: true}
	if err := store.SaveSearchSnapshot(ctx, sessionID, snapshot); err != nil {
		t.Fatal(err)
	}
	gotSnapshot, err := store.LoadSearchSnapshot(ctx, sessionID, snapshot.ID)
	if err != nil || gotSnapshot.Kind != snapshot.Kind || len(gotSnapshot.Items) != 1 || gotSnapshot.Items[0].Text != "b" {
		t.Fatalf("snapshot = %+v, %v", gotSnapshot, err)
	}
	other, err := store.Create(t.TempDir(), "model", "provider")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.LoadObservation(ctx, other, record.ID); err == nil {
		t.Fatal("observation crossed session boundary")
	}
	if _, err := store.LoadSearchSnapshot(ctx, other, snapshot.ID); err == nil {
		t.Fatal("snapshot crossed session boundary")
	}
}

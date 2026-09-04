package observation

import (
	"context"
	"errors"
	"fmt"
	"os"
	"testing"
	"time"
)

type testStore struct {
	records map[string]Record
	calls   int
	failAt  int
}

func (s *testStore) SaveObservation(_ context.Context, sessionID string, record Record) error {
	s.calls++
	if s.failAt > 0 && s.calls == s.failAt {
		return errors.New("store failure")
	}
	if s.records == nil {
		s.records = make(map[string]Record)
	}
	s.records[sessionKey(sessionID, record.ID)] = record
	return nil
}

func (s *testStore) LoadObservation(_ context.Context, sessionID, id string) (Record, error) {
	record, ok := s.records[sessionKey(sessionID, id)]
	if !ok {
		return Record{}, os.ErrNotExist
	}
	return record, nil
}

func TestRegistryScopesRecordsBySession(t *testing.T) {
	registry := NewRegistry()
	record := Record{ID: "obs-1", Path: "/tmp/a.go", StartLine: 1, EndLine: 1, Content: "x\n", Complete: true}
	if err := registry.Save(nil, "one", record); err != nil {
		t.Fatal(err)
	}
	if _, err := registry.Load(nil, "two", record.ID); err == nil {
		t.Fatal("observation leaked across session boundary")
	}
	got, err := registry.Load(nil, "one", record.ID)
	if err != nil || got.Content != record.Content || got.SessionID != "one" {
		t.Fatalf("scoped observation = %+v, %v", got, err)
	}
}

func TestRegistryBoundsPersistedLiveRecords(t *testing.T) {
	store := &testStore{}
	registry := NewRegistry()
	registry.SetPersistent(store)
	for i := 0; i < maxLiveObservationsPerSession+1; i++ {
		record := Record{
			ID: fmt.Sprintf("obs-%d", i), Path: "/tmp/a.go",
			StartLine: i + 1, EndLine: i + 1, Content: "x",
			CreatedAt: timeForTest(i),
		}
		if err := registry.Save(nil, "session", record); err != nil {
			t.Fatal(err)
		}
	}
	registry.mu.Lock()
	count := len(registry.records)
	_, live := registry.records[sessionKey("session", "obs-0")]
	registry.mu.Unlock()
	if count > maxLiveObservationsPerSession || live {
		t.Fatalf("live records = %d, oldest retained=%v", count, live)
	}
	if _, err := registry.Load(nil, "session", "obs-0"); err != nil {
		t.Fatalf("evicted observation should reload from storage: %v", err)
	}
}

func TestBindSessionKeepsPendingRecordsOnStoreFailure(t *testing.T) {
	store := &testStore{failAt: 2}
	registry := NewRegistry()
	registry.SetPersistent(store)
	for i := 0; i < 2; i++ {
		if err := registry.Save(nil, "", Record{ID: fmt.Sprintf("obs-%d", i), Path: "/tmp/a.go", StartLine: i + 1, EndLine: i + 1}); err != nil {
			t.Fatal(err)
		}
	}
	if err := registry.BindSession(nil, "session"); err == nil {
		t.Fatal("expected persistence failure")
	}
	registry.mu.Lock()
	for i := 0; i < 2; i++ {
		if _, ok := registry.records[sessionKey("", fmt.Sprintf("obs-%d", i))]; !ok {
			t.Fatalf("pending record %d was rekeyed after a failed bind", i)
		}
	}
	registry.mu.Unlock()
}

func timeForTest(i int) time.Time { return time.Unix(int64(i+1), 0).UTC() }

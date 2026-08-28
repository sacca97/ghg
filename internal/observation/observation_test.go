package observation

import "testing"

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

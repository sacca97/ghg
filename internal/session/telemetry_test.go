package session

import (
	"context"
	"encoding/json"
	"path/filepath"
	"testing"
)

func TestTelemetryIsOrderedAndSurvivesCompaction(t *testing.T) {
	st, err := Open(filepath.Join(t.TempDir(), "sessions.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	id, err := st.Create(t.TempDir(), "model", "provider")
	if err != nil {
		t.Fatal(err)
	}
	if err := st.AppendTelemetry(context.Background(), id, "model_call_start", map[string]string{"model": "m"}); err != nil {
		t.Fatal(err)
	}
	if err := st.RecordCompaction(id, 2, "summary"); err != nil {
		t.Fatal(err)
	}
	if err := st.AppendTelemetry(context.Background(), id, "model_call_end", map[string]int{"tokens": 3}); err != nil {
		t.Fatal(err)
	}
	events, err := st.ListTelemetry(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 2 || events[0].Seq != 1 || events[1].Seq != 2 || events[1].Kind != "model_call_end" {
		t.Fatalf("telemetry = %+v", events)
	}
	var payload map[string]int
	if err := json.Unmarshal(events[1].Payload, &payload); err != nil || payload["tokens"] != 3 {
		t.Fatalf("payload = %s, err = %v", events[1].Payload, err)
	}
}

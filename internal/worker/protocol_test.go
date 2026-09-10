package worker

import (
	"bytes"
	"errors"
	"strings"
	"testing"
)

func TestProtocolRoundTripAndLimits(t *testing.T) {
	var wire bytes.Buffer
	want := Frame{
		Version:   ProtocolVersion,
		SessionID: "session-1",
		Seq:       7,
		Type:      TypeEvent,
		Payload:   []byte(`{"text":"line 1\nline 2"}`),
	}
	if err := WriteFrame(&wire, want); err != nil {
		t.Fatalf("WriteFrame() error = %v", err)
	}
	got, err := NewDecoder(&wire).Read()
	if err != nil {
		t.Fatalf("Read() error = %v", err)
	}
	if string(got.Payload) != string(want.Payload) || got.Seq != want.Seq || got.Type != want.Type {
		t.Fatalf("Read() = %+v, want %+v", got, want)
	}

	tooLarge := strings.Repeat("x", 32)
	_, err = NewDecoderWithLimits(strings.NewReader(tooLarge), 16).Read()
	if !errors.Is(err, ErrFrameTooLarge) {
		t.Fatalf("oversized frame error = %v, want ErrFrameTooLarge", err)
	}
}

func TestProtocolRejectsUnknownTypeAndAllowsLongStreams(t *testing.T) {
	unknown := `{"version":1,"session_id":"session-1","type":"future"}` + "\n"
	_, err := NewDecoder(strings.NewReader(unknown)).Read()
	if !errors.Is(err, ErrUnknownType) {
		t.Fatalf("unknown type error = %v, want ErrUnknownType", err)
	}

	frames := `{"version":1,"session_id":"session-1","type":"attach"}` + "\n"
	decoder := NewDecoderWithLimits(strings.NewReader(strings.Repeat(frames, 1024)), 128)
	for i := 0; i < 1024; i++ {
		if _, err := decoder.Read(); err != nil {
			t.Fatalf("long stream frame %d error = %v", i, err)
		}
	}
}

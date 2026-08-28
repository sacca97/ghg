package lsp

import (
	"strings"
	"testing"
)

func TestReportBoundsDiagnosticsAndSiblingFiles(t *testing.T) {
	message := strings.Repeat("x", maxMsgLen+1000)
	edited := "/workspace/main.go"
	editedDiags := make([]Diagnostic, maxPerFile+5)
	for i := range editedDiags {
		editedDiags[i] = Diagnostic{Line: i + 1, Col: 1, Severity: SeverityError, Message: message}
	}
	siblings := make(map[string][]Diagnostic, maxSiblingFiles+2)
	for i := 0; i < maxSiblingFiles+2; i++ {
		path := "/workspace/sibling-" + string(rune('a'+i)) + ".go"
		siblings[path] = []Diagnostic{{Line: 1, Col: 1, Severity: SeverityError, Message: message}}
	}

	got := Report(edited, editedDiags, siblings)
	if len(got) > 50<<10 {
		t.Fatalf("diagnostic report exceeded tool output budget: %d bytes", len(got))
	}
	if strings.Count(got, "ERROR") > maxPerFile*(1+maxSiblingFiles) {
		t.Fatalf("diagnostic report exceeded per-file/sibling caps: %d errors", strings.Count(got, "ERROR"))
	}
	if !strings.Contains(got, "... and 5 more") || !strings.Contains(got, "more") {
		t.Fatalf("diagnostic report lost bounded omission markers: %q", got)
	}
	if strings.Contains(got, message) {
		t.Fatal("diagnostic message was not truncated")
	}
}

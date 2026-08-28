package tools

import (
	"strings"
	"testing"

	"github.com/sacca97/ghg/internal/artifact"
)

func TestModelTextDelimitsUntrustedBytesAndKeepsArtifactHintOutside(t *testing.T) {
	ref := artifact.Ref{
		ID:            "sha256:" + strings.Repeat("a", 64),
		Hash:          strings.Repeat("a", 64),
		OriginalBytes: 200,
		StoredBytes:   100,
		Complete:      false,
	}
	result := MarkUntrusted(ToolResult{
		Preview:  "returned text" + ArtifactReference(ref),
		Artifact: &ref,
	}, "mcp__docs__fetch")

	if !IsUntrusted(result) || result.Source != "mcp__docs__fetch" {
		t.Fatalf("untrusted result metadata = %+v", result)
	}
	got := ModelText(result)
	start := `<untrusted_tool_output source="mcp__docs__fetch">`
	if !strings.HasPrefix(got, start) || !strings.Contains(got, "returned text") {
		t.Fatalf("rendered result = %q", got)
	}
	close := strings.Index(got, "</untrusted_tool_output>")
	if close < 0 || !strings.HasSuffix(got, ArtifactReference(ref)) || strings.Index(got, ArtifactReference(ref)) < close {
		t.Fatalf("artifact hint must follow the untrusted block: %q", got)
	}

	trusted := ToolResult{Preview: "status"}
	if ModelText(trusted) != trusted.Preview {
		t.Fatal("trusted results should remain unchanged")
	}
}

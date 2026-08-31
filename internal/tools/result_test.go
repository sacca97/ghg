package tools

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/sacca97/ghg/internal/artifact"
	"github.com/sacca97/ghg/internal/llm"
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

func TestNormalizeResultCapsExplicitPreviewWithoutDroppingRetained(t *testing.T) {
	retained := strings.Repeat("retained", maxOutput)
	preview := strings.Repeat("preview", maxOutput)
	tool := Tool{
		Def: llm.NewTool("preview", "", `{"type":"object"}`),
		RunResult: func(context.Context, json.RawMessage) (ToolResult, error) {
			return ToolResult{
				Preview:       preview,
				Retained:      retained,
				OriginalBytes: int64(len(retained)),
				Complete:      true,
			}, nil
		},
	}
	result := ExecuteResult(context.Background(), []Tool{tool}, "preview", nil)
	if len(result.Preview) > maxOutput || !strings.Contains(result.Preview, "truncated") {
		t.Fatalf("explicit preview was not bounded: len=%d", len(result.Preview))
	}
	if result.Retained != retained {
		t.Fatal("normalizing a preview must preserve retained evidence")
	}
}

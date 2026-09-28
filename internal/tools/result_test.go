package tools

import (
	"context"
	"strings"
	"testing"

	"github.com/sacca97/ghg/internal/models"
)

func TestModelTextDelimitsUntrustedBytesAndKeepsOutputHintOutside(t *testing.T) {
	ref := models.OutputRef{
		ID:            "sha256:" + strings.Repeat("a", 64),
		Hash:          strings.Repeat("a", 64),
		OriginalBytes: 200,
		StoredBytes:   100,
		Complete:      false,
	}
	result := MarkUntrusted(ToolResult{
		Preview: "returned text" + OutputReference(ref),
		Output:  &ref,
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
	if close < 0 || !strings.HasSuffix(got, OutputReference(ref)) || strings.Index(got, OutputReference(ref)) < close {
		t.Fatalf("output hint must follow the untrusted block: %q", got)
	}

	trusted := ToolResult{Preview: "status"}
	if ModelText(trusted) != trusted.Preview {
		t.Fatal("trusted results should remain unchanged")
	}
}

func TestResultConstructors(t *testing.T) {
	text := strings.Repeat("x", maxOutput+1)
	result := NewTextResult(text, 3)
	if len(result.Preview) > maxOutput || result.Retained != text || result.OriginalBytes != int64(len(text)) || !result.Complete || result.ExitCode != 3 {
		t.Fatalf("materialized result = %+v", result)
	}

	capture := NewOutputCapture(4)
	_, _ = capture.WriteString("abcdefgh")
	result = capture.Result(1)
	if result.Retained != "abgh" || result.OriginalBytes != 8 || result.Complete || result.ExitCode != 1 {
		t.Fatalf("captured result = %+v", result)
	}
}

func TestWithOnUpdateIsPerContext(t *testing.T) {
	var first, second string
	ctx1 := WithOnUpdate(context.Background(), func(snapshot string) { first = snapshot })
	ctx2 := WithOnUpdate(context.Background(), func(snapshot string) { second = snapshot })

	onUpdate(ctx1)("one")
	onUpdate(ctx2)("two")
	if first != "one" || second != "two" {
		t.Fatalf("callbacks crossed contexts: first=%q second=%q", first, second)
	}
	if onUpdate(context.Background()) != nil {
		t.Fatal("a context without an update callback should remain quiet")
	}
}

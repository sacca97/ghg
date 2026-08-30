package agent

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/sacca97/ghg/internal/llm"
	"github.com/sacca97/ghg/internal/tools"
)

func TestToolTelemetryReportsPreviewRetentionAndRedirect(t *testing.T) {
	a := New(nil, "model", 100, "system")
	a.Tools = []tools.Tool{
		{
			Def: llm.NewTool("probe", "probe", `{"type":"object"}`),
			RunResult: func(context.Context, json.RawMessage) (tools.ToolResult, error) {
				return tools.MarkUntrusted(tools.TextResultWithSize(strings.Repeat("x", 100), "preview", 100, true, 0), "probe"), nil
			},
		},
	}
	var got ToolTelemetry
	a.runToolResultsWithTools(context.Background(), []llm.ToolCall{{ID: "call-1", Function: struct {
		Name      string `json:"name"`
		Arguments string `json:"arguments"`
	}{Name: "probe", Arguments: `{}`}}}, Events{OnToolTelemetry: func(value ToolTelemetry) { got = value }}, a.AllTools())
	if got.Name != "probe" || got.ID != "call-1" || got.PreviewBytes != len("preview") || got.RetainedBytes != 100 || got.OriginalBytes != 100 || !got.Truncated {
		t.Fatalf("telemetry = %+v", got)
	}

	redirect := tools.ExecuteResult(context.Background(), tools.All(), "bash", json.RawMessage(`{"command":"find ."}`))
	if redirect.Metadata["bash_redirect"] != "true" {
		t.Fatalf("redirect metadata = %+v", redirect.Metadata)
	}
}

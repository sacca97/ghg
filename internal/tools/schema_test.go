package tools

import (
	"encoding/json"
	"testing"
)

// Every built-in tool's JSON schema must parse — a malformed schema
// (trailing comma, stray quote) silently corrupts the provider request
// body for ALL tools, surfacing as cryptic marshal errors deep in the
// loop. This ratchet pins parseability at the source.
func TestBuiltinToolSchemasParse(t *testing.T) {
	for _, tool := range All() {
		var v any
		if err := json.Unmarshal(tool.Def.Function.Parameters, &v); err != nil {
			t.Errorf("%s: schema does not parse: %v", tool.Def.Function.Name, err)
		}
	}
}

func TestGrepSchemaAcceptsEitherPatternForm(t *testing.T) {
	var schema struct {
		Required []string `json:"required"`
		AnyOf    []struct {
			Required []string `json:"required"`
		} `json:"anyOf"`
	}
	if err := json.Unmarshal(grepTool().Def.Function.Parameters, &schema); err != nil {
		t.Fatal(err)
	}
	if len(schema.Required) != 0 || len(schema.AnyOf) != 3 {
		t.Fatalf("grep schema requirements = %+v", schema)
	}
	got := map[string]bool{}
	for _, branch := range schema.AnyOf {
		if len(branch.Required) != 1 {
			t.Fatalf("grep schema branch = %+v", branch.Required)
		}
		got[branch.Required[0]] = true
	}
	if !got["pattern"] || !got["patterns"] || !got["cursor"] || len(got) != 3 {
		t.Fatalf("grep schema does not require pattern, patterns, or cursor: %+v", got)
	}
}

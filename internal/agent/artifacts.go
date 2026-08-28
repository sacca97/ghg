package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/sacca97/ghg/internal/artifact"
	"github.com/sacca97/ghg/internal/llm"
	"github.com/sacca97/ghg/internal/tools"
)

// artifactTools are read-only and intentionally live on Agent rather than in
// tools.All: both operations need the active session boundary and the payload
// store. A model can discover and rehydrate evidence, but cannot supply a path
// or a different session id.
func artifactTools(a *Agent) []tools.Tool {
	return []tools.Tool{artifactListTool(a), artifactReadTool(a)}
}

type artifactListArgs struct {
	Tool     string `json:"tool"`
	CallID   string `json:"call_id"`
	Query    string `json:"query"`
	Since    string `json:"since"`
	Until    string `json:"until"`
	MaxItems int    `json:"limit"`
}

func artifactListTool(a *Agent) tools.Tool {
	return tools.Tool{
		Def: llm.NewTool("artifact_list",
			"List retained tool-result artifacts for the current session. Results are metadata only; use artifact_read with an id and byte range to rehydrate evidence. The list is bounded and never exposes filesystem paths.",
			`{"type":"object","properties":{"tool":{"type":"string","description":"Exact originating tool name, such as bash or read"},"call_id":{"type":"string","description":"Exact originating tool-call id"},"query":{"type":"string","description":"Match an artifact id, tool name, call id, or media type"},"since":{"type":"string","description":"Only artifacts created at or after this RFC3339 timestamp"},"until":{"type":"string","description":"Only artifacts created at or before this RFC3339 timestamp"},"limit":{"type":"integer","description":"Maximum entries (default 100, maximum 1000)"}}}`),
		Run: func(ctx context.Context, args json.RawMessage) (string, error) {
			result, err := runArtifactList(a, ctx, args)
			return result.Preview, err
		},
		RunResult: func(ctx context.Context, args json.RawMessage) (tools.ToolResult, error) {
			return runArtifactList(a, ctx, args)
		},
	}
}

func runArtifactList(a *Agent, ctx context.Context, args json.RawMessage) (tools.ToolResult, error) {
	var in artifactListArgs
	if err := json.Unmarshal(args, &in); err != nil {
		return tools.ToolResult{}, err
	}
	sessionID := a.currentSessionID()
	since, err := parseArtifactTime("since", in.Since)
	if err != nil {
		return tools.ToolResult{}, err
	}
	until, err := parseArtifactTime("until", in.Until)
	if err != nil {
		return tools.ToolResult{}, err
	}
	items, err := a.listArtifacts(ctx, sessionID, artifact.Filter{
		ToolName: in.Tool, ToolCallID: in.CallID, Query: in.Query, Since: since, Until: until,
	}, in.MaxItems)
	if err != nil {
		return tools.ToolResult{}, err
	}
	return tools.TextResult(formatArtifactList(items), ""), nil
}

func parseArtifactTime(name, value string) (time.Time, error) {
	if strings.TrimSpace(value) == "" {
		return time.Time{}, nil
	}
	t, err := time.Parse(time.RFC3339, value)
	if err != nil {
		return time.Time{}, fmt.Errorf("artifact %s must be RFC3339: %w", name, err)
	}
	return t, nil
}

func formatArtifactList(items []artifact.Metadata) string {
	if len(items) == 0 {
		return "(no artifacts)"
	}
	var b strings.Builder
	for _, item := range items {
		state := "complete"
		if !item.Complete {
			state = "head/tail only; middle omitted"
		}
		fmt.Fprintf(&b, "%s | tool=%s | call=%s | %d bytes original, %d stored | %s | created=%s\n",
			item.ID, item.ToolName, item.ToolCallID, item.OriginalBytes, item.StoredBytes,
			state, item.CreatedAt.UTC().Format(time.RFC3339))
	}
	return strings.TrimSuffix(b.String(), "\n")
}

type artifactReadArgs struct {
	ID     string `json:"id"`
	Offset int64  `json:"offset"`
	Limit  int64  `json:"limit"`
}

func artifactReadTool(a *Agent) tools.Tool {
	return tools.Tool{
		Def: llm.NewTool("artifact_read",
			"Read a bounded byte range from a retained artifact in the current session. Pass the artifact id returned by artifact_list or shown in a tool result; paths and cross-session access are not accepted.",
			`{"type":"object","properties":{"id":{"type":"string","description":"Artifact id, for example sha256:<64 hex characters>"},"offset":{"type":"integer","description":"Zero-based byte offset (default 0)"},"limit":{"type":"integer","description":"Maximum bytes to return (default 65536, maximum 1048576)"}},"required":["id"]}`),
		Run: func(ctx context.Context, args json.RawMessage) (string, error) {
			result, err := runArtifactRead(a, ctx, args)
			return result.Preview, err
		},
		RunResult: func(ctx context.Context, args json.RawMessage) (tools.ToolResult, error) {
			return runArtifactRead(a, ctx, args)
		},
	}
}

func runArtifactRead(a *Agent, ctx context.Context, args json.RawMessage) (tools.ToolResult, error) {
	var in artifactReadArgs
	if err := json.Unmarshal(args, &in); err != nil {
		return tools.ToolResult{}, err
	}
	if strings.TrimSpace(in.ID) == "" {
		return tools.ToolResult{}, fmt.Errorf("artifact id is required")
	}
	if in.Offset < 0 {
		return tools.ToolResult{}, fmt.Errorf("artifact offset must be non-negative")
	}
	if in.Limit < 0 {
		return tools.ToolResult{}, fmt.Errorf("artifact limit must be non-negative")
	}
	if a.ArtifactStore == nil {
		return tools.ToolResult{}, fmt.Errorf("artifact reading is unavailable: no artifact store")
	}
	sessionID := a.currentSessionID()
	meta, err := a.lookupArtifact(ctx, sessionID, in.ID)
	if err != nil {
		return tools.ToolResult{}, fmt.Errorf("artifact %q is not available in the current session: %w", in.ID, err)
	}
	data, err := a.ArtifactStore.Read(ctx, meta.Ref, in.Offset, in.Limit)
	if err != nil {
		return tools.ToolResult{}, err
	}
	state := "complete"
	if !meta.Complete {
		state = "head/tail retained; middle omitted"
	}
	header := fmt.Sprintf("[artifact %s: offset %d, %d bytes returned, %d bytes stored, %s]",
		meta.ID, in.Offset, len(data), meta.StoredBytes, state)
	out := header
	if len(data) > 0 {
		out += "\n" + string(data)
	}
	preview := tools.Truncate(out)
	return tools.MarkUntrusted(tools.ToolResult{
		Preview:       preview,
		Retained:      out,
		OriginalBytes: int64(len(out)),
		Complete:      len(out) == len(preview),
		Artifact:      &meta.Ref,
	}, "artifact_read"), nil
}

// lookupArtifact first checks the live message slice. A tool result is
// available to the next model request before the enclosing turn is persisted,
// and this also gives --no-session runs a useful temporary artifact catalog.
func (a *Agent) lookupArtifact(ctx context.Context, sessionID, id string) (artifact.Metadata, error) {
	for _, meta := range a.messageArtifacts() {
		if meta.ID == id {
			return meta, nil
		}
	}
	if a.ArtifactCatalog == nil {
		return artifact.Metadata{}, fmt.Errorf("no session artifact catalog")
	}
	if sessionID == "" {
		return artifact.Metadata{}, fmt.Errorf("no session artifact catalog")
	}
	return a.ArtifactCatalog.LookupArtifact(ctx, sessionID, id)
}

func (a *Agent) listArtifacts(ctx context.Context, sessionID string, filter artifact.Filter, limit int) ([]artifact.Metadata, error) {
	var items []artifact.Metadata
	if a.ArtifactCatalog != nil && sessionID != "" {
		var err error
		items, err = a.ArtifactCatalog.ListArtifacts(ctx, sessionID, filter, limit)
		if err != nil {
			return nil, err
		}
	}
	seen := make(map[string]bool, len(items))
	for _, item := range items {
		seen[item.ID+"\x00"+item.ToolCallID] = true
	}
	for _, item := range a.messageArtifacts() {
		if !artifactMatches(item, filter) {
			continue
		}
		key := item.ID + "\x00" + item.ToolCallID
		if seen[key] {
			continue
		}
		seen[key] = true
		items = append(items, item)
	}
	// Catalog queries are already bounded. The in-memory tail can add only
	// message count entries, so apply the same cap after de-duplication.
	if limit <= 0 {
		limit = 100
	}
	if len(items) > limit {
		items = items[:limit]
	}
	return items, nil
}

func (a *Agent) messageArtifacts() []artifact.Metadata {
	items := []artifact.Metadata{}
	now := time.Now().UTC()
	for i, msg := range a.MessagesSnapshot() {
		if msg.Artifact == nil {
			continue
		}
		ref := *msg.Artifact
		path, err := artifact.RelativePath(ref)
		if err != nil {
			continue
		}
		items = append(items, artifact.Metadata{
			Ref: ref, SessionID: a.currentSessionID(), MessageSeq: i,
			ToolCallID: msg.ToolCallID, ToolName: msg.Name, Path: path, CreatedAt: now,
		})
	}
	return items
}

func artifactMatches(item artifact.Metadata, filter artifact.Filter) bool {
	if filter.ToolName != "" && item.ToolName != filter.ToolName {
		return false
	}
	if filter.ToolCallID != "" && item.ToolCallID != filter.ToolCallID {
		return false
	}
	if filter.Query != "" && !strings.Contains(item.ID, filter.Query) &&
		!strings.Contains(item.ToolCallID, filter.Query) &&
		!strings.Contains(item.ToolName, filter.Query) &&
		!strings.Contains(item.MediaType, filter.Query) {
		return false
	}
	if !filter.Since.IsZero() && item.CreatedAt.Before(filter.Since) {
		return false
	}
	if !filter.Until.IsZero() && item.CreatedAt.After(filter.Until) {
		return false
	}
	return true
}

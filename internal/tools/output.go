package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/sacca97/ghg/internal/models"
	"github.com/sacca97/ghg/internal/session"
)

// OutputToolConfig supplies the current session and its output storage.
type OutputToolConfig struct {
	SessionID func() string
	Catalog   func() session.OutputCatalog
	Store     func() *session.OutputStore
	Messages  func() []models.Message
}

// OutputTools builds output tools from explicit session and storage dependencies.
func OutputTools(cfg OutputToolConfig) []Tool {
	return []Tool{outputListTool(cfg), outputReadTool(cfg)}
}

type outputListArgs struct {
	Tool     string `json:"tool"`
	CallID   string `json:"call_id"`
	Query    string `json:"query"`
	Since    string `json:"since"`
	Until    string `json:"until"`
	MaxItems int    `json:"limit"`
}

func outputListTool(cfg OutputToolConfig) Tool {
	return Tool{
		Def: models.NewTool("output_list",
			"List retained tool output for the current session. Results are metadata only; use output_read with an id and byte range to rehydrate evidence. The list is bounded and never exposes filesystem paths.",
			`{"type":"object","properties":{"tool":{"type":"string","description":"Exact originating tool name, such as bash or read"},"call_id":{"type":"string","description":"Exact originating tool-call id"},"query":{"type":"string","description":"Match an output id, tool name, call id, or media type"},"since":{"type":"string","description":"Only outputs created at or after this RFC3339 timestamp"},"until":{"type":"string","description":"Only outputs created at or before this RFC3339 timestamp"},"limit":{"type":"integer","description":"Maximum entries (default 100, maximum 1000)"}}}`),
		Run: func(ctx context.Context, args json.RawMessage) (string, error) {
			result, err := runOutputList(ctx, cfg, args)
			return result.Preview, err
		},
		RunResult: func(ctx context.Context, args json.RawMessage) (ToolResult, error) {
			return runOutputList(ctx, cfg, args)
		},
	}
}

func runOutputList(ctx context.Context, cfg OutputToolConfig, args json.RawMessage) (ToolResult, error) {
	var in outputListArgs
	if err := json.Unmarshal(args, &in); err != nil {
		return ToolResult{}, err
	}
	since, err := parseOutputTime("since", in.Since)
	if err != nil {
		return ToolResult{}, err
	}
	until, err := parseOutputTime("until", in.Until)
	if err != nil {
		return ToolResult{}, err
	}
	items, err := listOutputs(ctx, currentSessionID(cfg.SessionID), cfg, session.OutputFilter{
		ToolName: in.Tool, ToolCallID: in.CallID, Query: in.Query, Since: since, Until: until,
	}, in.MaxItems)
	if err != nil {
		return ToolResult{}, err
	}
	return TextResult(formatOutputList(items), ""), nil
}

func parseOutputTime(name, value string) (time.Time, error) {
	if strings.TrimSpace(value) == "" {
		return time.Time{}, nil
	}
	t, err := time.Parse(time.RFC3339, value)
	if err != nil {
		return time.Time{}, fmt.Errorf("output %s must be RFC3339: %w", name, err)
	}
	return t, nil
}

func formatOutputList(items []session.OutputMetadata) string {
	if len(items) == 0 {
		return "(no outputs)"
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

type outputReadArgs struct {
	ID     string `json:"id"`
	Offset int64  `json:"offset"`
	Limit  int64  `json:"limit"`
}

func outputReadTool(cfg OutputToolConfig) Tool {
	return Tool{
		Def: models.NewTool("output_read",
			"Read a bounded byte range from retained tool output in the current session. Pass the output id returned by output_list or shown in a tool result; paths and cross-session access are not accepted.",
			`{"type":"object","properties":{"id":{"type":"string","description":"Output id, for example sha256:<64 hex characters>"},"offset":{"type":"integer","description":"Zero-based byte offset (default 0)"},"limit":{"type":"integer","description":"Maximum bytes to return (default 65536, maximum 1048576)"}},"required":["id"]}`),
		Run: func(ctx context.Context, args json.RawMessage) (string, error) {
			result, err := runOutputRead(ctx, cfg, args)
			return result.Preview, err
		},
		RunResult: func(ctx context.Context, args json.RawMessage) (ToolResult, error) {
			return runOutputRead(ctx, cfg, args)
		},
	}
}

func runOutputRead(ctx context.Context, cfg OutputToolConfig, args json.RawMessage) (ToolResult, error) {
	var in outputReadArgs
	if err := json.Unmarshal(args, &in); err != nil {
		return ToolResult{}, err
	}
	if strings.TrimSpace(in.ID) == "" {
		return ToolResult{}, fmt.Errorf("output id is required")
	}
	if in.Offset < 0 {
		return ToolResult{}, fmt.Errorf("output offset must be non-negative")
	}
	if in.Limit < 0 {
		return ToolResult{}, fmt.Errorf("output limit must be non-negative")
	}
	if cfg.Store == nil || cfg.Store() == nil {
		return ToolResult{}, fmt.Errorf("output reading is unavailable: no output store")
	}
	id := currentSessionID(cfg.SessionID)
	var catalog session.OutputCatalog
	if cfg.Catalog != nil {
		catalog = cfg.Catalog()
	}
	meta, err := lookupOutput(ctx, id, catalog, cfg.Messages, in.ID)
	if err != nil {
		return ToolResult{}, fmt.Errorf("output %q is not available in the current session: %w", in.ID, err)
	}
	data, err := cfg.Store().Read(ctx, meta.OutputRef, in.Offset, in.Limit)
	if err != nil {
		return ToolResult{}, err
	}
	state := "complete"
	if !meta.Complete {
		state = "head/tail retained; middle omitted"
	}
	header := fmt.Sprintf("[output %s: offset %d, %d bytes returned, %d bytes stored, %s]",
		meta.ID, in.Offset, len(data), meta.StoredBytes, state)
	out := header
	if len(data) > 0 {
		out += "\n" + string(data)
	}
	preview := Truncate(out)
	return MarkUntrusted(ToolResult{
		Preview:       preview,
		Retained:      out,
		OriginalBytes: int64(len(out)),
		Complete:      len(out) == len(preview),
		Output:        &meta.OutputRef,
	}, "output_read"), nil
}

func currentSessionID(sessionID func() string) string {
	if sessionID == nil {
		return ""
	}
	return sessionID()
}

func lookupOutput(ctx context.Context, sessionID string, catalog session.OutputCatalog, messages func() []models.Message, id string) (session.OutputMetadata, error) {
	for _, meta := range messageOutputs(sessionID, messages) {
		if meta.ID == id {
			return meta, nil
		}
	}
	if catalog == nil || sessionID == "" {
		return session.OutputMetadata{}, fmt.Errorf("no session output catalog")
	}
	return catalog.LookupOutput(ctx, sessionID, id)
}

func listOutputs(ctx context.Context, sessionID string, cfg OutputToolConfig, filter session.OutputFilter, limit int) ([]session.OutputMetadata, error) {
	var items []session.OutputMetadata
	var outputCatalog session.OutputCatalog
	if cfg.Catalog != nil {
		outputCatalog = cfg.Catalog()
	}
	if outputCatalog != nil && sessionID != "" {
		var err error
		items, err = outputCatalog.ListOutputs(ctx, sessionID, filter, limit)
		if err != nil {
			return nil, err
		}
	}
	seen := make(map[string]bool, len(items))
	for _, item := range items {
		seen[item.ID+"\x00"+item.ToolCallID] = true
	}
	for _, item := range messageOutputs(sessionID, cfg.Messages) {
		if !outputMatches(item, filter) {
			continue
		}
		key := item.ID + "\x00" + item.ToolCallID
		if seen[key] {
			continue
		}
		seen[key] = true
		items = append(items, item)
	}
	if limit <= 0 {
		limit = 100
	}
	if len(items) > limit {
		items = items[:limit]
	}
	return items, nil
}

func messageOutputs(sessionID string, messages func() []models.Message) []session.OutputMetadata {
	if messages == nil {
		return nil
	}
	items := []session.OutputMetadata{}
	now := time.Now().UTC()
	for i, msg := range messages() {
		if msg.Output == nil {
			continue
		}
		ref := *msg.Output
		path, err := session.RelativePath(ref)
		if err != nil {
			continue
		}
		items = append(items, session.OutputMetadata{
			OutputRef: ref, SessionID: sessionID, MessageSeq: i,
			ToolCallID: msg.ToolCallID, ToolName: msg.Name, Path: path, CreatedAt: now,
		})
	}
	return items
}

func outputMatches(item session.OutputMetadata, filter session.OutputFilter) bool {
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

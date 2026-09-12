package models

// stripAuthored returns a copy of msgs with the internal Authored marker and
// SentAt timestamp cleared — they're ghg-local bookkeeping (input-history
// recall, the rewind picker) and must never reach the provider. It copies
// because req.Messages typically aliases the caller's conversation slice,
// which must keep the fields for storage/recall.
func stripAuthored(msgs []Message) []Message {
	return stripAuthoredWithBlocks(msgs, false)
}

// stripAuthoredPreserveBlocks clears ghg bookkeeping while retaining
// provider-native blocks for the adapter that owns them. The generic OpenAI
// serializer uses stripAuthored so one provider's opaque blocks never leak to
// another provider.
func stripAuthoredPreserveBlocks(msgs []Message) []Message {
	out := stripAuthoredWithBlocks(msgs, true)
	// Anthropic represents tool failures with tool_result.is_error. ExitCode
	// is still ghg metadata and is consumed only while this adapter builds
	// that block; it is never serialized directly.
	for i := range out {
		if msgs[i].Role == "tool" {
			out[i].ExitCode = msgs[i].ExitCode
		}
	}
	return out
}

func stripAuthoredWithBlocks(msgs []Message, preserveBlocks bool) []Message {
	out := make([]Message, len(msgs))
	copy(out, msgs)
	for i := range out {
		out[i].Authored = false
		out[i].SentAt = nil
		out[i].Usage = nil
		out[i].Model = ""
		out[i].RewoundFrom = ""
		out[i].Output = nil
		out[i].ExitCode = 0
		out[i].Source = ""
		out[i].StopReason = ""
		if !preserveBlocks {
			out[i].ProviderBlocks = nil
		}
		for j := range out[i].ToolCalls {
			out[i].ToolCalls[j].Output = nil
			out[i].ToolCalls[j].DurationMs = 0
			out[i].ToolCalls[j].ExitCode = 0
		}
	}
	// Backfill tool-message Name from the owning call (older sessions predate
	// the field; providers that require it only look at Name).
	names := map[string]string{}
	for _, m := range out {
		if m.Role == "assistant" {
			for _, tc := range m.ToolCalls {
				names[tc.ID] = tc.Function.Name
			}
		}
	}
	for i := range out {
		if out[i].Role == "tool" && out[i].Name == "" {
			out[i].Name = names[out[i].ToolCallID]
		}
	}
	return out
}

// ToolHistoryIndex records the tool-call relationships needed by history
// repairers.
type ToolHistoryIndex struct {
	calls     map[string]struct{}
	names     map[string]string
	results   map[string]struct{}
	immediate map[string]bool
}

// IndexToolHistory builds a tool-call relationship index.
func IndexToolHistory(msgs []Message) ToolHistoryIndex {
	index := ToolHistoryIndex{
		calls:     make(map[string]struct{}),
		names:     make(map[string]string),
		results:   make(map[string]struct{}),
		immediate: make(map[string]bool),
	}
	for i, msg := range msgs {
		if msg.Role == "tool" {
			index.results[msg.ToolCallID] = struct{}{}
			continue
		}
		if msg.Role != "assistant" {
			continue
		}
		for _, call := range msg.ToolCalls {
			index.calls[call.ID] = struct{}{}
			index.names[call.ID] = call.Function.Name
			index.immediate[call.ID] = false
			for _, result := range msgs[i+1:] {
				if result.Role == "tool" && result.ToolCallID == call.ID {
					index.immediate[call.ID] = true
					break
				}
				if result.Role == "assistant" || result.Role == "user" {
					break
				}
			}
		}
	}
	return index
}

func (i ToolHistoryIndex) HasCall(id string) bool {
	_, ok := i.calls[id]
	return ok
}

func (i ToolHistoryIndex) HasResult(id string) bool {
	_, ok := i.results[id]
	return ok
}

func (i ToolHistoryIndex) HasImmediateResult(id string) bool {
	return i.immediate[id]
}

func (i ToolHistoryIndex) CallName(id string) string {
	return i.names[id]
}

// repairToolHistory synthesizes results for unanswered tool calls and flattens orphan tool results into user messages.
// Leaves valid message histories unchanged.
func repairToolHistory(msgs []Message) []Message {
	index := IndexToolHistory(msgs)
	out := make([]Message, 0, len(msgs))
	var pending []string // unanswered call IDs from the last assistant message
	flush := func() {    // synthetics land after any real results in the run
		for _, id := range pending {
			out = append(out, Message{
				Role:       "tool",
				Content:    "(interrupted before execution)",
				ToolCallID: id,
				Name:       index.CallName(id),
			})
		}
		pending = nil
	}
	for _, m := range msgs {
		if m.Role == "tool" {
			if !index.HasCall(m.ToolCallID) {
				flush()
				// orphan: flatten into user context rather than drop the info
				out = append(out, Message{
					Role:    "user",
					Content: "[earlier tool result]\n" + m.Content,
				})
				continue
			}
			out = append(out, m)
			continue
		}
		flush()
		out = append(out, m)
		if m.Role == "assistant" {
			for _, tc := range m.ToolCalls {
				if !index.HasImmediateResult(tc.ID) {
					pending = append(pending, tc.ID)
				}
			}
		}
	}
	flush()
	return out
}

package agent

import (
	"strconv"
	"strings"

	"github.com/sacca97/ghg/internal/llm"
)

const (
	defaultAgingBudgetTokens = 256
	agedPreviewBytes         = 512
)

// ageResultMessages derives a request-only view. The authoritative message
// slice remains untouched, so Save, resume, fork, rewind, and compaction
// inspection continue to see exact raw evidence.
func ageResultMessages(msgs []llm.Message, contextLimit int) []llm.Message {
	if len(msgs) < 3 {
		return msgs
	}
	keepStart := agingTailStart(msgs, contextLimit)
	if keepStart <= 1 {
		return msgs
	}
	out := append([]llm.Message(nil), msgs...)
	changed := false
	for i := 1; i < keepStart; i++ {
		if out[i].Role != "tool" || out[i].ExitCode != 0 || out[i].Artifact == nil || len(out[i].Content) == 0 {
			continue
		}
		stub := agedToolResult(out[i])
		if len(stub) >= len(out[i].Content) {
			continue
		}
		out[i].Content = stub
		changed = true
	}
	if !changed {
		return msgs
	}
	return out
}

func agingTailStart(msgs []llm.Message, contextLimit int) int {
	budget := contextLimit / 8
	if budget < defaultAgingBudgetTokens {
		budget = defaultAgingBudgetTokens
	}
	start, used := len(msgs), 0
	for start > 1 {
		groupStart := compactGroupStart(msgs, start-1)
		cost := EstimateTokens(msgs[groupStart:start])
		if start < len(msgs) && used+cost > budget {
			break
		}
		start = groupStart
		used += cost
	}
	return start
}

func agedToolResult(msg llm.Message) string {
	name := msg.Name
	if name == "" {
		name = msg.Source
	}
	source := msg.Source
	if source == "" {
		source = name
	}
	ref := msg.Artifact
	state := "complete"
	if !ref.Complete {
		state = "head/tail retained; middle omitted"
	}
	var b strings.Builder
	b.WriteString("Aged tool result")
	b.WriteString(" source=" + strconv.Quote(source))
	b.WriteString(" tool=" + strconv.Quote(name))
	b.WriteString(" call_id=" + strconv.Quote(msg.ToolCallID))
	b.WriteString(" exit_code=" + strconv.Itoa(msg.ExitCode))
	b.WriteString(" original_bytes=" + strconv.FormatInt(ref.OriginalBytes, 10))
	b.WriteString(" stored_bytes=" + strconv.FormatInt(ref.StoredBytes, 10))
	b.WriteString(" completeness=" + state)
	b.WriteString(" recovery=" + ref.ID)
	if !ref.Complete || ref.StoredBytes == 0 {
		b.WriteString(" exact_preview=" + strconv.Quote(truncateField(msg.Content, agedPreviewBytes)))
	}
	b.WriteString("; use artifact_read with this id for retained evidence.")
	return b.String()
}

package agent

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"

	"github.com/sacca97/ghg/internal/llm"
)

const (
	defaultAgingBudgetTokens = 256
	maxAgingBudgetTokens     = 16 * 1024
	agedPreviewBytes         = 512
)

var readObservationHeader = regexp.MustCompile(`^\[observation ([^ ]+) path=(.*?) lines=([0-9]+)-([0-9]+) next_offset=[0-9]+\]\n`)

type readObservation struct {
	id         string
	path       string
	start, end int
}

type parsedReadObservation struct {
	readObservation
	prefix, header, body, suffix string
}

// ageResultMessages derives a request-only view. The authoritative message
// slice remains untouched, so Save, resume, fork, rewind, and compaction
// inspection continue to see exact raw evidence.
func ageResultMessages(msgs []llm.Message, contextLimit int) []llm.Message {
	if len(msgs) < 2 {
		return msgs
	}
	keepStart := agingTailStart(msgs, contextLimit)
	out := append([]llm.Message(nil), msgs...)
	changed := deduplicateReadResults(out)
	if keepStart > 1 {
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
	}
	if !changed {
		return msgs
	}
	return out
}

func agingTailStart(msgs []llm.Message, contextLimit int) int {
	budget := contextLimit / 8
	if budget > maxAgingBudgetTokens {
		budget = maxAgingBudgetTokens
	}
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

func deduplicateReadResults(msgs []llm.Message) bool {
	later := make(map[string][]readObservation)
	changed := false
	for i := len(msgs) - 1; i >= 0; i-- {
		msg := &msgs[i]
		if msg.Role != "tool" || msg.ExitCode != 0 || (msg.Name != "read" && msg.Source != "read") {
			continue
		}
		observation, ok := parseReadObservation(msg.Content)
		if !ok {
			continue
		}
		if newer := later[observation.path]; len(newer) > 0 {
			if content, deduplicated := removeSupersededReadLines(observation, newer); deduplicated {
				msg.Content = content
				changed = true
			}
		}
		later[observation.path] = append(later[observation.path], observation.readObservation)
	}
	return changed
}

func parseReadObservation(content string) (parsedReadObservation, bool) {
	start := strings.Index(content, "[observation ")
	if start < 0 || (start > 0 && !strings.HasPrefix(content, "<untrusted_tool_output ")) {
		return parsedReadObservation{}, false
	}
	match := readObservationHeader.FindStringSubmatch(content[start:])
	if len(match) != 5 {
		return parsedReadObservation{}, false
	}
	startLine, errStart := strconv.Atoi(match[3])
	endLine, errEnd := strconv.Atoi(match[4])
	if match[1] == "" || match[2] == "" || errStart != nil || errEnd != nil || startLine <= 0 || endLine < startLine {
		return parsedReadObservation{}, false
	}
	headerStart := start
	headerEnd := start + len(match[0])
	rest := content[headerEnd:]
	lines := strings.SplitAfter(rest, "\n")
	expected := endLine - startLine + 1
	if expected <= 0 || len(lines) < expected {
		return parsedReadObservation{}, false
	}
	for i := 0; i < expected; i++ {
		if !strings.HasPrefix(lines[i], strconv.Itoa(startLine+i)+"\t") {
			return parsedReadObservation{}, false
		}
	}
	return parsedReadObservation{
		readObservation: readObservation{id: match[1], path: match[2], start: startLine, end: endLine},
		prefix:          content[:headerStart],
		header:          content[headerStart:headerEnd],
		body:            strings.Join(lines[:expected], ""),
		suffix:          strings.Join(lines[expected:], ""),
	}, true
}

func removeSupersededReadLines(observation parsedReadObservation, newer []readObservation) (string, bool) {
	lines := strings.SplitAfter(observation.body, "\n")
	if len(lines) > 0 && lines[len(lines)-1] == "" {
		lines = lines[:len(lines)-1]
	}
	if len(lines) != observation.end-observation.start+1 {
		return observation.prefix + observation.header + observation.body + observation.suffix, false
	}
	omitted := make([]bool, len(lines))
	anyOmitted := false
	for i := range lines {
		lineNo := observation.start + i
		for _, candidate := range newer {
			if lineNo >= candidate.start && lineNo <= candidate.end {
				omitted[i] = true
				anyOmitted = true
				break
			}
		}
	}
	if !anyOmitted {
		return observation.prefix + observation.header + observation.body + observation.suffix, false
	}

	if startAllOmitted(omitted) {
		return observation.prefix + fmt.Sprintf("[observation %s superseded by observation %s for lines %d-%d]\n", observation.id, strings.Join(supersedingObservationIDs(observation.start, observation.end, newer), ","), observation.start, observation.end) + observation.suffix, true
	}
	var out strings.Builder
	out.WriteString(observation.prefix)
	out.WriteString(observation.header)
	for i := 0; i < len(lines); {
		if !omitted[i] {
			out.WriteString(lines[i])
			i++
			continue
		}
		start := i
		for i < len(lines) && omitted[i] {
			i++
		}
		end := i - 1
		ids := supersedingObservationIDs(observation.start+start, observation.start+end, newer)
		fmt.Fprintf(&out, "[lines %d-%d superseded by observation %s]\n", observation.start+start, observation.start+end, strings.Join(ids, ","))
	}
	out.WriteString(observation.suffix)
	return out.String(), true
}

func startAllOmitted(omitted []bool) bool {
	for _, value := range omitted {
		if !value {
			return false
		}
	}
	return true
}

func supersedingObservationIDs(start, end int, newer []readObservation) []string {
	seen := make(map[string]bool)
	ids := make([]string, 0, len(newer))
	for line := start; line <= end; line++ {
		for _, observation := range newer {
			if line < observation.start || line > observation.end || seen[observation.id] {
				continue
			}
			seen[observation.id] = true
			ids = append(ids, observation.id)
		}
	}
	return ids
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

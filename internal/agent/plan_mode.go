package agent

import (
	"strings"

	"github.com/sacca97/ghg/internal/tools"
)

// planModePrompt is injected as a transient system message on every model
// round while the agent is in Plan mode. It is kept as a plain constant here
// (alongside the mode implementation) rather than routed through prompt
// loading — Plan mode is a collaboration mode, not an agent definition.
const planModePrompt = `You are planning in a read-only collaboration mode. You have only these read-only tools: read, grep, glob, lsp, find_files, artifact_list, artifact_read, history_search, and history_read. You cannot write, edit, run shell commands, spawn tasks, or mutate memory — treat them as unavailable.

Inspect only the code necessary to understand requirements, locate relevant components, and resolve ambiguity. Do not reread files already inspected. Once you have sufficient evidence to make implementation decisions complete, synthesize your findings and produce the plan.

End your response with a Markdown implementation plan in a single, exact block:

<proposed_plan>
...Markdown plan...
</proposed_plan>

A response without that block is valid only when actively gathering necessary initial evidence or asking clarifying questions. Only emit <proposed_plan> once, as your final answer.`

// planSafeTools is the read-only allowlist Plan mode exposes. Enforcement
// happens when building "available", not only through prompting, so mutating
// and side-effecting tools are structurally unreachable in Plan mode. MCP tools
// are intentionally excluded because ghg cannot prove they are read-only.
var planSafeTools = map[string]bool{
	"read":           true,
	"grep":           true,
	"glob":           true,
	"lsp":            true,
	"find_files":     true,
	"artifact_list":  true,
	"artifact_read":  true,
	"history_search": true,
	"history_read":   true,
}

// planTools returns the Plan-mode read-only tool allowlist. It intentionally
// does not include the MCP set (unproven read-only) or any mutating built-in.
func (a *Agent) planTools() []tools.Tool {
	a.toolsMu.Lock()
	defer a.toolsMu.Unlock()
	var out []tools.Tool
	for _, t := range a.Tools {
		if planSafeTools[t.Def.Function.Name] {
			out = append(out, t)
		}
	}
	return out
}

// planStreamParser is a small stateful parser for the exact, line-delimited
// proposed-plan block, tolerant of tags divided across provider chunks. Normal
// text is forwarded to visible; the block body is forwarded to onPlan; wrapper
// tags are dropped from both.
type planStreamParser struct {
	visible func(string)
	onPlan  func(string)
	mode    int
	buf     string
}

const (
	ppModeText = iota
	ppModePlan
)

const (
	proposedPlanOpen  = "<proposed_plan>"
	proposedPlanClose = "</proposed_plan>"
)

// feed consumes one streaming delta, forwarding visible text and plan body
// fragments to the respective callbacks.
func (p *planStreamParser) feed(delta string) {
	p.buf += delta
	p.process()
}

func (p *planStreamParser) process() {
	for {
		var tag string
		if p.mode == ppModeText {
			tag = proposedPlanOpen
		} else {
			tag = proposedPlanClose
		}
		idx := strings.Index(p.buf, tag)
		if idx >= 0 {
			body, rest := p.buf[:idx], p.buf[idx+len(tag):]
			if body != "" {
				p.emit(body)
			}
			p.buf = rest
			if p.mode == ppModeText {
				p.mode = ppModePlan
			} else {
				p.mode = ppModeText
			}
			continue
		}
		// No complete tag yet. Hold back any trailing bytes that could start a
		// tag (so a split tag resolves across chunks) and flush the rest.
		keep := longestPrefixSuffix(p.buf, tag)
		flush := p.buf[:len(p.buf)-keep]
		if flush != "" {
			p.emit(flush)
		}
		p.buf = p.buf[len(p.buf)-keep:]
		return
	}
}

func (p *planStreamParser) emit(s string) {
	if p.mode == ppModeText {
		if p.visible != nil {
			p.visible(s)
		}
		return
	}
	if p.onPlan != nil {
		p.onPlan(s)
	}
}

// close flushes any trailing buffered content. An unfinished plan block at the
// end of the stream is treated as closed: its body is emitted as plan content.
func (p *planStreamParser) close() {
	if p.buf != "" {
		p.emit(p.buf)
		p.buf = ""
	}
}

// longestPrefixSuffix returns the length of the longest suffix of s that equals
// a prefix of tag. It is bounded by len(s).
func longestPrefixSuffix(s, tag string) int {
	max := len(tag) - 1
	if len(s) < max {
		max = len(s)
	}
	for n := max; n > 0; n-- {
		if strings.HasSuffix(s, tag[:n]) {
			return n
		}
	}
	return 0
}

// ExtractProposedPlan returns the body of the last complete
// <proposed_plan>...</proposed_plan> block in text. If a provider violates the
// one-block instruction, the last block wins.
func ExtractProposedPlan(text string) (string, bool) {
	var last string
	rest := text
	for {
		open := strings.Index(rest, proposedPlanOpen)
		if open < 0 {
			break
		}
		body := rest[open+len(proposedPlanOpen):]
		closeIdx := strings.Index(body, proposedPlanClose)
		if closeIdx < 0 {
			break
		}
		last = strings.TrimSpace(body[:closeIdx])
		rest = body[closeIdx+len(proposedPlanClose):]
	}
	return last, last != ""
}

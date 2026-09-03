package main

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/sacca97/ghg/internal/agent"
	"github.com/sacca97/ghg/internal/memory"
	"github.com/sacca97/ghg/internal/skills"
	workerwire "github.com/sacca97/ghg/internal/worker"
)

func (w *workerProcessState) contextDoctorReport() workerwire.ContextDoctorResult {
	var rows []agent.ContextDoctorRow
	system := ""
	if messages := w.ag.MessagesSnapshot(); len(messages) > 0 && messages[0].Role == "system" {
		system = messages[0].Content
	}
	rows = append(rows, agent.ContextDoctorRow{Label: "system prompt", Bytes: len(system)})
	skillsBlock := w.currentSkillsBlock()
	rows = append(rows, agent.ContextDoctorRow{Label: "skills", Bytes: len(skillsBlock)})
	var toolBytes int
	for _, tool := range w.ag.AllTools() {
		data, _ := json.Marshal(tool.Def)
		toolBytes += len(data) + 8
	}
	rows = append(rows, agent.ContextDoctorRow{Label: fmt.Sprintf("tool schemas (%d tools)", len(w.ag.AllTools())), Bytes: toolBytes, Note: "sent with every request"})
	if w.mcp != nil {
		if instructions := w.mcp.InstructionsBlock(); instructions != "" {
			rows = append(rows, agent.ContextDoctorRow{Label: "mcp instructions", Bytes: len(instructions)})
		}
	}
	if history := w.ag.MessagesSnapshot(); len(history) > 1 {
		rows = append(rows, agent.ContextDoctorRow{Label: "conversation history", Bytes: agent.EstimateTokens(history) * 4, Note: "estimated"})
	}
	if usage := w.ag.Usage(); usage.PromptTokens > 0 {
		rows = append(rows, agent.ContextDoctorRow{Label: "session spend so far", Note: fmt.Sprintf("%d in / %d out (actual)", usage.PromptTokens, usage.CompletionTokens)})
	}
	policy := ""
	if w.runtime != nil && w.runtime.Policy != nil {
		status := w.runtime.Policy.Status()
		policy = fmt.Sprintf("Execution policy: %s · backend: %s · network: %s\n  workspace: %s", status.Mode, status.Backend, status.Network, status.Workspace)
	}
	return workerwire.ContextDoctorResult{Report: strings.TrimRight(agent.FormatContextDoctor(rows, policy), "\n")}
}

func (w *workerProcessState) currentSkillsBlock() string {
	return skills.PromptBlock(skills.Scan(skills.DefaultDirs()...)) +
		memory.PromptBlock(memory.Installation(), memory.Session(w.sessionID))
}

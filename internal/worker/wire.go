package worker

// Wire payloads shared by the worker process (cmd/ghg) and its controllers
// (internal/tui, ghg attach). One definition instead of sibling copies that
// drift; the JSON tags are the protocol.

import (
	"time"

	"github.com/sacca97/ghg/internal/agent"
	goalstate "github.com/sacca97/ghg/internal/goal"
	"github.com/sacca97/ghg/internal/llm"
)

// Input starts one agent turn.
type Input struct {
	Input        string            `json:"input"`
	Authored     bool              `json:"authored"`
	Parts        []llm.ContentPart `json:"parts,omitempty"`
	Goal         *goalstate.Record `json:"goal,omitempty"`
	SystemPrompt string            `json:"system_prompt,omitempty"`
	At           int               `json:"at"`
	Snap         string            `json:"snap,omitempty"`
}

// TurnResult reports a finished turn.
type TurnResult struct {
	Final    string        `json:"final,omitempty"`
	Error    string        `json:"error,omitempty"`
	Usage    llm.Usage     `json:"usage"`
	At       int           `json:"at"`
	Snap     string        `json:"snap,omitempty"`
	Messages []llm.Message `json:"messages,omitempty"`
}

// CompactResult reports a finished compaction.
type CompactResult struct {
	Error    string        `json:"error,omitempty"`
	Usage    llm.Usage     `json:"usage"`
	Messages []llm.Message `json:"messages,omitempty"`
}

// TaskState is one background subagent as seen by the worker.
type TaskState struct {
	ID          string    `json:"id"`
	Description string    `json:"description"`
	Prompt      string    `json:"prompt,omitempty"`
	Status      string    `json:"status"`
	Report      string    `json:"report,omitempty"`
	StartedAt   time.Time `json:"started_at"`
	EndedAt     time.Time `json:"ended_at,omitempty"`
}

// Approval is one pending capability gate.
type Approval struct {
	ID      string `json:"id"`
	Tool    string `json:"tool"`
	Command string `json:"command"`
	Rule    string `json:"rule"`
}

// ApprovalAnswer answers a pending approval.
type ApprovalAnswer struct {
	ID       string `json:"id"`
	Decision string `json:"decision"`
	Redirect string `json:"redirect,omitempty"`
}

// ConfigureRequest retargets the idle worker's model route.
type ConfigureRequest struct {
	Model        string `json:"model,omitempty"`
	ModelName    string `json:"model_name,omitempty"`
	Provider     string `json:"provider,omitempty"`
	Role         string `json:"role,omitempty"`
	Protocol     string `json:"protocol,omitempty"`
	Effort       string `json:"effort,omitempty"`
	UpdateEffort bool   `json:"update_effort,omitempty"`
}

// PlanRequest asks for a detached plan proposal.
type PlanRequest struct {
	Goal string `json:"goal"`
}

// PlanResult reports the plan proposal (or its failure).
type PlanResult struct {
	Plan  agent.Plan `json:"plan"`
	Error string     `json:"error,omitempty"`
}

// PermissionRequest announces one pending approval on the event stream.
type PermissionRequest struct {
	Approval Approval `json:"approval"`
}

// Snapshot is the full controller-attach state.
type Snapshot struct {
	SessionID     string        `json:"session_id"`
	State         State         `json:"state"`
	Detached      bool          `json:"detached"`
	Model         string        `json:"model"`
	ModelName     string        `json:"model_name"`
	Provider      string        `json:"provider"`
	Role          string        `json:"role,omitempty"`
	Protocol      string        `json:"protocol,omitempty"`
	Effort        string        `json:"effort,omitempty"`
	ContextLimit  int           `json:"context_limit,omitempty"`
	ContextTokens int           `json:"context_tokens"`
	Usage         llm.Usage     `json:"usage"`
	Messages      []llm.Message `json:"messages,omitempty"`
	Tasks         []TaskState   `json:"tasks,omitempty"`
	Pending       *Approval     `json:"pending_approval,omitempty"`
	ActiveTool    string        `json:"active_tool,omitempty"`
	LiveText      string        `json:"live_text,omitempty"`
	LiveThink     string        `json:"live_think,omitempty"`
	LiveTool      string        `json:"live_tool_output,omitempty"`
}

// AppendRequest carries a local context message (shell-escape output) to the
// worker-owned conversation.
type AppendRequest struct {
	Content string `json:"content"`
}

// LSPStatus reports one worker-owned language server without exposing the
// manager or its process state to the controller.
type LSPStatus struct {
	Name  string `json:"name"`
	Root  string `json:"root,omitempty"`
	State string `json:"state"`
	Error string `json:"error,omitempty"`
}

package worker

// Wire payloads shared by the worker process (cmd/ghg) and its controllers
// (internal/tui, ghg attach). One definition instead of sibling copies that
// drift; the JSON tags are the protocol.

import (
	"time"

	"github.com/sacca97/ghg/internal/agent"
	"github.com/sacca97/ghg/internal/models"
)

// Input starts one agent turn.
type Input struct {
	Input        string               `json:"input"`
	Authored     bool                 `json:"authored"`
	Parts        []models.ContentPart `json:"parts,omitempty"`
	Goal         *agent.GoalRecord    `json:"goal,omitempty"`
	SystemPrompt string               `json:"system_prompt,omitempty"`
	At           int                  `json:"at"`
	Snap         string               `json:"snap,omitempty"`
	PlanMode     bool                 `json:"plan_mode,omitempty"`
	ReviewMode   bool                 `json:"review_mode,omitempty"`
	AskMode      bool                 `json:"ask_mode,omitempty"`
}

// TurnResult reports a finished turn.
type TurnResult struct {
	SessionID      string            `json:"session_id,omitempty"`
	Final          string            `json:"final,omitempty"`
	Error          string            `json:"error,omitempty"`
	Usage          models.Usage      `json:"usage"`
	ContextTokens  int               `json:"context_tokens"`
	ContextLimit   int               `json:"context_limit,omitempty"`
	Model          string            `json:"model,omitempty"`
	ModelName      string            `json:"model_name,omitempty"`
	Provider       string            `json:"provider,omitempty"`
	Role           string            `json:"role,omitempty"`
	Protocol       string            `json:"protocol,omitempty"`
	Effort         string            `json:"effort,omitempty"`
	At             int               `json:"at"`
	Snap           string            `json:"snap,omitempty"`
	Clean          bool              `json:"clean"`
	Messages       []models.Message  `json:"messages,omitempty"`
	Plan           string            `json:"plan,omitempty"`
	Review         string            `json:"review,omitempty"`
	ReviewMarkdown string            `json:"review_markdown,omitempty"`
	Goal           *agent.GoalRecord `json:"goal,omitempty"`
	GoalContinue   bool              `json:"goal_continue,omitempty"`
}

// CompactResult reports a finished compaction.
type CompactResult struct {
	Error    string           `json:"error,omitempty"`
	Usage    models.Usage     `json:"usage"`
	Messages []models.Message `json:"messages,omitempty"`
}

// RewindRequest replaces the worker's live prompt view with Messages. Cut is
// the conversation boundary used for workspace snapshot restoration.
type RewindRequest struct {
	Cut      int              `json:"cut"`
	Messages []models.Message `json:"messages"`
}

// HistoryResult is returned after a history-changing worker operation.
type HistoryResult struct {
	SessionID     string           `json:"session_id"`
	Messages      []models.Message `json:"messages,omitempty"`
	Usage         models.Usage     `json:"usage"`
	ContextTokens int              `json:"context_tokens"`
	Restored      int              `json:"restored,omitempty"`
}

// ChdirResult reports the worker's canonical working directory.
type ChdirResult struct {
	CWD string `json:"cwd"`
}

// ShellRequest asks the worker to run one shell escape.
type ShellRequest struct {
	Command string `json:"command"`
}

// ShellResult reports a completed shell escape.
type ShellResult struct {
	Command string `json:"command"`
	Output  string `json:"output"`
}

type GoalRequest struct {
	Action string            `json:"action"`
	Record *agent.GoalRecord `json:"record,omitempty"`
}

type GoalFromContextRequest struct {
	Window int `json:"window,omitempty"`
}

type GoalFromContextResult struct {
	Goal  *agent.GoalRecord `json:"goal,omitempty"`
	Usage models.Usage      `json:"usage"`
	Error string            `json:"error,omitempty"`
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
	Restored    bool      `json:"restored,omitempty"`
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
	Model                  string  `json:"model,omitempty"`
	ModelName              string  `json:"model_name,omitempty"`
	Provider               string  `json:"provider,omitempty"`
	Role                   string  `json:"role,omitempty"`
	Protocol               string  `json:"protocol,omitempty"`
	Effort                 string  `json:"effort,omitempty"`
	UpdateEffort           bool    `json:"update_effort,omitempty"`
	Mode                   string  `json:"mode,omitempty"`
	CompactModel           string  `json:"compact_model,omitempty"`
	CompactProvider        string  `json:"compact_provider,omitempty"`
	UpdateCompact          bool    `json:"update_compact,omitempty"`
	CompactThreshold       float64 `json:"compact_threshold,omitempty"`
	UpdateCompactThreshold bool    `json:"update_compact_threshold,omitempty"`
}

// PermissionRequest announces one pending approval on the event stream.
type PermissionRequest struct {
	Approval Approval `json:"approval"`
}

// Snapshot is the full controller-attach state.
type Snapshot struct {
	SessionID     string           `json:"session_id"`
	State         State            `json:"state"`
	Detached      bool             `json:"detached"`
	Model         string           `json:"model"`
	ModelName     string           `json:"model_name"`
	Provider      string           `json:"provider"`
	Role          string           `json:"role,omitempty"`
	Protocol      string           `json:"protocol,omitempty"`
	Effort        string           `json:"effort,omitempty"`
	ContextLimit  int              `json:"context_limit,omitempty"`
	ContextTokens int              `json:"context_tokens"`
	Usage         models.Usage     `json:"usage"`
	Messages      []models.Message `json:"messages,omitempty"`
	Tasks         []TaskState      `json:"tasks,omitempty"`
	Pending       *Approval        `json:"pending_approval,omitempty"`
	ActiveTool    string           `json:"active_tool,omitempty"`
	LiveText      string           `json:"live_text,omitempty"`
	LiveThink     string           `json:"live_think,omitempty"`
	LiveTool      string           `json:"live_tool_output,omitempty"`
	Mode          string           `json:"mode,omitempty"`
	LivePlan      string           `json:"live_plan,omitempty"`
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

// MCPStatus reports one worker-owned MCP server.
type MCPStatus struct {
	Name   string `json:"name"`
	State  string `json:"state"`
	Note   string `json:"note,omitempty"`
	Error  string `json:"error,omitempty"`
	Tools  int    `json:"tools,omitempty"`
	Source string `json:"source,omitempty"`
}

type MCPRequest struct {
	Name string `json:"name"`
}

type ContextDoctorResult struct {
	Report string `json:"report"`
}

// ForkRequest asks the worker to create a new session branching from the current one.
type ForkRequest struct {
	Cut   int    `json:"cut"`
	Title string `json:"title"`
}

// ForkResult reports the newly created session.
type ForkResult struct {
	NewSessionID string `json:"new_session_id"`
	OldSessionID string `json:"old_session_id"`
	Title        string `json:"title"`
	OldTitle     string `json:"old_title"`
}

// RenameRequest asks the worker to rename the session.
type RenameRequest struct {
	Title string `json:"title"`
}

// RenameResult reports the renamed session.
type RenameResult struct {
	SessionID string `json:"session_id"`
	Title     string `json:"title"`
}

package worker

// Wire payloads shared by the worker process (cmd/ghg) and its controllers
// (internal/tui, ghg attach). One definition instead of sibling copies that
// drift; the JSON tags are the protocol.

import (
	"time"

	"github.com/sacca97/ghg/internal/agent"
	"github.com/sacca97/ghg/internal/models"
)

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
	Continue     bool                 `json:"continue,omitempty"`
}

type TurnResult struct {
	SessionID      string            `json:"session_id,omitempty"`
	Final          string            `json:"final,omitempty"`
	Error          string            `json:"error,omitempty"`
	Interrupted    bool              `json:"interrupted,omitempty"`
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

type CompactResult struct {
	Error       string           `json:"error,omitempty"`
	Interrupted bool             `json:"interrupted,omitempty"`
	Usage       models.Usage     `json:"usage"`
	Messages    []models.Message `json:"messages,omitempty"`
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
	Goal        *agent.GoalRecord `json:"goal,omitempty"`
	Usage       models.Usage      `json:"usage"`
	Error       string            `json:"error,omitempty"`
	Interrupted bool              `json:"interrupted,omitempty"`
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

type Approval struct {
	ID      string `json:"id"`
	Tool    string `json:"tool"`
	Command string `json:"command"`
	Rule    string `json:"rule"`
}

type ApprovalAnswer struct {
	ID       string `json:"id"`
	Decision string `json:"decision"`
	Redirect string `json:"redirect,omitempty"`
}

// ConfigureRequest updates worker model routing or approval mode.
type ConfigureRequest struct {
	Model                  string  `json:"model,omitempty"`
	ModelName              string  `json:"model_name,omitempty"`
	Provider               string  `json:"provider,omitempty"`
	Role                   string  `json:"role,omitempty"`
	Protocol               string  `json:"protocol,omitempty"`
	Effort                 string  `json:"effort,omitempty"`
	UpdateEffort           bool    `json:"update_effort,omitempty"`
	DynamicReasoning       *bool   `json:"dynamic_reasoning,omitempty"`
	Mode                   string  `json:"mode,omitempty"`
	Approval               string  `json:"approval,omitempty"`
	CompactThreshold       float64 `json:"compact_threshold,omitempty"`
	UpdateCompactThreshold bool    `json:"update_compact_threshold,omitempty"`
	// PersistRoleModel stores Model/Provider as Role's configured route before
	// applying it. The worker owns configuration changes; a controller only
	// asks for them.
	PersistRoleModel bool `json:"persist_role_model,omitempty"`
	// PersistDynamicReasoning stores DynamicReasoning in the config instead of
	// only applying it to the live agent.
	PersistDynamicReasoning bool `json:"persist_dynamic_reasoning,omitempty"`
}

// PermissionRequest announces one pending approval on the event stream.
type PermissionRequest struct {
	Approval Approval `json:"approval"`
}

type QuestionOption struct {
	Label       string `json:"label"`
	Description string `json:"description,omitempty"`
}

type Question struct {
	ID       string           `json:"id"`
	Question string           `json:"question"`
	Options  []QuestionOption `json:"options"`
}

type QuestionRequest struct {
	ID        string     `json:"id"`
	Questions []Question `json:"questions"`
}

type QuestionAnswer struct {
	ID    string `json:"id"`
	Value string `json:"value"`
}

type QuestionAnswerRequest struct {
	ID        string           `json:"id"`
	Answers   []QuestionAnswer `json:"answers,omitempty"`
	Cancelled bool             `json:"cancelled,omitempty"`
}

// Snapshot is the full controller-attach state.
type Snapshot struct {
	SessionID       string           `json:"session_id"`
	State           State            `json:"state"`
	Detached        bool             `json:"detached"`
	Model           string           `json:"model"`
	ModelName       string           `json:"model_name"`
	Provider        string           `json:"provider"`
	Role            string           `json:"role,omitempty"`
	Protocol        string           `json:"protocol,omitempty"`
	Effort          string           `json:"effort,omitempty"`
	Approval        string           `json:"approval,omitempty"`
	ContextLimit    int              `json:"context_limit,omitempty"`
	ContextTokens   int              `json:"context_tokens"`
	Usage           models.Usage     `json:"usage"`
	Messages        []models.Message `json:"messages,omitempty"`
	Tasks           []TaskState      `json:"tasks,omitempty"`
	Pending         *Approval        `json:"pending_approval,omitempty"`
	PendingQuestion *QuestionRequest `json:"pending_question,omitempty"`
	ActiveTool      string           `json:"active_tool,omitempty"`
	LiveText        string           `json:"live_text,omitempty"`
	LiveThink       string           `json:"live_think,omitempty"`
	LiveTool        string           `json:"live_tool_output,omitempty"`
	Mode            string           `json:"mode,omitempty"`
	LivePlan        string           `json:"live_plan,omitempty"`
}

type StateEvent struct {
	State    State  `json:"state"`
	Detached bool   `json:"detached"`
	Mode     string `json:"mode"`
}

type ToolStartEvent struct {
	ID   string `json:"id"`
	Name string `json:"name"`
	Args string `json:"args"`
}

type ToolEndEvent struct {
	ID     string `json:"id"`
	Name   string `json:"name"`
	Result string `json:"result"`
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
	Cut      int              `json:"cut"`
	Title    string           `json:"title"`
	Messages []models.Message `json:"messages,omitempty"`
}

type ForkResult struct {
	NewSessionID string `json:"new_session_id"`
	OldSessionID string `json:"old_session_id"`
	Title        string `json:"title"`
	OldTitle     string `json:"old_title"`
}

type RenameRequest struct {
	Title string `json:"title"`
}

type RenameResult struct {
	SessionID string `json:"session_id"`
	Title     string `json:"title"`
}

type NotifyRequest struct {
	Action   string `json:"action,omitempty"`
	BotToken string `json:"bot_token,omitempty"`
	ChatID   string `json:"chat_id,omitempty"`
}

type NotifyResult struct {
	Enabled bool `json:"enabled"`
}

// SearchProviderRequest configures a SearXNG search endpoint.
type SearchProviderRequest struct {
	Action  string `json:"action"`
	Name    string `json:"name,omitempty"`
	BaseURL string `json:"base_url,omitempty"`
	APIKey  string `json:"api_key,omitempty"`
}

type SearchProviderInfo struct {
	Name      string `json:"name"`
	BaseURL   string `json:"base_url"`
	Active    bool   `json:"active"`
	HasAPIKey bool   `json:"has_api_key"`
}

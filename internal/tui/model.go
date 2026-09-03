package tui

import (
	"context"
	"fmt"
	"github.com/charmbracelet/bubbles/spinner"
	"github.com/charmbracelet/bubbles/textarea"
	"github.com/charmbracelet/bubbles/viewport"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/sacca97/ghg/internal/agent"
	"github.com/sacca97/ghg/internal/config"
	"github.com/sacca97/ghg/internal/lsp"
	"github.com/sacca97/ghg/internal/mcp"
	"github.com/sacca97/ghg/internal/memory"
	"github.com/sacca97/ghg/internal/models"
	"github.com/sacca97/ghg/internal/session"
	"github.com/sacca97/ghg/internal/skills"
	"github.com/sacca97/ghg/internal/tools"
	workerwire "github.com/sacca97/ghg/internal/worker"
	"os"
	"slices"
	"strings"
	"sync"
	"time"
)

// UI styles use the terminal's standard ANSI palette and default background.
var (
	youStyle      = lipgloss.NewStyle().Foreground(lipgloss.Color("12")).Bold(true)
	botStyle      = lipgloss.NewStyle().Foreground(lipgloss.Color("13")).Bold(true)
	toolStyle     = lipgloss.NewStyle().Foreground(lipgloss.Color("11"))
	dimStyle      = lipgloss.NewStyle().Foreground(lipgloss.Color("8"))
	errStyle      = lipgloss.NewStyle().Foreground(lipgloss.Color("9"))
	thinkingStyle = lipgloss.NewStyle().Foreground(lipgloss.Color("8")).Italic(true)
)

// messages sent from the agent goroutine
type textMsg string
type toolStartMsg struct{ id, name, args string }
type toolEndMsg struct{ id, name, result string }
type steeredMsg string

// goalFromContextMsg carries the model-formulated goal back from the
// /goal-from-context goroutine to the Update loop.
type goalFromContextMsg struct {
	goal   string
	record *agent.GoalRecord
	usage  models.Usage
	err    error
}

type goalUpdateMsg struct{ update agent.GoalUpdate }
type goalUpdateRecordMsg struct{ record agent.GoalRecord }

type compactMsg struct {
	took, kept int // messages removed / kept after compaction
	summary    string
	err        error
}
type turnDoneMsg struct {
	final          string
	err            error
	at             int    // conversation index the turn started at (snapshot key)
	snap           string // pre-turn workspace snapshot commit ("" = not a git repo)
	clean          bool   // the turn left the tree clean — snap is worthless, drop it
	plan           string // worker-authoritative proposed plan, when present
	review         string // worker-authoritative review, when present
	reviewMarkdown string
	goal           *agent.GoalRecord
	goalContinue   bool
	goalUpdates    []agent.GoalUpdate
	goalUsage      models.Usage
}
type catalogsMsg map[string]config.Catalog // background /models fetch result
type noticeMsg string                      // dim one-liner appended to the transcript
type usageMsg models.Usage                 // one request's token usage
type quitArmMsg struct{}                   // the idle ctrl+c arm window expired
type taskUpdateMsg struct{}                // a background subagent started/settled — redraw
type mcpStatusMsg struct {
	statuses []workerwire.MCPStatus
}                        // an MCP server changed state — redraw
type thinkMsg string     // streamed reasoning tokens
type planDeltaMsg string // streamed proposed plan markdown tokens
type imageMsg struct {   // ctrl+v clipboard image result
	path string // clipboard image saved to disk
	err  error
}

type workerFrameMsg struct {
	frame      workerwire.Frame
	client     *workerwire.Client
	generation uint64
}
type workerErrorMsg struct {
	err        error
	client     *workerwire.Client
	process    *workerwire.Process
	generation uint64
}
type workerPermissionMsg struct{ approval workerwire.Approval }
type workerCompactDoneMsg struct {
	err           error
	usage         models.Usage
	contextTokens int
}

// menu is the open completion dropdown.
type menu struct {
	head   string // input before the token being completed
	cands  []cand
	idx    int
	base   string // input when tab cycling started; esc reverts to it
	cyc    bool   // tab/shift+tab cycling with live preview
	cycled bool   // a cycle step already happened (first tab previews, not advances)
	frozen []cand // full candidate set for the cycle's prefix (nil = live filter)
}

type model struct {
	cfg *config.Config
	// agent is retained for headless compatibility tests. Interactive models
	// set workerOnly and keep their execution state in the projection below.
	agent           *agent.Agent
	runtime         *tools.ToolRuntime
	modelName       string
	provName        string
	modelID         string
	protocol        string
	role            string
	effort          string
	contextLimit    int
	usage           models.Usage
	messages        []models.Message
	workerOnly      bool
	sysPrompt       string
	input           textarea.Model
	spin            spinner.Model
	vp              viewport.Model
	blocks          []block // finalized transcript (raw; rendered at the current width)
	viewportContent string
	plainRows       []string // ANSI-free transcript rows, built only while selecting
	// msgBlock[i] is the block index rendering agent.Messages[i] (-1: none) —
	// rewind live-scroll uses it to jump to a message's transcript position.
	msgBlock []int
	follow   bool // auto-scroll to bottom on new content
	width    int
	height   int

	busy    bool
	current string // in-flight partial assistant line
	inMsg   bool   // "● " prefix already printed for this assistant segment

	showThinking bool      // ctrl+o: render reasoning timer
	thinkStart   time.Time // timestamp when reasoning began for current segment
	menu         *menu
	picker       *picker
	settings     *settings // ctrl+p settings
	cancel       context.CancelFunc
	prog         *tea.Program

	store     *session.Store
	sessionID string
	saved     int            // messages already persisted (index into agent.Messages)
	snapshots map[int]string // workspace snapshot ref per turn-start index (mirrors the snapshots table)

	hist     []string         // submitted inputs, for up/down recall
	pasteBuf string           // held paste text for the [Pasted ~N lines] placeholder (config collapsePaste)
	histIdx  int              // len(hist) == not navigating
	draft    string           // in-progress input saved while navigating history
	lastUp   time.Time        // last ↑ keypress; repeat detection for history rollover
	now      func() time.Time // test seam; defaults to time.Now

	turnStart time.Time // when the in-flight turn began; zero when idle (busy line shows elapsed)

	queue      []string // messages typed while busy, sent after the turn ends
	queueSel   int      // selected queued message, -1 = none (not navigating)
	interrupt1 bool     // first ctrl+c pressed while busy; second cancels
	quit1      bool     // first ctrl+c pressed while idle; second quits (armed briefly)

	goal           string // compatibility mirror of the active structured goal
	goalRounds     int    // compatibility mirror of the structured goal rounds
	goalRecord     *agent.GoalRecord
	proposedPlanMD string // latest /plan proposal (Markdown), waiting for /execute
	planCurrent    string // partial line of streamed plan markdown
	mode           string // user-visible operating mode: plan or execute
	reviewing      bool   // active one-shot /review turn; restores mode/role on completion
	wheel          wheelState
	selection      *selectionState

	mouseOn       bool   // startup mouse setting; nil config means enabled
	compactModel  string // config model name for compaction summaries; "" = the built-in default
	compactProv   string
	statusModelX  int                       // screen column where the bottom model control starts
	statusModelW  int                       // visible width of the bottom model control
	statusEffortX int                       // screen column where the bottom effort control starts
	statusEffortW int                       // visible width of the bottom effort control
	statusModeX   int                       // screen column where the bottom mode control starts
	statusModeW   int                       // visible width of the bottom mode control
	shortCWD      string                    // cached abbreviated working directory
	modelSlotW    int                       // cached max width across role models
	progDone      chan struct{}             // closed when the TUI exits; unblocks pending gates
	catalogs      map[string]config.Catalog // provider model lists (capabilities)
	profiles      models.Profiles           // embedded/user/trusted-project provider metadata
	mcpMgr        *mcp.Manager              // MCP server connections; nil when none configured
	mcpSeen       map[string]bool           // servers whose first settle was announced
	lspMgr        *lsp.Manager              // LSP diagnostics source for write/edit tool output
	// skillScan is the skills discovery seam (skills.Scan over DefaultDirs in
	// the real model): a field so the context doctor can be tested against
	// temp-dir skills instead of whatever the test machine happens to have.
	skillScan    func() []skills.Skill
	skillsCache  []skills.Skill
	skillsLoaded int

	iactive *interactive // in-flight interactive command; nil when idle

	perms      permRules   // saved allow-always rules
	permDialog *permDialog // open permission modal; the turn is paused on it

	tasksFocus bool      // the tasks dock owns ↑/↓/enter/esc instead of the input
	taskSel    int       // selected row in the dock (index into newest-first tasks)
	dockSkip   int       // non-task rows at the dock's top (focused hint) — click math skips them
	taskVP     *taskView // open per-task detail view; nil when on the main thread
	dockRows   int       // rendered dock height; layout() maintains it for click math

	rew    *rewindState     // open rewind picker (double-esc while idle)
	esc1   bool             // first idle esc pressed; second opens the rewind picker
	escClr bool             // first esc pressed with a draft; second clears it to history
	future []models.Message // clipped tail kept for forward travel after a rewind

	namePrompt *namePrompt // inline text prompt (fork naming, /rename)

	workerClient         *workerwire.Client
	workerProcess        *workerwire.Process
	workerGeneration     uint64
	workerRuntime        workerwire.Runtime
	workerDetached       bool
	workerState          workerwire.State
	workerLiveWork       bool
	workerTasks          map[string]workerwire.TaskState
	detachRequestID      string
	workerStartFailed    bool
	workerStartError     string
	workerContextTokens  int
	workerHistoryRequest string
	workerRewindRestore  string
	workerChdirRequest   string
	cautious             bool
}

func (m *model) messagesSnapshot() []models.Message {
	if m.workerOnly || m.workerClient != nil || m.workerProcess != nil {
		return slices.Clone(m.messages)
	}
	if m.agent != nil {
		return m.agent.MessagesSnapshot()
	}
	return slices.Clone(m.messages)
}

func (m *model) setMessages(messages []models.Message) {
	m.messages = slices.Clone(messages)
	if !m.workerOnly && m.agent != nil {
		m.agent.Messages = slices.Clone(messages)
	}
}

func (m *model) currentModelID() string {
	if m.modelID != "" {
		return m.modelID
	}
	if m.agent != nil {
		return m.agent.Model
	}
	return m.modelName
}

func (m *model) currentRole() string {
	if m.role != "" {
		return m.role
	}
	if m.agent != nil && m.agent.Role != "" {
		return m.agent.Role
	}
	return m.modeRole()
}

func (m *model) currentUsage() models.Usage {
	if m.workerOnly || m.workerClient != nil || m.workerProcess != nil {
		return m.usage
	}
	if m.agent != nil {
		return m.agent.Usage()
	}
	return m.usage
}

func (m *model) configureOutputAgent(ag *agent.Agent) {
	if ag == nil {
		return
	}
	ag.Runtime = m.runtime
	if m.runtime != nil && m.runtime.ApprovalMode == tools.ApprovalAutoReview {
		m.runtime.Reviewer = ag.ApproveForMe
	}
	ag.OutputCatalog = m.store
	ag.HistoryCatalog = m.store
	if m.store != nil {
		ag.SetObservationStore(m.store.ObservationRegistryStore())
		ag.SetSearchStore(m.store.SearchRegistryStore())
	}
	ag.SubagentsDisabled = !config.SubagentsEnabled(m.cfg)
}

func (m *model) Init() tea.Cmd {
	return textarea.Blink
}

func cwd() string {
	if wd, err := os.Getwd(); err == nil {
		return wd
	}
	return "?"
}

func (m *model) nowFn() time.Time {
	if m.now != nil {
		return m.now()
	}
	return time.Now()
}

// applyCompactModel points the agent's compaction summary call at the
// configured compaction model/models. An explicit compactModel remains a
// per-session override. Otherwise a configured tiny role is selected; a
// legacy config without roles keeps the built-in compaction model for
// backwards compatibility. A bad optional route falls back to the
// conversation's own model.
func (m *model) applyCompactModel() {
	if m.agent == nil {
		return
	}
	m.agent.CompactBackend, m.agent.CompactModel = nil, ""
	m.agent.CompactProvider, m.agent.CompactProtocol = "", ""
	cm := m.compactModel
	cp := m.compactProv
	if cm == "" {
		if cp == "" && len(m.cfg.Roles) > 0 {
			target, roleErr := m.cfg.ResolveRole(config.RoleTiny)
			if roleErr != nil {
				m.append(errStyle.Render("tiny role: " + roleErr.Error() + " — using current model"))
				return
			}
			cm, cp = target.Model, target.Provider
		}
		if cm == "" {
			cm = m.defaultCompactModelName()
		}
	}
	compact, _, _, err := agent.NewConfigured(agent.BuildOptions{
		Config: m.cfg, Profiles: m.profiles, Model: cm, Provider: cp,
		Role: config.RoleTiny, SystemPrompt: m.sysPrompt,
		AllowMissingCredentials: m.compactModel == "",
	})
	if err != nil {
		if m.compactModel != "" {
			m.append(errStyle.Render("compaction model: " + err.Error() + " — using current model"))
		}
		return
	}
	if compact == nil || compact.Backend == nil {
		if m.compactModel != "" {
			m.append(errStyle.Render("compaction model: no API key — using current model"))
		}
		return
	}
	m.agent.CompactBackend = compact.Backend
	m.agent.CompactModel = compact.Model
	m.agent.CompactProvider = compact.Provider
	m.agent.CompactProtocol = compact.Protocol
}

func (m *model) defaultCompactModelName() string {
	if m.cfg != nil && len(m.cfg.Roles) > 0 {
		if target, err := m.cfg.ResolveRole(config.RoleTiny); err == nil && target.Model != "" {
			return target.Model
		}
	}
	return config.DefaultCompactModel
}

// wireTasks makes the active agent's background-task registry nudge the UI on
// every start/settle. OnChange runs on the worker goroutine, so it only sends
// a message (never touches UI state directly).
func (m *model) wireTasks() {
	if m.agent == nil {
		return
	}
	// Persist every start/settle to the session store so --resume can restore
	// the dock. Headless-safe (no prog needed). The session id comes in as an
	// argument — published via SetSessionID — so this worker-goroutine
	// callback never races the UI goroutine reading m.sessionID.
	st := m.store
	m.agent.Tasks().OnRecord = func(sessionID string, t *agent.BackgroundTask) {
		if st == nil || sessionID == "" {
			return // no session row yet; the settle's OnRecord will land after one exists
		}
		if err := st.SaveTask(sessionID, session.Task{
			ID: t.ID, Description: t.Description, Prompt: t.Prompt,
			Status: string(t.Status), Report: t.Report,
			StartedAt: t.StartedAt, EndedAt: t.EndedAt,
		}); err != nil {
			config.LogEvent("session.task", "save failed: "+err.Error())
		}
	}
	m.agent.Tasks().SetSessionID(m.sessionID)
	m.agent.SetSessionID(m.sessionID)
	if m.prog == nil {
		return // headless (tests)
	}
	m.agent.Tasks().OnChange = func(*agent.BackgroundTask) {
		// Detached: OnChange runs on the subagent worker goroutine, and a
		// backed-up UI queue must never stall the agent (see sendTaskMsg).
		go m.prog.Send(taskUpdateMsg{})
	}
	// Point the MCP manager at the NEW agent — resume/model-switch replace
	// m.agent wholesale, and the OnChange closure captures the model, not a
	// specific agent, precisely so this handoff works.
	if m.mcpMgr != nil {
		m.agent.SetMCPTools(m.mcpMgr.Tools())
	}
}

// rebuildAgent replaces the live route while carrying session state to the
// new backend. Preview rebuilds stay local to the TUI; committed rebuilds also
// refresh the worker.
func (m *model) rebuildAgent(modelName, providerName, role string, preview bool) error {
	buildRole := role
	if buildRole == "" {
		buildRole = config.RoleDefault
	}
	ag, modelName, providerName, err := agent.NewConfigured(agent.BuildOptions{
		Config: m.cfg, Profiles: m.profiles, Model: modelName, Provider: providerName,
		Role: buildRole, SystemPrompt: m.sysPrompt,
	})
	if err != nil {
		return err
	}
	if ag == nil {
		return fmt.Errorf("agent route is unavailable")
	}
	ag.Role = buildRole
	ag.ReasoningToggle = m.reasoningToggleFor(providerName, ag.Model)
	if previous := m.agent; previous != nil {
		ag.Effort = previous.Effort
		if msgs := previous.MessagesSnapshot(); len(msgs) > 1 {
			ag.Messages = append(ag.Messages, msgs[1:]...)
		}
		ag.SetUsage(previous.Usage())
		ag.Todos = slices.Clone(previous.Todos)
		ag.CompactBackend, ag.CompactModel = previous.CompactBackend, previous.CompactModel
		ag.CompactProvider, ag.CompactProtocol = previous.CompactProvider, previous.CompactProtocol
		ag.CompactThreshold = previous.CompactThreshold
		ag.PlanMode, ag.ReviewMode = previous.PlanMode, previous.ReviewMode
		previous.ShareState(ag)
	} else {
		ag.CompactThreshold = config.CompactThreshold(m.cfg)
	}
	if role != "" && !preview {
		ag.PlanMode = (m.uiMode() == uiModePlan)
		ag.ReviewMode = false
	}
	m.agent, m.modelName, m.provName = ag, modelName, providerName
	ag.Effort = m.maxEffort()
	m.modelSlotW = m.statusModelSlotWidth()
	m.configureOutputAgent(ag)
	m.applyCompactModel()
	m.wireTasks()
	if !preview {
		if m.store != nil && m.sessionID != "" {
			_ = m.store.SetEffort(m.sessionID, ag.Effort)
		}
		m.syncWorkerConfiguration(true)
	}
	return nil
}

func (m *model) switchModel(name, prov string) {
	if m.workerClient != nil && m.workerLiveWork {
		m.append(dimStyle.Render("(worker is busy — change the model after this work finishes)"))
		return
	}
	if m.workerOnly {
		if err := m.activateRoute(name, prov, config.RoleDefault); err != nil {
			m.append(errStyle.Render(err.Error()))
			return
		}
		m.cfg.DefaultModel, m.cfg.DefaultProvider = m.modelName, m.provName
		_ = m.saveConfig()
		return
	}
	if err := m.rebuildAgent(name, prov, "", false); err != nil {
		m.append(errStyle.Render(err.Error()))
		return
	}
	m.cfg.DefaultModel, m.cfg.DefaultProvider = m.modelName, m.provName
	_ = m.saveConfig()
}

// openPicker starts the /resume picker on recent sessions.
func (m *model) openPicker() {
	if m.store == nil {
		m.append(errStyle.Render("session store unavailable"))
		return
	}
	metas, err := m.store.Recent(50)
	if err != nil {
		m.append(errStyle.Render(err.Error()))
		return
	}
	if len(metas) == 0 {
		m.append(dimStyle.Render("(no previous sessions)"))
		return
	}
	m.picker = &picker{metas: metas, previews: map[string][2]string{}}
	m.picker.loadPreview(m.store)
}

// openMenu starts tab completion: every candidate for the token's prefix is
// frozen into a cycle set and the first is previewed, so tab always inserts
// text — a single match completes outright, several cycle with preview.
func (m *model) openMenu() {
	head, cands := completions(m.input.Value(), m.modelCands(), m.providerCands(), m.authProviderCands(), m.skillCands(), effortCandsFor(m.effortsFor()))
	if len(cands) == 0 {
		return
	}
	m.menu = &menu{head: head, cands: cands}
	m.menuCycle(0)
}

// refreshMenu keeps a live dropdown open while typing a slash command, an
// @file mention, or a $skill, re-filtering on every keystroke; otherwise
// closes it. A frozen menu (tab cycling) keeps its candidate snapshot — the
// cycle range only changes when the completed text itself is edited.
func (m *model) refreshMenu() {
	if m.menu != nil && m.menu.cyc && m.menu.frozen != nil && m.menu.idx < len(m.menu.frozen) &&
		m.input.Value() == m.menu.head+m.menu.frozen[m.menu.idx].Text {
		return // previewing a frozen candidate; nothing to re-filter
	}
	val := m.input.Value()
	token := val[strings.LastIndexByte(val, ' ')+1:]
	if strings.HasPrefix(val, "/") || strings.HasPrefix(token, "@") || strings.HasPrefix(token, "$") {
		head, cands := completions(val, m.modelCands(), m.providerCands(), m.authProviderCands(), m.skillCands(), effortCandsFor(m.effortsFor()))
		if len(cands) > 0 {
			idx := 0
			if m.menu != nil && m.menu.idx < len(cands) && m.menu.frozen == nil {
				idx = m.menu.idx
			}
			m.menu = &menu{head: head, cands: cands, idx: idx}
			return
		}
	}
	m.menu = nil
}

// previewCand inserts the highlighted candidate as a tab-cycle preview (no
// trailing space). The frozen menu survives the input edit via refreshMenu.
func (m *model) previewCand() {
	m.input.SetValue(m.menu.head + m.menu.cands[m.menu.idx].Text)
	m.refreshMenu()
}

// acceptPreview commits a tab-cycle preview on enter: appends the trailing
// space (or stays open inside a directory) exactly like accept.
func (m *model) acceptPreview() {
	m.menu.cyc, m.menu.frozen = false, nil // committing: live filtering again
	v := m.input.Value()
	if !strings.HasSuffix(v, "/") {
		m.input.SetValue(v + " ")
	}
	m.refreshMenu()
}

// menuCycle moves the tab-cycle selection by delta, previewing the new
// candidate from the pre-cycle input. The cycle set is frozen on the first
// tab so cycling covers every candidate for the token's common prefix
// ("/m" tabs through all matching commands even when a command's own
// argument completion would use a narrower prefix).
func (m *model) menuCycle(delta int) {
	mu := m.menu
	if mu.frozen == nil {
		mu.cyc, mu.frozen = true, mu.cands
		mu.base = mu.head + mu.frozen[mu.idx].Text // esc reverts to here
	}
	if mu.cycled {
		mu.idx = (mu.idx + delta + len(mu.cands)) % len(mu.cands)
	} else {
		mu.cycled = true // first tab previews the current best match
	}
	m.previewCand()
}

// accept applies the selected candidate. Returns false if the input already
// equals it (nothing to complete).
func (m *model) accept() bool {
	c := m.menu.cands[m.menu.idx]
	v := m.menu.head + c.Text
	if !strings.HasSuffix(c.Text, "/") { // directories stay open for deeper completion
		v += " "
	}
	if strings.TrimRight(m.input.Value(), " ") == strings.TrimRight(v, " ") {
		m.menu = nil
		return false
	}
	m.input.SetValue(v)
	m.menu = nil
	m.refreshMenu()
	return true
}

func (m *model) modelCands() []cand {
	items := m.availableModelItems()
	available := make(map[string]bool, len(items))
	for _, it := range items {
		available[it.model] = true
	}
	out := make([]cand, 0, len(available))
	for name, mdl := range m.cfg.Models {
		if !available[name] {
			continue
		}
		out = append(out, cand{name, "via " + strings.Join(mdl.Providers, ", ")})
	}
	// catalog-advertised models are usable without a config entry (catalog
	// fallback in Resolve); offer them in completion too
	for _, it := range items {
		if it.fromCatalog {
			out = append(out, cand{it.model, "via " + it.provider + " (catalog)"})
		}
	}
	return out
}

func (m *model) providerCands() []cand {
	out := make([]cand, 0, len(m.cfg.Providers))
	for name, p := range m.cfg.Providers {
		out = append(out, cand{name, p.BaseURL})
	}
	return out
}

func (m *model) authProviderCands() []cand {
	ids := m.profiles.IDs()
	out := make([]cand, 0, len(ids))
	for _, id := range ids {
		profile, ok := m.profiles.Lookup(id)
		if !ok {
			continue
		}
		status := "not configured"
		if profile.Auth.Kind == models.AuthNone {
			status = "no key required"
		} else if m.authConfigured(id) {
			status = "configured"
		}
		out = append(out, cand{id, profile.DisplayName + " — " + status})
	}
	return out
}

func (m *model) scanSkills() []skills.Skill {
	if m.skillScan != nil {
		return m.skillScan()
	}
	return skills.Scan(skills.DefaultDirs()...)
}

// skillCands uses the startup snapshot while the completion menu is open.
// prepareTurn is the explicit refresh point for newly added skills.
func (m *model) skillCands() []cand {
	if m.skillsCache == nil {
		m.skillsCache = m.scanSkills()
		if m.skillsCache == nil {
			m.skillsCache = []skills.Skill{}
		}
		m.skillsLoaded = len(m.skillsCache)
	}
	out := make([]cand, 0, len(m.skillsCache))
	for _, s := range m.skillsCache {
		out = append(out, cand{"$" + s.Name, truncLine(s.Description, 80)})
	}
	return out
}

// prepareTurn refreshes the system prompt's skills block (so new skills load
// without a restart) and MCP server instructions (so late-arriving servers
// teach the model how to use their tools), then expands $skill / @file
// tokens in the input. It returns the expanded text plus any image parts
// extracted from @image tags.
func (m *model) prepareTurn(text string) (string, []models.ContentPart) {
	sk := m.scanSkills()
	if sk == nil {
		sk = []skills.Skill{}
	}
	m.skillsCache = sk
	m.skillsLoaded = len(sk)
	sys := m.sysPrompt
	if !m.workerOnly {
		var additions []string
		additions = append(additions, skills.PromptBlock(sk))
		if m.mcpMgr != nil {
			additions = append(additions, m.mcpMgr.InstructionsBlock())
		}
		additions = append(additions, memory.PromptBlock(memory.Installation(), memory.Session(m.sessionID)))
		sys = agent.CompileSystemPrompt(sys, additions...)
	}
	if m.agent != nil && !m.workerOnly {
		m.agent.SetSystemPrompt(sys)
	}
	expanded := expandMentions(expandSkills(text, sk))
	if !m.supportsVision() {
		// text-only model: leave @image tags as pointer notes (from
		// expandMentions) instead of inlining base64 the model would reject.
		return expanded, nil
	}
	parts, withNote := imageParts(text)
	return expanded + withNote, parts
}

// supportsVision reports whether the current model accepts image inputs, so
// @image tags are inlined only for models that can use them. A provider-
// advertised input_modalities entry (from /models, cached in the catalog)
// wins; otherwise the config's per-model vision flag decides (default false).
func (m *model) supportsVision() bool {
	modelID := m.modelName
	if m.agent != nil {
		modelID = m.agent.Model
	}
	if cat, ok := m.catalogs[m.provName]; ok {
		if vision, found := cat.SupportsVision(modelID); found {
			return vision
		}
	}
	if m.cfg != nil {
		if mc, ok := m.cfg.Models[m.modelName]; ok {
			return mc.Vision
		}
	}
	return false
}

// submit sends a message the human typed; it counts for input-history recall.
// drainQueueHead pops the oldest queued message and submits it as the next
// turn — the exact submission path of a typed message (system-prompt rebuild,
// history, transcript echo). Used by turnDoneMsg's queue drain and by the
// idle empty-enter recovery for a stranded queue. Callers handle `!` shell
// escapes before calling (they execute locally, not as a turn).
func (m *model) drainQueueHead() (tea.Model, tea.Cmd) {
	next := m.queue[0]
	m.queue = m.queue[1:]
	m.queueSel = -1
	m.hist = append(m.hist, next)
	m.histIdx = len(m.hist)
	return m.submit(next)
}

func (m *model) submit(text string) (tea.Model, tea.Cmd) {
	return m.submitTurn(text, true)
}

// submitGoal sends a ghg-injected goal-continuation; not a typed submission,
// so it must not appear in up-arrow input history.
func (m *model) submitGoal(text string) (tea.Model, tea.Cmd) {
	return m.submitTurn(text, false)
}

func (m *model) submitTurn(text string, authored bool) (tea.Model, tea.Cmd) {
	if !m.requireAgent() {
		return m, nil
	}
	// Establish the durable boundary before the agent can compact during this
	// turn, while still leaving truly idle sessions uncreated.
	if m.store != nil && m.sessionID == "" && !m.ensureSession() {
		return m, nil
	}
	if m.workerOnly {
		if !m.ensureWorker() {
			m.workerStartFailed = true
			m.busy = false
			m.turnStart = time.Time{}
			detail := m.workerStartError
			if detail == "" {
				detail = "worker could not be started"
			}
			m.append(errStyle.Render("worker unavailable: " + detail))
			return m, nil
		}
	} else if m.workerClient == nil && m.workerProcess == nil && m.prog != nil {
		m.ensureWorker()
	}
	m.busy = true
	m.turnStart = m.nowFn()
	prepared, parts := m.prepareTurn(text)
	userMsgIdx := 0
	userMsgIdx = len(m.messagesSnapshot())
	// Rewind bookkeeping: if a redo stack exists, this resubmission replaces a
	// clipped message. Record the replaced text on the new message (internal,
	// stripped before the provider) before discardFuture drops the stack.
	rewoundFrom := ""
	if authored && len(m.future) > 0 {
		for _, fm := range m.future {
			if fm.Role == "user" && fm.Authored {
				rewoundFrom = oneLine(fm.Content)
				break
			}
		}
	}
	m.discardFuture() // new activity while rewound kills the redo stack
	// settled subagents already reported into the transcript; clear them off
	// the dock strip so a new turn starts with only what's still running
	if m.agent != nil {
		m.agent.Tasks().ClearSettled()
	}
	goalCtx, hasGoal := m.goalRecordForSession()
	hasGoal = hasGoal && goalCtx.Status == agent.GoalStatusActive
	if m.workerClient != nil {
		preSnap := ""
		if !m.workerOnly {
			preSnap = session.SnapshotWorkspace(cwd())
		}
		return m.submitWorkerTurn(text, authored, prepared, parts, userMsgIdx, preSnap, func() *agent.GoalRecord {
			if !hasGoal {
				return nil
			}
			copy := goalCtx
			return &copy
		}())
	}
	if m.workerOnly {
		m.busy = false
		m.turnStart = time.Time{}
		m.append(errStyle.Render("worker unavailable: no connected worker"))
		return m, nil
	}
	ctx, cancel := context.WithCancel(context.Background())
	m.cancel = cancel
	p := m.prog
	// send is nil-safe: headless tests drive Update directly, so turn
	// callbacks drop their messages instead of panicking on a nil program
	send := func(msg tea.Msg) {
		if p != nil {
			p.Send(msg)
		}
	}

	// Coalesce streaming deltas (~25fps) so each SSE chunk doesn't cost a
	// full Update/View cycle. Reasoning tokens get their own buffer so
	// thinking and answer text never interleave within one update; both drain
	// on the same timer.
	var mu sync.Mutex
	var pend, thinkPend, pendPlan string
	var goalUpdates []agent.GoalUpdate
	var goalUsage models.Usage
	var timer *time.Timer
	flush := func() {
		mu.Lock()
		if timer != nil {
			timer.Stop()
			timer = nil
		}
		text, think, plan := pend, thinkPend, pendPlan
		pend, thinkPend, pendPlan = "", "", ""
		mu.Unlock()
		if think != "" {
			send(thinkMsg(think))
		}
		if text != "" {
			send(textMsg(text))
		}
		if plan != "" {
			send(planDeltaMsg(plan))
		}
	}
	schedule := func() {
		if timer == nil {
			timer = time.AfterFunc(40*time.Millisecond, flush)
		}
	}
	onText := func(d string) {
		mu.Lock()
		pend += d
		schedule()
		mu.Unlock()
	}
	onThink := func(d string) {
		mu.Lock()
		thinkPend += d
		schedule()
		mu.Unlock()
	}
	onPlanDelta := func(d string) {
		mu.Lock()
		pendPlan += d
		schedule()
		mu.Unlock()
	}

	go func() {
		events := agent.Events{
			OnText:      onText,
			OnThink:     onThink,
			OnPlanDelta: onPlanDelta,
			OnToolStart: func(id, n, a string) {
				flush()
				send(toolStartMsg{id, n, a})
			},
			OnToolEnd: func(id, n, r string) { send(toolEndMsg{id, n, r}) },
			OnSteer: func(s string) {
				flush()
				send(steeredMsg(s))
			},
			OnCompactionReady: func(raw []models.Message, summary string, cutoff int) error {
				return m.store.PersistCompaction(m.sessionID, m.saved, raw, m.modelName, m.provName, summary, cutoff)
			},
			OnCompacted: func(sum string, _ int) {
				send(compactMsg{summary: sum})
			},
			OnUsage: func(u models.Usage) {
				mu.Lock()
				goalUsage.Add(u)
				mu.Unlock()
				send(usageMsg(u))
			},
			OnGoalUpdate: func(update agent.GoalUpdate) {
				mu.Lock()
				goalUpdates = append(goalUpdates, update)
				mu.Unlock()
				send(goalUpdateMsg{update: update})
			},
			OnRetry: func(ev models.RetryEvent) {
				flush()
				send(noticeMsg(fmt.Sprintf("⚠ request failed (%s) — retrying in %s (attempt %d/%d)",
					ev.Err, ev.Delay.Round(time.Millisecond), ev.Attempt+1, ev.Max)))
			},
		}
		preSnap := session.SnapshotWorkspace(cwd())
		var final string
		var err error
		switch {
		case len(parts) > 0:
			if hasGoal {
				final, err = m.agent.TurnWithImagesAndGoal(ctx, prepared, parts, goalCtx, events)
			} else {
				final, err = m.agent.TurnWithImages(ctx, prepared, parts, events)
			}
		case authored:
			if hasGoal {
				final, err = m.agent.TurnAuthoredWithGoal(ctx, prepared, goalCtx, events)
			} else {
				final, err = m.agent.TurnAuthored(ctx, prepared, events)
			}
		default:
			if hasGoal {
				final, err = m.agent.TurnWithGoal(ctx, prepared, goalCtx, events)
			} else {
				final, err = m.agent.Turn(ctx, prepared, events)
			}
		}
		flush()
		mu.Lock()
		updates := slices.Clone(goalUpdates)
		usage := goalUsage
		mu.Unlock()
		// stamp rewind provenance on the submitted message (appended by turn)
		if rewoundFrom != "" && userMsgIdx < len(m.agent.Messages) {
			m.agent.Messages[userMsgIdx].RewoundFrom = rewoundFrom
		}
		send(turnDoneMsg{final: final, err: err, at: userMsgIdx, snap: preSnap, clean: session.WorkspaceClean(cwd()), goalUpdates: updates, goalUsage: usage})
	}()
	m.append(youStyle.Render("❯ ") + linkifyFilePaths(text, realFileExists))
	if authored {
		// map the message index to its block for rewind live-scroll
		for len(m.msgBlock) <= userMsgIdx {
			m.msgBlock = append(m.msgBlock, -1)
		}
		m.msgBlock[userMsgIdx] = len(m.blocks) - 1
	}
	return m, m.spin.Tick
}

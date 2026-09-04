package tui

import (
	"context"
	"os"
	"slices"
	"strings"
	"time"

	"github.com/charmbracelet/bubbles/spinner"
	"github.com/charmbracelet/bubbles/textarea"
	"github.com/charmbracelet/bubbles/viewport"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/sacca97/ghg/internal/agent"
	"github.com/sacca97/ghg/internal/config"
	"github.com/sacca97/ghg/internal/models"
	"github.com/sacca97/ghg/internal/session"
	"github.com/sacca97/ghg/internal/skills"
	workerwire "github.com/sacca97/ghg/internal/worker"
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
	cfg             *config.Config
	modelName       string
	provName        string
	modelID         string
	protocol        string
	role            string
	effort          string
	contextLimit    int
	usage           models.Usage
	messages        []models.Message
	sysPrompt       string
	input           textarea.Model
	spin            spinner.Model
	vp              viewport.Model
	blocks          []block // finalized transcript (raw; rendered at the current width)
	transcriptDirty bool
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
	catalogs      map[string]config.Catalog // provider model lists (capabilities)
	profiles      models.Profiles           // embedded/user/trusted-project provider metadata
	// skillScan is the skills discovery seam (skills.Scan over DefaultDirs in
	// the real model): a field so the context doctor can be tested against
	// temp-dir skills instead of whatever the test machine happens to have.
	skillScan    func() []skills.Skill
	skillsCache  []skills.Skill
	skillsLoaded int

	iactive *interactive // in-flight interactive command; nil when idle

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
	workerMCPStatuses    []workerwire.MCPStatus
	cautious             bool
}

func (m *model) messagesSnapshot() []models.Message {
	return slices.Clone(m.messages)
}

func (m *model) messageCount() int {
	return len(m.messages)
}

func (m *model) setMessages(messages []models.Message) {
	m.messages = slices.Clone(messages)
}

func (m *model) currentModelID() string {
	if m.modelID != "" {
		return m.modelID
	}
	return m.modelName
}

func (m *model) currentRole() string {
	if m.role != "" {
		return m.role
	}
	return m.modeRole()
}

func (m *model) currentUsage() models.Usage {
	return m.usage
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

func (m *model) defaultCompactModelName() string {
	if m.cfg != nil && len(m.cfg.Roles) > 0 {
		if target, err := m.cfg.ResolveRole(config.RoleTiny); err == nil && target.Model != "" {
			return target.Model
		}
	}
	return config.DefaultCompactModel
}

func (m *model) switchModel(name, prov string) {
	if m.workerClient != nil && m.workerLiveWork {
		m.append(dimStyle.Render("(worker is busy — change the model after this work finishes)"))
		return
	}
	if err := m.activateRoute(name, prov, config.RoleDefault); err != nil {
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
	head, cands := m.completionCandidates(m.input.Value())
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
		head, cands := m.completionCandidates(val)
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

func (m *model) completionCandidates(input string) (string, []cand) {
	return completions(input, m.modelCands(), m.providerCands(), m.authProviderCands(), m.skillCands(), effortCandsFor(m.effortsFor()))
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
	modelID := m.currentModelID()
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

func (m *model) submitAsk(text string) (tea.Model, tea.Cmd) {
	return m.submitTurnMode(text, true, true)
}

// submitGoal sends a ghg-injected goal-continuation; not a typed submission,
// so it must not appear in up-arrow input history.
func (m *model) submitGoal(text string) (tea.Model, tea.Cmd) {
	return m.submitTurn(text, false)
}

func (m *model) submitTurn(text string, authored bool) (tea.Model, tea.Cmd) {
	return m.submitTurnMode(text, authored, false)
}

func (m *model) submitTurnMode(text string, authored, ask bool) (tea.Model, tea.Cmd) {
	if !m.requireAgent() {
		return m, nil
	}
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
	m.busy = true
	m.turnStart = m.nowFn()
	prepared, parts := m.prepareTurn(text)
	userMsgIdx := 0
	userMsgIdx = m.messageCount()
	m.discardFuture() // new activity while rewound kills the redo stack
	goalCtx, hasGoal := m.goalRecordForSession()
	if ask {
		goalCtx, hasGoal = agent.GoalRecord{}, false
	}
	hasGoal = hasGoal && goalCtx.Status == agent.GoalStatusActive
	return m.submitWorkerTurn(text, authored, prepared, parts, userMsgIdx, "", func() *agent.GoalRecord {
		if !hasGoal {
			return nil
		}
		copy := goalCtx
		return &copy
	}(), ask)
}

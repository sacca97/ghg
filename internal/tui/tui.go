// Package tui is ghg's interactive Bubble Tea session.
package tui

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/charmbracelet/bubbles/spinner"
	"github.com/charmbracelet/bubbles/textarea"
	"github.com/charmbracelet/bubbles/viewport"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"

	"github.com/sacca97/ghg/internal/agent"
	"github.com/sacca97/ghg/internal/artifact"
	"github.com/sacca97/ghg/internal/config"
	goalstate "github.com/sacca97/ghg/internal/goal"
	"github.com/sacca97/ghg/internal/llm"
	"github.com/sacca97/ghg/internal/lsp"
	"github.com/sacca97/ghg/internal/mcp"
	"github.com/sacca97/ghg/internal/memory"
	"github.com/sacca97/ghg/internal/provider"
	"github.com/sacca97/ghg/internal/session"
	"github.com/sacca97/ghg/internal/skills"
	"github.com/sacca97/ghg/internal/tools"
	"github.com/sacca97/ghg/internal/tools/bashrun"
	workerwire "github.com/sacca97/ghg/internal/worker"

	"github.com/muesli/termenv"
	"golang.org/x/term"
)

// UI styles use AdaptiveColor so they stay legible on both dark and light
// terminal backgrounds (detected at startup by detectColorScheme).
var (
	youStyle  = lipgloss.NewStyle().Foreground(lipgloss.AdaptiveColor{Light: "21", Dark: "12"}).Bold(true) // blue
	botStyle  = lipgloss.NewStyle().Foreground(lipgloss.AdaptiveColor{Light: "90", Dark: "13"}).Bold(true) // purple/magenta
	toolStyle = lipgloss.NewStyle().Foreground(lipgloss.AdaptiveColor{Light: "136", Dark: "11"})           // amber
	dimStyle  = lipgloss.NewStyle().Foreground(lipgloss.AdaptiveColor{Light: "240", Dark: "245"})          // mid gray
	errStyle  = lipgloss.NewStyle().Foreground(lipgloss.AdaptiveColor{Light: "124", Dark: "9"})            // red
	// thinkingStyle renders reasoning tokens: dim and italic so they're
	// visually distinct from the answer.
	thinkingStyle = lipgloss.NewStyle().Foreground(lipgloss.AdaptiveColor{Light: "240", Dark: "245"}).Italic(true)
)

// messages sent from the agent goroutine
type textMsg string
type toolStartMsg struct{ id, name, args string }
type toolEndMsg struct{ id, name, result string }
type steeredMsg string

// goalFromContextMsg carries the model-formulated goal back from the
// /goal-from-context goroutine to the Update loop.
type goalFromContextMsg struct {
	goal string
	err  error
}

type goalUpdateMsg struct{ update agent.GoalUpdate }

type compactMsg struct {
	took, kept int // messages removed / kept after compaction
	summary    string
	err        error
}
type turnDoneMsg struct {
	final       string
	err         error
	at          int    // conversation index the turn started at (snapshot key)
	snap        string // pre-turn workspace snapshot commit ("" = not a git repo)
	clean       bool   // the turn left the tree clean — snap is worthless, drop it
	goalUpdates []agent.GoalUpdate
	goalUsage   llm.Usage
}
type catalogsMsg map[string]config.Catalog // background /models fetch result
type noticeMsg string                      // dim one-liner appended to the transcript
type usageMsg llm.Usage                    // one request's token usage
type quitArmMsg struct{}                   // the idle ctrl+c arm window expired
type taskUpdateMsg struct{}                // a background subagent started/settled — redraw
type mcpStatusMsg struct{}                 // an MCP server changed state — redraw
type thinkMsg string                       // streamed reasoning tokens
type planDeltaMsg string                   // streamed proposed plan markdown tokens
type imageMsg struct {                     // ctrl+v clipboard image result
	path string // clipboard image saved to disk
	err  error
}

type workerFrameMsg struct{ frame workerwire.Frame }
type workerErrorMsg struct {
	err     error
	process *workerwire.Process
}
type workerPermissionMsg struct{ approval workerApproval }
type workerCompactDoneMsg struct {
	err   error
	usage llm.Usage
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
	cfg       *config.Config
	agent     *agent.Agent
	runtime   *tools.ToolRuntime
	modelName string
	provName  string
	sysPrompt string
	// cfgExtra pins scalar settings this session explicitly changed (theme,
	// effort, …): the config watcher applies file values only for keys not
	// pinned here, so a local pick this session survives another session's
	// unrelated save while still syncing changes made elsewhere.
	cfgExtra map[string]string
	cfgMod   time.Time // last observed config.json mod time (watcher baseline)

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
	mpicker      *modelPicker
	settings     *settings // ctrl+p settings
	cancel       context.CancelFunc
	prog         *tea.Program

	store         *session.Store
	sessionID     string
	saved         int            // messages already persisted (index into agent.Messages)
	snapshots     map[int]string // workspace snapshot ref per turn-start index (mirrors the snapshots table)
	artifactStore *artifact.Store

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
	goalRecord     *goalstate.Record
	titled         bool   // an auto-title has been attempted for this session
	proposedPlanMD string // latest /plan proposal (Markdown), waiting for /execute
	planCurrent    string // partial line of streamed plan markdown
	mode           string // user-visible operating mode: plan or execute
	wheel          wheelState
	selection      *selectionState

	mouseOn       bool   // startup mouse setting; nil config means enabled
	themeHow      string // how auto theme detection resolved (env var, OSC query, …) — captured at startup/theme change for /report; never re-queried
	compactModel  string // config model name for compaction summaries; "" = the built-in default
	compactProv   string
	statusModelX  int                       // screen column where the bottom model control starts
	statusModelW  int                       // visible width of the bottom model control
	statusEffortX int                       // screen column where the bottom effort control starts
	statusEffortW int                       // visible width of the bottom effort control
	statusModeX   int                       // screen column where the bottom mode control starts
	statusModeW   int                       // visible width of the bottom mode control
	catalogs      map[string]config.Catalog // provider model lists (capabilities)
	profiles      provider.Profiles         // embedded/user/trusted-project provider metadata
	definitions   map[string]agent.Definition
	mcpMgr        *mcp.Manager    // MCP server connections; nil when none configured
	mcpSeen       map[string]bool // servers whose first settle was announced
	lspMgr        *lsp.Manager    // LSP diagnostics source for write/edit tool output
	// skillScan is the skills discovery seam (skills.Scan over DefaultDirs in
	// the real model): a field so the context doctor can be tested against
	// temp-dir skills instead of whatever the test machine happens to have.
	skillScan    func() []skills.Skill
	skillsCache  []skills.Skill
	skillsLoaded int

	irunner *interactiveRunner // installed on tools.InteractiveBash at startup
	iactive *interactive       // in-flight interactive command; nil when idle

	perms      permRules   // saved allow-always rules
	permDialog *permDialog // open permission modal; the turn is paused on it

	tasksFocus bool      // the tasks dock owns ↑/↓/enter/esc instead of the input
	taskSel    int       // selected row in the dock (index into newest-first tasks)
	dockSkip   int       // non-task rows at the dock's top (focused hint) — click math skips them
	taskVP     *taskView // open per-task detail view; nil when on the main thread
	dockRows   int       // rendered dock height; layout() maintains it for click math

	rew    *rewindState  // open rewind picker (double-esc while idle)
	esc1   bool          // first idle esc pressed; second opens the rewind picker
	escClr bool          // first esc pressed with a draft; second clears it to history
	future []llm.Message // clipped tail kept for forward travel after a rewind

	namePrompt *namePrompt // inline text prompt (fork naming, /rename)

	workerClient        *workerwire.Client
	workerProcess       *workerwire.Process
	workerRuntime       workerwire.Runtime
	workerLastSeq       uint64
	workerDetached      bool
	workerState         workerwire.State
	workerLiveWork      bool
	workerTasks         map[string]workerTaskState
	detachRequestID     string
	workerStartFailed   bool
	workerContextTokens int
	cautious            bool
}

// picker is the /resume session picker. metas is newest-first; the list is
// rendered oldest-at-top so newest sits at the bottom.
type picker struct {
	metas    []session.Meta
	idx      int                  // selected index into metas (0 = newest)
	previews map[string][2]string // id -> last user, last assistant
	pendingD bool                 // first 'd' pressed; second deletes the selected session
}

// Run starts the interactive session. It returns the id of the session that
// was active on exit ("" if nothing was said).
func Run(cfg *config.Config, modelName, provName, sysPrompt, resumeID string, cautious bool) (string, error) {
	return runTUI(cfg, modelName, provName, sysPrompt, resumeID, cautious, true)
}

// RunAttached opens the normal TUI on an already-running session worker. It
// does not launch a second worker and is intentionally the only caller of the
// attach-only path.
func RunAttached(cfg *config.Config, modelName, provName, sysPrompt, sessionID string, cautious bool) (string, error) {
	return runTUI(cfg, modelName, provName, sysPrompt, sessionID, cautious, false)
}

func runTUI(cfg *config.Config, modelName, provName, sysPrompt, resumeID string, cautious, launchWorker bool) (string, error) {
	// Trust gate first: before ghg reads a single file, ask whether this
	// folder's contents may steer the model. Persisted per absolute path in
	// ~/.ghg/trusted.json (claude-code's per-project trust dialog).
	if ok, err := checkTrust(); err != nil {
		return "", err
	} else if !ok {
		return "", fmt.Errorf("folder not trusted")
	}
	// main builds the prompt before this gate on a first run. Add the project
	// instructions now that the user has explicitly trusted the folder; the
	// marker keeps already-trusted launches from appending the block twice.
	if wd, wdErr := os.Getwd(); wdErr == nil && !strings.Contains(sysPrompt, "<project_instructions>") {
		if project := config.ProjectInstructions(wd, true); project != "" {
			sysPrompt += "\n\n" + project
		}
	}

	profiles, err := provider.Load(provider.LoadOptions{ProjectTrusted: true})
	if err != nil {
		return "", err
	}
	definitions, err := agent.LoadAgentDefinitions(agent.DefinitionLoadOptions{ProjectTrusted: true})
	if err != nil {
		return "", err
	}
	// The interactive TUI can start in a degraded state so the user can use
	// /auth to configure the first provider. The strict builder remains used by
	// all headless/explicit agent paths, so missing credentials still fail fast
	// there.
	var ag *agent.Agent
	var mn, pn string
	if modelName == "" && provName == "" {
		// A fresh interactive session is an acting session. The fast role may
		// be absent; ResolveRole then falls back through default to the legacy
		// defaultModel/defaultProvider route.
		ag, mn, pn, err = buildAgentForModeWithProfilesOptional(cfg, config.ModeActing, sysPrompt, profiles)
	} else {
		// An explicit -m/-p selection remains an explicit route and wins over
		// role defaults.
		ag, mn, pn, err = buildAgentWithProfilesOptional(cfg, modelName, provName, sysPrompt, profiles)
	}
	if err != nil {
		return "", err
	}
	runtime, runtimeCleanup, err := tools.NewConfiguredRuntime(".", cfg.Execution, false, cfg.PostEdit)
	if err != nil {
		return "", err
	}
	defer runtimeCleanup()
	if ag != nil {
		ag.Runtime = runtime
		if runtime.ApprovalMode == tools.ApprovalAutoReview {
			runtime.Reviewer = ag.ApproveForMe
		}
	}

	ti := newInput()

	// default on: "" (config never set / pre-feature file) means medium, not
	// off; an explicit "off" in the file is honored
	if ag != nil {
		ag.Effort = cfg.DefaultEffort
		if ag.Effort == "" {
			ag.Effort = "medium"
		}
	}
	// Mouse capture is on by default. Explicit config wins and remains a
	// startup-only escape hatch for terminals where application mouse capture
	// is undesirable.
	mouseOn := true
	if cfg.Mouse != nil {
		mouseOn = *cfg.Mouse
	}
	showThinking := true // default on; "thinking": false in config opts out
	if cfg.Thinking != nil {
		showThinking = *cfg.Thinking
	}
	m := &model{
		cfg: cfg, agent: ag, runtime: runtime, modelName: mn, provName: pn, sysPrompt: sysPrompt,
		input: ti, spin: spinner.New(spinner.WithSpinner(spinner.Dot)), follow: true, saved: 1,
		catalogs: config.LoadCatalogs(), profiles: profiles, definitions: definitions, mouseOn: mouseOn, now: time.Now, showThinking: showThinking,
		compactModel: cfg.CompactModel, compactProv: cfg.CompactProvider,
		mode:      uiModeExecute,
		cautious:  cautious,
		skillScan: func() []skills.Skill { return skills.Scan(skills.DefaultDirs()...) },
	}
	m.perms = loadPermRules()
	runtime.HumanGate = m.askPermission
	runtime.OnAudit = func(audit tools.ExecutionAudit) {
		message := audit.Disposition + " " + audit.Request.Fingerprint
		if audit.Error != "" {
			message += ": " + audit.Error
		}
		config.LogEvent("execution.policy", message)
	}
	runtime.OnReviewerCall = func(call tools.ReviewerCall) {
		message := fmt.Sprintf("%s/%s %s %dms", call.Provider, call.Model, call.Purpose, call.LatencyMS)
		if call.Error != "" {
			message += ": " + call.Error
		}
		config.LogEvent("execution.reviewer", message)
	}
	defer func() {
		// These are process-local compatibility hooks. Runtime consumers use the
		// per-agent context; clear the globals so a later terminal/session cannot
		// inherit a closed TUI's callbacks.
		tools.InteractiveBash = nil
		tools.Gate = nil
	}()
	m.initArtifacts()
	m.applyCompactModel()
	if m.agent != nil {
		m.agent.CompactThreshold = compactThresholdFor(cfg)
	}
	m.wireTasks() // redraw the UI when background subagents start/settle
	// LSP shares the same runtime and manager across the TUI, Plan mode, and
	// delegated agents. Servers still spawn lazily on covered file access.
	m.lspMgr = lsp.NewManager(lsp.FromConfigMap(cfg.LSPServers))
	m.lspMgr.SetRuntime(runtime)

	// MCP: merge ghg's own config with imported claude (.mcp.json) and codex
	// (~/.codex/config.toml) servers — gated by the mcpImport policy, whose
	// blocked entries stay visible in /mcp — then kick concurrent connects in
	// the background. Tool calls block on that server's first settle only, so a
	// slow/hung server never delays startup. Discovery problems (a broken
	// .mcp.json) land as a transcript note, not a startup failure.
	if wd, wdErr := os.Getwd(); wdErr == nil {
		disc := mcp.LoadMergedFiltered(wd, mcp.FromConfigMap(cfg.MCPServers), mcp.ImportPolicyFrom(cfg.MCPImport))
		merged, mcpErrs := disc.Merged, disc.Errs
		if len(merged) > 0 || len(disc.Blocked) > 0 || len(mcpErrs) > 0 {
			m.mcpMgr = mcp.NewManager(merged)
			m.mcpMgr.SetRuntime(runtime)
			m.mcpMgr.SetBlocked(disc.Blocked)
			// MCP connects settle in the background; push each new tool set
			// into the CURRENT agent (mutex-guarded on the agent side) so
			// servers that connect after turn 1 show up without a restart.
			// The closure reads m.agent at call time: resume/model-switch
			// replace the agent, and wireTasks re-points the manager at it.
			m.mcpMgr.SetOnChange(func() {
				if m.agent != nil {
					m.agent.SetMCPTools(m.mcpMgr.Tools())
				}
				if m.prog != nil { // nil in headless tests
					m.prog.Send(mcpStatusMsg{})
				}
			})
			m.mcpMgr.Start(context.Background())
			if m.agent != nil {
				ag.SetMCPTools(m.mcpMgr.Tools())
			}
			for src, derr := range mcpErrs {
				m.append(errStyle.Render(fmt.Sprintf("mcp: %s: %s", src, derr)))
			}
		}
	}
	// Permission prompts remain opt-in for routine commands (--cautious), while
	// the runtime always owns exceptional capability escalation.
	if cautious {
		m.installPermGate()
	}
	if dir, derr := config.Dir(); derr == nil {
		if st, serr := session.Open(dir + "/sessions.db"); serr == nil {
			m.store = st
			m.configureArtifactAgent(m.agent)
			m.wireTasks()
			defer func() { _ = st.Close() }()
			// Seed input recall with user messages from ALL sessions (every
			// folder), so ↑ cycles global history, not just this session's.
			// UserHistory is newest-first; hist is oldest-first (up-arrow walks
			// back from the end), so reverse it into place.
			if hist, herr := st.UserHistory(500); herr == nil && len(hist) > 0 {
				for i := len(hist) - 1; i >= 0; i-- {
					m.hist = append(m.hist, hist[i])
				}
				m.histIdx = len(m.hist)
			}
		} else {
			config.LogEvent("session.open", "FAILED: "+serr.Error())
			m.append(errStyle.Render("sessions disabled: " + serr.Error()))
		}
	}
	if resumeID != "" {
		if m.agent == nil {
			return "", fmt.Errorf("cannot resume without a configured provider; run ghg without --resume and use /auth first")
		}
		if m.store == nil {
			return "", fmt.Errorf("cannot resume: session store unavailable")
		}
		if err := m.resume(resumeID); err != nil {
			return "", err
		}
	}
	m.startupReport()

	// Use the alternate screen for the interactive surface. Bubble Tea restores
	// the caller's original screen when p.Run returns, so the transcript and
	// status chrome cannot remain as an inline frame after exit. The viewport
	// still owns chat scrolling while the TUI is active.
	//
	opts := []tea.ProgramOption{tea.WithoutSignalHandler(), tea.WithAltScreen()}
	if m.mouseOn {
		opts = append(opts, tea.WithMouseCellMotion())
	}
	// pick the glamour style that matches the pick/detection resolution;
	// keep how detection resolved so /report can name the source
	m.themeHow = m.applyTheme(cfg.Theme)
	if m.cfgExtra == nil {
		m.cfgExtra = map[string]string{}
	}
	if dir, err := config.Dir(); err == nil { // watcher baseline: only later saves sync
		if fi, err := os.Stat(filepath.Join(dir, "config.json")); err == nil {
			m.cfgMod = fi.ModTime()
		}
	}
	p := tea.NewProgram(m, opts...)
	m.prog = p
	// The worker launches lazily on the first worker-backed turn: it captures
	// GHG_WORKER_CWD and the session store state at launch, so starting it
	// here froze both before any /cd — and created empty sessions when the
	// user opened and immediately exited the TUI. The attach-only path still
	// dials at startup: `ghg attach` exists to show the live worker's state.
	if !launchWorker {
		if err := m.attachWorkerProcess(resumeID); err != nil {
			return "", err
		}
	}
	// Bubble Tea normally restores raw mode on every exit path. Keep a second
	// snapshot of the caller's tty as a last-resort guard: ghg also manages
	// mouse escape sequences outside Bubble Tea, and a startup/shutdown error
	// must never leave the parent shell without its original interrupt mode.
	restoreTTY := captureTTYState()
	if restoreTTY != nil {
		defer restoreTTY()
	}
	// install the interactive bash runner so the agent's bash tool can hand
	// sudo/ssh-style prompts to the user with a 15s inactivity timeout.
	m.irunner = newInteractiveRunner(p)
	tools.InteractiveBash = m.irunner
	go m.fetchCatalogs(false)
	go func() { p.Send(cfgSyncTick{}) }()     // start the config watcher
	go func() { p.Send(scheduleTickMsg{}) }() // start the wakeup channel
	// Bubble Tea's signal handler stops listening after the first SIGINT. That
	// is incompatible with ghg's deliberate double-ctrl+c exit confirmation:
	// a second signal can otherwise take the process down before the cleanup
	// defer runs. Keep one owner for SIGINT/SIGTERM and feed SIGINT through the
	// same model path as a raw ctrl+c key.
	stopSignals := forwardSignals(p)
	defer stopSignals()
	_, err = p.Run()
	if restoreTTY != nil {
		restoreTTY()
	}
	m.stopWorker()
	// Shut MCP servers down first (graceful: stdin close → SIGTERM → SIGKILL)
	// so a clean stdio server never becomes a KillAll target.
	if m.mcpMgr != nil {
		m.mcpMgr.Close()
	}
	// LSP servers get the same courtesy (shutdown/exit, then SIGKILL).
	if m.lspMgr != nil {
		m.lspMgr.Close()
	}
	// Make sure no agent-spawned child process (a server the model started, a
	// watcher, a daemon) outlives ghg. KillAll SIGKILLs every tracked process
	// group and waits for them.
	bashrun.KillAll()
	return m.sessionID, err
}

// startupReport prints one block naming what ghg loaded — skills (with
// validation warnings, pi's [Skill conflicts] lesson: a silently truncated or
// unparseable SKILL.md is a broken skill the user never learns about) and MCP
// servers — plus degraded-mode notices. Skipped on resume (the transcript
// already carries the past).
func (m *model) startupReport() {
	sk, problems := skills.ScanDetailed(skills.DefaultDirs()...)
	m.skillsCache = sk
	m.skillsLoaded = len(sk)
	var b strings.Builder
	var warned bool

	line := func(format string, args ...any) {
		fmt.Fprintf(&b, format+"\n", args...)
	}
	for _, s := range sk {
		if s.Warning != "" {
			line("  ⚠ %s: %s", s.Name, s.Warning)
			warned = true
		}
	}
	for _, p := range problems {
		line("  ⚠ %s: %s", p.Path, p.Err)
		warned = true
	}
	if m.mcpMgr != nil {
		sts := m.mcpMgr.Statuses()
		var parts []string
		for _, st := range sts {
			switch st.Status {
			case mcp.StatusReady:
				parts = append(parts, fmt.Sprintf("%s ✓ (%d tools)", st.Name, st.Tools))
			case mcp.StatusFailed:
				parts = append(parts, fmt.Sprintf("%s ✗", st.Name))
				warned = true
			case mcp.StatusDisabled:
				parts = append(parts, fmt.Sprintf("%s ○", st.Name))
			default:
				parts = append(parts, st.Name+" ◌")
			}
		}
		if len(parts) > 0 {
			line("mcp: %s", strings.Join(parts, " · "))
		}
	}
	if m.agent == nil {
		line(m.degradedProviderNote())
		warned = true
	}
	if b.Len() == 0 {
		return
	}
	out := strings.TrimRight(b.String(), "\n")
	if warned {
		m.append(errStyle.Render(out))
	} else {
		m.append(dimStyle.Render(out))
	}
}

// degradedProviderNote is the short actionable message shown when the TUI can
// open but no usable provider credential is available.
func (m *model) degradedProviderNote() string {
	return "No provider has been configured — run /auth"
}

// requireAgent keeps agent-dependent commands harmless during the cold TUI
// state. The note is deliberately the same onboarding hint shown at startup.
func (m *model) requireAgent() bool {
	if m.agent != nil {
		return true
	}
	m.append(m.degradedProviderNote())
	return false
}

// forwardSignals keeps signal handling alive for the whole TUI lifetime. A
// signal.Notify channel is intentionally buffered so a rapid double press is
// not lost while the first key is being processed by Bubble Tea.
func forwardSignals(p *tea.Program) func() {
	signals := make(chan os.Signal, 2)
	done := make(chan struct{})
	var stopOnce sync.Once
	var wg sync.WaitGroup

	signal.Notify(signals, os.Interrupt, syscall.SIGTERM)
	wg.Add(1)
	go func() {
		defer wg.Done()
		for {
			select {
			case <-done:
				return
			case sig := <-signals:
				if sig == os.Interrupt {
					p.Send(tea.KeyMsg{Type: tea.KeyCtrlC})
				} else {
					p.Quit()
				}
			}
		}
	}()

	return func() {
		stopOnce.Do(func() {
			signal.Stop(signals)
			close(done)
			wg.Wait()
		})
	}
}

// captureTTYState returns a best-effort restore function for the terminal that
// owns the interactive session. Bubble Tea already does this internally; the
// extra snapshot protects the parent shell if startup or shutdown fails before
// Bubble Tea can restore its raw-mode state.
func captureTTYState() func() {
	fd := int(os.Stdin.Fd())
	var closeTTY func()
	if !term.IsTerminal(fd) {
		tty, err := os.Open("/dev/tty")
		if err != nil {
			return nil
		}
		fd = int(tty.Fd())
		closeTTY = func() { _ = tty.Close() }
	}
	state, err := term.GetState(fd)
	if err != nil {
		if closeTTY != nil {
			closeTTY()
		}
		return nil
	}
	var once sync.Once
	return func() {
		once.Do(func() {
			_ = term.Restore(fd, state)
			if closeTTY != nil {
				closeTTY()
			}
		})
	}
}

// fetchCatalogs refreshes each provider's cached model list in the background,
// enriches missing context and reasoning metadata from models.dev, and sends
// the merged result to the UI. force bypasses both catalog TTLs (/model refresh)
// so newly announced models and metadata appear immediately.
func (m *model) fetchCatalogs(force bool) {
	cats := config.LoadCatalogs()
	if cats == nil { // defensive; LoadCatalogs already returns non-nil
		cats = map[string]config.Catalog{}
	}
	dirty := false
	for name, prov := range m.cfg.Providers {
		resolved, resolveErr := m.profiles.Resolve(provider.Instance{
			Name: name, Profile: prov.Profile, BaseURL: prov.BaseURL, Protocol: prov.API,
		})
		if resolveErr != nil {
			config.LogEvent("catalog.fetch", name+" skipped: "+resolveErr.Error())
			continue
		}
		// Catalog adapters are capability-driven. Static/none catalogs and
		// protocols without a compiled discovery adapter remain usable for
		// configured models; they simply do not trigger a GET /models here.
		if resolved.Catalog.Kind != provider.CatalogOpenAIModels && resolved.Catalog.Kind != provider.CatalogAnthropicModels {
			continue
		}
		if c, ok := cats[name]; ok && !force && !c.Stale() && c.BaseURL == resolved.BaseURL {
			continue
		}
		key := ""
		if resolved.RequiresAPIKey() {
			var keyErr error
			key, keyErr = prov.ResolveKey()
			if keyErr != nil {
				config.LogEvent("catalog.fetch", name+" skipped: "+keyErr.Error())
				continue
			}
			if key == "" {
				// A cold TUI is allowed to run without credentials so /auth can
				// repair it in place. Do not turn its background catalog refresh
				// into an unauthenticated request or a noisy retry loop.
				continue
			}
		}
		backend, backendErr := llm.NewBackend(llm.BackendConfig{
			Protocol:   llm.Protocol(resolved.Protocol),
			BaseURL:    resolved.BaseURL,
			APIKey:     key,
			Headers:    resolved.DefaultHeaders,
			AuthKind:   resolved.Auth.Kind,
			AuthHeader: resolved.Auth.Header,
			MaxRetries: m.cfg.MaxRetries,
		})
		if backendErr != nil {
			config.LogEvent("catalog.fetch", name+" skipped: "+backendErr.Error())
			continue
		}
		catalogBackend, ok := backend.(llm.CatalogBackend)
		if !ok {
			continue
		}
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		infos, err := catalogBackend.Models(ctx)
		cancel()
		if err != nil {
			config.LogEvent("catalog.fetch", name+" failed: "+err.Error())
			continue // keep any stale cache
		}
		config.LogEvent("catalog.fetch", fmt.Sprintf("%s ok: %d models", name, len(infos)))
		cats[name] = config.Catalog{FetchedAt: time.Now(), BaseURL: resolved.BaseURL, Models: config.ModelInfoLites(infos)}
		dirty = true
	}
	metadata := config.LoadModelsDev()
	wanted := m.modelsDevWanted(cats)
	needsMetadata := force || metadata.Stale() || metadata.Version < 5
	if !needsMetadata {
		for id := range wanted {
			if !metadata.HasModel(id) {
				needsMetadata = true
				break
			}
		}
	}
	if m.prog != nil && len(wanted) > 0 && needsMetadata {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		fresh, err := config.FetchModelsDev(ctx, wanted)
		cancel()
		if err != nil {
			config.LogEvent("catalog.metadata", "models.dev failed: "+err.Error())
		} else {
			metadata = fresh
			if err := config.SaveModelsDev(metadata); err != nil {
				config.LogEvent("catalog.metadata", "models.dev cache write failed: "+err.Error())
			}
		}
	}
	for name, cat := range cats {
		if enriched, changed := enrichCatalogMetadata(cat, metadata, m.modelsDevProviderIDs(name)); changed {
			cats[name] = enriched
			dirty = true
		}
	}
	if dirty {
		_ = config.SaveCatalogs(cats) // best-effort; the TUI still gets the fresh data
	}
	if m.prog != nil { // nil in tests that drive the command dispatch directly
		m.prog.Send(catalogsMsg(cats))
	}
}

// resume replaces the conversation with a stored session.
func (m *model) resume(id string) error {
	meta, msgs, err := m.store.Load(id)
	if err != nil {
		return err
	}
	var restoredGoal goalstate.Record
	hasGoal, err := func() (bool, error) {
		record, ok, err := m.store.LoadGoal(meta.ID)
		if err != nil {
			return false, err
		}
		if !ok && strings.TrimSpace(meta.Goal) != "" {
			record = goalstate.New(meta.Goal)
			record.ID = "legacy-" + meta.ID
			ok = true
		}
		if ok {
			restoredGoal = record
			// A process can only resume an active goal after an explicit
			// command. Treat an active record loaded from disk as interrupted,
			// so a restart never silently launches autonomous work.
			if restoredGoal.Status == goalstate.StatusActive {
				restoredGoal.Status = goalstate.StatusPaused
				restoredGoal.Blocker = "process ended; resume explicitly"
				restoredGoal.UpdatedAt = m.nowFn().UTC()
				if err := m.store.CheckpointGoal(meta.ID, restoredGoal); err != nil {
					return false, err
				}
			}
		}
		return ok, nil
	}()
	if err != nil {
		return err
	}
	// prefer the session's model/provider; fall back to current on error.
	// The session's own effort wins; a row that pre-dates per-session effort
	// ("") inherits the current default and gets stamped on the next save.
	effort := meta.Effort
	if effort == "" {
		effort = m.agent.Effort
	}
	if ag, mn, pn, err := buildAgent(m.cfg, meta.Model, meta.Provider, m.sysPrompt); err == nil {
		m.agent, m.modelName, m.provName = ag, mn, pn
	} else {
		m.agent = agent.New(m.agent.Backend, m.agent.Model, m.agent.MaxTokens, m.sysPrompt)
		m.agent.ModelName, m.agent.Provider = m.modelName, m.provName
		m.agent.ContextLimit = m.contextLimitFor(m.provName, m.agent.Model)
	}
	m.configureArtifactAgent(m.agent)
	m.applyCompactModel()
	m.agent.CompactThreshold = compactThresholdFor(m.cfg)
	m.wireTasks()
	// Publish before restoring so the settled rows record against this session.
	m.agent.Tasks().SetSessionID(meta.ID)
	m.agent.SetSessionID(meta.ID)
	if err := m.agent.BindState(context.Background()); err != nil {
		config.LogEvent("session.state", "bind failed: "+err.Error())
	}
	// Restore the session's background subagents into the dock. Everything
	// comes back settled: a process exit kills in-flight subagents, so a row
	// still "running" on disk means it died with the last exit.
	if tasks, terr := m.store.LoadTasks(meta.ID); terr == nil {
		for _, st := range tasks {
			status := agent.TaskStatus(st.Status)
			if status == agent.TaskRunning {
				status, st.Report = agent.TaskError, "interrupted — ghg exited before this subagent finished"
			}
			m.agent.RestoreTask(agent.BackgroundTask{
				ID: st.ID, Description: st.Description, Prompt: st.Prompt,
				Status: status, Report: st.Report,
				StartedAt: st.StartedAt, EndedAt: st.EndedAt,
				Restored: true,
			})
		}
	} else {
		config.LogEvent("session.task", "load failed: "+terr.Error())
	}
	m.agent.Messages = append(m.agent.Messages, msgs...)
	m.agent.RebuildTouched(msgs)
	m.agent.LoadTodosJSON(m.store.Todos(meta.ID))
	m.snapshots = m.store.Snapshots(meta.ID)
	// restore the cumulative token totals saved with the session; a row that
	// pre-dates the usage columns reads zero, so rebuild by summing the
	// per-message usage already stored on each assistant message. Either way
	// the next persist stamps the columns, so reconstruction happens once.
	in, cached, out := meta.UsageIn, meta.UsageCached, meta.UsageOut
	if in == 0 && out == 0 {
		for _, msg := range msgs {
			if msg.Usage != nil {
				in += msg.Usage.PromptTokens
				out += msg.Usage.CompletionTokens
				cached += msg.Usage.Cached()
			}
		}
	}
	if in > 0 || out > 0 {
		u := llm.Usage{PromptTokens: in, CompletionTokens: out}
		if cached > 0 {
			u.PromptTokensDetails = &struct {
				CachedTokens int `json:"cached_tokens"`
			}{CachedTokens: cached}
		}
		m.agent.SetUsage(u)
	}
	if slices.Contains(m.effortsFor(), effort) {
		m.agent.Effort = effort
	}
	m.sessionID = meta.ID
	bashrun.SetMarkers(meta.ID, m.agent.Model)
	m.saved = len(m.agent.Messages)
	// Add this session's user messages to recall, skipping any already present
	// from the global cross-session seed (resume runs after that seed).
	seen := make(map[string]bool, len(m.hist))
	for _, h := range m.hist {
		seen[h] = true
	}
	for _, msg := range msgs {
		// Authored only: steered subagent reports and goal prompts are stored
		// as role "user" but were never typed — ↑ must not recall them.
		text := msg.TextContent()
		if msg.Role == "user" && msg.Authored && !seen[text] {
			seen[text] = true
			m.hist = append(m.hist, text)
		}
	}
	m.histIdx = len(m.hist)
	m.blocks = nil
	m.msgBlock = nil
	m.future = nil // a different session's tail isn't this session's redo
	m.proposedPlanMD = ""
	m.planCurrent = ""
	m.goalRecord = nil
	if hasGoal {
		m.applyGoalRecord(restoredGoal)
	} else {
		m.goal = ""
		m.goalRounds = 0
	}
	m.append(dimStyle.Render(fmt.Sprintf("resumed %s · %s · %s @ %s", meta.ID, meta.Title, m.modelName, m.provName)))
	interrupted := 0
	for _, msg := range msgs {
		if msg.Role == "tool" && strings.HasPrefix(msg.Content, "Error: tool call interrupted") {
			interrupted++
		}
	}
	if interrupted > 0 {
		m.append(dimStyle.Render(fmt.Sprintf("⚠ %d tool call(s) were interrupted when this session last ended; the model knows and can retry them.", interrupted)))
	}
	if hasGoal {
		m.append(dimStyle.Render(fmt.Sprintf("◎ goal %s restored (%s) — /goal resume to keep working on it", restoredGoal.ID, restoredGoal.Status)))
	}
	m.seedTranscript(msgs, 1)
	return nil
}

// seedTranscript re-renders stored messages into the viewport. Blocks are
// appended in one batch with a single refreshVP at the end: a resumed
// session costs one render pass, not one per message. base is the
// conversation index of msgs[0] (1 for full transcripts — the system prompt
// is never rendered); msgBlock is extended so rewind can map messages to
// their blocks.
func (m *model) seedTranscript(msgs []llm.Message, base int) {
	for i, msg := range msgs {
		bi := -1
		switch msg.Role {
		case "user":
			bi = len(m.blocks)
			m.blocks = append(m.blocks, block{kind: blockText, text: youStyle.Render("❯ ") + linkifyFilePaths(msg.TextContent(), realFileExists)})
		case "assistant":
			if strings.TrimSpace(msg.TextContent()) != "" {
				bi = len(m.blocks)
				m.blocks = append(m.blocks, block{kind: blockAssistant, text: strings.TrimRight(msg.TextContent(), "\n")})
			}
			for _, tc := range msg.ToolCalls {
				m.blocks = append(m.blocks, block{kind: blockText, text: toolStyle.Render("⚒ "+tc.Function.Name+" ") + dimStyle.Render(tc.Function.Arguments)})
			}
		case "tool":
			// Synthetic results synthesized at load for interrupted calls get
			// an inline row so the user sees what the model sees; real tool
			// results stay folded under their assistant block.
			if strings.HasPrefix(msg.Content, "Error: tool call interrupted") {
				m.blocks = append(m.blocks, block{kind: blockText, text: errStyle.Render("⚒ "+msg.Name+" ") + dimStyle.Render("— interrupted: session ended before a result was recorded")})
			}
		}
		for len(m.msgBlock) <= base+i {
			m.msgBlock = append(m.msgBlock, -1)
		}
		m.msgBlock[base+i] = bi
	}
	m.follow = true
	m.refreshVP()
}

// persist writes any unsaved messages to the session store and re-stamps the
// session's bookkeeping (goal, effort) — the effort stamp is what a resume
// restores, so it runs even when no new messages landed.
func (m *model) ensureSession() bool {
	if m.store == nil || m.agent == nil || m.sessionID != "" {
		return m.sessionID != ""
	}
	id, err := m.store.Create(cwd(), m.modelName, m.provName)
	if err != nil {
		config.LogEvent("session.save", "create failed: "+err.Error())
		m.append(errStyle.Render("session save failed: " + err.Error()))
		return false
	}
	m.sessionID = id
	bashrun.SetMarkers(id, m.agent.Model)
	m.agent.Tasks().SetSessionID(id) // publish before Save so a settling subagent records
	m.agent.SetSessionID(id)         // scopes the per-session memory file
	if err := m.agent.BindState(context.Background()); err != nil {
		config.LogEvent("session.state", "bind failed: "+err.Error())
	}
	return true
}

func (m *model) persist() {
	// The worker is the sole owner of session writes once attached. The TUI
	// keeps a shadow agent for rendering, but saving it here can race the
	// worker and duplicate rows.
	if m.workerClient != nil || m.workerProcess != nil {
		return
	}
	if m.store == nil || m.agent == nil {
		return
	}
	msgs := m.agent.MessagesSnapshot()
	if m.sessionID == "" {
		if len(msgs) <= m.saved {
			return // nothing new to say; don't create an empty session row
		}
		if !m.ensureSession() {
			return
		}
	}
	// Bookkeeping re-stamps every persist — even one with no new messages —
	// so a resume restores the structured goal lifecycle, effort, and the
	// cumulative token totals that survive a compaction rewrite of messages.
	if record, ok := m.goalRecordForSession(); ok {
		m.persistGoal(record, false)
	}
	_ = m.store.SetEffort(m.sessionID, m.agent.Effort)
	_ = m.store.SetTodos(m.sessionID, m.agent.TodosJSON())
	if u := m.agent.Usage(); u.PromptTokens > 0 || u.CompletionTokens > 0 {
		_ = m.store.SetUsage(m.sessionID, u.PromptTokens, u.Cached(), u.CompletionTokens)
	}
	if len(msgs) <= m.saved {
		return
	}
	if err := m.store.Save(m.sessionID, m.saved, msgs, m.modelName, m.provName); err != nil {
		config.LogEvent("session.save", "FAILED id="+m.sessionID+": "+err.Error())
		m.append(errStyle.Render("session save failed: " + err.Error()))
		return
	}
	m.saved = len(msgs)
}

// setTheme switches the color scheme ("light"/"dark"/"auto") live and
// persists the pick to the global config: markdown re-renders under the new
// glamour style and every AdaptiveColor UI style follows lipgloss. A theme
// file change in ANOTHER running ghg session is picked up live via
// syncThemeMsg.
func (m *model) setTheme(theme string) {
	if theme != "light" && theme != "dark" {
		theme = "auto"
	}
	how := m.applyTheme(theme)
	m.themeHow = how // explicit picks return "" — detection source no longer applies
	m.cfg.Theme = theme
	if theme == "auto" {
		m.cfg.Theme = "" // auto persists as "" (omitted = auto-detect)
	}
	if m.cfgExtra == nil {
		m.cfgExtra = map[string]string{}
	}
	if theme == "auto" {
		delete(m.cfgExtra, "theme") // explicit pick, not omission
	} else {
		m.cfgExtra["theme"] = theme
	}
	if err := m.cfg.Save(); err != nil {
		m.append(errStyle.Render("config save failed: " + err.Error()))
	}
	m.refreshVP() // re-render the transcript under the new scheme
}

// applyTheme points rendering at a scheme WITHOUT persisting: auto re-detects
// (re-reading the terminal background so switching dark→auto can't stay dark),
// explicit picks override detection directly. Called by setTheme, startup, and
// the config watcher. how (only meaningful for auto) names the detection
// source for /report.
func (m *model) applyTheme(theme string) (how string) {
	switch theme {
	case "light":
		SetLightTheme(true)
		lipgloss.SetHasDarkBackground(false)
		setSchemeOverride("light")
	case "dark":
		SetLightTheme(false)
		lipgloss.SetHasDarkBackground(true)
		setSchemeOverride("dark")
	default: // auto: don't touch m.cfg.Theme — setTheme owns persistence
		setSchemeOverride("")
		how = detectColorScheme()
	}
	return how
}

// setEffort changes the reasoning effort and stores it both ways: as the new
// global default (every future session starts here) and on the live session
// row (resuming this conversation restores it). "" = off. Callers that only
// reconcile state (model switch / catalog refresh dropping an unsupported
// level) use resetEffort instead so a quiet reconciliation never rewrites
// the user's chosen global default.
func (m *model) setEffort(lv string) {
	if m.agent == nil {
		return
	}
	if m.workerClient != nil && m.workerLiveWork {
		m.append(dimStyle.Render("(worker is busy — change reasoning after this work finishes)"))
		return
	}
	m.agent.Effort = lv
	m.cfg.DefaultEffort = lv
	if err := m.cfg.Save(); err != nil {
		m.append(errStyle.Render("config save failed: " + err.Error()))
	}
	if m.store != nil && m.sessionID != "" {
		_ = m.store.SetEffort(m.sessionID, lv) // best-effort; persist() re-stamps
	}
	m.syncWorkerConfiguration(true)
}

// resetEffort applies a level without touching the global default.
func (m *model) resetEffort(lv string) {
	if m.agent == nil {
		return
	}
	m.agent.Effort = lv
	if m.store != nil && m.sessionID != "" {
		_ = m.store.SetEffort(m.sessionID, lv)
	}
	if !m.workerLiveWork {
		m.syncWorkerConfiguration(true)
	}
}

// setGoal creates a fresh structured goal or records an explicit user clear.
// A cleared goal remains in the ledger as paused so its ID, progress, and
// blocker history are not silently discarded.
func (m *model) setGoal(objective string) {
	objective = strings.TrimSpace(objective)
	if objective == "" {
		if record, ok := m.goalRecordForSession(); ok {
			record.Status = goalstate.StatusPaused
			record.Progress = ""
			record.Blocker = "cleared by user"
			record.UpdatedAt = m.nowFn().UTC()
			m.applyGoalRecord(record)
			m.persistGoal(record, true)
		} else {
			m.goal = ""
			m.goalRounds = 0
			m.goalRecord = nil
			if m.store != nil && m.sessionID != "" {
				_ = m.store.ClearGoal(m.sessionID)
			}
		}
		m.goal = ""
		return
	}
	record := goalstate.New(objective)
	if err := record.Validate(); err != nil {
		m.append(errStyle.Render("goal: " + err.Error()))
		return
	}
	m.applyGoalRecord(record)
	m.persistGoal(record, true)
}

// resumeGoal is the only path that turns a persisted non-active goal back into
// active work. This keeps process restart and blocked/limited goals explicit.
func (m *model) resumeGoal() bool {
	record, ok := m.goalRecordForSession()
	if !ok {
		m.append(errStyle.Render("no goal to resume — set one with /goal <text>"))
		return false
	}
	if record.Status == goalstate.StatusComplete {
		m.append(dimStyle.Render("goal is complete — set a new goal with /goal <text>"))
		return false
	}
	record.Status = goalstate.StatusActive
	record.Blocker = ""
	record.UpdatedAt = m.nowFn().UTC()
	m.applyGoalRecord(record)
	m.persistGoal(record, true)
	m.append(dimStyle.Render("◎ resuming goal " + record.ID + ": " + record.Objective))
	return true
}

func artifactsDisabled(cfg *config.Config) bool {
	return cfg != nil && cfg.Artifacts != nil && cfg.Artifacts.Enabled != nil && !*cfg.Artifacts.Enabled
}

// initArtifacts creates the persistent payload store once for the process.
// The session index is attached later, after sessions.db opens; the agent can
// still use its live message slice for references created before the first
// persist.
func (m *model) initArtifacts() {
	if artifactsDisabled(m.cfg) {
		if m.agent != nil {
			m.agent.ArtifactsDisabled = true
		}
		return
	}
	dir, err := config.Dir()
	if err != nil {
		config.LogEvent("artifact.open", "home failed: "+err.Error())
		return
	}
	maxBytes := artifact.DefaultMaxBytes
	if m.cfg.Artifacts != nil && m.cfg.Artifacts.MaxBytes > 0 {
		maxBytes = m.cfg.Artifacts.MaxBytes
	}
	store, err := artifact.NewWithLimit(filepath.Join(dir, "artifacts"), maxBytes)
	if err != nil {
		config.LogEvent("artifact.open", "FAILED: "+err.Error())
		m.append(errStyle.Render("artifacts disabled: " + err.Error()))
		return
	}
	m.artifactStore = store
	m.configureArtifactAgent(m.agent)
}

func (m *model) configureArtifactAgent(ag *agent.Agent) {
	if ag == nil {
		return
	}
	ag.Runtime = m.runtime
	if m.runtime != nil && m.runtime.ApprovalMode == tools.ApprovalAutoReview {
		m.runtime.Reviewer = ag.ApproveForMe
	}
	ag.ArtifactStore = m.artifactStore
	ag.ArtifactCatalog = m.store
	ag.HistoryCatalog = m.store
	if m.store != nil {
		ag.SetObservationStore(m.store.ObservationRegistryStore())
		ag.SetSearchStore(m.store.SearchRegistryStore())
	}
	ag.ArtifactsDisabled = artifactsDisabled(m.cfg)
	ag.SubagentsDisabled = !config.SubagentsEnabled(m.cfg)
	if !ag.ArtifactsDisabled {
		ag.ArtifactWriter = m.artifactStore
	} else {
		ag.ArtifactWriter = nil
	}
	m.configureAgentRoles(ag)
}

// configureAgentRoles gives delegated foreground and background tasks the
// same profile-aware construction path as the main session. The task tool
// asks for the tiny role; an absent role follows the config resolver's normal
// default/legacy fallback.
func (m *model) configureAgentRoles(ag *agent.Agent) {
	if ag == nil || m.cfg == nil {
		return
	}
	ag.SubagentFactory = func(_ context.Context, role, systemPrompt string) (*agent.Agent, error) {
		sub, _, _, err := buildAgentForRoleWithProfiles(m.cfg, role, systemPrompt, m.profiles)
		return sub, err
	}
}

func buildAgent(cfg *config.Config, modelName, provName, sysPrompt string) (*agent.Agent, string, string, error) {
	wd, err := os.Getwd()
	if err != nil {
		return nil, "", "", fmt.Errorf("provider profiles: current directory: %w", err)
	}
	profiles, err := provider.Load(provider.LoadOptions{ProjectTrusted: config.Trusted(wd)})
	if err != nil {
		return nil, "", "", err
	}
	return buildAgentWithProfiles(cfg, modelName, provName, sysPrompt, profiles)
}

func buildAgentWithProfiles(cfg *config.Config, modelName, provName, sysPrompt string, profiles provider.Profiles) (*agent.Agent, string, string, error) {
	return buildAgentWithProfilesMode(cfg, modelName, provName, sysPrompt, profiles, false)
}

// buildAgentWithProfilesOptional is the interactive-only variant. It returns
// a nil agent when the selected profile simply has no credential yet, while
// preserving hard errors for unknown routes, broken secret references, bad
// profiles, and invalid backend configuration.
func buildAgentWithProfilesOptional(cfg *config.Config, modelName, provName, sysPrompt string, profiles provider.Profiles) (*agent.Agent, string, string, error) {
	return buildAgentWithProfilesMode(cfg, modelName, provName, sysPrompt, profiles, true)
}

func buildAgentForRoleWithProfiles(cfg *config.Config, role, sysPrompt string, profiles provider.Profiles) (*agent.Agent, string, string, error) {
	return buildAgentForRoleWithProfilesMode(cfg, role, sysPrompt, profiles, false)
}

func buildAgentForRoleWithProfilesOptional(cfg *config.Config, role, sysPrompt string, profiles provider.Profiles) (*agent.Agent, string, string, error) {
	return buildAgentForRoleWithProfilesMode(cfg, role, sysPrompt, profiles, true)
}

func buildAgentForModeWithProfiles(cfg *config.Config, mode, sysPrompt string, profiles provider.Profiles) (*agent.Agent, string, string, error) {
	return buildAgentForRoleWithProfiles(cfg, config.RoleForMode(mode), sysPrompt, profiles)
}

func buildAgentForModeWithProfilesOptional(cfg *config.Config, mode, sysPrompt string, profiles provider.Profiles) (*agent.Agent, string, string, error) {
	return buildAgentForRoleWithProfilesOptional(cfg, config.RoleForMode(mode), sysPrompt, profiles)
}

func buildAgentForRoleWithProfilesMode(cfg *config.Config, role, sysPrompt string, profiles provider.Profiles, allowMissingKey bool) (*agent.Agent, string, string, error) {
	target, err := cfg.ResolveRole(role)
	if err != nil {
		// An entirely empty config is the interactive cold-start state. Keep
		// it usable for /auth, while an invalid configured role remains fatal.
		if allowMissingKey && len(cfg.Providers) == 0 && len(cfg.Models) == 0 && len(cfg.Roles) == 0 {
			return nil, cfg.DefaultModel, cfg.DefaultProvider, nil
		}
		return nil, "", "", err
	}
	ag, modelName, provName, err := buildAgentWithProfilesMode(cfg, target.Model, target.Provider, sysPrompt, profiles, allowMissingKey)
	if ag != nil {
		ag.Role = target.Role
	}
	return ag, modelName, provName, err
}

func buildAgentWithProfilesMode(cfg *config.Config, modelName, provName, sysPrompt string, profiles provider.Profiles, allowMissingKey bool) (*agent.Agent, string, string, error) {
	requestedModel, requestedProvider := modelName, provName
	route, err := cfg.Resolve(modelName, provName)
	if err != nil {
		// A provider-less config is an unconfigured cold-start state. Other
		// resolution failures remain hard errors so typos and broken routing
		// cannot be mistaken for an invitation to configure a key.
		if allowMissingKey && len(cfg.Providers) == 0 && requestedModel == "" && requestedProvider == "" {
			if modelName == "" {
				modelName = cfg.DefaultModel
			}
			if provName == "" {
				provName = cfg.DefaultProvider
			}
			return nil, modelName, provName, nil
		}
		return nil, "", "", err
	}
	modelName, provName = route.ModelName, route.ProviderName
	prov, mdl, apiID := route.Provider, route.Model, route.APIID
	resolved, err := profiles.ResolveModel(provider.Instance{
		Name: provName, Profile: prov.Profile, BaseURL: prov.BaseURL, Protocol: prov.API,
	}, apiID)
	if err != nil {
		return nil, "", "", err
	}
	key := ""
	if resolved.RequiresAPIKey() {
		key, err = prov.ResolveKey()
		if err != nil {
			return nil, "", "", err
		}
		if key == "" {
			if allowMissingKey {
				return nil, modelName, provName, nil
			}
			return nil, "", "", fmt.Errorf("no API key for provider %q (set apiKey/apiKeyEnv in ~/.ghg/config.json)", provName)
		}
	}
	// Two distinct limits:
	//   - ContextLimit: the input window (provider's context_length, then the
	//     config's context, then models.dev). Drives the context status and
	//     proactive compaction.
	//   - MaxTokens: the OUTPUT cap sent as max_tokens. Priority: config maxOut
	//     → provider's max_completion_tokens → config context (last resort).
	cat, hasCat := config.LoadCatalogs()[provName]
	ctxLimit := mdl.ContextWindow()
	if hasCat {
		if n := cat.ContextLength(apiID); n > 0 {
			ctxLimit = n
		}
	}
	if ctxLimit <= 0 {
		if n := config.LoadModelsDev().ContextLength(apiID, modelsDevProviderIDs(resolved, provName)...); n > 0 {
			ctxLimit = n
		}
	}
	maxOut := 0
	if hasCat {
		maxOut = cat.MaxCompletionTokens(apiID)
	}
	if maxOut <= 0 {
		maxOut = ctxLimit // generous default; provider clamps if it's too high
	}
	protocol := resolved.Protocol
	if strings.TrimSpace(mdl.API) != "" {
		protocol = strings.TrimSpace(mdl.API)
	}
	backend, err := llm.NewBackend(llm.BackendConfig{
		Protocol:   llm.Protocol(protocol),
		BaseURL:    resolved.BaseURL,
		APIKey:     key,
		Headers:    resolved.DefaultHeaders,
		AuthKind:   resolved.Auth.Kind,
		AuthHeader: resolved.Auth.Header,
		MaxRetries: cfg.MaxRetries,
	})
	if err != nil {
		return nil, "", "", err
	}
	ag := agent.New(backend, apiID, maxOut, sysPrompt)
	ag.ModelName, ag.Provider = modelName, provName
	ag.Role = config.RoleDefault
	ag.ContextLimit = ctxLimit
	if hasCat {
		if info := cat.Find(apiID); info != nil {
			ag.ReasoningToggle = info.ReasoningToggle
		}
	}
	if !ag.ReasoningToggle {
		if info, ok := config.LoadModelsDev().ReasoningFor(apiID, modelsDevProviderIDs(resolved, provName)...); ok {
			ag.ReasoningToggle = info.Toggle
		}
	}
	return ag, modelName, provName, nil
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

// detectColorScheme figures out whether the terminal background is light and
// calls SetLightTheme so markdown renders with a matching (high-contrast)
// glamour style. Priority:
//  1. GHG_THEME=light|dark (explicit env override)
//  2. COLORFGBG (set by many terminals; last field is the bg color index)
//  3. an OSC 11 background query on /dev/tty with a short timeout
//  4. default: dark (the safe assumption for coding terminals)
//
// The config theme is NOT consulted here — applyTheme handles explicit picks
// before auto ever reaches detection. detectColorScheme returns a short
// human-readable note naming the source of the decision (shown by /theme auto
// so a wrong pick is diagnosable).
func detectColorScheme() string {
	setScheme := func(light bool) {
		SetLightTheme(light)                  // glamour markdown style
		lipgloss.SetHasDarkBackground(!light) // AdaptiveColor picks
	}
	switch strings.ToLower(os.Getenv("GHG_THEME")) {
	case "light":
		setScheme(true)
		return "GHG_THEME"
	case "dark":
		setScheme(false)
		return "GHG_THEME"
	}
	if v := os.Getenv("COLORFGBG"); v != "" {
		if i := strings.LastIndex(v, ";"); i >= 0 {
			var bg int
			if _, err := fmt.Sscanf(v[i+1:], "%d", &bg); err == nil {
				// standard settings: 0-6 dark, 7+ light (15 = white)
				setScheme(bg == 7 || bg >= 8)
				return "COLORFGBG"
			}
		}
	}
	// Query the terminal directly whenever we have one. termenv's query refuses
	// to run inside tmux/screen (TERM=screen*/tmux*) and silently assumes a dark
	// background — wrong for a tmux user on a light terminal. queryTerminal-
	// Background reaches the REAL terminal via DCS passthrough inside tmux, and
	// via a plain OSC 11 query otherwise, so use it first and keep termenv only
	// as a fallback for terminals it can query directly.
	tty, err := os.OpenFile("/dev/tty", os.O_RDWR, 0)
	if err != nil {
		SetUnknownTheme()
		return "undetermined (no tty) — neutral default"
	}
	defer tty.Close()
	inTmux := os.Getenv("TMUX") != "" ||
		strings.HasPrefix(os.Getenv("TERM"), "screen") ||
		strings.HasPrefix(os.Getenv("TERM"), "tmux")
	if light, ok := queryTerminalBackground(tty, inTmux); ok {
		setScheme(light)
		if inTmux {
			return "terminal query (via tmux passthrough)"
		}
		return "terminal query"
	}
	// fallback: termenv's own query (non-tmux terminals it can reach)
	type result struct{ light bool }
	done := make(chan result, 1)
	go func() {
		o := termenv.NewOutput(tty)
		done <- result{light: !o.HasDarkBackground()}
	}()
	select {
	case r := <-done:
		setScheme(r.light)
		return "terminal query"
	case <-time.After(300 * time.Millisecond):
		// No reliable signal: don't force a dark guess. Neutral default keeps
		// text at the terminal's own colors instead of inverting contrast.
		SetUnknownTheme()
		if inTmux {
			return "undetermined (tmux needs: set -g allow-passthrough on) — neutral default"
		}
		return "undetermined (query timed out) — neutral default"
	}
}

func (m *model) nowFn() time.Time {
	if m.now != nil {
		return m.now()
	}
	return time.Now()
}

func compactThresholdFor(cfg *config.Config) float64 {
	return config.CompactThreshold(cfg)
}

// applyCompactModel points the agent's compaction summary call at the
// configured compaction model/provider. An explicit compactModel remains a
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
	route, err := m.cfg.Resolve(cm, cp)
	if err != nil {
		if m.compactModel != "" { // a picked model failing is worth a note; a missing default isn't
			m.append(errStyle.Render("compaction model: " + err.Error() + " — using current model"))
		}
		return
	}
	prov, mdl, apiID := route.Provider, route.Model, route.APIID
	provName := route.ProviderName
	resolved, resolveErr := m.profiles.ResolveModel(provider.Instance{
		Name: provName, Profile: prov.Profile, BaseURL: prov.BaseURL, Protocol: prov.API,
	}, apiID)
	if resolveErr != nil {
		if m.compactModel != "" {
			m.append(errStyle.Render("compaction model: " + resolveErr.Error() + " — using current model"))
		}
		return
	}
	key := ""
	var keyErr error
	if resolved.RequiresAPIKey() {
		key, keyErr = prov.ResolveKey()
	}
	if !resolved.RequiresAPIKey() || (keyErr == nil && key != "") {
		protocol := resolved.Protocol
		if strings.TrimSpace(mdl.API) != "" {
			protocol = strings.TrimSpace(mdl.API)
		}
		backend, backendErr := llm.NewBackend(llm.BackendConfig{
			Protocol:   llm.Protocol(protocol),
			BaseURL:    resolved.BaseURL,
			APIKey:     key,
			Headers:    resolved.DefaultHeaders,
			AuthKind:   resolved.Auth.Kind,
			AuthHeader: resolved.Auth.Header,
			MaxRetries: m.cfg.MaxRetries,
		})
		if backendErr == nil {
			m.agent.CompactBackend = backend
			m.agent.CompactModel = apiID
			m.agent.CompactProvider = provName
			m.agent.CompactProtocol = protocol
		} else if m.compactModel != "" {
			m.append(errStyle.Render("compaction model: " + backendErr.Error() + " — using current model"))
		}
	} else if m.compactModel != "" {
		if keyErr != nil {
			m.append(errStyle.Render("compaction model: " + keyErr.Error() + " — using current model"))
		} else {
			m.append(errStyle.Render("compaction model: no API key — using current model"))
		}
	}
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

func (m *model) switchModel(name, prov string) {
	if m.workerClient != nil && m.workerLiveWork {
		m.append(dimStyle.Render("(worker is busy — change the model after this work finishes)"))
		return
	}
	previous := m.agent
	ag, mn, pn, err := buildAgent(m.cfg, name, prov, m.sysPrompt)
	if err != nil {
		m.append(errStyle.Render(err.Error()))
		return
	}
	ag.ReasoningToggle = m.reasoningToggleFor(pn, ag.Model)
	if previous != nil {
		ag.Effort = previous.Effort
		ag.Messages = append(ag.Messages, previous.Messages[1:]...) // carry history
		ag.CompactBackend, ag.CompactModel = previous.CompactBackend, previous.CompactModel
		ag.CompactProvider, ag.CompactProtocol = previous.CompactProvider, previous.CompactProtocol
		ag.CompactThreshold = previous.CompactThreshold
		previous.ShareState(ag)
	} else {
		ag.Effort = m.cfg.DefaultEffort
		if ag.Effort == "" {
			ag.Effort = "medium"
		}
		ag.CompactThreshold = compactThresholdFor(m.cfg)
	}
	m.agent, m.modelName, m.provName = ag, mn, pn
	m.configureArtifactAgent(m.agent)
	m.applyCompactModel()
	m.wireTasks()
	m.syncWorkerConfiguration(true)
	if !slices.Contains(m.effortsFor(), ag.Effort) {
		m.resetEffort("") // the new model doesn't support the current level
	}
	m.cfg.DefaultModel, m.cfg.DefaultProvider = mn, pn // store the switch as the new default
	if err := m.cfg.Save(); err != nil {
		m.append(errStyle.Render("config save failed: " + err.Error()))
	}
}

// pickerKey handles keys while the /resume picker is open.
func (m *model) pickerKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	p := m.picker
	switch msg.Type {
	case tea.KeyEsc, tea.KeyCtrlC:
		p.pendingD = false
		m.picker = nil
	case tea.KeyUp, tea.KeyCtrlP, tea.KeyShiftTab: // older sessions sit above
		p.pendingD = false
		if p.idx < len(p.metas)-1 {
			p.idx++
			p.loadPreview(m.store)
		}
	case tea.KeyDown, tea.KeyCtrlN, tea.KeyTab: // newer sessions sit below
		p.pendingD = false
		if p.idx > 0 {
			p.idx--
			p.loadPreview(m.store)
		}
	case tea.KeyEnter:
		p.pendingD = false
		if len(p.metas) == 0 {
			m.picker = nil
			return m, nil
		}
		id := p.metas[p.idx].ID
		m.picker = nil
		if err := m.resume(id); err != nil {
			m.append(errStyle.Render(err.Error()))
		}
	case tea.KeyRunes:
		switch string(msg.Runes) {
		case "k":
			p.pendingD = false
			if p.idx < len(p.metas)-1 {
				p.idx++
				p.loadPreview(m.store)
			}
		case "j":
			p.pendingD = false
			if p.idx > 0 {
				p.idx--
				p.loadPreview(m.store)
			}
		case "d":
			if !p.pendingD {
				p.pendingD = true
				return m, nil
			}
			p.pendingD = false
			if len(p.metas) == 0 {
				return m, nil
			}
			toDelete := p.metas[p.idx]
			if m.store != nil {
				if err := m.store.DeleteSession(toDelete.ID); err != nil {
					m.append(errStyle.Render("delete session failed: " + err.Error()))
					return m, nil
				}
			}
			if m.sessionID == toDelete.ID {
				m.sessionID = ""
				m.saved = 0
				m.workerContextTokens = 0
			}
			p.metas = slices.Delete(p.metas, p.idx, p.idx+1)
			delete(p.previews, toDelete.ID)
			if len(p.metas) == 0 {
				m.picker = nil
				m.append(dimStyle.Render("◎ deleted session " + toDelete.ID))
				return m, nil
			}
			if p.idx >= len(p.metas) {
				p.idx = len(p.metas) - 1
			}
			p.loadPreview(m.store)
			m.append(dimStyle.Render("◎ deleted session " + toDelete.ID))
		default:
			p.pendingD = false
		}
	default:
		p.pendingD = false
	}
	return m, nil
}

func (p *picker) loadPreview(store *session.Store) {
	id := p.metas[p.idx].ID
	if _, ok := p.previews[id]; !ok {
		u, a := store.LastExchange(id)
		p.previews[id] = [2]string{u, a}
	}
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
	head, cands := completionsWithAuth(m.input.Value(), m.modelCands(), m.providerCands(), m.authProviderCands(), m.skillCands(), effortCandsFor(m.effortsFor()))
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
		head, cands := completionsWithAuth(val, m.modelCands(), m.providerCands(), m.authProviderCands(), m.skillCands(), effortCandsFor(m.effortsFor()))
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
	available := make(map[string]bool)
	for _, it := range m.availableModelItems() {
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
	for _, it := range m.availableModelItems() {
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
		if profile.Auth.Kind == provider.AuthNone {
			status = "no key required"
		} else if m.authConfigured(id) {
			status = "configured"
		}
		out = append(out, cand{id, profile.DisplayName + " — " + status})
	}
	return out
}

// skillCands uses the startup snapshot while the completion menu is open.
// prepareTurn is the explicit refresh point for newly added skills.
func (m *model) skillCands() []cand {
	if m.skillsCache == nil {
		m.skillsCache = skills.Scan(skills.DefaultDirs()...)
		if m.skillsCache == nil {
			m.skillsCache = []skills.Skill{}
		}
		m.skillsLoaded = len(m.skillsCache)
	}
	out := make([]cand, 0, len(m.skillsCache))
	for _, s := range m.skillsCache {
		d := s.Description
		if len(d) > 80 {
			d = d[:80] + "…"
		}
		out = append(out, cand{"$" + s.Name, d})
	}
	return out
}

// prepareTurn refreshes the system prompt's skills block (so new skills load
// without a restart) and MCP server instructions (so late-arriving servers
// teach the model how to use their tools), then expands $skill / @file
// tokens in the input. It returns the expanded text plus any image parts
// extracted from @image tags.
func (m *model) prepareTurn(text string) (string, []llm.ContentPart) {
	sk := skills.Scan(skills.DefaultDirs()...)
	if sk == nil {
		sk = []skills.Skill{}
	}
	m.skillsCache = sk
	m.skillsLoaded = len(sk)
	sys := m.sysPrompt + skills.PromptBlock(sk)
	if m.mcpMgr != nil {
		sys += m.mcpMgr.InstructionsBlock()
	}
	sys += memory.PromptBlock(memory.Installation(), memory.Session(m.sessionID))
	if m.agent != nil && len(m.agent.Messages) > 0 {
		m.agent.Messages[0].Content = sys
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
	if m.store != nil && m.sessionID == "" {
		m.ensureSession()
	}
	if m.workerClient == nil && m.workerProcess == nil && m.prog != nil {
		m.ensureWorker()
	}
	m.busy = true
	m.turnStart = m.nowFn()
	prepared, parts := m.prepareTurn(text)
	userMsgIdx := len(m.agent.Messages) // where Turn will append this message
	// Snapshot the pre-turn workspace so a rewind past this turn restores the
	// files it is about to change. "" = not a git repo; a clean tree still
	// snapshots here (as HEAD) — turnDone drops it if the turn changed nothing.
	preSnap := snapshotWorkspace()
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
	hasGoal = hasGoal && goalCtx.Status == goalstate.StatusActive
	if m.workerClient != nil {
		return m.submitWorkerTurn(text, authored, prepared, parts, userMsgIdx, preSnap, func() *goalstate.Record {
			if !hasGoal {
				return nil
			}
			copy := goalCtx
			return &copy
		}())
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
	var goalUsage llm.Usage
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
			OnCompactionReady: func(raw []llm.Message, summary string, cutoff int) error {
				return m.store.PersistCompaction(m.sessionID, m.saved, raw, m.modelName, m.provName, summary, cutoff)
			},
			OnCompacted: func(sum string, _ int) {
				send(compactMsg{summary: sum})
			},
			OnUsage: func(u llm.Usage) {
				mu.Lock()
				goalUsage.PromptTokens += u.PromptTokens
				goalUsage.CompletionTokens += u.CompletionTokens
				goalUsage.CacheCreationTokens += u.CacheCreationTokens
				if cached := u.Cached(); cached > 0 {
					if goalUsage.PromptTokensDetails == nil {
						goalUsage.PromptTokensDetails = &struct {
							CachedTokens int `json:"cached_tokens"`
						}{}
					}
					goalUsage.PromptTokensDetails.CachedTokens += cached
				}
				mu.Unlock()
				send(usageMsg(u))
			},
			OnGoalUpdate: func(update agent.GoalUpdate) {
				mu.Lock()
				goalUpdates = append(goalUpdates, update)
				mu.Unlock()
				send(goalUpdateMsg{update: update})
			},
			OnRetry: func(ev llm.RetryEvent) {
				flush()
				send(noticeMsg(fmt.Sprintf("⚠ request failed (%s) — retrying in %s (attempt %d/%d)",
					ev.Err, ev.Delay.Round(time.Millisecond), ev.Attempt+1, ev.Max)))
			},
		}
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
		send(turnDoneMsg{final: final, err: err, at: userMsgIdx, snap: preSnap, clean: workspaceClean(), goalUpdates: updates, goalUsage: usage})
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

// busyCmd reports whether a slash command should be handled immediately while
// a turn is in flight. Settings/views are safe; /plan and /execute also need
// to report their busy state themselves rather than being queued as literal
// chat text (queued text is submitted to the model verbatim after the turn).
func busyCmd(text string) bool {
	fields := strings.Fields(text)
	if len(fields) == 0 {
		return false
	}
	switch fields[0] {
	case "/help", "/theme", "/effort", "/tasks", "/cd", "/pwd", "/report", "/detach":
		return true
	case "/plan", "/execute": // handled immediately so a slash command is not sent as chat text
		return true
	case "/auth": // must run now even while busy: an inline key queued as a chat message would be sent to the model
		return true
	case "/goal": // status, clear, and rounds are settings; resume/<text> submit turns
		return len(fields) == 1 || fields[1] == "clear" || fields[1] == "rounds"
	}
	return false
}

func (m *model) command(text string) (tea.Model, tea.Cmd) {
	fields := strings.Fields(text)
	if len(fields) == 0 {
		return m, nil
	}
	switch fields[0] {
	case "/quit", "/exit", "/q":
		return m, tea.Quit
	case "/detach":
		live := m.busy || m.workerState == workerwire.StateRunning || m.workerState == workerwire.StateWaitingApproval || m.workerLiveWork
		if m.workerClient == nil || !live {
			m.append(dimStyle.Render("(nothing running to detach)"))
			return m, nil
		}
		if m.detachRequestID != "" {
			return m, nil
		}
		requestID := workerRequestID("detach")
		if err := m.workerClient.Send(workerwire.CommandDetach, requestID, nil); err != nil {
			m.append(errStyle.Render("detach failed: " + err.Error()))
			return m, nil
		}
		m.detachRequestID = requestID
		return m, nil
	case "/clear":
		if m.busy {
			m.append(dimStyle.Render("(busy — /clear after this turn)"))
			return m, nil
		}
		if !m.requireAgent() {
			return m, nil
		}
		// A worker owns the durable session. Stop it before creating the fresh
		// session boundary below; the next submitted turn starts a new worker.
		if m.workerClient != nil || m.workerProcess != nil {
			m.stopWorker()
			m.workerStartFailed = false
		}
		m.agent.Messages = m.agent.Messages[:1] // keep system prompt
		m.agent.ResetUsage()                    // zero the status line's spend counters
		m.workerContextTokens = 0
		m.blocks = nil
		m.msgBlock = nil
		m.future = nil // no redo across a cleared conversation
		m.proposedPlanMD = ""
		m.planCurrent = ""
		m.setGoal("") // clear before detaching so the old session's goal is dropped too
		m.goalRecord = nil
		m.sessionID = "" // next turn starts a fresh session
		m.agent.Tasks().SetSessionID("")
		m.agent.SetSessionID("")
		m.agent.ResetState()
		m.saved = 1
		m.append(dimStyle.Render("(conversation cleared)"))
	case "/memory":
		m.memoryCommand(fields[1:])
	case "/schedule":
		m.scheduleCommand(fields[1:])
	case "/me":
		return m, m.openMe()
	case "/compact":
		if len(fields) == 1 && m.workerClient != nil {
			if m.busy {
				m.append(dimStyle.Render("(busy — /compact after this turn)"))
				return m, nil
			}
			if !m.requireAgent() {
				return m, nil
			}
			requestID := workerRequestID("compact")
			m.busy = true
			m.turnStart = m.nowFn()
			m.append(dimStyle.Render("◎ compacting…"))
			m.cancel = func() {
				if m.workerClient != nil {
					_ = m.workerClient.Send(workerwire.CommandCancel, requestID+"-cancel", nil)
				}
			}
			if err := m.workerClient.Send(workerwire.CommandCompact, requestID, nil); err != nil {
				m.busy = false
				m.cancel = nil
				m.append(errStyle.Render("compact failed: " + err.Error()))
			}
			return m, m.spin.Tick
		}
		if len(fields) > 1 {
			switch fields[1] {
			case "retry":
				m.compactRetry()
				return m, nil
			case "log":
				m.compactLog()
				return m, nil
			}
			m.compactCommand(fields[1:])
			return m, nil
		}
		if m.busy {
			m.append(dimStyle.Render("(busy — /compact will land after this turn)"))
			return m, nil
		}
		if !m.requireAgent() {
			return m, nil
		}
		if m.store != nil && m.sessionID == "" {
			m.ensureSession()
		}
		m.busy = true
		m.append(dimStyle.Render("◎ compacting…"))
		p := m.prog
		ag := m.agent // capture the current conversation for the summary call
		ctx, cancel := context.WithCancel(context.Background())
		m.cancel = cancel
		go func() {
			took := len(ag.Messages)
			var summary string
			err := ag.ManualCompact(ctx, agent.Events{
				OnCompactionReady: func(messages []llm.Message, summary string, cutoff int) error {
					return m.store.PersistCompaction(m.sessionID, m.saved, messages, m.modelName, m.provName, summary, cutoff)
				},
				OnCompacted: func(s string, _ int) { summary = s },
			})
			if p != nil { // nil in headless tests; compaction still ran
				p.Send(compactMsg{took: took - len(ag.Messages), kept: len(ag.Messages), summary: summary, err: err})
				p.Send(turnDoneMsg{}) // clear busy state
			}
		}()
		return m, m.spin.Tick
	case "/mcp":
		return m.mcpCommand(fields)
	case "/lsp":
		return m.lspCommand(fields)
	case "/cd":
		m.cdCommand(strings.TrimSpace(strings.TrimPrefix(text, "/cd")))
		return m, nil
	case "/pwd":
		m.append(dimStyle.Render(cwd()))
		return m, nil
	case "/tasks":
		if len(fields) > 1 { // /tasks <id>: jump straight into the detail view
			m.openTask(fields[1])
			return m, nil
		}
		// bare /tasks focuses the dock if it exists, else prints the list
		if len(m.dockTasks()) > 0 {
			m.tasksFocus = true
			m.clampTaskSel()
			return m, nil
		}
		m.append(m.tasksView())
		return m, nil
	case "/theme":
		if len(fields) > 1 {
			switch fields[1] {
			case "light", "dark", "auto":
				m.setTheme(fields[1])
			default:
				m.append(errStyle.Render("usage: /theme light|dark|auto"))
			}
		} else {
			m.openPaletteOn("theme") // bare: open the switcher, don't toggle blind
		}
		return m, nil
	case "/effort":
		if len(fields) > 1 {
			if !m.requireAgent() {
				return m, nil
			}
			levels := m.effortsFor()
			lv, ok := parseEffort(levels, fields[1])
			if !ok {
				names := make([]string, len(levels))
				for i, e := range levels {
					names[i] = effortLabel(e)
				}
				m.append(errStyle.Render("unknown effort level; " + m.agent.Model + " supports: " + strings.Join(names, ", ")))
				break
			}
			m.setEffort(lv)
		} else {
			m.openPaletteOn("reasoning effort") // bare: open the level selector
		}
	case "/goal-from-context":
		if !m.requireAgent() {
			return m, nil
		}
		if m.busy {
			m.append(dimStyle.Render("(busy — /goal-from-context after this turn)"))
			return m, nil
		}
		window := agent.GoalFromContextDefaultWindow
		if len(fields) > 1 {
			n, err := strconv.Atoi(fields[1])
			if err != nil || n < 2 {
				m.append(errStyle.Render("usage: /goal-from-context [n] — n ≥ 2 messages of context (default " + strconv.Itoa(agent.GoalFromContextDefaultWindow) + ")"))
				return m, nil
			}
			window = n
		}
		tail, err := agent.GoalFromContextMessages(m.agent.Messages, window)
		if err != nil {
			m.append(errStyle.Render(err.Error()))
			return m, nil
		}
		// one non-streaming call on the CURRENT model (the compact-model
		// override is deliberately ignored) distills the tail into a goal
		m.busy = true
		m.append(dimStyle.Render(fmt.Sprintf("◎ formulating goal from the last %d messages…", len(tail))))
		p := m.prog
		// ag may drift from m.agent if the user /model-switches mid-formulation:
		// usage lands on the old agent, the goal submits on the new one. The
		// call itself is safe (Complete touches no Agent state, AddUsage is
		// mutex-protected) and the window is seconds — not worth a guard.
		ag := m.agent
		ctx, cancel := context.WithCancel(context.Background())
		m.cancel = cancel
		prompt := agent.BuildGoalFromContextPrompt(tail)
		formulate := func() (string, error) {
			reasoningEffort, reasoningEnabled := ag.ReasoningRequest()
			message, usage, err := ag.CompleteWithRoute(ctx, ag.Backend, ag.Role, ag.Provider, ag.Protocol, llm.Request{
				Model:            ag.Model,
				MaxTokens:        8192,
				Messages:         []llm.Message{{Role: "user", Content: prompt}},
				ReasoningEffort:  reasoningEffort,
				ReasoningEnabled: reasoningEnabled,
			}, agent.Events{})
			ag.AddUsage(usage) // the formulation call is session spend too
			return message.TextContent(), err
		}
		if p == nil {
			// headless (tests): run inline on the caller's goroutine — with
			// no program to pump messages the Update handler can't run, so
			// apply the same notes/goal here; the goal loop itself never
			// starts without a running program
			goal, err := formulate()
			m.busy = false
			m.cancel = nil
			switch {
			case err != nil && err != context.Canceled:
				m.append(errStyle.Render("goal-from-context failed: " + err.Error()))
			case err == nil && strings.TrimSpace(goal) == "":
				m.append(errStyle.Render("goal-from-context: model returned an empty goal"))
			case err == nil:
				m.setGoal(strings.TrimSpace(goal))
				m.append(dimStyle.Render("◎ goal set: " + m.goal))
			}
			return m, nil
		}
		go func() {
			goal, err := formulate()
			// the msg handler owns busy/cancel: on success it submits (busy
			// belongs to the new turn), on failure it clears them directly —
			// a turnDoneMsg{} here would either cancel-proof the fresh turn
			// (success) or re-engage a paused goal's loop (failure)
			p.Send(goalFromContextMsg{goal: goal, err: err})
		}()
		return m, m.spin.Tick
	case "/plan":
		return m.planCommand(text)
	case "/execute":
		return m.executeCommand(text)
	case "/export", "/export-result":
		return m.exportResultCommand(text)
	case "/export-chat", "/export-log":
		args := strings.TrimSpace(strings.TrimPrefix(strings.TrimPrefix(text, "/export-chat"), "/export-log"))
		return m.exportResultCommand("/export-result chat " + args)
	case "/goal":
		switch {
		case len(fields) == 1:
			record, ok := m.goalRecordForSession()
			if !ok {
				m.append(dimStyle.Render("no goal set — /goal <text> to set one"))
			} else {
				m.append(dimStyle.Render(fmt.Sprintf("◎ goal %s (%s, round %d/%d): %s", record.ID, record.Status, record.Rounds, m.goalMaxRounds(), record.Objective)))
				if record.Progress != "" {
					m.append(dimStyle.Render("  progress: " + record.Progress))
				}
				if record.Blocker != "" {
					m.append(dimStyle.Render("  blocker: " + record.Blocker))
				}
			}
		case fields[1] == "clear":
			m.setGoal("")
			m.append(dimStyle.Render("(goal cleared)"))
		case fields[1] == "rounds":
			m.goalRoundsCommand(fields[2:])
		case fields[1] == "resume":
			if !m.requireAgent() {
				break
			}
			if !m.resumeGoal() {
				break
			}
			record, _ := m.goalRecordForSession()
			return m.submitGoal(goalContinuePrompt(record.Objective))
		default:
			if !m.requireAgent() {
				break
			}
			goal := strings.TrimSpace(strings.TrimPrefix(text, "/goal"))
			m.setGoal(goal)
			m.append(dimStyle.Render("◎ goal set: " + goal))
			return m.submit(goal)
		}
	case "/fork":
		if m.busy {
			m.append(dimStyle.Render("(busy — /fork after this turn)"))
			return m, nil
		}
		m.forkCommand(strings.TrimSpace(strings.TrimPrefix(text, "/fork")))
		return m, nil
	case "/rename":
		if m.busy {
			m.append(dimStyle.Render("(busy — /rename after this turn)"))
			return m, nil
		}
		m.renameCommand(strings.TrimSpace(strings.TrimPrefix(text, "/rename")))
		return m, nil
	case "/resume":
		if !m.requireAgent() {
			break
		}
		if m.busy {
			m.append(dimStyle.Render("(busy — /resume after this turn)"))
			return m, nil
		}
		if len(fields) > 1 {
			if err := m.resume(fields[1]); err != nil {
				m.append(errStyle.Render(err.Error()))
			}
			break
		}
		m.openPicker()
	case "/context-doctor":
		m.append(m.doctorReport())
	case "/report":
		m.append(m.reportBlock())
	case "/help":
		m.append(dimStyle.Render(helpText()))
	case "/auth":
		m.authCommand(fields[1:])
	case "/model":
		if len(fields) < 2 {
			m.openModelPicker()
			break
		}
		if fields[1] == "refresh" {
			m.append(dimStyle.Render("refreshing model catalogs…"))
			go func() {
				m.fetchCatalogs(true)
				if m.prog != nil {
					m.prog.Send(noticeMsg("model catalogs refreshed — /model shows newly announced models"))
				}
			}()
			break
		}
		prov := ""
		if len(fields) > 2 {
			prov = fields[2]
		}
		name := fields[1]
		resolved, ok, alts := resolveModelFuzzy(m.cfg, name)
		if !ok {
			if len(alts) > 0 {
				m.append(errStyle.Render(fmt.Sprintf("ambiguous model %q — did you mean: %s?", name, strings.Join(alts, ", "))))
				return m, nil
			}
			m.append(errStyle.Render("unknown model " + name))
			return m, nil
		}
		m.switchModel(resolved, prov)
	default:
		m.append(errStyle.Render("unknown command " + fields[0]))
	}
	return m, nil
}

// compactCommand handles "/compact <args…>": off restores the built-in
// default compaction model, "<model> [provider]" selects one (persisted).
func (m *model) compactCommand(args []string) {
	if len(args) == 0 {
		return
	}
	if m.agent == nil {
		m.append(m.degradedProviderNote())
		return
	}
	if args[0] == "off" {
		m.compactModel, m.compactProv = "", ""
		m.applyCompactModel()
		m.cfg.CompactModel, m.cfg.CompactProvider = "", ""
		if err := m.cfg.Save(); err != nil {
			m.append(errStyle.Render("config save failed: " + err.Error()))
		}
		return
	}
	if _, ok := m.cfg.Models[args[0]]; !ok {
		m.append(errStyle.Render("unknown model " + args[0]))
		return
	}
	m.compactModel = args[0]
	m.compactProv = ""
	if len(args) > 1 {
		m.compactProv = args[1]
	}
	m.applyCompactModel()
	if m.agent.CompactModel == "" { // resolve failed; don't persist a broken pick
		m.compactModel, m.compactProv = "", ""
		return
	}
	m.cfg.CompactModel, m.cfg.CompactProvider = m.compactModel, m.compactProv
	if err := m.cfg.Save(); err != nil {
		m.append(errStyle.Render("config save failed: " + err.Error()))
	}
}

// compactPct returns the live threshold percent (the default when unset).
// cfg.CompactPct is the authoritative value; the agent's float is derived.
func (m *model) compactPct() int {
	if m.cfg == nil {
		return config.DefaultCompactPct
	}
	pct := m.cfg.CompactPct
	if pct == 0 {
		pct = config.DefaultCompactPct
	}
	return min(max(pct, 10), 90)
}

// setCompactPct applies a compaction-threshold percent (clamped 10–90): the
// agent compacts proactively once the estimated context use crosses it.
// Persisted as the new default. settings-driven, so no transcript note — the
// row's [NN%] badge is the feedback (same as the effort/theme steppers).
func (m *model) setCompactPct(pct int) {
	if m.agent == nil {
		m.append(m.degradedProviderNote())
		return
	}
	pct = min(max(pct, 10), 90)
	m.agent.CompactThreshold = float64(pct) / 100
	m.cfg.CompactPct = pct
	if err := m.cfg.Save(); err != nil {
		m.append(errStyle.Render("config save failed: " + err.Error()))
	}
}

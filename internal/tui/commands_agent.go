package tui

import (
	"context"
	"errors"
	"fmt"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/sacca97/ghg/internal/agent"
	"github.com/sacca97/ghg/internal/auth"
	"github.com/sacca97/ghg/internal/config"
	"github.com/sacca97/ghg/internal/models"
	workerwire "github.com/sacca97/ghg/internal/worker"

	"os"
	"os/exec"
	"strings"
	"time"
)

// /auth is profile-driven onboarding. The bare form lists available profiles;
// /auth <id> opens the masked input prompt, while /auth <id> <key> accepts a
// direct key for users who prefer shell paste. All validation and persistence
// details live in the shared auth package so the CLI and TUI cannot drift.
func (m *model) authCommand(args []string) {
	if len(args) == 0 {
		m.listAuthProfiles()
		return
	}

	name := strings.TrimSpace(args[0])
	resolved, err := auth.ResolveProfile(m.profiles, name)
	if err != nil {
		m.append(errStyle.Render(err.Error()))
		return
	}
	if resolved.RequiresOAuth() {
		m.startOAuthLogin(name, resolved)
		return
	}
	if !resolved.RequiresAPIKey() {
		m.append(errStyle.Render(fmt.Sprintf("provider %q takes no API key", name)))
		return
	}

	if len(args) > 1 {
		m.authProvider(name, resolved, config.TrimKey(strings.Join(args[1:], "")), false)
		return
	}
	m.openNamePrompt("🔑 "+resolved.Profile.DisplayName+" API key (masked, enter to save, esc cancels):", "", func(key string) {
		key = config.TrimKey(key)
		if key == "" {
			m.append(dimStyle.Render("auth cancelled"))
			return
		}
		m.authProvider(name, resolved, key, false)
	})
	m.namePrompt.mask = true
}

func (m *model) listAuthProfiles() {
	ids := m.profiles.IDs()
	if len(ids) == 0 {
		m.append(dimStyle.Render("no provider profiles available"))
		return
	}
	m.append(dimStyle.Render("provider profiles:"))
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
		m.append(dimStyle.Render(fmt.Sprintf("  %s — %s (%s)", id, profile.DisplayName, status)))
	}
	m.append(dimStyle.Render("use /auth <provider> [key] — bare provider prompts for a masked key"))
}

func (m *model) authConfigured(id string) bool {
	for name, configured := range m.cfg.Providers {
		resolved, err := m.profiles.Resolve(config.ProviderInstance(name, configured))
		if err != nil || resolved.Profile.ID != id {
			continue
		}
		if resolved.RequiresOAuth() {
			st, err := auth.DefaultCodexCredentialManager().Status(context.Background())
			return err == nil && st.Configured && !st.Expired
		}
		if !resolved.RequiresAPIKey() {
			return true
		}
		if key, err := configured.ResolveKey(); err == nil && key != "" {
			return true
		}
	}
	return false
}

// authResultMsg carries a finished profile validation back to the UI
// goroutine. The key exists only in this short-lived message and is never
// appended to the transcript or passed to event logging.
type authResultMsg struct {
	name        string
	profile     models.Resolved
	key         string
	envMode     bool
	models      []models.ModelInfo
	validated   bool
	unvalidated bool
	confirmed   bool
	catalogErr  error
	err         error
}

// authProvider validates a profile in the background, then persists the
// profile-derived provider entry and hot-swaps the current route.
func (m *model) authProvider(name string, resolved models.Resolved, key string, envMode bool) {
	if key == "" && !envMode {
		m.append(errStyle.Render(fmt.Sprintf("/auth %s needs a key (%s)", name, auth.KeyHint(resolved))))
		return
	}
	m.append(dimStyle.Render("validating key against " + resolved.Profile.DisplayName + "…"))
	if m.prog == nil {
		return // tests drive applyAuthResult directly; no program to report to
	}
	go func() {
		result, err := auth.Authenticate(context.Background(), m.profiles, name, key, m.cfg.MaxRetries)
		msg := authResultMsg{
			name:        name,
			profile:     resolved,
			key:         key,
			envMode:     envMode,
			validated:   result.Validated,
			unvalidated: result.NeedsConfirmation,
			catalogErr:  result.CatalogErr,
			err:         err,
		}
		if result.Name != "" {
			msg.name = result.Name
		}
		if result.Profile.Profile.ID != "" {
			msg.profile = result.Profile
		}
		msg.models = result.Models
		sendProg(m.prog, msg)
	}()
}

// applyAuthResult commits a validated auth result. Unvalidated credentials
// require a separate explicit confirmation prompt; that prompt is unmasked
// because it accepts only yes/no and never the credential itself.
func (m *model) applyAuthResult(res authResultMsg) {
	if res.err != nil {
		m.append(errStyle.Render(fmt.Sprintf("%s rejected the key: %s", res.name, res.err)))
		return
	}
	if res.unvalidated && !res.confirmed {
		if m.prog == nil {
			m.append(errStyle.Render(fmt.Sprintf("%s could not be validated; key not stored", res.name)))
			return
		}
		pending := res
		m.openNamePrompt(fmt.Sprintf("⚠ %s could not be validated; type yes to store it:", res.name), "", func(answer string) {
			if !strings.EqualFold(strings.TrimSpace(answer), "yes") {
				m.append(dimStyle.Render(fmt.Sprintf("%s not configured", pending.name)))
				return
			}
			pending.confirmed = true
			m.applyAuthResult(pending)
		})
		return
	}

	if res.name == "" {
		m.append(errStyle.Render("auth result has no provider profile"))
		return
	}
	if res.profile.Profile.ID == "" {
		resolved, err := auth.ResolveProfile(m.profiles, res.name)
		if err != nil {
			m.append(errStyle.Render(err.Error()))
			return
		}
		res.profile = resolved
	}
	infos := res.models
	if m.prog == nil {
		infos = nil
	}
	catalogSeeded := m.prog != nil && len(infos) > 0
	if err := auth.CommitCredential(m.cfg, res.name, res.profile, res.key, res.envMode, infos); err != nil {
		var catalogErr *auth.CatalogError
		if errors.As(err, &catalogErr) {
			catalogSeeded = false
			m.append(dimStyle.Render("(catalog cache write failed; /model refresh will retry)"))
		} else {
			m.append(errStyle.Render("auth failed: " + err.Error()))
			return
		}
	}
	if catalogSeeded {
		m.catalogs = config.LoadCatalogs()
		m.updateCatalogs(m.catalogs)
	}

	// A cold TUI has no agent to rebuild. Promote it to the first live agent
	// from the freshly seeded catalog (or the configured default model when a
	// catalog was not returned). If roles are configured, prefer the current
	// mode's role when it belongs to the provider just authenticated. A running
	// session only accepts auth refreshes for its current provider, preserving
	// its conversation and route.
	modelName, roleName := m.authRoute(res.name, res.models, catalogSeeded)
	if modelName == "" {
		m.append(dimStyle.Render(fmt.Sprintf("✓ %s configured — choose a model with /model before starting a turn", res.name)))
		return
	}
	if roleName == "" {
		m.cfg.DefaultModel = modelName
		m.cfg.DefaultProvider = res.name
		if err := m.cfg.Save(); err != nil {
			m.append(errStyle.Render("config save failed: " + err.Error()))
			return
		}
	}
	route, err := resolveDisplayRoute(m.cfg, m.profiles, modelName, res.name, roleName)
	if err != nil {
		m.append(errStyle.Render("provider configured but route could not be selected: " + err.Error()))
		return
	}
	m.modelName, m.provName, m.modelID = route.ModelName, route.ProviderName, route.APIID
	m.protocol, m.role, m.contextLimit = route.Protocol, route.Role, route.ContextLimit
	m.effort = m.maxEffort()
	m.modelSlotW = m.statusModelSlotWidth()
	if m.workerClient != nil {
		m.syncWorkerConfiguration(true)
	}
	if res.catalogErr != nil {
		m.append(dimStyle.Render("(catalog prefetch failed; /model refresh will retry)"))
	}

	switch {
	case res.unvalidated:
		m.append(dimStyle.Render(fmt.Sprintf("✓ %s configured — key stored unvalidated", res.name)))
	case len(res.models) > 0:
		m.append(dimStyle.Render(fmt.Sprintf("✓ %s configured — %d models in the catalog; /model lists them", res.name, len(res.models))))
	default:
		m.append(dimStyle.Render(fmt.Sprintf("✓ %s configured — credentials validated", res.name)))
	}
}

func firstCatalogModel(models []models.ModelInfo) string {
	for _, model := range models {
		if id := strings.TrimSpace(model.ID); id != "" {
			return id
		}
	}
	return ""
}

func (m *model) authRoute(provider string, infos []models.ModelInfo, catalogSeeded bool) (modelName, roleName string) {
	if len(m.cfg.Roles) > 0 {
		if target, err := m.cfg.ResolveRole(m.modeRole()); err == nil && target.Model != "" && target.Provider == provider {
			return target.Model, target.Role
		}
	}
	if catalogSeeded {
		modelName = firstCatalogModel(infos)
	}
	if modelName == "" {
		modelName = m.cfg.DefaultModel
	}
	return modelName, ""
}

type authOAuthWaitingMsg struct {
	name string
	url  string
}

type authOAuthResultMsg struct {
	name     string
	profile  models.Resolved
	models   []models.ModelInfo
	err      error
	modelErr error
}

func (m *model) startOAuthLogin(name string, resolved models.Resolved) {
	m.append(dimStyle.Render("starting OAuth login for " + resolved.Profile.DisplayName + "…"))
	if m.prog == nil {
		return
	}
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
		defer cancel()
		opts := auth.LoginOptions{
			OpenBrowser: true,
			Printer: func(url string) {
				sendProg(m.prog, authOAuthWaitingMsg{name: name, url: url})
			},
		}
		_, err := auth.Login(ctx, opts)
		if err != nil {
			sendProg(m.prog, authOAuthResultMsg{name: name, profile: resolved, err: err})
			return
		}
		var modelInfos []models.ModelInfo
		var modelErr error
		backend, berr := auth.NewBackend(resolved, "", "", 0)
		if berr != nil {
			modelErr = berr
		} else if cat, ok := backend.(models.CatalogBackend); ok {
			mctx, mcancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer mcancel()
			modelInfos, modelErr = cat.Models(mctx)
		}
		sendProg(m.prog, authOAuthResultMsg{
			name:     name,
			profile:  resolved,
			models:   modelInfos,
			modelErr: modelErr,
		})
	}()
}

func (m *model) applyOAuthResult(res authOAuthResultMsg) {
	if res.err != nil {
		m.append(errStyle.Render(fmt.Sprintf("%s OAuth login failed: %s", res.name, res.err)))
		return
	}
	if err := m.cfg.UpsertOAuthProvider(res.name, res.profile); err != nil {
		m.append(errStyle.Render("config update failed: " + err.Error()))
		return
	}
	if err := m.cfg.Save(); err != nil {
		m.append(errStyle.Render("config save failed: " + err.Error()))
		return
	}

	catalogSeeded := false
	if len(res.models) > 0 {
		if err := config.SaveCatalog(res.name, res.profile.BaseURL, res.models); err != nil {
			m.append(dimStyle.Render("(catalog cache write failed; /model refresh will retry)"))
		} else {
			catalogSeeded = true
			m.catalogs = config.LoadCatalogs()
			m.updateCatalogs(m.catalogs)
		}
	}

	if catalogSeeded {
		m.append(dimStyle.Render(fmt.Sprintf("✓ %s configured — %d models added to the catalog.", res.profile.Profile.DisplayName, len(res.models))))
	} else {
		m.append(dimStyle.Render(fmt.Sprintf("✓ %s configured.", res.profile.Profile.DisplayName)))
		if res.modelErr != nil {
			m.append(errStyle.Render(fmt.Sprintf("model discovery failed: %s — run /model refresh to retry", res.modelErr)))
		}
	}
	m.refreshMenu()
}

// goalMaxRounds resolves the goal-loop round cap: per-project override
// (~/.ghg/projects.json, keyed by cwd) beats the global default
// (goalMaxRounds in ~/.ghg/config.json), which falls back to
// config.DefaultGoalMaxRounds. Set either with /goal rounds.
func (m *model) goalMaxRounds() int {
	if wd, err := os.Getwd(); err == nil {
		if n := config.ProjectGoalMaxRounds(wd); n > 0 {
			return n
		}
	}
	if m.cfg != nil && m.cfg.GoalMaxRounds > 0 {
		return m.cfg.GoalMaxRounds
	}
	return config.DefaultGoalMaxRounds
}

// currentGoalRecord returns the authoritative in-memory goal.
func (m *model) currentGoalRecord() (agent.GoalRecord, bool) {
	if m.goalRecord == nil {
		return agent.GoalRecord{}, false
	}
	return *m.goalRecord, true
}

func (m *model) currentGoal() string {
	if m.goalRecord != nil && m.goalRecord.Status == agent.GoalStatusActive {
		return m.goalRecord.Objective
	}
	return ""
}

func (m *model) applyGoalRecord(record agent.GoalRecord) {
	copy := record
	m.goalRecord = &copy
}

func (m *model) goalRecordForSession() (agent.GoalRecord, bool) {
	record, ok := m.currentGoalRecord()
	if !ok {
		return agent.GoalRecord{}, false
	}
	if record.CreatedAt.IsZero() {
		record.CreatedAt = m.nowFn()
	}
	if record.UpdatedAt.IsZero() {
		record.UpdatedAt = record.CreatedAt
	}
	return record, true
}

// applyGoalUpdate applies a worker checkpoint to the in-memory projection. The
// update is accepted only while the same goal ID is active;
// clearing a goal while a request is in flight therefore wins over a late
// model callback.
func (m *model) applyGoalUpdate(update agent.GoalUpdate) bool {
	record, ok := m.goalRecordForSession()
	if !ok || record.Status != agent.GoalStatusActive {
		return false
	}
	before := record
	accepted, err := agent.ApplyUpdate(&record, update)
	if err != nil {
		m.append(errStyle.Render("invalid goal update: " + err.Error()))
		return false
	}
	if !accepted {
		return false
	}
	if record.Status == before.Status && record.Progress == before.Progress && record.Blocker == before.Blocker {
		return true
	}
	record.UpdatedAt = m.nowFn().UTC()
	m.applyGoalRecord(record)
	return true
}

// goalRoundsCommand implements /goal rounds: bare reports the effective cap
// and where it comes from, a number sets the per-project override (--global
// sets the config default instead), and "default" clears the override.
func (m *model) goalRoundsCommand(args []string) {
	global := false
	var num string
	for _, a := range args {
		if a == "--global" || a == "-g" {
			global = true
		} else if num == "" {
			num = a
		} else {
			m.append(errStyle.Render("usage: /goal rounds [n|default] [--global]"))
			return
		}
	}
	wd, _ := os.Getwd()
	proj := config.ProjectGoalMaxRounds(wd)
	cfgN := 0
	if m.cfg != nil {
		cfgN = m.cfg.GoalMaxRounds
	}

	switch num {
	case "":
		src := fmt.Sprintf("built-in default (%d)", config.DefaultGoalMaxRounds)
		if proj > 0 {
			src = "project override"
		} else if cfgN > 0 {
			src = "global config"
		}
		m.append(dimStyle.Render(fmt.Sprintf("◎ goal rounds: %d (%s) — /goal rounds <n>|default [--global]", m.goalMaxRounds(), src)))
		return
	case "default":
		// clear
	default:
		n := 0
		if _, err := fmt.Sscan(num, &n); err != nil || n <= 0 {
			m.append(errStyle.Render("rounds must be a positive number (or \"default\")"))
			return
		}
		if global {
			m.cfg.GoalMaxRounds = n
			if err := m.cfg.Save(); err != nil {
				m.append(errStyle.Render("couldn't save config: " + err.Error()))
				return
			}
			m.append(dimStyle.Render(fmt.Sprintf("◎ global goal rounds: %d%s", n, overriddenNote(proj))))
			return
		}
		if err := config.SetProjectGoalMaxRounds(wd, n); err != nil {
			m.append(errStyle.Render("couldn't save project override: " + err.Error()))
			return
		}
		m.append(dimStyle.Render(fmt.Sprintf("◎ goal rounds for this project: %d", n)))
		return
	}

	// "default": clear the override at the chosen scope
	if global {
		m.cfg.GoalMaxRounds = 0
		if err := m.cfg.Save(); err != nil {
			m.append(errStyle.Render("couldn't save config: " + err.Error()))
			return
		}
		m.append(dimStyle.Render(fmt.Sprintf("◎ global goal rounds reset to %d%s", config.DefaultGoalMaxRounds, overriddenNote(proj))))
		return
	}
	if err := config.SetProjectGoalMaxRounds(wd, 0); err != nil {
		m.append(errStyle.Render("couldn't save project override: " + err.Error()))
		return
	}
	m.append(dimStyle.Render(fmt.Sprintf("◎ project goal rounds cleared — using %d", m.goalMaxRounds())))
}

// overriddenNote flags when a project override still wins over a global change.
func overriddenNote(proj int) string {
	if proj > 0 {
		return fmt.Sprintf(" (this project overrides it with %d)", proj)
	}
	return ""
}

// planCommand switches to Plan mode and submits the goal as an ordinary turn.
func (m *model) planCommand(text string) (tea.Model, tea.Cmd) {
	goal := strings.TrimSpace(strings.TrimPrefix(text, "/plan"))
	if err := m.setMode(uiModePlan); err != nil {
		m.append(errStyle.Render("plan mode failed: " + err.Error()))
		return m, nil
	}
	if goal == "" {
		m.append(dimStyle.Render("switched to plan mode (read-only exploration)"))
		return m, nil
	}
	return m.submitTurn(goal, true)
}

// executeCommand runs a supplied plan, or the most recent /plan proposal,
// through the fast role in Execute mode.
func (m *model) executeCommand(text string) (tea.Model, tea.Cmd) {
	if m.busy {
		m.append(dimStyle.Render("(busy — /execute after this turn)"))
		return m, nil
	}
	if !m.requireAgent() {
		return m, nil
	}
	arg := strings.TrimSpace(strings.TrimPrefix(text, "/execute"))
	var planMD string
	if arg == "" {
		if m.proposedPlanMD == "" {
			m.append(errStyle.Render("no plan to execute — use /plan <goal> or /execute <plan>"))
			return m, nil
		}
		planMD = m.proposedPlanMD
	} else {
		planMD = arg
	}

	if err := m.setMode(uiModeExecute); err != nil {
		m.append(errStyle.Render("execute mode failed: " + err.Error()))
		return m, nil
	}

	prompt := fmt.Sprintf("Execute the following approved plan. Create and maintain a todowrite\nchecklist while implementing it.\n\n%s", planMD)
	return m.submitTurn(prompt, true)
}

// reviewCommand runs a one-shot, read-only review using the current model.
func (m *model) reviewCommand(text string) (tea.Model, tea.Cmd) {
	if m.busy {
		m.append(dimStyle.Render("(busy — /review after this turn)"))
		return m, nil
	}
	target := strings.TrimSpace(strings.TrimPrefix(text, "/review"))
	if target == "" {
		m.append(dimStyle.Render("usage: /review <target or instructions>"))
		return m, nil
	}
	if !m.requireAgent() {
		return m, nil
	}

	m.reviewing = true
	return m.submitTurn(target, true)
}

// askCommand answers a question with read-only repository access.
func (m *model) askCommand(text string) (tea.Model, tea.Cmd) {
	if m.busy {
		m.append(dimStyle.Render("(busy — /ask after this turn)"))
		return m, nil
	}
	question := strings.TrimSpace(strings.TrimPrefix(text, "/ask"))
	if question == "" {
		m.append(dimStyle.Render("usage: /ask <question>"))
		return m, nil
	}
	if !m.requireAgent() {
		return m, nil
	}
	return m.submitAsk(question)
}

// /me — open ~/.ghg/me.md in $EDITOR. The file is appended to every
// session's system prompt (the built-in operating rules stay — they carry
// the safety rails), so this is the user's standing-instructions surface.
// tea.ExecProcess suspends the renderer for the edit, then resumes.
func (m *model) openMe() tea.Cmd {
	path := config.MePath()
	if path == "" {
		m.append(errStyle.Render("/me: cannot locate ~/.ghg"))
		return nil
	}
	editor := os.Getenv("VISUAL")
	if editor == "" {
		editor = os.Getenv("EDITOR")
	}
	if editor == "" {
		editor = "vi"
	}
	m.append(dimStyle.Render("editing " + path + " — save and quit to apply (next turn picks it up)"))
	c := exec.Command(editor, path)
	c.Stdin, c.Stdout, c.Stderr = os.Stdin, os.Stdout, os.Stderr
	return tea.ExecProcess(c, func(err error) tea.Msg {
		return meEditedMsg{path, err}
	})
}

type meEditedMsg struct {
	path string
	err  error
}

// setGoal creates a fresh structured goal or records an explicit user clear.
// A cleared goal remains in the ledger as paused so its ID, progress, and
// blocker history are not silently discarded.
func (m *model) setGoal(objective string) {
	objective = strings.TrimSpace(objective)
	if m.workerClient == nil && !m.ensureWorker() {
		m.append(errStyle.Render("goal: worker unavailable: " + m.workerStartError))
		return
	}
	if objective == "" {
		if record, ok := m.goalRecordForSession(); ok {
			record.Status = agent.GoalStatusPaused
			record.Progress = ""
			record.Blocker = "cleared by user"
			record.UpdatedAt = m.nowFn().UTC()
			m.sendWorkerGoal(record, "clear")
		} else {
			m.goalRecord = nil
			if m.store != nil && m.sessionID != "" {
				_ = m.store.ClearGoal(m.sessionID)
			}
		}
		return
	}
	record := agent.NewGoal(objective)
	if err := record.Validate(); err != nil {
		m.append(errStyle.Render("goal: " + err.Error()))
		return
	}
	m.applyGoalRecord(record)
	m.sendWorkerGoal(record, "set")
}

func (m *model) sendWorkerGoal(record agent.GoalRecord, action string) {
	if m.workerClient == nil {
		return
	}
	if err := m.workerClient.Send(workerwire.CommandGoal, workerRequestID("goal"), workerwire.GoalRequest{Action: action, Record: &record}); err != nil {
		m.append(errStyle.Render("goal: worker: " + err.Error()))
	}
}

// resumeGoal is the only path that turns a persisted non-active goal back into
// active work. This keeps process restart and blocked/limited goals explicit.
func (m *model) resumeGoal() bool {
	record, ok := m.goalRecordForSession()
	if !ok {
		m.append(errStyle.Render("no goal to resume — set one with /goal <text>"))
		return false
	}
	if record.Status == agent.GoalStatusComplete {
		m.append(dimStyle.Render("goal is complete — set a new goal with /goal <text>"))
		return false
	}
	record.Status = agent.GoalStatusActive
	record.Blocker = ""
	record.UpdatedAt = m.nowFn().UTC()
	m.applyGoalRecord(record)
	m.sendWorkerGoal(record, "resume")
	m.append(dimStyle.Render("◎ resuming goal " + record.ID + ": " + record.Objective))
	return true
}

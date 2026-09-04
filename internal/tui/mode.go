package tui

import (
	"errors"
	"fmt"
	"slices"
	"strings"

	"github.com/sacca97/ghg/internal/config"
	"github.com/sacca97/ghg/internal/models"
)

const (
	uiModePlan    = "plan"
	uiModeExecute = "execute"
)

var modelRoleLabels = []string{config.RoleDefault, config.RoleSmart, config.RoleFast, config.RoleTiny}

type displayRoute struct {
	ModelName    string
	ProviderName string
	APIID        string
	Protocol     string
	Role         string
	Effort       string
	ContextLimit int
}

func resolveDisplayRoute(cfg *config.Config, profiles models.Profiles, modelName, providerName, role string) (displayRoute, error) {
	if cfg == nil {
		return displayRoute{}, errors.New("configuration unavailable")
	}
	if modelName == "" && providerName == "" {
		target, err := cfg.ResolveRole(role)
		if err != nil {
			return displayRoute{}, err
		}
		modelName, providerName = target.Model, target.Provider
	}
	route := displayRoute{ModelName: modelName, ProviderName: providerName, Role: role, Effort: cfg.DefaultEffort}
	if route.Effort == "" {
		route.Effort = "medium"
	}
	if modelName == "" {
		return route, nil
	}
	resolved, err := cfg.Resolve(modelName, providerName)
	if err != nil {
		return displayRoute{}, err
	}
	route.ModelName = resolved.ModelName
	route.ProviderName = resolved.ProviderName
	route.APIID = resolved.APIID
	route.Protocol = resolved.Provider.API
	if profile, profileErr := profiles.ResolveModel(models.Instance{
		Name: resolved.ProviderName, Profile: resolved.Provider.Profile,
		BaseURL: resolved.Provider.BaseURL, Protocol: models.Protocol(resolved.Provider.API),
	}, resolved.APIID); profileErr == nil {
		route.Protocol = string(profile.Protocol)
	}
	if cat, ok := config.LoadCatalogs()[route.ProviderName]; ok {
		route.ContextLimit = cat.ContextLength(route.APIID)
	}
	if route.ContextLimit <= 0 {
		route.ContextLimit = resolved.Model.ContextWindow()
	}
	if route.ContextLimit <= 0 {
		route.ContextLimit = config.LoadModelsDev().ContextLength(route.APIID, resolved.Provider.Profile, route.ProviderName)
	}
	return route, nil
}

func (m *model) uiMode() string {
	if m.mode == uiModePlan {
		return uiModePlan
	}
	return uiModeExecute
}

func (m *model) modeRole() string {
	if m.uiMode() == uiModePlan {
		return config.RoleSmart
	}
	return config.RoleFast
}

func roleConfigName(label string) string {
	if label == uiModePlan {
		return config.RoleSmart
	}
	return label
}

func (m *model) activeRoleLabel() string {
	if m.role != "" {
		if slices.Contains(modelRoleLabels, m.role) {
			return m.role
		}
	}
	return config.RoleDefault
}

// setMode changes the user-visible operating mode without changing the active
// model or role.
func (m *model) setMode(mode string) error {
	mode = strings.ToLower(strings.TrimSpace(mode))
	if mode != uiModePlan && mode != uiModeExecute {
		return fmt.Errorf("unknown mode %q (want plan or execute)", mode)
	}
	m.mode = mode
	m.syncWorkerConfiguration(false)
	return nil
}

// switchRole replaces the live route while preserving the conversation and
// cumulative usage. Unlike /model, this is an execution detail and does not
// rewrite the user's configured default route.
func (m *model) switchRole(role string) error {
	target, err := m.roleRoute(role)
	if err != nil {
		return err
	}
	return m.activateRoute(target.Model, target.Provider, role)
}

// activateRoute installs a concrete role route while preserving the current
// conversation and session-local state. It deliberately does not rewrite the
// configured default route: choosing a role model edits that role only.
func (m *model) activateRoute(modelName, providerName, role string) error {
	if m.workerClient != nil && m.workerLiveWork {
		return errors.New("worker is busy; change the model after this work finishes")
	}
	route, err := resolveDisplayRoute(m.cfg, m.profiles, modelName, providerName, role)
	if err != nil {
		return err
	}
	m.modelName, m.provName, m.modelID = route.ModelName, route.ProviderName, route.APIID
	m.protocol, m.role, m.contextLimit = route.Protocol, role, route.ContextLimit
	m.effort = m.maxEffort()
	m.modelSlotW = m.statusModelSlotWidth()
	m.syncWorkerConfiguration(true)
	return nil
}

// roleRoute returns the concrete route currently configured for the user-
// facing role label. It uses the same fallback rules as agent construction.
func (m *model) roleRoute(label string) (config.ResolvedRole, error) {
	if m.cfg == nil {
		return config.ResolvedRole{}, fmt.Errorf("configuration unavailable")
	}
	return m.cfg.ResolveRole(roleConfigName(label))
}

func (m *model) roleModelPanel(label string, direct bool) *ppanel {
	items := m.availableModelItems()
	items = annotateModelAvailability(m.cfg, m.profiles, items)
	pp := &ppanel{
		kind:       panelModel,
		title:      label + " model",
		items:      items,
		role:       label,
		staleHints: staleCatalogs(m.cfg, config.LoadCatalogs()),
		direct:     direct,
	}
	if target, err := m.roleRoute(label); err == nil {
		for i, it := range items {
			if it.model == target.Model && it.provider == target.Provider {
				pp.idx = i
				break
			}
		}
	}
	return pp
}

func (m *model) modelRolePanel(direct bool) *ppanel {
	idx := 0
	active := m.activeRoleLabel()
	for i, label := range modelRoleLabels {
		if label == active {
			idx = i
			break
		}
	}
	return &ppanel{
		kind:   panelRole,
		title:  "Model role",
		list:   slices.Clone(modelRoleLabels),
		midx:   idx,
		direct: direct,
	}
}

func (m *model) modePanel(direct bool) *ppanel {
	modes := []string{uiModePlan, uiModeExecute}
	idx := 0
	if m.uiMode() == uiModeExecute {
		idx = 1
	}
	return &ppanel{kind: panelMode, title: "Mode", list: modes, midx: idx, direct: direct}
}

// cycleStatusModel advances through the routes already selected for the four
// role slots. The active operating mode is deliberately left untouched: mode
// and model are independent bottom-bar controls. The role-first picker remains
// available from ctrl+p or /model for changing a slot's configured route.
func (m *model) cycleStatusModel() {
	roles := []string{config.RoleSmart, config.RoleDefault, config.RoleFast, config.RoleTiny}
	current := m.activeRoleLabel()
	idx := -1
	for i, label := range roles {
		if label == current {
			idx = i
			break
		}
	}
	if idx < 0 {
		idx = 0
	}

	var lastErr error
	for step := 1; step <= len(roles); step++ {
		label := roles[(idx+step)%len(roles)]
		target, err := m.roleRoute(label)
		if err != nil {
			lastErr = err
			continue
		}
		if target.Model == "" {
			lastErr = fmt.Errorf("role %q has no configured model", roleConfigName(label))
			continue
		}
		if err := m.activateRoute(target.Model, target.Provider, roleConfigName(label)); err != nil {
			lastErr = err
			continue
		}
		return
	}
	if lastErr != nil {
		m.append(errStyle.Render("model: " + lastErr.Error()))
	}
}

// cycleStatusMode advances the two visible operating modes.
func (m *model) cycleStatusMode() {
	next := uiModePlan
	if m.uiMode() == uiModePlan {
		next = uiModeExecute
	}
	if err := m.setMode(next); err != nil {
		m.append(errStyle.Render("mode: " + err.Error()))
	}
}

func (m *model) selectRoleModel(label string, item modelItem) error {
	if m.cfg == nil {
		return fmt.Errorf("configuration unavailable")
	}
	role := roleConfigName(label)
	if !config.IsRole(role) {
		return fmt.Errorf("unknown model role %q", label)
	}
	if m.cfg.Roles == nil {
		m.cfg.Roles = make(map[string]config.RoleConfig)
	}
	previous, hadPrevious := m.cfg.Roles[role]
	m.cfg.Roles[role] = config.RoleConfig{Model: item.model, Provider: item.provider}
	if err := m.cfg.Save(); err != nil {
		if hadPrevious {
			m.cfg.Roles[role] = previous
		} else {
			delete(m.cfg.Roles, role)
		}
		return fmt.Errorf("config save failed: %w", err)
	}

	if err := m.activateRoute(item.model, item.provider, role); err != nil {
		return err
	}
	return nil
}

// defaultEfforts are the fallback levels when the provider doesn't advertise
// supported reasoning efforts; "" means off (parameter omitted from requests).
var defaultEfforts = []string{"", "low", "medium", "high"}

// effortCands completes /effort for models without advertised levels.
var effortCands = []cand{
	{"off", "No reasoning effort parameter sent"},
	{"low", "Fast, shallow reasoning"},
	{"medium", "Balanced reasoning"},
	{"high", "Deep reasoning, slower"},
}

func (m *model) currentEffort() string {
	return m.effort
}

// effortsFor returns the cycle of effort levels available for the current
// model. A known models.dev/provider surface is authoritative: its effort
// values are returned verbatim (with "none" folded into off), a toggle-only
// surface becomes off/on, and an explicitly empty surface becomes off only.
// Unknown models retain the legacy low/medium/high fallback.
func (m *model) effortsFor() []string {
	if c, ok := m.catalogs[m.provName]; ok {
		apiID := m.currentModelID()
		for _, mi := range c.Models {
			if mi.ID != apiID {
				continue
			}
			if !mi.ReasoningKnown && len(mi.ReasoningEfforts) == 0 && !mi.ReasoningToggle {
				break // the model has no reasoning metadata yet: use defaults
			}
			out := []string{""}
			if mi.ReasoningToggle && len(mi.ReasoningEfforts) == 0 {
				return append(out, "on")
			}
			for _, e := range mi.ReasoningEfforts {
				e = strings.TrimSpace(e)
				if strings.EqualFold(e, "none") || strings.EqualFold(e, "off") || e == "" {
					continue // "none"/"off" are our off ("")
				}
				if !slices.Contains(out, e) {
					out = append(out, e)
				}
			}
			return out
		}
	}
	return defaultEfforts
}

// maxEffort returns the highest supported reasoning effort for the current model.
// effortsFor orders levels ascending with "" (off) first, so the last element
// is always the maximum supported effort.
func (m *model) maxEffort() string {
	levels := m.effortsFor()
	if len(levels) == 0 {
		return ""
	}
	return levels[len(levels)-1]
}

// nextEffort cycles cur to the following level in levels, wrapping; an
// unknown cur resets to levels[0].
func nextEffort(levels []string, cur string) string {
	for i, e := range levels {
		if e == cur {
			return levels[(i+1)%len(levels)]
		}
	}
	return levels[0]
}

// effortLabel renders a level for display ("" shows as off).
func effortLabel(e string) string {
	if e == "" {
		return "off"
	}
	return e
}

// parseEffort validates user input against levels ("off" maps to "").
func parseEffort(levels []string, s string) (string, bool) {
	if s == "off" {
		return "", true
	}
	for _, e := range levels[1:] {
		if s == e {
			return e, true
		}
	}
	return "", false
}

// effortCandsFor builds /effort completion candidates from levels.
func effortCandsFor(levels []string) []cand {
	out := make([]cand, 0, len(levels))
	for _, e := range levels {
		out = append(out, cand{effortLabel(e), ""})
	}
	return out
}

// updateCatalogs replaces the cached catalogs (called when the background
// fetch completes).
func (m *model) updateCatalogs(cats map[string]config.Catalog) {
	m.catalogs = cats
	m.contextLimit = m.contextLimitFor(m.provName, m.currentModelID())
	if !slices.Contains(m.effortsFor(), m.effort) {
		m.resetEffort("")
		m.append(dimStyle.Render("⚡ effort reset to off: not supported by " + m.modelName))
	}
}

// setEffort changes the reasoning effort and stores it both ways: as the new
// global default (every future session starts here) and on the live session
// row (resuming this conversation restores it). "" = off. Callers that only
// reconcile state (model switch / catalog refresh dropping an unsupported
// level) use resetEffort instead so a quiet reconciliation never rewrites
// the user's chosen global default.
func (m *model) setEffort(lv string) {
	if m.workerClient != nil && m.workerLiveWork {
		m.append(dimStyle.Render("(worker is busy — change reasoning after this work finishes)"))
		return
	}
	m.effort = lv
	m.cfg.DefaultEffort = lv
	_ = m.saveConfig()
	m.syncWorkerConfiguration(true)
}

// resetEffort applies a level without touching the global default.
func (m *model) resetEffort(lv string) {
	m.effort = lv
	if !m.workerLiveWork {
		m.syncWorkerConfiguration(true)
	}
}

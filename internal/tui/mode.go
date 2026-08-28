package tui

import (
	"fmt"
	"strings"

	"github.com/sacca97/ghg/internal/agent"
	"github.com/sacca97/ghg/internal/config"
)

const (
	uiModePlan    = "plan"
	uiModeExecute = "execute"
)

var modelRoleLabels = []string{"default", "plan", "fast", "tiny"}

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

func roleLabel(name string) string {
	if name == config.RoleSmart {
		return uiModePlan
	}
	return name
}

func (m *model) activeRoleLabel() string {
	if m.agent != nil {
		if label := roleLabel(m.agent.Role); contains(modelRoleLabels, label) {
			return label
		}
	}
	return m.modeRoleLabel()
}

func (m *model) modeRoleLabel() string {
	if m.uiMode() == uiModePlan {
		return uiModePlan
	}
	return config.RoleFast
}

// setMode changes the user-visible operating mode and activates its default
// role. A cold-start TUI can choose a mode before /auth creates the first
// agent; the selected mode is then honored by auth promotion.
func (m *model) setMode(mode string) error {
	mode = strings.ToLower(strings.TrimSpace(mode))
	if mode != uiModePlan && mode != uiModeExecute {
		return fmt.Errorf("unknown mode %q (want plan or execute)", mode)
	}
	if m.agent != nil {
		if err := m.switchRole(map[string]string{
			uiModePlan:    config.RoleSmart,
			uiModeExecute: config.RoleFast,
		}[mode]); err != nil {
			return err
		}
	}
	m.mode = mode
	return nil
}

// activateRoute installs a concrete role route while preserving the current
// conversation and session-local state. It deliberately does not rewrite the
// configured default route: choosing a role model edits that role only.
func (m *model) activateRoute(modelName, providerName, role string) error {
	if m.cfg == nil {
		if m.agent == nil {
			return fmt.Errorf("no agent configured")
		}
		m.agent.Role = role
		return nil
	}
	ag, mn, pn, err := buildAgentWithProfiles(m.cfg, modelName, providerName, m.sysPrompt, m.profiles)
	if err != nil {
		return err
	}
	if ag == nil {
		return fmt.Errorf("role %q has no configured agent", role)
	}
	ag.Role = role
	ag.ReasoningToggle = m.reasoningToggleFor(pn, ag.Model)
	if old := m.agent; old != nil {
		ag.Effort = old.Effort
		if msgs := old.MessagesSnapshot(); len(msgs) > 1 {
			ag.Messages = append(ag.Messages, msgs[1:]...)
		}
		ag.SetUsage(old.Usage())
		ag.Todos = append([]agent.Todo(nil), old.Todos...)
		ag.CompactBackend = old.CompactBackend
		ag.CompactModel = old.CompactModel
		ag.CompactProvider = old.CompactProvider
		ag.CompactProtocol = old.CompactProtocol
		ag.CompactThreshold = old.CompactThreshold
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
	if !contains(m.effortsFor(), ag.Effort) {
		m.resetEffort("")
	}
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
		list:   append([]string(nil), modelRoleLabels...),
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
	roles := []string{uiModePlan, config.RoleDefault, config.RoleFast, config.RoleTiny}
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

// cycleStatusMode advances the two visible operating modes and lets setMode
// enforce the corresponding smart/fast role before the next turn.
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

	// Plan mode is intentionally locked to smart. In execute mode the chosen
	// role is also the concrete route for the next ordinary turn, so the user
	// can pick any of the four role targets.
	if m.uiMode() == uiModeExecute || role == config.RoleSmart {
		if err := m.activateRoute(item.model, item.provider, role); err != nil {
			return err
		}
	}
	return nil
}

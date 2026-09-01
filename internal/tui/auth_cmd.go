package tui

import (
	"context"
	"fmt"
	"strings"

	"github.com/sacca97/ghg/internal/agent"
	"github.com/sacca97/ghg/internal/auth"
	"github.com/sacca97/ghg/internal/config"
	"github.com/sacca97/ghg/internal/llm"
	"github.com/sacca97/ghg/internal/provider"
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
		if profile.Auth.Kind == provider.AuthNone {
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
		resolved, err := m.profiles.Resolve(provider.Instance{
			Name: name, Profile: configured.Profile, BaseURL: configured.BaseURL, Protocol: configured.API,
		})
		if err != nil || resolved.Profile.ID != id {
			continue
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
	profile     provider.Resolved
	key         string
	envMode     bool
	models      []llm.ModelInfo
	validated   bool
	unvalidated bool
	confirmed   bool
	catalogErr  error
	err         error
}

// authProvider validates a profile in the background, then persists the
// profile-derived provider entry and hot-swaps the current route.
func (m *model) authProvider(name string, resolved provider.Resolved, key string, envMode bool) {
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
		m.prog.Send(msg)
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
	if err := m.cfg.UpsertProviderKey(res.name, res.profile, res.key, res.envMode); err != nil {
		m.append(errStyle.Render("auth failed: " + err.Error()))
		return
	}
	if err := m.cfg.Save(); err != nil {
		m.append(errStyle.Render("config save failed: " + err.Error()))
		return
	}

	catalogSeeded := false
	if m.prog != nil && len(res.models) > 0 {
		if err := config.SaveCatalog(res.name, res.profile.BaseURL, res.models); err != nil {
			m.append(dimStyle.Render("(catalog cache write failed; /model refresh will retry)"))
		} else {
			catalogSeeded = true
			m.catalogs = config.LoadCatalogs()
			m.updateCatalogs(m.catalogs)
		}
	}

	// A cold TUI has no agent to rebuild. Promote it to the first live agent
	// from the freshly seeded catalog (or the configured default model when a
	// catalog was not returned). If roles are configured, prefer the current
	// mode's role when it belongs to the provider just authenticated. A running
	// session only accepts auth refreshes for its current provider, preserving
	// its conversation and route.
	if m.agent == nil {
		modelName := ""
		roleName := ""
		if len(m.cfg.Roles) > 0 {
			if target, roleErr := m.cfg.ResolveRole(m.modeRole()); roleErr == nil && target.Model != "" && target.Provider == res.name {
				modelName, roleName = target.Model, target.Role
			}
		}
		if catalogSeeded {
			if modelName == "" {
				modelName = firstCatalogModel(res.models)
			}
		}
		if modelName == "" {
			modelName = m.cfg.DefaultModel
		}
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
		var ag *agent.Agent
		var mn, pn string
		var err error
		if roleName != "" {
			ag, mn, pn, err = buildAgentForRoleWithProfiles(m.cfg, roleName, m.sysPrompt, m.profiles)
		} else {
			ag, mn, pn, err = buildAgentWithProfiles(m.cfg, modelName, res.name, m.sysPrompt, m.profiles)
		}
		if err != nil {
			m.append(errStyle.Render("provider configured but first agent could not be built: " + err.Error()))
			return
		}
		ag.Effort = m.cfg.DefaultEffort
		if ag.Effort == "" {
			ag.Effort = "medium"
		}
		m.agent, m.modelName, m.provName = ag, mn, pn
		m.configureArtifactAgent(m.agent)
		m.applyCompactModel()
		m.agent.CompactThreshold = compactThresholdFor(m.cfg)
		m.wireTasks()
	} else if m.provName == res.name && m.modelName != "" {
		role := m.agent.Role
		if ag, _, _, err := buildAgentWithProfiles(m.cfg, m.modelName, m.provName, m.sysPrompt, m.profiles); err == nil {
			ag.Effort = m.agent.Effort
			ag.Role = role
			if len(m.agent.Messages) > 1 {
				ag.Messages = append(ag.Messages, m.agent.Messages[1:]...)
			}
			ag.CompactBackend, ag.CompactModel = m.agent.CompactBackend, m.agent.CompactModel
			ag.CompactProvider, ag.CompactProtocol = m.agent.CompactProvider, m.agent.CompactProtocol
			ag.CompactThreshold = m.agent.CompactThreshold
			m.agent = ag
			m.configureArtifactAgent(m.agent)
			m.wireTasks()
		}
	}
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

func firstCatalogModel(models []llm.ModelInfo) string {
	for _, model := range models {
		if id := strings.TrimSpace(model.ID); id != "" {
			return id
		}
	}
	return ""
}

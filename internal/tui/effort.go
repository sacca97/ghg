package tui

import (
	"slices"
	"strings"

	"github.com/sacca97/ghg/internal/config"
)

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
	if m.agent == nil {
		return ""
	}
	return m.agent.Effort
}

// effortsFor returns the cycle of effort levels available for the current
// model. A known models.dev/provider surface is authoritative: its effort
// values are returned verbatim (with "none" folded into off), a toggle-only
// surface becomes off/on, and an explicitly empty surface becomes off only.
// Unknown models retain the legacy low/medium/high fallback.
func (m *model) effortsFor() []string {
	if c, ok := m.catalogs[m.provName]; ok {
		apiID := m.modelName
		if m.agent != nil {
			apiID = m.agent.Model
		}
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
	if m.agent == nil {
		return
	}
	m.agent.ReasoningToggle = m.reasoningToggleFor(m.provName, m.agent.Model)
	if n := m.contextLimitFor(m.provName, m.agent.Model); n != m.agent.ContextLimit {
		m.agent.ContextLimit = n // /models is the source of truth
	}
	if !slices.Contains(m.effortsFor(), m.agent.Effort) {
		m.resetEffort("")
		m.append(dimStyle.Render("⚡ effort reset to off: not supported by " + m.agent.Model))
	}
}

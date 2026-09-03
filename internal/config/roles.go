package config

import (
	"fmt"
	"maps"
	"slices"
	"strings"
)

// Role names are deliberately a small, stable surface. A role selects a
// model route; it does not describe a second provider or wire adapter.
const (
	RoleDefault = "default"
	RoleSmart   = "smart"
	RoleFast    = "fast"
	RoleTiny    = "tiny"

	ModeActing = "acting"
)

// Model routes a model to one or more providers that serve it.
type Model struct {
	Name      string   `json:"name,omitempty"`
	Providers []string `json:"providers"`    // provider keys, first is the default
	ID        string   `json:"id,omitempty"` // model id sent to the API; defaults to the map key
	// API overrides a profile route for this model. It is useful as an escape
	// hatch when a provider announces a model before its profile route table is
	// updated.
	API string `json:"api,omitempty"`
	// Context is the model's context window (max INPUT tokens). The provider's
	// /models context_length overrides it when advertised; this is the fallback
	// and the value shown for providers that don't report one.
	Context int `json:"context,omitempty"`
	// MaxOut caps OUTPUT tokens (the max_tokens request param). 0 uses the
	// provider's max_completion_tokens when advertised, else a sane default.
	MaxOut int `json:"maxOut,omitempty"`
	// MaxTokens is the legacy field name for Context (it was misnamed: it held
	// the context window, not an output cap). Read on load for back-compat.
	MaxTokens int `json:"maxTokens,omitempty"`
	// Vision reports whether the model accepts image inputs. When false (the
	// default), @image tags are NOT inlined as base64 vision parts — the model
	// gets a pointer note instead, so a text-only model isn't sent a request it
	// would reject. A provider-advertised input_modalities entry overrides this.
	Vision bool `json:"vision,omitempty"`
}

// ContextWindow returns the model's context (input) size, honoring the legacy
// maxTokens field for configs written before the rename.
func (m Model) ContextWindow() int {
	if m.Context > 0 {
		return m.Context
	}
	return m.MaxTokens
}

// ResolvedRoute contains the canonical provider and model resolution result.
type ResolvedRoute struct {
	Provider     Provider
	Model        Model
	ProviderName string
	ModelName    string
	APIID        string
}

// Resolve picks the provider and API model id for a model name.
// provider may be "" to use the config default routing.
func (c *Config) Resolve(model, provider string) (ResolvedRoute, error) {
	if model == "" {
		model = c.DefaultModel
	}
	m, ok := c.Models[model]
	if !ok {
		// Catalog fallback: a provider-advertised model needs no config entry;
		// config entries stay authoritative overrides when present.
		var err error
		m, provider, err = c.resolveFromCatalog(model, provider)
		if err != nil {
			return ResolvedRoute{}, err
		}
	}
	if provider == "" {
		provider = c.DefaultProvider
	}
	if provider == "" && len(m.Providers) > 0 {
		provider = m.Providers[0]
	}
	p, ok := c.Providers[provider]
	if !ok {
		return ResolvedRoute{}, fmt.Errorf("unknown provider %q (providers: %s)", provider, keys(c.Providers))
	}
	id := m.ID
	if id == "" {
		id = model
	}
	return ResolvedRoute{
		Provider:     p,
		Model:        m,
		ProviderName: provider,
		ModelName:    model,
		APIID:        id,
	}, nil
}

func keys[V any](m map[string]V) string {
	keys := slices.Sorted(maps.Keys(m))
	return strings.Join(keys, ", ")
}

// RoleConfig is the JSONC entry for one model role.
type RoleConfig struct {
	Provider string `json:"provider,omitempty"`
	Model    string `json:"model,omitempty"`
}

// ResolvedRole is the concrete model/provider target selected for a role.
// Model is the config/catalog name; the wire model id is resolved later by
// Config.Resolve and the provider profile factory.
type ResolvedRole struct {
	Role     string
	Model    string
	Provider string
}

// SupportedRoles returns the accepted role names in stable order.
func SupportedRoles() []string {
	return []string{RoleDefault, RoleSmart, RoleFast, RoleTiny}
}

// IsRole reports whether name is one of the supported role names.
func IsRole(name string) bool {
	switch name {
	case RoleDefault, RoleSmart, RoleFast, RoleTiny:
		return true
	default:
		return false
	}
}

// RoleForMode returns the default role for a user-visible operating mode.
func RoleForMode(mode string) string {
	if strings.EqualFold(strings.TrimSpace(mode), "plan") {
		return RoleSmart
	}
	return RoleFast
}

// ResolveRole applies role -> configured entry -> configured default role ->
// legacy defaultModel/defaultProvider. A configured entry is authoritative:
// an invalid route is returned as an error instead of silently falling back.
func (c *Config) ResolveRole(role string) (ResolvedRole, error) {
	role = strings.TrimSpace(role)
	if role == "" {
		role = RoleDefault
	}
	if !IsRole(role) {
		return ResolvedRole{}, fmt.Errorf("unknown role %q (roles: %s)", role, strings.Join(SupportedRoles(), ", "))
	}

	target, configured := c.Roles[role]
	if !configured && role != RoleDefault {
		target, configured = c.Roles[RoleDefault]
	}
	if configured {
		model := strings.TrimSpace(target.Model)
		if model == "" {
			return ResolvedRole{}, fmt.Errorf("role %q has no model", role)
		}
		provider := strings.TrimSpace(target.Provider)
		route, err := c.Resolve(model, provider)
		if err != nil {
			return ResolvedRole{}, fmt.Errorf("role %q: %w", role, err)
		}
		return ResolvedRole{Role: role, Model: route.ModelName, Provider: route.ProviderName}, nil
	}

	// An empty legacy target is allowed through here so the interactive cold
	// start can render and offer /auth. Strict callers still get the existing
	// Config.Resolve error when they try to build an agent.
	model := strings.TrimSpace(c.DefaultModel)
	provider := strings.TrimSpace(c.DefaultProvider)
	if model == "" {
		return ResolvedRole{Role: role}, nil
	}
	route, err := c.Resolve(model, provider)
	if err != nil {
		return ResolvedRole{}, err
	}
	return ResolvedRole{Role: role, Model: route.ModelName, Provider: route.ProviderName}, nil
}

// ValidateRoles rejects stale or misspelled role keys before they can be
// mistaken for supported configuration.
func (c *Config) ValidateRoles() error {
	var invalid []string
	for name := range c.Roles {
		if !IsRole(name) {
			invalid = append(invalid, name)
		}
	}
	if len(invalid) == 0 {
		return nil
	}
	slices.Sort(invalid)
	return fmt.Errorf("unknown role %q (roles: %s)", invalid[0], strings.Join(SupportedRoles(), ", "))
}

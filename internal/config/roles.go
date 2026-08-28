package config

import (
	"fmt"
	"sort"
	"strings"
)

// Role names are deliberately a small, stable surface. A role selects a
// model route; it does not describe a second provider or wire adapter.
const (
	RoleDefault = "default"
	RoleSmart   = "smart"
	RoleFast    = "fast"
	RoleTiny    = "tiny"

	ModePlanning = "planning"
	ModeActing   = "acting"
)

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
// Planning is deliberately the more capable path; ordinary turns optimize
// for latency. Callers can still override either choice with an explicit role.
func RoleForMode(mode string) string {
	if mode == ModePlanning {
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
		_, mdl, _, err := c.Resolve(model, provider)
		if err != nil {
			return ResolvedRole{}, fmt.Errorf("role %q: %w", role, err)
		}
		return ResolvedRole{Role: role, Model: model, Provider: resolvedProviderName(c, model, mdl, provider)}, nil
	}

	// An empty legacy target is allowed through here so the interactive cold
	// start can render and offer /auth. Strict callers still get the existing
	// Config.Resolve error when they try to build an agent.
	model := strings.TrimSpace(c.DefaultModel)
	provider := strings.TrimSpace(c.DefaultProvider)
	if model == "" {
		return ResolvedRole{Role: role}, nil
	}
	_, mdl, _, err := c.Resolve(model, provider)
	if err != nil {
		return ResolvedRole{}, err
	}
	return ResolvedRole{Role: role, Model: model, Provider: resolvedProviderName(c, model, mdl, provider)}, nil
}

func resolvedProviderName(c *Config, model string, mdl Model, provider string) string {
	if provider != "" {
		return provider
	}
	// Catalog fallback records its owning provider in the synthesized model;
	// that owner must win over a global default provider. Configured model
	// entries retain the existing defaultProvider override semantics.
	if _, configured := c.Models[model]; !configured && len(mdl.Providers) > 0 {
		return mdl.Providers[0]
	}
	if c.DefaultProvider != "" {
		return c.DefaultProvider
	}
	if len(mdl.Providers) > 0 {
		return mdl.Providers[0]
	}
	return ""
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
	sort.Strings(invalid)
	return fmt.Errorf("unknown role %q (roles: %s)", invalid[0], strings.Join(SupportedRoles(), ", "))
}

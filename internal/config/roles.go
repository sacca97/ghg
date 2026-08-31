package config

import (
	"fmt"
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

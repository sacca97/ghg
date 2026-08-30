package config

import (
	"fmt"
	"strings"

	"github.com/sacca97/ghg/internal/provider"
)

// TrimKey normalizes a pasted API key: whitespace and a stray leading
// "Bearer " both break Authorization headers, and both happen in practice
// when copying from dashboards.
func TrimKey(s string) string {
	s = strings.TrimSpace(s)
	if rest, ok := strings.CutPrefix(s, "Bearer "); ok {
		s = strings.TrimSpace(rest)
	}
	return s
}

// UpsertProviderKey registers a provider instance from a resolved profile.
// Credentials remain on the JSONC instance: literal mode stores apiKey and
// env mode stores only the profile-declared apiKeyEnv. The two modes never
// leave both fields populated.
func (c *Config) UpsertProviderKey(name string, resolved provider.Resolved, key string, envMode bool) error {
	name = strings.TrimSpace(name)
	if name == "" {
		return fmt.Errorf("provider name is required")
	}
	if !resolved.RequiresAPIKey() {
		return fmt.Errorf("provider %q takes no API key", name)
	}

	p := Provider{
		Name:    resolved.Profile.DisplayName,
		Profile: resolved.Profile.ID,
		BaseURL: resolved.BaseURL,
		API:     resolved.Protocol,
	}
	if p.Name == "" {
		p.Name = name
	}
	if envMode {
		envVar := strings.TrimSpace(resolved.Auth.EnvVar)
		if envVar == "" {
			return fmt.Errorf("provider %q profile %q does not declare auth.env_var", name, resolved.Profile.ID)
		}
		p.APIKeyEnv = envVar
	} else {
		p.APIKey = TrimKey(key)
		if p.APIKey == "" {
			return fmt.Errorf("provider %q needs an API key", name)
		}
	}

	if c.Providers == nil {
		c.Providers = map[string]Provider{}
	}
	c.Providers[name] = p
	authMode := "literal"
	if envMode {
		authMode = "environment"
	}
	logf("config.provider", "upserted provider %q profile=%q auth=%s", name, p.Profile, authMode)
	return nil
}

// AnyProviderConfigured reports whether any configured provider currently has
// a resolvable credential. It is the generic provider onboarding check.
func (c *Config) AnyProviderConfigured() bool {
	for _, p := range c.Providers {
		if p.Key() != "" {
			return true
		}
	}
	return false
}

// AnyProviderUsable is the profile-aware form used when auth:none providers
// should count as usable even though they intentionally have no key.
func (c *Config) AnyProviderUsable(profiles provider.Profiles) bool {
	for name, p := range c.Providers {
		resolved, err := profiles.Resolve(provider.Instance{
			Name: name, Profile: p.Profile, BaseURL: p.BaseURL, Protocol: p.API,
		})
		if err != nil {
			continue
		}
		if !resolved.RequiresAPIKey() {
			return true
		}
		if key, err := p.ResolveKey(); err == nil && key != "" {
			return true
		}
	}
	return false
}

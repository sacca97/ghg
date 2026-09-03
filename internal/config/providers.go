package config

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/sacca97/ghg/internal/models"
)

// Provider is an API endpoint that can serve models.
type Provider struct {
	Name      string `json:"name,omitempty"`
	Profile   string `json:"profile,omitempty"` // reusable YAML profile; empty keeps legacy anonymous behavior
	BaseURL   string `json:"baseUrl"`
	API       string `json:"api"`              // legacy protocol selector; profiles use canonical protocol names
	APIKey    string `json:"apiKey,omitempty"` // literal key or a secret reference ("$VAR"/"${VAR}"/"!cmd"); apiKeyEnv is another option
	APIKeyEnv string `json:"apiKeyEnv,omitempty"`
}

// Key returns the resolved API key for the provider, "" when none is
// configured. Unresolvable secret references degrade to "" like a missing
// key; ResolveKey reports the error for callers that can surface it.
func (p Provider) Key() string {
	k, _ := p.ResolveKey()
	return k
}

// ResolveKey is Key with error detail: apiKey/apiKeyEnv may hold a secret
// reference (see ResolveSecret), resolved here at the point of use so the
// config file and session store hold only references and a missing var only
// errors when the provider is actually used. The resolved value never enters
// the event log.
func (p Provider) ResolveKey() (string, error) {
	if p.APIKeyEnv != "" {
		if v := os.Getenv(p.APIKeyEnv); v != "" {
			return v, nil
		}
	}
	if p.APIKey != "" {
		k, err := ResolveSecret(p.APIKey)
		if err != nil {
			return "", fmt.Errorf("provider %q apiKey: %w", p.Name, err)
		}
		return k, nil
	}
	// ponytail: special-case fallback to the inf CLI's stored key; generalize to apiKeyFile if more providers need it
	// when the profile or legacy URL identifies the built-in default service.
	if p.Profile == "inference" || strings.Contains(p.BaseURL, "api.inference.net") {
		return infKey(), nil
	}
	return "", nil
}

// infKey reads apiKey/codingAgentApiKey from ~/.inf/config.json (written by `inf auth set-key`).
func infKey() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	data, err := os.ReadFile(filepath.Join(home, ".inf", "config.json"))
	if err != nil {
		return ""
	}
	var c struct {
		APIKey            string `json:"apiKey"`
		CodingAgentAPIKey string `json:"codingAgentApiKey"`
	}
	if json.Unmarshal(data, &c) != nil {
		return ""
	}
	if c.APIKey != "" {
		return c.APIKey
	}
	return c.CodingAgentAPIKey
}

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
func (c *Config) UpsertProviderKey(name string, resolved models.Resolved, key string, envMode bool) error {
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
		API:     string(resolved.Protocol),
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

// UpsertOAuthProvider registers an OAuth provider instance from a resolved profile without an API key.
func (c *Config) UpsertOAuthProvider(name string, resolved models.Resolved) error {
	name = strings.TrimSpace(name)
	if name == "" {
		return fmt.Errorf("provider name is required")
	}
	p := Provider{
		Name:    resolved.Profile.DisplayName,
		Profile: resolved.Profile.ID,
		BaseURL: resolved.BaseURL,
		API:     string(resolved.Protocol),
	}
	if p.Name == "" {
		p.Name = name
	}
	if c.Providers == nil {
		c.Providers = map[string]Provider{}
	}
	c.Providers[name] = p
	logf("config.provider", "upserted oauth provider %q profile=%q", name, p.Profile)
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
func (c *Config) AnyProviderUsable(profiles models.Profiles) bool {
	for name, p := range c.Providers {
		resolved, err := profiles.Resolve(models.Instance{
			Name: name, Profile: p.Profile, BaseURL: p.BaseURL, Protocol: models.Protocol(p.API),
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

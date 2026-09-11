package config

import (
	"errors"
	"fmt"
	"net/url"
	"strings"
)

// SearchProvider is a trusted, user-configured search endpoint. API keys are
// never copied into model messages or status output.
type SearchProvider struct {
	Type    string `json:"type"`
	BaseURL string `json:"baseUrl"`
	APIKey  string `json:"apiKey,omitempty"`
}

func (c *Config) ValidateSearchProviders() error {
	if c == nil {
		return nil
	}
	for name, provider := range c.SearchProviders {
		if err := validateSearchProvider(name, provider); err != nil {
			return err
		}
	}
	if c.SearchProvider != "" && c.SearchProvider != "brave" {
		if _, ok := c.SearchProviders[c.SearchProvider]; !ok {
			return fmt.Errorf("selected search provider %q is not configured", c.SearchProvider)
		}
	}
	return nil
}

func (c *Config) AddSearchProvider(name, baseURL, apiKey string) error {
	if c == nil {
		return errors.New("config is nil")
	}
	name = strings.TrimSpace(name)
	provider := SearchProvider{Type: "searxng", BaseURL: strings.TrimSpace(baseURL), APIKey: strings.TrimSpace(apiKey)}
	if err := validateSearchProvider(name, provider); err != nil {
		return err
	}
	if c.SearchProviders == nil {
		c.SearchProviders = make(map[string]SearchProvider)
	}
	c.SearchProviders[name] = provider
	if c.SearchProvider == "" || c.SearchProvider == "brave" {
		c.SearchProvider = name
	}
	return nil
}

func (c *Config) SelectSearchProvider(name string) error {
	if c == nil {
		return errors.New("config is nil")
	}
	name = strings.TrimSpace(name)
	if name == "" || name == "brave" {
		c.SearchProvider = ""
		return nil
	}
	if _, ok := c.SearchProviders[name]; !ok {
		return fmt.Errorf("search provider %q is not configured", name)
	}
	c.SearchProvider = name
	return nil
}

func (c *Config) RemoveSearchProvider(name string) error {
	if c == nil {
		return errors.New("config is nil")
	}
	name = strings.TrimSpace(name)
	if _, ok := c.SearchProviders[name]; !ok {
		return fmt.Errorf("search provider %q is not configured", name)
	}
	delete(c.SearchProviders, name)
	if c.SearchProvider == name {
		c.SearchProvider = ""
	}
	return nil
}

func validateSearchProvider(name string, provider SearchProvider) error {
	if name == "" || len(name) > 64 || strings.ContainsAny(name, " \t\r\n/\\") {
		return fmt.Errorf("search provider name %q is invalid", name)
	}
	if provider.Type != "" && provider.Type != "searxng" {
		return fmt.Errorf("search provider %q has unsupported type %q", name, provider.Type)
	}
	u, err := url.Parse(strings.TrimSpace(provider.BaseURL))
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" {
		return fmt.Errorf("search provider %q must use an http(s) base URL without credentials, query, or fragment", name)
	}
	if strings.ContainsAny(provider.APIKey, "\r\n") || len(provider.APIKey) > 4096 {
		return fmt.Errorf("search provider %q API key is invalid", name)
	}
	return nil
}

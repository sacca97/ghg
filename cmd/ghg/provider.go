package main

import (
	"context"
	"fmt"
	"os"

	"github.com/sacca97/ghg/internal/agent"
	"github.com/sacca97/ghg/internal/config"
	"github.com/sacca97/ghg/internal/llm"
	"github.com/sacca97/ghg/internal/provider"
)

func loadProviderProfiles() (provider.Profiles, error) {
	wd, err := os.Getwd()
	if err != nil {
		return provider.Profiles{}, fmt.Errorf("provider profiles: current directory: %w", err)
	}
	return provider.Load(provider.LoadOptions{ProjectTrusted: config.Trusted(wd)})
}

func newProviderBackend(profiles provider.Profiles, name string, p config.Provider, key string, maxRetries int, modelID, modelAPI string) (llm.Backend, error) {
	resolved, err := profiles.ResolveModel(provider.Instance{
		Name: name, Profile: p.Profile, BaseURL: p.BaseURL, Protocol: p.API,
	}, modelID)
	if err != nil {
		return nil, err
	}
	if resolved.RequiresAPIKey() && key == "" {
		return nil, fmt.Errorf("no API key for provider %q (set apiKey/apiKeyEnv in ~/.ghg/config.json)", name)
	}
	protocol := resolved.Protocol
	if modelAPI != "" {
		protocol = modelAPI
	}
	return llm.NewBackend(llm.BackendConfig{
		Protocol:   llm.Protocol(protocol),
		BaseURL:    resolved.BaseURL,
		APIKey:     key,
		Headers:    resolved.DefaultHeaders,
		AuthKind:   resolved.Auth.Kind,
		AuthHeader: resolved.Auth.Header,
		MaxRetries: maxRetries,
	})
}

// newAgentFromRoute constructs a fully configured agent for a resolved route.
func newAgentFromRoute(cfg *config.Config, profiles provider.Profiles, route config.ResolvedRoute, role, systemPrompt string) (*agent.Agent, error) {
	key, err := route.Provider.ResolveKey()
	if err != nil {
		return nil, err
	}
	resolved, err := profiles.ResolveModel(provider.Instance{
		Name: route.ProviderName, Profile: route.Provider.Profile, BaseURL: route.Provider.BaseURL, Protocol: route.Provider.API,
	}, route.APIID)
	if err != nil {
		return nil, err
	}
	backend, err := newProviderBackend(profiles, route.ProviderName, route.Provider, key, cfg.MaxRetries, route.APIID, route.Model.API)
	if err != nil {
		return nil, err
	}
	catalogs := config.LoadCatalogs()
	cat, hasCatalog := catalogs[route.ProviderName]
	contextLimit := route.Model.ContextWindow()
	if hasCatalog {
		if n := cat.ContextLength(route.APIID); n > 0 {
			contextLimit = n
		}
	}
	if contextLimit <= 0 {
		contextLimit = config.LoadModelsDev().ContextLength(route.APIID, modelsDevProviderIDs(resolved, route.ProviderName)...)
	}
	maxOut := route.Model.MaxOut
	if maxOut <= 0 && hasCatalog {
		maxOut = cat.MaxCompletionTokens(route.APIID)
	}
	if maxOut <= 0 {
		maxOut = contextLimit
	}
	ag := agent.New(backend, route.APIID, maxOut, systemPrompt)
	ag.ModelName = route.ModelName
	ag.Provider = route.ProviderName
	ag.Role = role
	ag.ContextLimit = contextLimit
	if hasCatalog {
		if info := cat.Find(route.APIID); info != nil {
			ag.ReasoningToggle = info.ReasoningToggle
		}
	}
	if !ag.ReasoningToggle {
		if info, ok := config.LoadModelsDev().ReasoningFor(route.APIID, modelsDevProviderIDs(resolved, route.ProviderName)...); ok {
			ag.ReasoningToggle = info.Toggle
		}
	}
	ag.CompactThreshold = config.CompactThreshold(cfg)
	configureSubagentFactory(ag, cfg, profiles)
	return ag, nil
}

func modelsDevProviderIDs(resolved provider.Resolved, instanceName string) []string {
	ids := make([]string, 0, 3)
	for _, id := range []string{resolved.Catalog.ModelsDev, resolved.Profile.ID, instanceName} {
		if id == "" {
			continue
		}
		seen := false
		for _, existing := range ids {
			if existing == id {
				seen = true
				break
			}
		}
		if !seen {
			ids = append(ids, id)
		}
	}
	return ids
}

// newRoleAgent resolves and constructs one of the configured model roles.
// Role resolution is kept above the wire factory so every role gets the same
// profile routing, auth policy, and adapter selection as an explicit model.
func newRoleAgent(cfg *config.Config, profiles provider.Profiles, role, systemPrompt string) (*agent.Agent, string, string, error) {
	target, err := cfg.ResolveRole(role)
	if err != nil {
		return nil, "", "", err
	}
	route, err := cfg.Resolve(target.Model, target.Provider)
	if err != nil {
		return nil, "", "", err
	}
	ag, err := newAgentFromRoute(cfg, profiles, route, target.Role, systemPrompt)
	if err != nil {
		return nil, "", "", err
	}
	return ag, route.ModelName, route.ProviderName, nil
}

func newModeAgent(cfg *config.Config, profiles provider.Profiles, mode, systemPrompt string) (*agent.Agent, string, string, error) {
	return newRoleAgent(cfg, profiles, config.RoleForMode(mode), systemPrompt)
}

func configureSubagentFactory(ag *agent.Agent, cfg *config.Config, profiles provider.Profiles) {
	if ag == nil || cfg == nil {
		return
	}
	ag.SubagentFactory = func(_ context.Context, role, systemPrompt string) (*agent.Agent, error) {
		sub, _, _, err := newRoleAgent(cfg, profiles, role, systemPrompt)
		return sub, err
	}
}

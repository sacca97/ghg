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

// newRoleAgent resolves and constructs one of the configured model roles.
// Role resolution is kept above the wire factory so every role gets the same
// profile routing, auth policy, and adapter selection as an explicit model.
func newRoleAgent(cfg *config.Config, profiles provider.Profiles, role, systemPrompt string) (*agent.Agent, string, string, error) {
	target, err := cfg.ResolveRole(role)
	if err != nil {
		return nil, "", "", err
	}
	prov, mdl, apiID, err := cfg.Resolve(target.Model, target.Provider)
	if err != nil {
		return nil, "", "", err
	}
	providerName := target.Provider
	if providerName == "" {
		providerName = cfg.DefaultProvider
		if providerName == "" && len(mdl.Providers) > 0 {
			providerName = mdl.Providers[0]
		}
	}
	key, err := prov.ResolveKey()
	if err != nil {
		return nil, "", "", err
	}
	backend, err := newProviderBackend(profiles, providerName, prov, key, cfg.MaxRetries, apiID, mdl.API)
	if err != nil {
		return nil, "", "", err
	}
	maxOut := mdl.MaxOut
	if maxOut <= 0 {
		maxOut = mdl.ContextWindow()
	}
	ag := agent.New(backend, apiID, maxOut, systemPrompt)
	ag.ModelName, ag.Provider, ag.Role = target.Model, providerName, target.Role
	ag.ContextLimit = mdl.ContextWindow()
	configureSubagentFactory(ag, cfg, profiles)
	return ag, target.Model, providerName, nil
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

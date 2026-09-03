package agent

import (
	"context"
	"fmt"

	"github.com/sacca97/ghg/internal/auth"
	"github.com/sacca97/ghg/internal/config"
	"github.com/sacca97/ghg/internal/models"
)

type BuildOptions struct {
	Config                  *config.Config
	Profiles                models.Profiles
	Model                   string
	Provider                string
	Role                    string
	SystemPrompt            string
	AllowMissingCredentials bool
}

func NewConfigured(opts BuildOptions) (*Agent, string, string, error) {
	if opts.Config == nil {
		return nil, "", "", fmt.Errorf("agent configuration is nil")
	}
	cfg := opts.Config
	route, err := cfg.Resolve(opts.Model, opts.Provider)
	if err != nil {
		if opts.AllowMissingCredentials && len(cfg.Providers) == 0 && opts.Model == "" && opts.Provider == "" {
			return nil, cfg.DefaultModel, cfg.DefaultProvider, nil
		}
		return nil, "", "", err
	}

	modelName, providerName := route.ModelName, route.ProviderName
	resolved, err := opts.Profiles.ResolveModel(models.Instance{
		Name: route.ProviderName, Profile: route.Provider.Profile,
		BaseURL: route.Provider.BaseURL, Protocol: models.Protocol(route.Provider.API),
	}, route.APIID)
	if err != nil {
		return nil, "", "", err
	}

	key := ""
	if resolved.RequiresAPIKey() {
		key, err = route.Provider.ResolveKey()
		if err != nil {
			return nil, "", "", err
		}
		if key == "" {
			if opts.AllowMissingCredentials {
				return nil, modelName, providerName, nil
			}
			return nil, "", "", fmt.Errorf("no API key for provider %q (set apiKey/apiKeyEnv in ~/.ghg/config.json)", providerName)
		}
	} else if resolved.RequiresOAuth() {
		status, _ := auth.DefaultCodexCredentialManager().Status(context.Background())
		if !status.Configured {
			if opts.AllowMissingCredentials {
				return nil, modelName, providerName, nil
			}
			return nil, "", "", fmt.Errorf("provider %q requires authentication; run 'ghg auth codex-subscription' to log in", providerName)
		}
	}

	catalogs := config.LoadCatalogs()
	cat, hasCatalog := catalogs[providerName]
	contextLimit := route.Model.ContextWindow()
	if hasCatalog {
		if n := cat.ContextLength(route.APIID); n > 0 {
			contextLimit = n
		}
	}
	if contextLimit <= 0 {
		contextLimit = config.LoadModelsDev().ContextLength(route.APIID, modelProviderIDs(resolved, providerName)...)
	}
	maxOut := 0
	if hasCatalog {
		maxOut = cat.MaxCompletionTokens(route.APIID)
	}
	if maxOut <= 0 {
		maxOut = contextLimit
	}

	backend, err := auth.NewBackend(resolved, key, route.Model.API, cfg.MaxRetries)
	if err != nil {
		return nil, "", "", err
	}
	ag := New(backend, route.APIID, maxOut, opts.SystemPrompt)
	ag.ModelName, ag.Provider = modelName, providerName
	ag.Role = opts.Role
	if ag.Role == "" {
		ag.Role = config.RoleDefault
	}
	ag.ContextLimit = contextLimit
	if hasCatalog {
		if info := cat.Find(route.APIID); info != nil {
			ag.ReasoningToggle = info.ReasoningToggle
		}
	}
	if !ag.ReasoningToggle {
		if info, ok := config.LoadModelsDev().ReasoningFor(route.APIID, modelProviderIDs(resolved, providerName)...); ok {
			ag.ReasoningToggle = info.Toggle
		}
	}
	ag.CompactThreshold = config.CompactThreshold(cfg)
	ag.SubagentsDisabled = !config.SubagentsEnabled(cfg)
	ag.SubagentFactory = func(ctx context.Context, role, systemPrompt string) (*Agent, error) {
		sub, _, _, err := NewConfiguredForRole(cfg, opts.Profiles, role, systemPrompt, false)
		return sub, err
	}
	return ag, modelName, providerName, nil
}

func NewConfiguredForRole(cfg *config.Config, profiles models.Profiles, role, systemPrompt string, allowMissingCredentials bool) (*Agent, string, string, error) {
	if cfg == nil {
		return nil, "", "", fmt.Errorf("agent configuration is nil")
	}
	target, err := cfg.ResolveRole(role)
	if err != nil {
		if allowMissingCredentials && len(cfg.Providers) == 0 && len(cfg.Models) == 0 && len(cfg.Roles) == 0 {
			return nil, cfg.DefaultModel, cfg.DefaultProvider, nil
		}
		return nil, "", "", err
	}
	ag, modelName, providerName, err := NewConfigured(BuildOptions{
		Config: cfg, Profiles: profiles, Model: target.Model, Provider: target.Provider,
		Role: target.Role, SystemPrompt: systemPrompt,
		AllowMissingCredentials: allowMissingCredentials,
	})
	return ag, modelName, providerName, err
}

func modelProviderIDs(resolved models.Resolved, instanceName string) []string {
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

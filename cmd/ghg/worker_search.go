package main

import (
	"fmt"
	"sort"
	"strings"

	"github.com/sacca97/ghg/internal/config"
	workerwire "github.com/sacca97/ghg/internal/worker"
)

func (w *workerProcessState) searchProviderCommand(request workerwire.SearchProviderRequest) ([]workerwire.SearchProviderInfo, error) {
	cfg, err := config.Load()
	if err != nil {
		return nil, fmt.Errorf("load search providers: %w", err)
	}
	action := strings.ToLower(strings.TrimSpace(request.Action))
	switch action {
	case "", "list":
	case "add":
		if err := cfg.AddSearchProvider(request.Name, request.BaseURL, request.APIKey); err != nil {
			return nil, err
		}
	case "use":
		if err := cfg.SelectSearchProvider(request.Name); err != nil {
			return nil, err
		}
	case "remove":
		if err := cfg.RemoveSearchProvider(request.Name); err != nil {
			return nil, err
		}
	default:
		return nil, fmt.Errorf("search provider action must be list, add, use, or remove")
	}
	if action != "list" && action != "" {
		if err := cfg.Save(); err != nil {
			return nil, fmt.Errorf("save search providers: %w", err)
		}
		w.publish("notice", searchProviderNotice(cfg), true)
	}
	return searchProviderInfos(cfg), nil
}

func searchProviderInfos(cfg *config.Config) []workerwire.SearchProviderInfo {
	names := make([]string, 0, len(cfg.SearchProviders))
	for name := range cfg.SearchProviders {
		names = append(names, name)
	}
	sort.Strings(names)
	providers := make([]workerwire.SearchProviderInfo, 0, len(names))
	for _, name := range names {
		provider := cfg.SearchProviders[name]
		providers = append(providers, workerwire.SearchProviderInfo{
			Name: name, BaseURL: provider.BaseURL, Active: cfg.SearchProvider == name, HasAPIKey: provider.APIKey != "",
		})
	}
	return providers
}

func searchProviderNotice(cfg *config.Config) string {
	active := cfg.SearchProvider
	if active == "" {
		active = "brave"
	}
	return "search provider: " + active
}

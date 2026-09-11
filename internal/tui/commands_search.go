package tui

import (
	"fmt"
	"sort"
	"strings"

	"github.com/sacca97/ghg/internal/config"
)

func (m *model) searchProvidersCommand(args []string) {
	cfg := m.cfg
	if cfg == nil {
		var err error
		cfg, err = config.Load()
		if err != nil {
			m.append(errStyle.Render("search providers: " + err.Error()))
			return
		}
		m.cfg = cfg
	}
	if len(args) == 0 || args[0] == "list" {
		m.listSearchProviders(cfg)
		return
	}
	switch strings.ToLower(args[0]) {
	case "add":
		if len(args) != 3 && len(args) != 4 {
			m.append(errStyle.Render("usage: /search-providers add <name> <base-url> [api-key]"))
			return
		}
		if len(args) == 4 {
			m.saveSearchProvider(cfg, args[1], args[2], args[3])
			return
		}
		name, baseURL := args[1], args[2]
		m.openNamePrompt("⌕ SearXNG API key (optional, masked; enter to save, esc cancels):", "", func(apiKey string) {
			m.saveSearchProvider(cfg, name, baseURL, apiKey)
		})
		m.namePrompt.mask = true
	case "use":
		if len(args) != 2 {
			m.append(errStyle.Render("usage: /search-providers use <name|brave>"))
			return
		}
		if err := cfg.SelectSearchProvider(args[1]); err != nil {
			m.append(errStyle.Render("search providers: " + err.Error()))
			return
		}
		if err := m.saveConfig(); err == nil {
			m.append(dimStyle.Render("search provider: " + activeSearchProviderLabel(cfg)))
		}
	case "remove":
		if len(args) != 2 {
			m.append(errStyle.Render("usage: /search-providers remove <name>"))
			return
		}
		if err := cfg.RemoveSearchProvider(args[1]); err != nil {
			m.append(errStyle.Render("search providers: " + err.Error()))
			return
		}
		if err := m.saveConfig(); err == nil {
			m.append(dimStyle.Render("removed search provider " + args[1]))
		}
	default:
		m.append(errStyle.Render("usage: /search-providers [list|add|use|remove]"))
	}
}

func (m *model) saveSearchProvider(cfg *config.Config, name, baseURL, apiKey string) {
	if err := cfg.AddSearchProvider(name, baseURL, strings.TrimSpace(apiKey)); err != nil {
		m.append(errStyle.Render("search providers: " + err.Error()))
		return
	}
	if err := m.saveConfig(); err == nil {
		m.append(dimStyle.Render("search provider " + name + " saved and selected"))
	}
}

func (m *model) listSearchProviders(cfg *config.Config) {
	if len(cfg.SearchProviders) == 0 {
		m.append(dimStyle.Render("search providers: brave (set BRAVE_SEARCH_API_KEY), no SearXNG endpoints"))
		return
	}
	names := make([]string, 0, len(cfg.SearchProviders))
	for name := range cfg.SearchProviders {
		names = append(names, name)
	}
	sort.Strings(names)
	lines := []string{"search providers (active: " + activeSearchProviderLabel(cfg) + "):"}
	for _, name := range names {
		provider := cfg.SearchProviders[name]
		key := "no key"
		if provider.APIKey != "" {
			key = "key configured"
		}
		lines = append(lines, fmt.Sprintf("  %s — %s (%s)", name, provider.BaseURL, key))
	}
	m.append(dimStyle.Render(strings.Join(lines, "\n")))
}

func activeSearchProviderLabel(cfg *config.Config) string {
	if cfg.SearchProvider == "" {
		return "brave"
	}
	return cfg.SearchProvider
}

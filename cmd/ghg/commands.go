package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"time"

	"github.com/sacca97/ghg/internal/auth"
	"github.com/sacca97/ghg/internal/config"
	"github.com/sacca97/ghg/internal/models"
	"github.com/sacca97/ghg/internal/session"
)

func loadProviderProfiles() (models.Profiles, error) {
	wd, err := os.Getwd()
	if err != nil {
		return models.Profiles{}, fmt.Errorf("provider profiles: current directory: %w", err)
	}
	project, err := config.NewProjectContext(wd, config.Trusted(wd))
	if err != nil {
		return models.Profiles{}, fmt.Errorf("provider profiles: project context: %w", err)
	}
	return loadProviderProfilesForProject(project)
}

func loadProviderProfilesForProject(project config.ProjectContext) (models.Profiles, error) {
	return models.Load(models.LoadOptions{
		ProjectDir:     filepath.Join(project.Root, ".ghg", "providers"),
		ProjectTrusted: project.Trusted,
	})
}

func defaultEffort(cfg *config.Config) string {
	if cfg.DefaultEffort == "" {
		return "medium"
	}
	return cfg.DefaultEffort
}

func modelsCLI(args []string) error {
	fs := flag.NewFlagSet("models", flag.ContinueOnError)
	format := fs.String("format", "text", "output format: text or json")
	all := fs.Bool("all", false, "list all configured catalog models")
	refresh := fs.Bool("refresh", false, "refresh provider catalogs before listing")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *format != "text" && *format != "json" {
		return fmt.Errorf("unknown --format %q (want text|json)", *format)
	}
	cfg, err := config.Load()
	if err != nil {
		return err
	}
	if *refresh {
		profiles, err := loadProviderProfiles()
		if err != nil {
			return err
		}
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		if _, err := config.FetchCatalogs(ctx, cfg, profiles, true, config.CatalogBackendFactory(auth.NewBackend)); err != nil {
			return err
		}
	}
	if *all {
		choices := configuredCatalogModels(cfg)
		if *format == "json" {
			return json.NewEncoder(os.Stdout).Encode(choices)
		}
		for _, choice := range choices {
			fmt.Printf("%s/%s\n", choice.Provider, choice.Model)
		}
		return nil
	}
	roleModels := make(map[string]string, len(config.SupportedRoles()))
	for _, role := range config.SupportedRoles() {
		resolved, resolveErr := cfg.ResolveRole(role)
		if resolveErr == nil && resolved.Model != "" {
			roleModels[role] = resolved.Model
		}
	}
	if *format == "json" {
		return json.NewEncoder(os.Stdout).Encode(roleModels)
	}
	for _, role := range config.SupportedRoles() {
		if model := roleModels[role]; model != "" {
			fmt.Printf("%s\t%s\n", role, model)
		}
	}
	return nil
}

type catalogModelChoice struct {
	Model            string   `json:"model"`
	Provider         string   `json:"provider"`
	ReasoningEfforts []string `json:"reasoningEfforts,omitempty"`
}

func configuredCatalogModels(cfg *config.Config) []catalogModelChoice {
	seen := map[string]struct{}{}
	var choices []catalogModelChoice
	catalogs := config.LoadCatalogs()
	add := func(model, provider string) {
		if model == "" || provider == "" {
			return
		}
		if _, ok := cfg.Providers[provider]; !ok {
			return
		}
		key := provider + "\x00" + model
		if _, ok := seen[key]; ok {
			return
		}
		seen[key] = struct{}{}
		choice := catalogModelChoice{Model: model, Provider: provider}
		if info := catalogs[provider].Find(model); info != nil {
			choice.ReasoningEfforts = info.SupportedEfforts()
		}
		choices = append(choices, choice)
	}
	for model, definition := range cfg.Models {
		for _, provider := range definition.Providers {
			add(model, provider)
		}
	}
	for _, role := range config.SupportedRoles() {
		if target, err := cfg.ResolveRole(role); err == nil {
			add(target.Model, target.Provider)
		}
	}
	for provider, catalog := range catalogs {
		for _, model := range catalog.Models {
			add(model.ID, provider)
		}
	}
	sort.Slice(choices, func(i, j int) bool {
		if choices[i].Model != choices[j].Model {
			return choices[i].Model < choices[j].Model
		}
		return choices[i].Provider < choices[j].Provider
	})
	return choices
}

func sessionsCLI(args []string) error {
	fs := flag.NewFlagSet("sessions", flag.ContinueOnError)
	format := fs.String("format", "text", "output format: text or json")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *format != "text" && *format != "json" {
		return fmt.Errorf("unknown --format %q (want text|json)", *format)
	}
	dir, err := config.Dir()
	if err != nil {
		return err
	}
	st, err := session.Open(filepath.Join(dir, "sessions.db"))
	if err != nil {
		return err
	}
	defer func() { _ = st.Close() }()
	metas, err := st.Recent(50)
	if err != nil {
		return err
	}
	if len(metas) == 0 {
		if *format == "json" {
			return json.NewEncoder(os.Stdout).Encode([]any{})
		}
		fmt.Println("no sessions yet")
		return nil
	}
	if *format == "json" {
		items := make([]sessionListItem, len(metas))
		for i, mt := range metas {
			items[i] = sessionListItem{ID: mt.ID, Title: mt.Title, Model: mt.Model, UpdatedAt: mt.UpdatedAt}
		}
		return json.NewEncoder(os.Stdout).Encode(items)
	}
	for _, mt := range metas {
		title := mt.Title
		if title == "" {
			title = "(untitled)"
		}
		fmt.Printf("%s  %-40s  %s  %s\n", mt.ID, trunc(title, 40), mt.Model, ago(mt.UpdatedAt))
	}
	return nil
}

type sessionListItem struct {
	ID        string    `json:"id"`
	Title     string    `json:"title"`
	Model     string    `json:"model"`
	UpdatedAt time.Time `json:"updated_at"`
}

func trunc(s string, n int) string {
	if n <= 0 {
		return ""
	}
	runes := []rune(s)
	if len(runes) <= n {
		return s
	}
	return string(runes[:n-1]) + "…"
}

func ago(t time.Time) string {
	d := time.Since(t)
	switch {
	case d < time.Minute:
		return "just now"
	case d < time.Hour:
		return fmt.Sprintf("%dm ago", int(d.Minutes()))
	case d < 24*time.Hour:
		return fmt.Sprintf("%dh ago", int(d.Hours()))
	default:
		return t.Format("2006-01-02")
	}
}

func outputsCLI(args []string) error {
	if len(args) == 0 || args[0] != "gc" {
		return fmt.Errorf("usage: ghg outputs gc [--max-age duration] [--max-bytes N]")
	}
	fs := flag.NewFlagSet("outputs gc", flag.ContinueOnError)
	maxAge := fs.Duration("max-age", 0, "remove unreferenced payloads older than this duration")
	maxBytes := fs.Int64("max-bytes", 0, "remove oldest unreferenced payloads until the store is at most N bytes")
	fs.Usage = func() {
		fmt.Fprintln(fs.Output(), "usage: ghg outputs gc [--max-age duration] [--max-bytes N]")
		fs.PrintDefaults()
	}
	if err := fs.Parse(args[1:]); err != nil {
		return err
	}
	if *maxAge < 0 || *maxBytes < 0 {
		return fmt.Errorf("cleanup limits must be non-negative")
	}
	if *maxAge == 0 && *maxBytes == 0 {
		return fmt.Errorf("provide --max-age or --max-bytes")
	}
	dir, err := config.Dir()
	if err != nil {
		return err
	}
	st, err := session.Open(filepath.Join(dir, "sessions.db"))
	if err != nil {
		return err
	}
	defer func() { _ = st.Close() }()
	payloads, err := session.NewOutputStore(filepath.Join(dir, "outputs"))
	if err != nil {
		return err
	}
	st.Outputs = payloads
	removed, err := st.GarbageCollectOutputs(context.Background(), *maxAge, *maxBytes)
	if err != nil {
		return err
	}
	fmt.Printf("removed %d unreferenced output payload(s)\n", removed)
	return nil
}

func artifactsCLI(args []string) error { return outputsCLI(args) }

package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"time"

	"github.com/sacca97/ghg/internal/config"
	"github.com/sacca97/ghg/internal/models"
	"github.com/sacca97/ghg/internal/session"
)

func loadProviderProfiles() (models.Profiles, error) {
	wd, err := os.Getwd()
	if err != nil {
		return models.Profiles{}, fmt.Errorf("provider profiles: current directory: %w", err)
	}
	return models.Load(models.LoadOptions{ProjectTrusted: config.Trusted(wd)})
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
	Model    string `json:"model"`
	Provider string `json:"provider"`
}

func configuredCatalogModels(cfg *config.Config) []catalogModelChoice {
	seen := map[string]struct{}{}
	var choices []catalogModelChoice
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
		choices = append(choices, catalogModelChoice{Model: model, Provider: provider})
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
	for provider, catalog := range config.LoadCatalogs() {
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

const installURL = "https://raw.githubusercontent.com/sacca97/ghg/main/install.sh"

func updateCLI() error {
	fmt.Printf("ghg %s — updating to the latest release via\n  curl -fsSL %s | sh\n\n", version, installURL)
	cmd := exec.Command("sh", "-c", "curl -fsSL "+installURL+" | sh")
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("update failed: %w", err)
	}
	fmt.Println("\nghg updated — restart any running sessions to use the new version.")
	return nil
}

func sessionsCLI() error {
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
		fmt.Println("no sessions yet")
		return nil
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

func trunc(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n-1] + "…"
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

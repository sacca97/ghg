package main

import (
	"bufio"
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"golang.org/x/term"

	"github.com/sacca97/ghg/internal/auth"
	"github.com/sacca97/ghg/internal/config"
	"github.com/sacca97/ghg/internal/models"
)

// authCLI validates and stores a credential for any loaded provider profile.
// Profile metadata is the source of truth for the endpoint, wire protocol,
// auth header, environment variable, and setup hint.
func authCLI(args []string) error {
	if len(args) == 0 {
		profiles, err := loadProviderProfiles()
		if err != nil {
			return err
		}
		return fmt.Errorf("usage: ghg auth <provider> [--env] [<key>] | ghg auth status [provider] | ghg auth logout [provider] (providers: %s)", strings.Join(profiles.IDs(), ", "))
	}

	subcmd := strings.TrimSpace(args[0])
	if subcmd == "status" {
		target := "codex-subscription"
		if len(args) > 1 {
			target = strings.TrimSpace(args[1])
		}
		return authStatusCLI(target)
	}
	if subcmd == "logout" {
		target := "codex-subscription"
		if len(args) > 1 {
			target = strings.TrimSpace(args[1])
		}
		return authLogoutCLI(target)
	}

	name := subcmd
	if len(args) > 1 && args[1] == "status" {
		return authStatusCLI(name)
	}
	if len(args) > 1 && args[1] == "logout" {
		return authLogoutCLI(name)
	}

	profiles, err := loadProviderProfiles()
	if err != nil {
		return err
	}
	resolved, err := auth.ResolveProfile(profiles, name)
	if err != nil {
		return err
	}
	if resolved.RequiresOAuth() {
		return authOAuthCLI(name, resolved)
	}
	if !resolved.RequiresAPIKey() {
		return fmt.Errorf("provider %q takes no API key", name)
	}

	fs := flag.NewFlagSet("auth "+name, flag.ContinueOnError)
	envMode := fs.Bool("env", false, "store the profile's environment variable instead of a literal key")
	if err := fs.Parse(args[1:]); err != nil {
		return err
	}
	if fs.NArg() > 1 {
		return fmt.Errorf("usage: ghg auth <provider> [--env] [<key>]")
	}
	if *envMode && resolved.Auth.EnvVar == "" {
		return fmt.Errorf("provider %q profile %q does not declare auth.env_var", name, resolved.Profile.ID)
	}

	key := ""
	fromEnv := false
	if fs.NArg() == 1 {
		key = config.TrimKey(fs.Arg(0))
	}
	if key == "" && *envMode {
		key = config.TrimKey(os.Getenv(resolved.Auth.EnvVar))
		fromEnv = key != ""
		if key == "" {
			return fmt.Errorf("environment variable %s is not set", resolved.Auth.EnvVar)
		}
	}
	if key == "" {
		key, err = promptKey(resolved.Profile.DisplayName + " API key: ")
		if err != nil && !errors.Is(err, io.EOF) {
			return err
		}
		key = config.TrimKey(key)
	}
	if key == "" {
		return fmt.Errorf("no API key provided for provider %q (%s)", name, auth.KeyHint(resolved))
	}

	fmt.Printf("validating key against %s… ", resolved.Profile.DisplayName)
	result, err := auth.Authenticate(context.Background(), profiles, name, key, 0)
	if err != nil {
		fmt.Println("failed")
		return err
	}
	if result.NeedsConfirmation {
		fmt.Println("unable to validate")
		if !confirmUnvalidated(name) {
			return fmt.Errorf("provider %q was not configured", name)
		}
	} else {
		fmt.Println("ok")
	}

	cfg, err := config.Load()
	if err != nil {
		return err
	}
	if err := cfg.UpsertProviderKey(name, result.Profile, key, *envMode); err != nil {
		return err
	}
	if err := cfg.Save(); err != nil {
		return err
	}
	if result.Validated && len(result.Models) > 0 {
		if err := config.SaveCatalog(name, result.Profile.BaseURL, result.Models); err != nil {
			fmt.Printf("catalog prefetch failed; provider remains configured: %v\n", err)
		}
	}
	if result.CatalogErr != nil {
		fmt.Printf("catalog prefetch failed; provider remains configured: %v\n", result.CatalogErr)
	}
	if *envMode && !fromEnv {
		offerShellExport(name, resolved.Auth.EnvVar, key)
	}

	if result.NeedsConfirmation {
		fmt.Printf("%s configured (key unvalidated).\n", name)
	} else if len(result.Models) > 0 {
		fmt.Printf("%s configured — %d models added to the catalog.\n", name, len(result.Models))
	} else {
		fmt.Printf("%s configured — credentials validated.\n", name)
	}
	return nil
}

func promptKey(prompt string) (string, error) {
	fmt.Print(prompt)
	if term.IsTerminal(int(os.Stdin.Fd())) {
		key, err := term.ReadPassword(int(os.Stdin.Fd()))
		fmt.Println()
		return string(key), err
	}
	return bufio.NewReader(os.Stdin).ReadString('\n')
}

func confirmUnvalidated(providerName string) bool {
	fmt.Printf("%s could not be validated; store the key anyway? [y/N] ", providerName)
	line, err := bufio.NewReader(os.Stdin).ReadString('\n')
	if err != nil && !errors.Is(err, io.EOF) {
		return false
	}
	switch strings.ToLower(strings.TrimSpace(line)) {
	case "y", "yes":
		return true
	default:
		return false
	}
}

// offerShellExport optionally persists an environment-mode key in the user's
// shell rc file. The key is never printed; writing it requires an explicit
// confirmation and follows the same 0600 permissions as the config file.
func offerShellExport(providerName, envVar, key string) {
	if envVar == "" || key == "" {
		return
	}
	rc := shellRC()
	if rc == "" || !term.IsTerminal(int(os.Stdin.Fd())) {
		fmt.Printf("set %s in your shell before running ghg.\n", envVar)
		return
	}
	fmt.Printf("append %s to %s for future runs? [y/N] ", envVar, rc)
	line, err := bufio.NewReader(os.Stdin).ReadString('\n')
	if err != nil && !errors.Is(err, io.EOF) {
		return
	}
	if answer := strings.ToLower(strings.TrimSpace(line)); answer != "y" && answer != "yes" {
		fmt.Printf("set %s in your shell before running ghg.\n", envVar)
		return
	}
	f, err := os.OpenFile(rc, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		fmt.Printf("could not update %s; set %s in your shell.\n", rc, envVar)
		return
	}
	defer func() { _ = f.Close() }()
	_, _ = fmt.Fprintf(f, "\n# ghg auth %s\nexport %s=%s\n", providerName, envVar, shellQuote(key))
	fmt.Printf("saved %s in %s.\n", envVar, rc)
}

func shellQuote(value string) string {
	return "'" + strings.ReplaceAll(value, "'", "'\"'\"'") + "'"
}

func shellRC() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	switch filepath.Base(os.Getenv("SHELL")) {
	case "zsh":
		return filepath.Join(home, ".zshrc")
	case "bash":
		return filepath.Join(home, ".bashrc")
	default:
		return ""
	}
}

func authOAuthCLI(name string, resolved models.Resolved) error {
	fmt.Printf("Starting OAuth login for %s…\n", resolved.Profile.DisplayName)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	opts := auth.LoginOptions{
		OpenBrowser: true,
		Printer: func(url string) {
			fmt.Printf("Open the following authorization URL in your browser:\n\n  %s\n\nWaiting for callback on http://localhost:1455…\n", url)
		},
	}
	creds, err := auth.Login(ctx, opts)
	if err != nil {
		return fmt.Errorf("OAuth login failed: %w", err)
	}
	fmt.Println("OAuth login successful!")
	if creds.AccountID != "" {
		fmt.Printf("Account ID: %s\n", creds.AccountID)
	}
	return configureOAuthPostLogin(name, resolved)
}

func configureOAuthPostLogin(name string, resolved models.Resolved) error {
	cfg, err := config.Load()
	if err != nil {
		return err
	}
	if err := cfg.UpsertOAuthProvider(name, resolved); err != nil {
		return err
	}
	if err := cfg.Save(); err != nil {
		return err
	}

	backend, berr := auth.NewBackend(resolved, "", "", 0)
	var modelErr error
	if berr != nil {
		modelErr = berr
	} else if cat, ok := backend.(models.CatalogBackend); ok {
		modelsCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		models, err := cat.Models(modelsCtx)
		if err != nil {
			modelErr = err
		} else if len(models) > 0 {
			_ = config.SaveCatalog(name, resolved.BaseURL, models)
			fmt.Printf("%s configured — %d models added to the catalog.\n", resolved.Profile.DisplayName, len(models))
			return nil
		}
	}
	fmt.Printf("%s configured.\n", resolved.Profile.DisplayName)
	if modelErr != nil {
		fmt.Printf("Warning: model discovery failed: %s (run /model refresh to retry).\n", modelErr.Error())
	}
	return nil
}

func authStatusCLI(name string) error {
	mgr := auth.DefaultCodexCredentialManager()
	st, err := mgr.Status(context.Background())
	if err != nil {
		return err
	}
	if !st.Configured {
		fmt.Printf("Provider %q is not authenticated.\n", name)
		return nil
	}
	fmt.Printf("Provider %q authentication status:\n", name)
	if st.AccountID != "" {
		fmt.Printf("  Account ID: %s\n", st.AccountID)
	}
	if st.Expired {
		fmt.Printf("  Status:     expired at %s\n", st.ExpiresAt.Format(time.RFC3339))
	} else {
		fmt.Printf("  Status:     valid until %s\n", st.ExpiresAt.Format(time.RFC3339))
	}
	return nil
}

func authLogoutCLI(name string) error {
	mgr := auth.DefaultCodexCredentialManager()
	if err := mgr.Logout(context.Background()); err != nil {
		return err
	}
	fmt.Printf("Logged out from %q.\n", name)
	return nil
}

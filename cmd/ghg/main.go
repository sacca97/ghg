// ghg is a minimal coding agent ghg.
package main

import (
	_ "embed"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/sacca97/ghg/internal/agent"
	"github.com/sacca97/ghg/internal/config"
	"github.com/sacca97/ghg/internal/session"
	"github.com/sacca97/ghg/internal/tui"
)

var version = "dev" // set via -ldflags "-X main.version=..."

//go:embed system-prompt.md
var embeddedSystemPrompt string

func systemPrompt() string {
	wd, _ := os.Getwd()
	return systemPromptForProject(config.Trusted(wd))
}

// systemPromptForProject builds the stable prompt prefix. Headless runs are
// explicitly trusted automation, while the interactive caller starts with the
// persisted trust state and tui.Run fills the first-run gap after its prompt.
func systemPromptForProject(projectTrusted bool) string {
	wd, _ := os.Getwd()
	prompt := strings.TrimRight(embeddedSystemPrompt, "\n") + "\n\nCurrent working directory: " + wd
	if extra := config.MeInstructions(); extra != "" {
		prompt += "\n\nStanding instructions from the user (~/.ghg/me.md — treat as user rules):\n" + extra
	}
	if project := config.ProjectInstructions(wd, projectTrusted); project != "" {
		prompt += "\n\n" + project
	}
	// the skills block is appended fresh each turn by the TUI, so newly added
	// skills are picked up without restarting
	return prompt
}

// continueSessionID resolves --continue without changing the existing
// resume/picker path. Only sessions with persisted messages are eligible.
func continueSessionID() (string, error) {
	wd, err := os.Getwd()
	if err != nil {
		return "", err
	}
	dir, err := config.Dir()
	if err != nil {
		return "", err
	}
	st, err := session.Open(filepath.Join(dir, "sessions.db"))
	if err != nil {
		return "", err
	}
	defer func() { _ = st.Close() }()
	meta, err := st.MostRecentForCWD(wd)
	if err != nil {
		return "", fmt.Errorf("--continue: %w", err)
	}
	return meta.ID, nil
}

func main() {
	if os.Getenv("GHG_INTERNAL_WORKER") == "1" {
		if err := runWorkerProcess(); err != nil {
			fmt.Fprintln(os.Stderr, "ghg worker:", err)
			os.Exit(1)
		}
		return
	}
	modelFlag := flag.String("m", "", "model name from ~/.ghg/config.json (default: defaultModel)")
	providerFlag := flag.String("p", "", "provider to route the model through (default: model's first provider)")
	versionFlag := flag.Bool("version", false, "print version")
	resumeFlag := flag.String("resume", "", "resume a previous session by id (or unique prefix)")
	continueFlag := flag.Bool("continue", false, "resume the most recent session for the current working directory")
	benchFlag := flag.Bool("bench", false, "do full startup init (config, routing, key, agent) then exit; for `task benchmark`")
	cautiousFlag := flag.Bool("cautious", false, "ask before running commands / writing files")
	sandboxFlag := flag.String("sandbox", "", "execution sandbox: read-only, workspace-write, or danger-full-access")
	networkFlag := flag.String("network", "", "execution network: deny or host")
	approvalFlag := flag.String("approval", "", "exceptional capability approval: ask, auto-review, or never")
	flag.Parse()
	die := func(err error) {
		fmt.Fprintln(os.Stderr, "ghg:", err)
		os.Exit(1)
	}

	if *versionFlag {
		fmt.Println("ghg", version)
		return
	}

	// `ghg mcp ...` — server management and the MCP server mode.
	if flag.NArg() > 0 && flag.Arg(0) == "mcp" {
		if err := mcpCLI(flag.Args()[1:], version); err != nil {
			die(err)
		}
		return
	}

	// `ghg run ...` — non-interactive one-turn mode for scripting; no TTY or
	// trust prompt required (headless use implies trusted automation).
	if flag.NArg() > 0 && flag.Arg(0) == "run" {
		if err := runCLI(flag.Args()[1:]); err != nil {
			die(err)
		}
		return
	}

	// `ghg sessions` — list stored sessions (the scriptable companion to run).
	if flag.NArg() > 0 && flag.Arg(0) == "sessions" {
		if err := sessionsCLI(); err != nil {
			die(err)
		}
		return
	}

	if flag.NArg() > 0 && flag.Arg(0) == "ps" {
		if err := workerPSCLI(); err != nil {
			die(err)
		}
		return
	}
	if flag.NArg() > 0 && flag.Arg(0) == "attach" {
		if err := workerAttachCLI(flag.Args()[1:]); err != nil {
			die(err)
		}
		return
	}
	if flag.NArg() > 0 && flag.Arg(0) == "stop" {
		if err := workerStopCLI(flag.Args()[1:]); err != nil {
			die(err)
		}
		return
	}

	// `ghg artifacts gc` — reclaim unreferenced content-addressed tool
	// results without ever deleting a payload still indexed by a session.
	if flag.NArg() > 0 && flag.Arg(0) == "artifacts" {
		if err := artifactsCLI(flag.Args()[1:]); err != nil {
			die(err)
		}
		return
	}

	// `ghg update` — re-run the install script to get the latest release.
	if flag.NArg() > 0 && flag.Arg(0) == "update" {
		if err := updateCLI(); err != nil {
			die(err)
		}
		return
	}

	// `ghg auth ...` — profile-driven provider key onboarding.
	if flag.NArg() > 0 && flag.Arg(0) == "auth" {
		if err := authCLI(flag.Args()[1:]); err != nil {
			die(err)
		}
		return
	}

	cfg, err := config.Load()
	if err != nil {
		fmt.Fprintln(os.Stderr, "ghg:", err)
		os.Exit(1)
	}
	if err := cfg.ApplyExecutionOverrides(*sandboxFlag, *networkFlag, *approvalFlag); err != nil {
		fmt.Fprintln(os.Stderr, "ghg:", err)
		os.Exit(1)
	}

	if *benchFlag {
		if *modelFlag == "" && *providerFlag == "" {
			profiles, err := loadProviderProfiles()
			if err != nil {
				fmt.Fprintln(os.Stderr, "ghg:", err)
				os.Exit(1)
			}
			if _, _, _, err := newModeAgent(cfg, profiles, config.ModeActing, systemPrompt()); err != nil {
				fmt.Fprintln(os.Stderr, "ghg:", err)
				os.Exit(1)
			}
			return
		}
		prov, mdl, id, err := cfg.Resolve(*modelFlag, *providerFlag)
		if err != nil {
			fmt.Fprintln(os.Stderr, "ghg:", err)
			os.Exit(1)
		}
		profiles, err := loadProviderProfiles()
		if err != nil {
			fmt.Fprintln(os.Stderr, "ghg:", err)
			os.Exit(1)
		}
		provName := *providerFlag
		if provName == "" {
			provName = cfg.DefaultProvider
			if provName == "" && len(mdl.Providers) > 0 {
				provName = mdl.Providers[0]
			}
		}
		backend, err := newProviderBackend(profiles, provName, prov, "bench", cfg.MaxRetries, id, mdl.API)
		if err != nil {
			fmt.Fprintln(os.Stderr, "ghg:", err)
			os.Exit(1)
		}
		_ = agent.New(backend, id, mdl.MaxTokens, systemPrompt())
		return
	}
	if *continueFlag && *resumeFlag != "" {
		fmt.Fprintln(os.Stderr, "ghg: --continue and --resume are mutually exclusive")
		os.Exit(1)
	}
	resumeID := *resumeFlag
	if *continueFlag {
		resumeID, err = continueSessionID()
		if err != nil {
			fmt.Fprintln(os.Stderr, "ghg:", err)
			os.Exit(1)
		}
	}
	tui.Version = version // /report names the build in the bug-report bundle
	_, err = tui.Run(cfg, *modelFlag, *providerFlag, systemPrompt(), resumeID, *cautiousFlag)
	if err != nil {
		fmt.Fprintln(os.Stderr, "ghg:", err)
		os.Exit(1)
	}
}

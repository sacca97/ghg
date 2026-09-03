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
	flag.Usage = func() {
		fmt.Fprintln(flag.CommandLine.Output(), "usage: ghg [flags] [prompt]")
		flag.PrintDefaults()
		fmt.Fprintln(flag.CommandLine.Output(), `
commands:
  run       execute one headless turn
  sessions  list saved sessions
  ps        list background workers
  attach    attach to a background worker
  stop      stop a background worker
  outputs   collect unreferenced output payloads
  mcp       manage MCP servers
  auth      configure provider credentials
  export    export session results
  update    update ghg`)
	}
	flag.Parse()
	die := func(err error) {
		fmt.Fprintln(os.Stderr, "ghg:", err)
		os.Exit(1)
	}

	if *versionFlag {
		fmt.Println("ghg", version)
		return
	}

	if flag.NArg() > 0 {
		args := flag.Args()[1:]
		switch flag.Arg(0) {
		case "run":
			if err := runCLI(args); err != nil {
				die(err)
			}
			return
		case "sessions":
			if err := sessionsCLI(); err != nil {
				die(err)
			}
			return
		case "ps":
			if err := workerPSCLI(); err != nil {
				die(err)
			}
			return
		case "attach":
			if err := workerAttachCLI(args); err != nil {
				die(err)
			}
			return
		case "stop":
			if err := workerStopCLI(args); err != nil {
				die(err)
			}
			return
		case "outputs", "artifacts":
			if err := outputsCLI(args); err != nil {
				die(err)
			}
			return
		case "mcp":
			if err := mcpCLI(args, version); err != nil {
				die(err)
			}
			return
		case "auth":
			if err := authCLI(args); err != nil {
				die(err)
			}
			return
		case "export":
			if err := exportCLI(args); err != nil {
				die(err)
			}
			return
		case "update":
			if err := updateCLI(); err != nil {
				die(err)
			}
			return
		default:
			die(fmt.Errorf("unknown command %q (see ghg --help)", flag.Arg(0)))
		}
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
		profiles, err := loadProviderProfiles()
		if err != nil {
			fmt.Fprintln(os.Stderr, "ghg:", err)
			os.Exit(1)
		}
		if *modelFlag == "" && *providerFlag == "" {
			if _, _, _, err := agent.NewConfiguredForRole(cfg, profiles, config.RoleForMode(config.ModeActing), systemPrompt(), false); err != nil {
				fmt.Fprintln(os.Stderr, "ghg:", err)
				os.Exit(1)
			}
			return
		}
		if _, _, _, err := agent.NewConfigured(agent.BuildOptions{
			Config: cfg, Profiles: profiles, Model: *modelFlag, Provider: *providerFlag,
			Role: config.RoleForMode(config.ModeActing), SystemPrompt: systemPrompt(),
		}); err != nil {
			fmt.Fprintln(os.Stderr, "ghg:", err)
			os.Exit(1)
		}
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

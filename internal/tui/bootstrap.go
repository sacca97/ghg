package tui

import (
	"fmt"
	"os"
	"os/signal"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/charmbracelet/bubbles/spinner"
	tea "github.com/charmbracelet/bubbletea"

	"github.com/sacca97/ghg/internal/config"
	"github.com/sacca97/ghg/internal/models"
	"github.com/sacca97/ghg/internal/session"
	"github.com/sacca97/ghg/internal/skills"
	"golang.org/x/term"
)

// Run starts an interactive session.
func Run(cfg *config.Config, modelName, provName, sysPrompt, resumeID string, cautious bool) (string, error) {
	return runTUI(cfg, modelName, provName, sysPrompt, resumeID, cautious, true)
}

// RunAttached opens the TUI for an existing worker.
func RunAttached(cfg *config.Config, modelName, provName, sysPrompt, sessionID string, cautious bool) (string, error) {
	return runTUI(cfg, modelName, provName, sysPrompt, sessionID, cautious, false)
}

func runTUI(cfg *config.Config, modelName, provName, sysPrompt, resumeID string, cautious, launchWorker bool) (string, error) {
	wd, err := os.Getwd()
	if err != nil {
		return "", err
	}
	if ok, trustErr := config.CheckTrust(wd); trustErr != nil {
		return "", trustErr
	} else if !ok {
		return "", fmt.Errorf("folder not trusted")
	}
	if !strings.Contains(sysPrompt, "<project_instructions>") {
		if project := config.ProjectInstructions(wd, true); project != "" {
			sysPrompt += "\n\n" + project
		}
	}

	profiles, err := models.Load(models.LoadOptions{ProjectTrusted: true})
	if err != nil {
		return "", err
	}
	role := config.RoleForMode(config.ModeActing)
	if modelName != "" || provName != "" {
		role = config.RoleDefault
	}
	route, routeErr := resolveDisplayRoute(cfg, profiles, modelName, provName, role)
	if routeErr != nil {
		return "", routeErr
	}

	ti := newInput()
	mouseOn := true
	if cfg.Mouse != nil {
		mouseOn = *cfg.Mouse
	}
	showThinking := true
	if cfg.Thinking != nil {
		showThinking = *cfg.Thinking
	}
	m := &model{
		cfg: cfg, modelName: route.ModelName, provName: route.ProviderName,
		modelID: route.APIID, protocol: route.Protocol, role: route.Role,
		effort: route.Effort, contextLimit: route.ContextLimit,
		messages:  []models.Message{{Role: "system", Content: sysPrompt}},
		sysPrompt: sysPrompt,
		input:     ti, spin: spinner.New(spinner.WithSpinner(spinner.Dot)), follow: true,
		catalogs: config.LoadCatalogs(), profiles: profiles, mouseOn: mouseOn, now: time.Now, showThinking: showThinking,
		compactModel: cfg.CompactModel, compactProv: cfg.CompactProvider,
		mode:      uiModeExecute,
		cautious:  cautious,
		skillScan: func() []skills.Skill { return skills.Scan(skills.DefaultDirs()...) },
		shortCWD:  shortCWD(),
	}
	m.modelSlotW = m.statusModelSlotWidth()
	if dir, derr := config.Dir(); derr == nil {
		if st, serr := session.Open(dir + "/sessions.db"); serr == nil {
			m.store = st
			defer func() { _ = st.Close() }()
			if hist, herr := st.UserHistory(500); herr == nil && len(hist) > 0 {
				for i := len(hist) - 1; i >= 0; i-- {
					m.hist = append(m.hist, hist[i])
				}
				m.histIdx = len(m.hist)
			}
		} else {
			config.LogEvent("session.open", "FAILED: "+serr.Error())
			m.append(errStyle.Render("sessions disabled: " + serr.Error()))
		}
	}
	if resumeID != "" {
		if m.store == nil {
			return "", fmt.Errorf("cannot resume: session store unavailable")
		}
		if err := m.resume(resumeID); err != nil {
			return "", err
		}
	}
	m.startupReport()

	opts := []tea.ProgramOption{tea.WithoutSignalHandler(), tea.WithAltScreen()}
	if m.mouseOn {
		opts = append(opts, tea.WithMouseCellMotion())
	}
	p := tea.NewProgram(m, opts...)
	m.prog = p
	if !launchWorker {
		if err := m.attachWorkerProcess(resumeID); err != nil {
			return "", err
		}
	}
	restoreTTY := captureTTYState()
	if restoreTTY != nil {
		defer restoreTTY()
	}
	stopSignals := forwardSignals(p)
	defer stopSignals()
	_, err = p.Run()
	if restoreTTY != nil {
		restoreTTY()
	}
	m.stopWorker()
	return m.sessionID, err
}

func (m *model) startupReport() {
	sk, problems := skills.ScanDetailed(skills.DefaultDirs()...)
	m.skillsCache = sk
	m.skillsLoaded = len(sk)
	var warnLines []string
	var infoLines []string

	for _, s := range sk {
		if s.Warning != "" {
			warnLines = append(warnLines, fmt.Sprintf("  ⚠ %s: %s", s.Name, s.Warning))
		}
	}
	for _, p := range problems {
		warnLines = append(warnLines, fmt.Sprintf("  ⚠ %s: %s", p.Path, p.Err))
	}
	if m.modelName == "" || m.provName == "" {
		warnLines = append(warnLines, m.degradedProviderNote())
	}
	if len(warnLines) == 0 && len(infoLines) == 0 {
		return
	}
	var out []string
	for _, w := range warnLines {
		out = append(out, errStyle.Render(w))
	}
	for _, inf := range infoLines {
		out = append(out, dimStyle.Render(inf))
	}
	m.append(strings.Join(out, "\n"))
}

func forwardSignals(p *tea.Program) func() {
	signals := make(chan os.Signal, 2)
	done := make(chan struct{})
	var stopOnce sync.Once
	var wg sync.WaitGroup

	signal.Notify(signals, os.Interrupt, syscall.SIGTERM)
	wg.Add(1)
	go func() {
		defer wg.Done()
		var lastInterrupt time.Time
		const confirmWindow = 2 * time.Second
		for {
			select {
			case <-done:
				return
			case sig := <-signals:
				if sig == os.Interrupt {
					now := time.Now()
					if !lastInterrupt.IsZero() && now.Sub(lastInterrupt) <= confirmWindow {
						p.Quit()
						return
					}
					lastInterrupt = now
					p.Send(tea.KeyMsg{Type: tea.KeyCtrlC})
				} else {
					p.Quit()
				}
			}
		}
	}()

	return func() {
		stopOnce.Do(func() {
			signal.Stop(signals)
			close(done)
			wg.Wait()
		})
	}
}

func captureTTYState() func() {
	fd := int(os.Stdin.Fd())
	var closeTTY func()
	if !term.IsTerminal(fd) {
		tty, err := os.Open("/dev/tty")
		if err != nil {
			return nil
		}
		fd = int(tty.Fd())
		closeTTY = func() { _ = tty.Close() }
	}
	state, err := term.GetState(fd)
	if err != nil {
		if closeTTY != nil {
			closeTTY()
		}
		return nil
	}
	var once sync.Once
	return func() {
		once.Do(func() {
			_ = term.Restore(fd, state)
			if closeTTY != nil {
				closeTTY()
			}
		})
	}
}

package tui

import (
	"net"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/sacca97/ghg/internal/config"
	workerwire "github.com/sacca97/ghg/internal/worker"
)

func lastBlock(m *model) string {
	if len(m.blocks) == 0 {
		return ""
	}
	return m.blocks[len(m.blocks)-1].text
}

type workerFrameResult struct {
	frame workerwire.Frame
	err   error
}

func workerTestFrames(t *testing.T, m *model, id string) <-chan workerFrameResult {
	t.Helper()
	server, client := net.Pipe()
	done := make(chan struct{})
	frames := make(chan workerFrameResult, 1)
	m.workerClient = workerwire.NewClient(client, id)
	t.Cleanup(func() {
		close(done)
		_ = server.Close()
		_ = client.Close()
	})
	go func() {
		decoder := workerwire.NewDecoder(server)
		for {
			frame, err := decoder.Read()
			select {
			case frames <- workerFrameResult{frame: frame, err: err}:
			case <-done:
				return
			}
			if err != nil {
				return
			}
		}
	}()
	return frames
}

func nextWorkerFrame(t *testing.T, frames <-chan workerFrameResult) workerwire.Frame {
	t.Helper()
	select {
	case result := <-frames:
		if result.err != nil {
			t.Fatalf("read worker frame: %v", result.err)
		}
		return result.frame
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for worker frame")
		return workerwire.Frame{}
	}
}

func compactCmdModel() *model {
	// NOTE: any test that drives setEffort/switchModel writes
	// through cfg.Save(); TestMain points GHG_HOME at a scratch dir so
	// those writes can never reach the real ~/.ghg/config.json.
	m := &model{
		input:   newInput(),
		mouseOn: true, // matches the Run() default (wheel scroll + app selection)
		cfg: &config.Config{
			Roles:     map[string]config.RoleConfig{config.RoleDefault: {Model: "kimi-k3-fast"}},
			Providers: map[string]config.Provider{"inference": {BaseURL: "https://x", APIKey: "k"}},
			Models: map[string]config.Model{
				"kimi-k3-fast": {Providers: []string{"inference"}},
				"glm-5.2-fast": {Providers: []string{"inference"}},
			},
		},
		modelName: "kimi-k3-fast",
		provName:  "inference",
		effort:    "",
		catalogs: map[string]config.Catalog{
			"inference": {Models: []config.ModelInfoLite{
				{ID: "kimi-k3-fast", ContextLength: 131072},
			}},
		},
	}
	m.width = 80
	m.input.SetWidth(78)
	return m
}

func modelCmdModel() *model {
	m := &model{
		input: newInput(),
		cfg: &config.Config{
			Roles:     map[string]config.RoleConfig{config.RoleDefault: {Model: "kimi-k3-fast"}},
			Providers: map[string]config.Provider{"inference": {BaseURL: "https://x", APIKey: "k"}},
			Models: map[string]config.Model{
				"kimi-k3-fast": {Providers: []string{"inference"}},
				"glm-5.2-fast": {Providers: []string{"inference"}},
			},
		},
		modelName: "kimi-k3-fast",
		provName:  "inference",
	}
	m.width = 80
	m.input.SetWidth(78)
	return m
}

func typeStr(t *testing.T, m *model, s string) *model {
	t.Helper()
	for _, r := range s {
		tm, _ := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{r}})
		m = tm.(*model)
	}
	return m
}

// Regression: typing /model and pressing enter must open the interactive
// picker, NOT insert a newline. (The newline bug was KeyCtrlM == KeyEnter
// being forwarded to the textarea; this guards against its return.)

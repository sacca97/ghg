package tui

import (
	"io"
	"os"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"
)

type signalTestModel struct {
	ctrlCs chan struct{}
	count  int
}

func (m *signalTestModel) Init() tea.Cmd { return nil }

func (m *signalTestModel) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	key, ok := msg.(tea.KeyMsg)
	if !ok || key.Type != tea.KeyCtrlC {
		return m, nil
	}
	m.count++
	if m.count == 1 {
		close(m.ctrlCs)
		return m, nil
	}
	return m, tea.Quit
}

func (*signalTestModel) View() string { return "signal test" }

func TestForwardSignalsPreservesDoubleCtrlC(t *testing.T) {
	m := &signalTestModel{ctrlCs: make(chan struct{})}
	p := tea.NewProgram(m,
		tea.WithInput(nil),
		tea.WithOutput(io.Discard),
		tea.WithoutSignalHandler(),
	)
	stopSignals := forwardSignals(p)
	defer stopSignals()

	done := make(chan struct{})
	var (
		final  tea.Model
		runErr error
	)
	go func() {
		final, runErr = p.Run()
		close(done)
	}()

	proc, err := os.FindProcess(os.Getpid())
	if err != nil {
		t.Fatal(err)
	}
	if err := proc.Signal(os.Interrupt); err != nil {
		t.Fatal(err)
	}
	select {
	case <-m.ctrlCs:
	case <-time.After(time.Second):
		p.Kill()
		t.Fatal("first interrupt was not forwarded")
	}
	if err := proc.Signal(os.Interrupt); err != nil {
		t.Fatal(err)
	}

	select {
	case <-done:
	case <-time.After(time.Second):
		p.Kill()
		t.Fatal("second interrupt did not quit the program")
	}
	if runErr != nil {
		t.Fatalf("double ctrl+c should quit gracefully, got %v", runErr)
	}
	if got := final.(*signalTestModel).count; got != 2 {
		t.Fatalf("forwarded ctrl+c count = %d, want 2", got)
	}
}

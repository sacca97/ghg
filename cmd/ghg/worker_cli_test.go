package main

import (
	"errors"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/sacca97/ghg/internal/config"
	workerwire "github.com/sacca97/ghg/internal/worker"
)

func TestWorkerPSCleansStoppedState(t *testing.T) {
	t.Setenv("GHG_HOME", t.TempDir())
	dir, err := config.Dir()
	if err != nil {
		t.Fatal(err)
	}
	runtimeFile, err := workerwire.NewRuntime(dir, "stopped-session")
	if err != nil {
		t.Fatal(err)
	}
	if err := runtimeFile.WriteState(workerwire.StateRecord{
		State:     workerwire.StateIdle,
		UpdatedAt: time.Now().UTC(),
	}); err != nil {
		t.Fatal(err)
	}

	var runErr error
	printed := captureStdout(t, func() { runErr = workerPSCLI() })
	if runErr != nil {
		t.Fatal(runErr)
	}
	if !strings.Contains(printed, "no live workers") {
		t.Fatalf("ps output = %q, want no live workers", printed)
	}
	if _, err := os.Stat(runtimeFile.StatePath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("stopped state still exists: %v", err)
	}
}

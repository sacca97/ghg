package tools

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestPostEditHooksRunSortedBatchAndBoundFailures(t *testing.T) {
	dir := t.TempDir()
	first := filepath.Join(dir, "a.go")
	last := filepath.Join(dir, "z.go")
	ignored := filepath.Join(dir, "note.txt")
	logPath := filepath.Join(dir, "hook-args.log")
	for _, path := range []string{first, last, ignored} {
		if err := os.WriteFile(path, []byte("before\n"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	quotedLog := "'" + strings.ReplaceAll(logPath, "'", "'\\''") + "'"
	runtime := &ToolRuntime{PostEditHooks: []PostEditHook{{
		Command:    []string{"/bin/sh", "-c", "printf '%s\\n' \"$@\" > " + quotedLog + "; printf 'formatted\\n' > \"$1\"", "post-edit"},
		Extensions: []string{".go"},
		Timeout:    time.Second,
	}}}
	if reports := runtime.RunPostEditHooks(context.Background(), []string{last, ignored, first, first}); len(reports) != 0 {
		t.Fatalf("silent hook reports = %+v", reports)
	}
	args, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatal(err)
	}
	if string(args) != first+"\n"+last+"\n" {
		t.Fatalf("hook paths = %q", args)
	}
	formatted, err := os.ReadFile(first)
	if err != nil {
		t.Fatal(err)
	}
	if string(formatted) != "formatted\n" {
		t.Fatalf("hook did not modify the selected file: %q", formatted)
	}
	writeArgs, err := json.Marshal(map[string]string{"path": first, "content": "before\n"})
	if err != nil {
		t.Fatal(err)
	}
	writeResult := ExecuteResult(WithRuntime(context.Background(), runtime), All(), "write", writeArgs)
	if writeResult.ExitCode != 0 || !strings.Contains(writeResult.Preview, "postEdit final bytes") {
		t.Fatalf("write did not report formatter readback: %+v", writeResult)
	}
	formatted, err = os.ReadFile(first)
	if err != nil {
		t.Fatal(err)
	}
	if string(formatted) != "formatted\n" {
		t.Fatalf("write did not retain formatter output: %q", formatted)
	}

	failure := &ToolRuntime{PostEditHooks: []PostEditHook{{
		Command: []string{"/bin/sh", "-c", "i=0; while [ $i -lt 12000 ]; do printf x; i=$((i+1)); done; exit 7", "post-edit"},
		Timeout: time.Second,
	}}}
	reports := failure.RunPostEditHooks(context.Background(), []string{first})
	if len(reports) != 1 || !strings.HasPrefix(reports[0].Status, "exit:") || reports[0].OmittedStdout == 0 || len(reports[0].Stdout) > hookOutputLimit {
		t.Fatalf("bounded failure report = %+v", reports)
	}
}

package config

import (
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// ghg.log records file mutation events for state debugging.
// Log writes are best-effort and ignore write failures.

const (
	logFileName = "ghg.log"
	// logMaxBytes caps the log file before single-generation rotation.
	logMaxBytes = 1 << 20 // 1 MiB
)

var logMu sync.Mutex

// LogEvent appends one timestamped line to ~/.ghg/ghg.log. op is a short
// verb ("config.save", "config.load", "catalog.fetch", ...); detail is
// free-form context. Best-effort: errors are swallowed by design.
func LogEvent(op, detail string) {
	logMu.Lock()
	defer logMu.Unlock()
	dir, err := Dir()
	if err != nil {
		return
	}
	p := filepath.Join(dir, logFileName)
	if st, err := os.Stat(p); err == nil && st.Size() > logMaxBytes {
		_ = os.Rename(p, p+".1") // single-generation rotation
	}
	f, err := os.OpenFile(p, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return
	}
	defer f.Close()
	fmt.Fprintf(f, "%s %-16s pid=%d %s\n", time.Now().UTC().Format(time.RFC3339), op, os.Getpid(), detail)
}

// logf is the printf-style convenience form.
func logf(op, format string, args ...any) {
	LogEvent(op, fmt.Sprintf(format, args...))
}

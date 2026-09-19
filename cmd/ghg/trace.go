package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/sacca97/ghg/internal/config"
	"github.com/sacca97/ghg/internal/session"
)

type traceRecord struct {
	SessionID string          `json:"session_id"`
	Seq       int             `json:"seq"`
	Kind      string          `json:"kind"`
	Payload   json.RawMessage `json:"payload"`
	CreatedAt string          `json:"created_at"`
}

func traceCLI(args []string) error {
	fs := flag.NewFlagSet("trace", flag.ContinueOnError)
	jsonl := fs.Bool("jsonl", false, "write one JSON object per event")
	fs.Usage = func() {
		fmt.Fprintln(os.Stderr, "usage: ghg trace <session> [--jsonl]")
		fs.PrintDefaults()
	}
	parseArgs := make([]string, 0, len(args))
	for _, arg := range args {
		if arg == "--jsonl" {
			*jsonl = true
			continue
		}
		parseArgs = append(parseArgs, arg)
	}
	if err := fs.Parse(parseArgs); err != nil {
		return err
	}
	if fs.NArg() != 1 || strings.TrimSpace(fs.Arg(0)) == "" {
		return fmt.Errorf("usage: ghg trace <session> [--jsonl]")
	}
	dir, err := config.Dir()
	if err != nil {
		return err
	}
	store, err := session.Open(filepath.Join(dir, "sessions.db"))
	if err != nil {
		return err
	}
	defer func() { _ = store.Close() }()
	meta, _, err := store.Load(strings.TrimSpace(fs.Arg(0)))
	if err != nil {
		return fmt.Errorf("resolve session %q: %w", fs.Arg(0), err)
	}
	events, err := store.ListTelemetry(context.Background(), meta.ID)
	if err != nil {
		return fmt.Errorf("load trace: %w", err)
	}
	return writeTrace(os.Stdout, meta.ID, events, *jsonl)
}

func writeTrace(w io.Writer, sessionID string, events []session.TelemetryEvent, jsonl bool) error {
	if jsonl {
		enc := json.NewEncoder(w)
		for _, event := range events {
			if err := enc.Encode(traceRecord{
				SessionID: sessionID,
				Seq:       event.Seq,
				Kind:      event.Kind,
				Payload:   event.Payload,
				CreatedAt: event.CreatedAt.UTC().Format(time.RFC3339Nano),
			}); err != nil {
				return err
			}
		}
		return nil
	}
	for _, event := range events {
		payload, err := json.Marshal(event.Payload)
		if err != nil {
			return err
		}
		if _, err := fmt.Fprintf(w, "%d\t%s\t%s\t%s\n", event.Seq, event.CreatedAt.UTC().Format("15:04:05.000"), event.Kind, payload); err != nil {
			return err
		}
	}
	return nil
}

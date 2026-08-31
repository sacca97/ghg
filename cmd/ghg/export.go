// `ghg export` — export structured workflow results (plans, reviews) to files or stdout.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/sacca97/ghg/internal/config"
	"github.com/sacca97/ghg/internal/export"
	"github.com/sacca97/ghg/internal/session"
)

func exportCLI(args []string) error {
	fs := flag.NewFlagSet("export", flag.ContinueOnError)
	sessionFlag := fs.String("session", "", "session id (defaults to most recent session for cwd)")
	resultFlag := fs.String("result", "latest", "result id to export or 'latest'")
	kindFlag := fs.String("kind", "", "result kind: plan, review, or any if empty")
	formatFlag := fs.String("format", "markdown", "export format: markdown or json")
	outputFlag := fs.String("output", "", "destination file path (if empty, writes to stdout)")
	forceFlag := fs.Bool("force", false, "overwrite destination file if it exists")

	fs.Usage = func() {
		fmt.Fprintln(os.Stderr, "usage: ghg export [--session id] [--result id|latest] [--kind plan|review|chat] [--format markdown|json|text] [--output path] [--force]")
		fs.PrintDefaults()
	}
	if err := fs.Parse(args); err != nil {
		return err
	}

	wd, err := os.Getwd()
	if err != nil {
		return err
	}
	dir, err := config.Dir()
	if err != nil {
		return err
	}
	st, err := session.Open(filepath.Join(dir, "sessions.db"))
	if err != nil {
		return err
	}
	defer func() { _ = st.Close() }()

	sessionID := strings.TrimSpace(*sessionFlag)
	if sessionID == "" {
		meta, err := st.MostRecentForCWD(wd)
		if err != nil {
			return fmt.Errorf("no session specified and failed to find recent session: %w", err)
		}
		sessionID = meta.ID
	} else {
		// Validate/resolve prefix
		meta, _, err := st.Load(sessionID)
		if err != nil {
			return fmt.Errorf("resolve session %q: %w", sessionID, err)
		}
		sessionID = meta.ID
	}

	ctx := context.Background()
	var record session.WorkflowResultRecord
	resultTarget := strings.TrimSpace(*resultFlag)
	kind := strings.TrimSpace(*kindFlag)

	if resultTarget == "" || resultTarget == "latest" {
		if kind == "chat" || kind == "log" || kind == "transcript" {
			_, msgs, err := st.Load(sessionID)
			if err != nil {
				return err
			}
			if len(msgs) == 0 {
				return fmt.Errorf("no messages found in session %s", sessionID)
			}
			payload, _ := json.Marshal(msgs)
			record = session.WorkflowResultRecord{
				ResultID:  "chat-latest",
				SessionID: sessionID,
				Kind:      "chat",
				Version:   1,
				Payload:   string(payload),
			}
		} else if kind == "message" || kind == "last" || kind == "response" {
			_, msgs, err := st.Load(sessionID)
			if err != nil {
				return err
			}
			var lastMsgText string
			for i := len(msgs) - 1; i >= 0; i-- {
				if (msgs[i].Role == "assistant" || msgs[i].Role == "model") && strings.TrimSpace(msgs[i].TextContent()) != "" {
					lastMsgText = msgs[i].TextContent()
					break
				}
			}
			if lastMsgText == "" {
				return fmt.Errorf("no assistant message found in session %s", sessionID)
			}
			record = session.WorkflowResultRecord{
				ResultID:  "msg-latest",
				SessionID: sessionID,
				Kind:      "message",
				Version:   1,
				Payload:   lastMsgText,
			}
		} else {
			rec, ok, err := st.LatestWorkflowResult(ctx, sessionID, kind)
			if err != nil {
				return fmt.Errorf("lookup latest workflow result: %w", err)
			}
			if !ok {
				if kind == "" {
					// Fallback to last assistant message
					_, msgs, lerr := st.Load(sessionID)
					if lerr == nil {
						for i := len(msgs) - 1; i >= 0; i-- {
							if (msgs[i].Role == "assistant" || msgs[i].Role == "model") && strings.TrimSpace(msgs[i].TextContent()) != "" {
								rec = session.WorkflowResultRecord{
									ResultID:  "msg-latest",
									SessionID: sessionID,
									Kind:      "message",
									Version:   1,
									Payload:   msgs[i].TextContent(),
								}
								ok = true
								break
							}
						}
					}
				}
				if !ok {
					if kind != "" {
						return fmt.Errorf("no workflow result of kind %q found for session %s", kind, sessionID)
					}
					return fmt.Errorf("no workflow results or assistant messages found for session %s", sessionID)
				}
			}
			record = rec
		}
	} else {
		rec, err := st.LoadWorkflowResult(ctx, sessionID, resultTarget)
		if err != nil {
			return err
		}
		if kind != "" && rec.Kind != kind {
			return fmt.Errorf("workflow result %s has kind %q, want %q", resultTarget, rec.Kind, kind)
		}
		record = rec
	}

	rendered, err := export.RenderResult(record, *formatFlag)
	if err != nil {
		return fmt.Errorf("render result: %w", err)
	}

	outputDest := strings.TrimSpace(*outputFlag)
	if outputDest == "" {
		_, err := os.Stdout.Write(rendered)
		return err
	}

	finalPath, err := export.WriteExportFile(outputDest, rendered, *forceFlag, wd)
	if err != nil {
		if errors.Is(err, export.ErrDestinationExists) {
			return fmt.Errorf("%w (use --force to overwrite)", err)
		}
		return fmt.Errorf("write export: %w", err)
	}

	fmt.Fprintf(os.Stderr, "Exported %s result %s to %s\n", record.Kind, record.ResultID, finalPath)
	return nil
}

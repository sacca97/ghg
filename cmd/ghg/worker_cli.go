package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/sacca97/ghg/internal/config"
	"github.com/sacca97/ghg/internal/session"
	"github.com/sacca97/ghg/internal/tui"
	workerwire "github.com/sacca97/ghg/internal/worker"
)

func workerPSCLI() error {
	dir, err := config.Dir()
	if err != nil {
		return err
	}
	states, err := workerwire.ListStates(dir)
	if err != nil {
		return err
	}
	if len(states) == 0 {
		fmt.Println("no worker sessions")
		return nil
	}
	for _, state := range states {
		live := ""
		runtimeFile, runtimeErr := workerwire.NewRuntime(dir, state.SessionID)
		if runtimeErr == nil {
			if workerRuntimeLive(runtimeFile) {
				live = " live"
			} else if state.State == workerwire.StateRunning || state.State == workerwire.StateWaitingApproval || state.State == workerwire.StateStopping {
				state.State = workerwire.StateInterrupted
				state.Detached = false
				state.Detail = "worker exited before clean shutdown"
				_ = runtimeFile.WriteState(state)
			}
		}
		fmt.Printf("%s  %-19s  %s%s\n", state.SessionID, state.State, state.UpdatedAt.Local().Format("2006-01-02 15:04"), live)
	}
	return nil
}

func workerAttachCLI(args []string) error {
	if len(args) != 1 || strings.TrimSpace(args[0]) == "" {
		return errors.New("usage: ghg attach <session>")
	}
	id := strings.TrimSpace(args[0])
	dir, err := config.Dir()
	if err != nil {
		return err
	}
	store, err := session.Open(filepath.Join(dir, "sessions.db"))
	if err != nil {
		return err
	}
	meta, _, err := store.Load(id)
	_ = store.Close()
	if err != nil {
		return err
	}
	if meta.CWD != "" {
		if err := os.Chdir(meta.CWD); err != nil {
			return fmt.Errorf("session workspace: %w", err)
		}
	}
	cfg, err := config.Load()
	if err != nil {
		return err
	}
	tui.Version = version
	attachErr := error(nil)
	if _, err := tui.RunAttached(cfg, meta.Model, meta.Provider, systemPrompt(), meta.ID, false); err == nil {
		return nil
	} else {
		attachErr = err
	}
	// A detached worker may have completed its idle grace period. Reopen the
	// durable session normally, which starts a fresh worker from its persisted
	// state; a still-present socket keeps the original attach error visible.
	runtimeFile, runtimeErr := workerwire.NewRuntime(dir, meta.ID)
	if runtimeErr == nil && !workerRuntimeLive(runtimeFile) {
		// The lock, not the socket pathname, establishes ownership. A crashed
		// worker can leave the pathname behind; once the lock is available it is
		// safe to remove that stale endpoint and reopen the durable session.
		_ = runtimeFile.RemoveSocket()
		_, err = tui.Run(cfg, meta.Model, meta.Provider, systemPrompt(), meta.ID, false)
		return err
	}
	return fmt.Errorf("attach session %s failed: %w", meta.ID, attachErr)
}

func workerRuntimeLive(runtimeFile workerwire.Runtime) bool {
	return runtimeFile.Live()
}

func workerStopCLI(args []string) error {
	if len(args) != 1 || strings.TrimSpace(args[0]) == "" {
		return errors.New("usage: ghg stop <session>")
	}
	dir, err := config.Dir()
	if err != nil {
		return err
	}
	runtimeFile, err := workerwire.NewRuntime(dir, strings.TrimSpace(args[0]))
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	client, err := workerwire.Dial(ctx, runtimeFile, 0)
	if err != nil {
		return err
	}
	defer client.Close()
	if _, err := nextWorkerCLIFrame(ctx, client); err != nil {
		return err
	}
	if err := client.Send(workerwire.CommandStop, "stop-"+fmt.Sprint(time.Now().UnixNano()), nil); err != nil {
		return err
	}
	for {
		frame, err := nextWorkerCLIFrame(ctx, client)
		if err != nil {
			return err
		}
		if frame.Type == workerwire.TypeAck {
			fmt.Println("stop requested")
			return nil
		}
		if frame.Type == workerwire.TypeError {
			return errors.New("worker rejected stop request")
		}
	}
}

func nextWorkerCLIFrame(ctx context.Context, client *workerwire.Client) (workerwire.Frame, error) {
	frames, errs := client.Frames(), client.Errors()
	for frames != nil || errs != nil {
		select {
		case frame, ok := <-frames:
			if !ok {
				frames = nil
				continue
			}
			if frame.Type == workerwire.TypeAlreadyControlled {
				return workerwire.Frame{}, errors.New("worker already has a controlling client")
			}
			if frame.Type == workerwire.TypeError {
				var payload workerwire.ErrorPayload
				if json.Unmarshal(frame.Payload, &payload) == nil && payload.Message != "" {
					return workerwire.Frame{}, errors.New(payload.Message)
				}
				return workerwire.Frame{}, errors.New("worker rejected request")
			}
			return frame, nil
		case err, ok := <-errs:
			if !ok {
				errs = nil
				continue
			}
			if err != nil {
				return workerwire.Frame{}, err
			}
		case <-ctx.Done():
			return workerwire.Frame{}, ctx.Err()
		}
	}
	return workerwire.Frame{}, errors.New("worker connection closed")
}

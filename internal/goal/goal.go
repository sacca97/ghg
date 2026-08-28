// Package goal contains the provider- and UI-neutral lifecycle state for a
// long-running coding goal. Persistence and agent execution live in their own
// packages; this package only defines the contract shared by both.
package goal

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"time"
)

// Status is the durable lifecycle state of a goal.
type Status string

const (
	StatusActive        Status = "active"
	StatusPaused        Status = "paused"
	StatusBlocked       Status = "blocked"
	StatusUsageLimited  Status = "usage-limited"
	StatusBudgetLimited Status = "budget-limited"
	StatusComplete      Status = "complete"
)

const (
	// MaxObjectiveBytes and MaxNoteBytes bound user/model-authored data that is
	// persisted in the goal ledger and injected into later requests.
	MaxObjectiveBytes = 4000
	MaxNoteBytes      = 4000
)

// Record is one goal's current durable state. Usage fields count requests made
// while this goal was active; session-wide usage remains owned by agent.Agent.
type Record struct {
	ID               string    `json:"id"`
	Objective        string    `json:"objective"`
	Status           Status    `json:"status"`
	Rounds           int       `json:"rounds"`
	PromptTokens     int       `json:"prompt_tokens"`
	CachedTokens     int       `json:"cached_tokens"`
	CompletionTokens int       `json:"completion_tokens"`
	Progress         string    `json:"progress,omitempty"`
	Blocker          string    `json:"blocker,omitempty"`
	CreatedAt        time.Time `json:"created_at"`
	UpdatedAt        time.Time `json:"updated_at"`
}

// Update is the structured result of the model-facing update_goal tool.
// Active is a progress checkpoint; complete and blocked are terminal for the
// current goal run. Paused and limit states are controlled by the host.
type Update struct {
	GoalID   string
	Status   Status
	Progress string
	Blocker  string
}

// New creates an active goal record with a process-independent identifier.
func New(objective string) Record {
	now := time.Now().UTC()
	return Record{
		ID:        NewID(),
		Objective: strings.TrimSpace(objective),
		Status:    StatusActive,
		CreatedAt: now,
		UpdatedAt: now,
	}
}

// NewID returns a compact random goal identifier. The fallback is only for the
// extremely unlikely case that the OS random source is unavailable; it still
// keeps IDs distinct within normal process lifetimes.
func NewID() string {
	b := make([]byte, 8)
	if _, err := rand.Read(b); err == nil {
		return hex.EncodeToString(b)
	}
	return fmt.Sprintf("%x", time.Now().UnixNano())
}

// Validate checks the durable record before it is written or injected.
func (r Record) Validate() error {
	if strings.TrimSpace(r.ID) == "" {
		return errors.New("goal id is required")
	}
	if strings.TrimSpace(r.Objective) == "" {
		return errors.New("goal objective is required")
	}
	if len(r.Objective) > MaxObjectiveBytes {
		return fmt.Errorf("goal objective exceeds %d bytes", MaxObjectiveBytes)
	}
	if !ValidStatus(r.Status) {
		return fmt.Errorf("invalid goal status %q", r.Status)
	}
	if r.Rounds < 0 || r.PromptTokens < 0 || r.CachedTokens < 0 || r.CompletionTokens < 0 {
		return errors.New("goal accounting values cannot be negative")
	}
	if len(r.Progress) > MaxNoteBytes || len(r.Blocker) > MaxNoteBytes {
		return fmt.Errorf("goal checkpoint exceeds %d bytes", MaxNoteBytes)
	}
	return nil
}

// Validate checks a model-authored state change. The model may checkpoint
// progress, declare a genuine blocker, or declare completion. Host-controlled
// pause and limit states cannot be forged through the tool.
func (u Update) Validate(currentID string) error {
	if strings.TrimSpace(u.GoalID) != "" && strings.TrimSpace(u.GoalID) != currentID {
		return fmt.Errorf("goal id %q does not match the active goal", u.GoalID)
	}
	if len(u.Progress) > MaxNoteBytes || len(u.Blocker) > MaxNoteBytes {
		return fmt.Errorf("goal checkpoint exceeds %d bytes", MaxNoteBytes)
	}
	switch u.Status {
	case StatusActive:
		if strings.TrimSpace(u.Blocker) != "" {
			return errors.New("an active goal cannot have a blocker; use status blocked")
		}
		if strings.TrimSpace(u.Progress) == "" {
			return errors.New("active goal updates require a progress note")
		}
	case StatusBlocked:
		if strings.TrimSpace(u.Blocker) == "" {
			return errors.New("blocked goal updates require a blocker")
		}
	case StatusComplete:
		if strings.TrimSpace(u.Progress) == "" {
			return errors.New("completed goal updates require a verification note")
		}
	default:
		return fmt.Errorf("model may set only active, blocked, or complete (got %q)", u.Status)
	}
	return nil
}

// ValidStatus reports whether s is one of the durable lifecycle states.
func ValidStatus(s Status) bool {
	switch s {
	case StatusActive, StatusPaused, StatusBlocked, StatusUsageLimited, StatusBudgetLimited, StatusComplete:
		return true
	default:
		return false
	}
}

// Terminal reports whether the state represents a finished goal rather than a
// goal that can be resumed.
func (s Status) Terminal() bool { return s == StatusComplete }

// Resumable reports whether an explicit resume may put the goal back to work.
func (s Status) Resumable() bool {
	switch s {
	case StatusPaused, StatusBlocked, StatusUsageLimited, StatusBudgetLimited:
		return true
	default:
		return false
	}
}

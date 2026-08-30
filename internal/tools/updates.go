package tools

import (
	"context"
	"slices"

	"github.com/sacca97/ghg/internal/observation"
	"github.com/sacca97/ghg/internal/search"
)

type onUpdateKey struct{}
type observationStoreKey struct{}
type searchStoreKey struct{}
type searchHintsKey struct{}

// SearchHints are presentation hints supplied by the agent. They do not
// change which matches a search returns; they only improve first-page order.
type SearchHints struct {
	Touched  []string
	Modified []string
}

type observationStore interface {
	Save(context.Context, string, observation.Record) error
	Load(context.Context, string, string) (observation.Record, error)
}

type searchStore interface {
	Save(context.Context, string, search.Snapshot) error
	Load(context.Context, string, string) (search.Snapshot, error)
}

// WithOnUpdate attaches a per-tool-call callback for accumulated output
// snapshots. The value lives in the call context rather than package state, so
// parallel tool calls cannot deliver output to one another's listeners.
func WithOnUpdate(ctx context.Context, fn func(snapshot string)) context.Context {
	if fn == nil {
		return ctx
	}
	return context.WithValue(ctx, onUpdateKey{}, fn)
}

func onUpdate(ctx context.Context) func(snapshot string) {
	fn, _ := ctx.Value(onUpdateKey{}).(func(string))
	return fn
}

// WithObservationStore supplies the session-scoped observation registry used
// by read and stateful edit. sessionID is part of the context value so an
// edit cannot accidentally authorize bytes from another session.
func WithObservationStore(ctx context.Context, sessionID string, store observationStore) context.Context {
	if store == nil {
		return ctx
	}
	return context.WithValue(ctx, observationStoreKey{}, observationContext{sessionID: sessionID, store: store})
}

type observationContext struct {
	sessionID string
	store     observationStore
}

func observationContextFor(ctx context.Context) (string, observationStore) {
	value, _ := ctx.Value(observationStoreKey{}).(observationContext)
	return value.sessionID, value.store
}

// WithSearchStore supplies the stable snapshot registry used by grep/glob
// pagination. The registry can mirror snapshots into the session database.
func WithSearchStore(ctx context.Context, sessionID string, store searchStore) context.Context {
	if store == nil {
		return ctx
	}
	return context.WithValue(ctx, searchStoreKey{}, searchContext{sessionID: sessionID, store: store})
}

type searchContext struct {
	sessionID string
	store     searchStore
}

func searchContextFor(ctx context.Context) (string, searchStore) {
	value, _ := ctx.Value(searchStoreKey{}).(searchContext)
	return value.sessionID, value.store
}

// WithSearchHints attaches non-authoritative ranking hints for one search.
func WithSearchHints(ctx context.Context, hints SearchHints) context.Context {
	hints.Touched = slices.Clone(hints.Touched)
	hints.Modified = slices.Clone(hints.Modified)
	return context.WithValue(ctx, searchHintsKey{}, hints)
}

func searchHintsFor(ctx context.Context) SearchHints {
	hints, _ := ctx.Value(searchHintsKey{}).(SearchHints)
	return hints
}

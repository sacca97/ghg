package tools

import "context"

type onUpdateKey struct{}

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

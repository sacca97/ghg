package tools

import (
	"context"
	"testing"
)

func TestWithOnUpdateIsPerContext(t *testing.T) {
	var first, second string
	ctx1 := WithOnUpdate(context.Background(), func(snapshot string) { first = snapshot })
	ctx2 := WithOnUpdate(context.Background(), func(snapshot string) { second = snapshot })

	onUpdate(ctx1)("one")
	onUpdate(ctx2)("two")
	if first != "one" || second != "two" {
		t.Fatalf("callbacks crossed contexts: first=%q second=%q", first, second)
	}
	if onUpdate(context.Background()) != nil {
		t.Fatal("a context without an update callback should remain quiet")
	}
}

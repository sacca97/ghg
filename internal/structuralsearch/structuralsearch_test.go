package structuralsearch

import (
	"context"
	"testing"
)

func TestSearchGoPatternsReturnsExactRanges(t *testing.T) {
	source := []byte("package p\n\ntype T struct{}\n\nfunc (é *T) target(x int) {\n\t_ = x\n}\n")
	matches, err := Search(context.Background(), Query{
		Language: "go",
		Patterns: []string{"func ($_ $TYPE) $NAME($$$ARGS) { $$$BODY }"},
	}, source)
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if len(matches) != 1 {
		t.Fatalf("got %d matches, want 1: %+v", len(matches), matches)
	}
	got := string(source[matches[0].StartByte:matches[0].EndByte])
	want := "func (é *T) target(x int) {\n\t_ = x\n}"
	if got != want {
		t.Fatalf("match text = %q, want %q", got, want)
	}
	if matches[0].StartByte != 28 || matches[0].EndByte != 65 {
		t.Fatalf("match range = %+v, want bytes 28:65", matches[0])
	}
}

func TestSearchRejectsInvalidQueryAndCancellation(t *testing.T) {
	if _, err := Search(context.Background(), Query{Language: "javascript", Patterns: []string{"func $NAME()"}}, nil); err == nil {
		t.Fatal("unsupported language accepted")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := Search(ctx, Query{Language: "go", Patterns: []string{"func $NAME()"}}, nil); err == nil {
		t.Fatal("canceled search accepted")
	}
}

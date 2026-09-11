package search

import (
	"os"
	"path/filepath"
	"strconv"
	"testing"
)

var benchmarkFuzzyResult []string

// BenchmarkFuzzyFilesColdIndex measures the synchronous first-completion walk
// that the TUI performs after an index invalidation. Setup stays outside the
// timed section so this measures traversal and ranking, not fixture creation.
func BenchmarkFuzzyFilesColdIndex(b *testing.B) {
	root := b.TempDir()
	dir := filepath.Join(root, "pkg")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		b.Fatal(err)
	}
	for i := 0; i < 10_000; i++ {
		path := filepath.Join(dir, "file-"+strconv.Itoa(i)+".go")
		if err := os.WriteFile(path, []byte("package p\n"), 0o644); err != nil {
			b.Fatal(err)
		}
	}

	b.ReportAllocs()
	b.ResetTimer()
	for b.Loop() {
		InvalidateFileIndex(root)
		benchmarkFuzzyResult = FuzzyFiles(root, "file-9999", 20)
	}
}

// BenchmarkFuzzyPaths100K measures the ranking cost independently of the
// filesystem walk, which is the data needed before adding derived path
// metadata to fuzzyHit.
func BenchmarkFuzzyPaths100K(b *testing.B) {
	files := make([]string, 100_000)
	for i := range files {
		files[i] = "internal/pkg/file-" + strconv.Itoa(i) + ".go"
	}

	b.ReportAllocs()
	for b.Loop() {
		benchmarkFuzzyResult = FuzzyPaths(files, "file-99999", 20)
	}
}

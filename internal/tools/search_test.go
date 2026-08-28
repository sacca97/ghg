package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

func writeSearchFile(t *testing.T, root, name, content string) string {
	t.Helper()
	path := filepath.Join(root, name)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	resolved, err := filepath.EvalSymlinks(path)
	if err != nil {
		t.Fatal(err)
	}
	return filepath.ToSlash(resolved)
}

func TestGrepTool(t *testing.T) {
	dir := t.TempDir()
	writeSearchFile(t, dir, ".gitignore", "ignored/\n*.tmp\n")
	match := writeSearchFile(t, dir, "src/a.go", "package src\nneedle one\nother\n")
	writeSearchFile(t, dir, "src/b.txt", "needle in the wrong type\n")
	writeSearchFile(t, dir, "ignored/hidden.go", "needle in an ignored file\n")
	writeSearchFile(t, dir, "scratch.tmp", "needle in a temporary file\n")
	if err := os.WriteFile(filepath.Join(dir, "binary.go"), []byte("needle\x00not text\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(dir, "src", "a.go"), filepath.Join(dir, "src", "link.go")); err != nil {
		t.Logf("symlink fixture unavailable: %v", err)
	}

	out := run(t, "grep", fmt.Sprintf(`{"pattern":"needle","path":%q,"include":"**/*.go"}`, dir))
	if !strings.Contains(out, match+":2:needle one") {
		t.Fatalf("expected matching line, got %q", out)
	}
	for _, unwanted := range []string{"hidden.go", "scratch.tmp", "binary.go", "link.go", "b.txt"} {
		if strings.Contains(out, unwanted) {
			t.Fatalf("grep returned unwanted file %q: %q", unwanted, out)
		}
	}

	out = Execute(context.Background(), All(), "grep", json.RawMessage(fmt.Sprintf(`{"pattern":"NEEDLE","path":%q,"case_sensitive":false,"literal":true}`, dir)))
	if !strings.Contains(out, match+":2:needle one") {
		t.Fatalf("case-insensitive literal search missed match: %q", out)
	}

	out = run(t, "grep", fmt.Sprintf(`{"pattern":"package","path":%q,"include":"*.go"}`, filepath.Join(dir, "src")))
	if !strings.Contains(out, match+":1:package src") {
		t.Fatalf("grep pattern relative to selected directory missed match: %q", out)
	}
}

func TestGlobToolPatternsAndOrdering(t *testing.T) {
	dir := t.TempDir()
	writeSearchFile(t, dir, ".gitignore", "ignored/\n*.tmp\n")
	rootGo := writeSearchFile(t, dir, "root.go", "package root\n")
	nestedGo := writeSearchFile(t, dir, "nested/z.go", "package nested\n")
	writeSearchFile(t, dir, "nested/a.txt", "text\n")
	writeSearchFile(t, dir, "nested/no.tmp", "ignored\n")
	writeSearchFile(t, dir, "ignored/no.go", "ignored\n")
	hiddenGo := writeSearchFile(t, dir, ".hidden.go", "package hidden\n")

	out := run(t, "glob", fmt.Sprintf(`{"pattern":"**/*.go","path":%q}`, dir))
	lines := nonEmptySearchLines(out)
	want := []string{nestedGo, rootGo, hiddenGo}
	sort.Strings(want)
	if strings.Join(lines, "\n") != strings.Join(want, "\n") {
		t.Fatalf("glob ordering/results:\n got %q\nwant %q", lines, want)
	}

	out = run(t, "glob", fmt.Sprintf(`{"pattern":"*.go","path":%q}`, dir))
	lines = nonEmptySearchLines(out)
	want = []string{rootGo, hiddenGo}
	sort.Strings(want)
	if strings.Join(lines, "\n") != strings.Join(want, "\n") {
		t.Fatalf("non-recursive glob:\n got %q\nwant %q", lines, want)
	}

	out = run(t, "glob", fmt.Sprintf(`{"pattern":"**/z.?o","path":%q}`, dir))
	if strings.TrimSpace(out) != nestedGo {
		t.Fatalf("glob ? pattern: got %q, want %q", out, nestedGo)
	}
	out = run(t, "glob", fmt.Sprintf(`{"pattern":"**/[az].go","path":%q}`, dir))
	if strings.TrimSpace(out) != nestedGo {
		t.Fatalf("glob character class: got %q, want %q", out, nestedGo)
	}
	out = run(t, "glob", fmt.Sprintf(`{"pattern":"*.go","path":%q}`, filepath.Join(dir, "nested")))
	if strings.TrimSpace(out) != nestedGo {
		t.Fatalf("glob pattern relative to selected directory: got %q, want %q", out, nestedGo)
	}

	if err := os.Symlink(filepath.Join(dir, "nested"), filepath.Join(dir, "linkdir")); err == nil {
		out = run(t, "glob", fmt.Sprintf(`{"pattern":"**/*.go","path":%q}`, dir))
		if strings.Contains(out, "linkdir") {
			t.Fatalf("glob followed a symlink: %q", out)
		}
		out = run(t, "glob", fmt.Sprintf(`{"pattern":"**/*.go","path":%q}`, filepath.Join(dir, "linkdir")))
		if !strings.Contains(out, "symlink") {
			t.Fatalf("explicit symlink path should be rejected: %q", out)
		}
	}
}

func TestGitignoreRules(t *testing.T) {
	dir := t.TempDir()
	resolvedDir, err := filepath.EvalSymlinks(dir)
	if err != nil {
		t.Fatal(err)
	}
	writeSearchFile(t, dir, ".gitignore", "*.log\n!important.log\nignored/\n!ignored/\nblocked/\n!blocked/keep.go\n/only-root.txt\n./only-dot.txt\nlogs/*.txt\ncache-dir/\n")
	writeSearchFile(t, dir, "a.log", "ignored\n")
	important := writeSearchFile(t, dir, "important.log", "kept\n")
	ignoredKeep := writeSearchFile(t, dir, "ignored/keep.go", "kept\n")
	blockedKeep := writeSearchFile(t, dir, "blocked/keep.go", "blocked\n")
	writeSearchFile(t, dir, "only-root.txt", "ignored\n")
	writeSearchFile(t, dir, "only-dot.txt", "ignored\n")
	nestedOnly := writeSearchFile(t, dir, "nested/only-root.txt", "kept\n")
	nestedDot := writeSearchFile(t, dir, "nested/only-dot.txt", "kept\n")
	writeSearchFile(t, dir, "logs/root.txt", "ignored\n")
	nestedLog := writeSearchFile(t, dir, "nested/logs/nested.txt", "kept\n")
	cacheFile := writeSearchFile(t, dir, "cache", "kept\n")
	writeSearchFile(t, dir, "cache-dir/inside.go", "ignored\n")
	writeSearchFile(t, dir, "nested/ignore.tmp", "ignored\n")
	nestedKeep := writeSearchFile(t, dir, "nested/keep.tmp", "kept\n")
	writeSearchFile(t, dir, "nested/.gitignore", "*.tmp\n!keep.tmp\n")

	out := run(t, "glob", fmt.Sprintf(`{"pattern":"**/*","path":%q}`, dir))
	for _, want := range []string{important, ignoredKeep, nestedOnly, nestedDot, nestedLog, cacheFile, nestedKeep} {
		if !strings.Contains(out, want) {
			t.Fatalf("expected %s in glob output %q", want, out)
		}
	}
	for _, unwanted := range []string{
		filepath.ToSlash(filepath.Join(resolvedDir, "a.log")),
		blockedKeep,
		filepath.ToSlash(filepath.Join(resolvedDir, "only-root.txt")),
		filepath.ToSlash(filepath.Join(resolvedDir, "only-dot.txt")),
		filepath.ToSlash(filepath.Join(resolvedDir, "logs", "root.txt")),
		filepath.ToSlash(filepath.Join(resolvedDir, "cache-dir", "inside.go")),
		filepath.ToSlash(filepath.Join(resolvedDir, "nested", "ignore.tmp")),
	} {
		if strings.Contains(out, unwanted) {
			t.Fatalf("unexpected ignored path %s in glob output %q", unwanted, out)
		}
	}
}

func TestSearchLimitsCancellationAndInvalidArguments(t *testing.T) {
	dir := t.TempDir()
	for i := 0; i < 3; i++ {
		writeSearchFile(t, dir, fmt.Sprintf("%d.txt", i), "needle\n")
	}
	out := run(t, "glob", fmt.Sprintf(`{"pattern":"**/*.txt","path":%q,"max_results":1}`, dir))
	if !strings.Contains(out, "result limit reached") {
		t.Fatalf("expected result-limit marker, got %q", out)
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	out = Execute(ctx, All(), "glob", json.RawMessage(fmt.Sprintf(`{"pattern":"**/*","path":%q}`, dir)))
	if !strings.Contains(out, "context canceled") {
		t.Fatalf("expected cancellation error, got %q", out)
	}

	for name, call := range map[string]string{
		"invalid regex": fmt.Sprintf(`{"pattern":"[","path":%q}`, dir),
		"invalid glob":  fmt.Sprintf(`{"pattern":"[","path":%q}`, dir),
		"parent glob":   fmt.Sprintf(`{"pattern":"../*","path":%q}`, dir),
		"missing path":  fmt.Sprintf(`{"pattern":"**/*","path":%q}`, filepath.Join(dir, "missing")),
	} {
		t.Run(name, func(t *testing.T) {
			tool := "grep"
			if name != "invalid regex" {
				tool = "glob"
			}
			out := run(t, tool, call)
			if !strings.HasPrefix(out, "Error:") {
				t.Fatalf("expected argument error, got %q", out)
			}
		})
	}
}

func TestMalformedGitignore(t *testing.T) {
	dir := t.TempDir()
	writeSearchFile(t, dir, ".gitignore", "[\n")
	out := run(t, "glob", fmt.Sprintf(`{"pattern":"**/*","path":%q}`, dir))
	if !strings.Contains(out, ".gitignore:1") || !strings.Contains(out, "invalid ignore pattern") {
		t.Fatalf("expected malformed ignore error, got %q", out)
	}
}

func TestExplicitIgnoredFileIsSearchable(t *testing.T) {
	dir := t.TempDir()
	writeSearchFile(t, dir, ".gitignore", "ignored/\n")
	file := writeSearchFile(t, dir, "ignored/file.txt", "needle\n")
	out := run(t, "grep", fmt.Sprintf(`{"pattern":"needle","path":%q}`, file))
	if !strings.Contains(out, file+":1:needle") {
		t.Fatalf("explicit ignored file should be searchable: %q", out)
	}
}

func nonEmptySearchLines(s string) []string {
	var lines []string
	for _, line := range strings.Split(strings.TrimSpace(s), "\n") {
		if line != "" {
			lines = append(lines, line)
		}
	}
	return lines
}

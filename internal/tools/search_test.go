package tools

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sacca97/ghg/internal/sandbox"
	"github.com/sacca97/ghg/internal/search"
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
	if !strings.Contains(out, match+":\n  2:needle one") {
		t.Fatalf("expected matching line, got %q", out)
	}
	for _, unwanted := range []string{"hidden.go", "scratch.tmp", "binary.go", "link.go", "b.txt"} {
		if strings.Contains(out, unwanted) {
			t.Fatalf("grep returned unwanted file %q: %q", unwanted, out)
		}
	}

	out = Execute(context.Background(), All(), "grep", json.RawMessage(fmt.Sprintf(`{"pattern":"NEEDLE","path":%q,"case_sensitive":false,"literal":true}`, dir)))
	if !strings.Contains(out, match+":\n  2:needle one") {
		t.Fatalf("case-insensitive literal search missed match: %q", out)
	}

	out = run(t, "grep", fmt.Sprintf(`{"pattern":"package","path":%q,"include":"*.go"}`, filepath.Join(dir, "src")))
	if !strings.Contains(out, match+":\n  1:package src") {
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
	if !strings.Contains(out, nestedGo) {
		t.Fatalf("glob ? pattern: got %q, want %q", out, nestedGo)
	}
	out = run(t, "glob", fmt.Sprintf(`{"pattern":"**/[az].go","path":%q}`, dir))
	if !strings.Contains(out, nestedGo) {
		t.Fatalf("glob character class: got %q, want %q", out, nestedGo)
	}
	out = run(t, "glob", fmt.Sprintf(`{"pattern":"*.go","path":%q}`, filepath.Join(dir, "nested")))
	if !strings.Contains(out, nestedGo) {
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

func TestSearchUsesBoundedDefaultLimit(t *testing.T) {
	dir := t.TempDir()
	for i := 0; i < 30; i++ {
		writeSearchFile(t, dir, fmt.Sprintf("%02d.txt", i), "needle\n")
	}
	result := ExecuteResult(context.Background(), All(), "grep", json.RawMessage(fmt.Sprintf(`{"pattern":"needle","path":%q}`, dir)))
	if result.ExitCode != 0 {
		t.Fatalf("default grep = %+v", result)
	}
	if result.Metadata["search_displayed"] != "25" || result.Metadata["search_remaining"] != "5" {
		t.Fatalf("default grep metadata = %+v", result.Metadata)
	}
}

func TestSearchRejectsOutsidePathsBeforeInspection(t *testing.T) {
	workspace := t.TempDir()
	outside := t.TempDir()
	existing := writeSearchFile(t, outside, "secret.txt", "needle\n")
	missing := filepath.Join(outside, "missing.txt")
	policy, err := sandbox.NewPolicy(sandbox.PolicyConfig{Workspace: workspace, Mode: sandbox.ModeWorkspaceWrite})
	if err != nil {
		t.Fatal(err)
	}
	runtime, err := NewToolRuntime(policy, ApprovalNever, true)
	if err != nil {
		t.Fatal(err)
	}
	ctx := WithRuntime(context.Background(), runtime)
	for _, tc := range []struct {
		name string
		run  func(context.Context, json.RawMessage) (ToolResult, error)
		args func(string) json.RawMessage
	}{
		{name: "grep", run: runGrepResult, args: func(path string) json.RawMessage {
			return json.RawMessage(fmt.Sprintf(`{"pattern":"needle","path":%q}`, path))
		}},
		{name: "find_files", run: runFindFilesResult, args: func(path string) json.RawMessage {
			return json.RawMessage(fmt.Sprintf(`{"query":"secret","path":%q}`, path))
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			for _, path := range []string{existing, missing} {
				_, err := tc.run(ctx, tc.args(path))
				if !errors.Is(err, sandbox.ErrOutsideRoot) {
					t.Errorf("%s path error = %v, want ErrOutsideRoot", path, err)
				}
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
	if !strings.Contains(out, file+":\n  1:needle") {
		t.Fatalf("explicit ignored file should be searchable: %q", out)
	}
}

func TestGrepPatternsGroupingAndStableCursor(t *testing.T) {
	dir := t.TempDir()
	first := writeSearchFile(t, dir, "src/app.ts", "TODO one\nFIXME two\n")
	second := writeSearchFile(t, dir, "utils.ts", "TODO three\nFIXME four\n")
	registry := search.NewRegistry()
	ctx := WithSearchStore(context.Background(), "session-1", registry)
	ctx = WithSearchHints(ctx, SearchHints{Touched: []string{first}})
	args := fmt.Sprintf(`{"patterns":["TODO","FIXME"],"path":%q,"max_results":2}`, dir)
	page := ExecuteResult(ctx, All(), "grep", json.RawMessage(args))
	if page.Metadata["search_cursor"] == "" || page.Metadata["search_remaining"] != "2" {
		t.Fatalf("first page metadata = %+v", page.Metadata)
	}
	if !strings.Contains(page.Preview, first+":") || !strings.Contains(page.Preview, "TODO one") {
		t.Fatalf("first grouped page = %q", page.Preview)
	}
	page2Args := fmt.Sprintf(`{"cursor":%q,"max_results":2}`, page.Metadata["search_cursor"])
	page2 := ExecuteResult(ctx, All(), "grep", json.RawMessage(page2Args))
	if page2.Metadata["search_cursor"] != "" || !strings.Contains(page2.Preview, second+":") || !strings.Contains(page2.Preview, "FIXME four") {
		t.Fatalf("second stable page = %+v", page2)
	}
	if strings.Contains(page2.Preview, "TODO one") {
		t.Fatalf("second page repeated first page: %q", page2.Preview)
	}
}

func TestGrepRankingKeepsNoisyFilesFromFloodingFirstPage(t *testing.T) {
	dir := t.TempDir()
	noisy := strings.Repeat("TODO noisy\n", 30)
	writeSearchFile(t, dir, "zz/noisy.txt", noisy)
	app := writeSearchFile(t, dir, "src/app.ts", "TODO relevant\n")
	utils := writeSearchFile(t, dir, "utils.ts", "TODO helper\n")
	ctx := WithSearchHints(context.Background(), SearchHints{Touched: []string{app}})
	result := ExecuteResult(ctx, All(), "grep", json.RawMessage(fmt.Sprintf(`{"pattern":"TODO","path":%q,"max_results":6}`, dir)))
	if !strings.Contains(result.Preview, app) || !strings.Contains(result.Preview, utils) {
		t.Fatalf("relevant files missing from first page: %q", result.Preview)
	}
	if strings.Count(result.Preview, "TODO noisy") > searchPerFileCap {
		t.Fatalf("noisy file exceeded display cap: %q", result.Preview)
	}
	var displayed int
	if _, err := fmt.Sscan(result.Metadata["search_displayed"], &displayed); err != nil || displayed > 6 {
		t.Fatalf("search page exceeded max_results: displayed=%q result=%q", result.Metadata["search_displayed"], result.Preview)
	}
}

func TestGrepPageDoesNotOverflowOnWholeFileGroups(t *testing.T) {
	dir := t.TempDir()
	first := writeSearchFile(t, dir, "a.txt", strings.Repeat("TODO\n", 4))
	writeSearchFile(t, dir, "b.txt", strings.Repeat("TODO\n", 4))
	ctx := WithSearchStore(context.Background(), "session-1", search.NewRegistry())
	ctx = WithSearchHints(ctx, SearchHints{Touched: []string{first}})
	result := ExecuteResult(ctx, All(), "grep", json.RawMessage(fmt.Sprintf(`{"pattern":"TODO","path":%q,"max_results":6}`, dir)))
	var displayed int
	if _, err := fmt.Sscan(result.Metadata["search_displayed"], &displayed); err != nil {
		t.Fatalf("search page metadata = %+v", result.Metadata)
	}
	if displayed > 6 {
		t.Fatalf("grouped page overflowed max_results: %d\n%s", displayed, result.Preview)
	}
	if !strings.Contains(result.Preview, first) {
		t.Fatalf("ranked first file missing: %q", result.Preview)
	}
}

func TestSearchPreviewPaginationDoesNotSkipLongResults(t *testing.T) {
	dir := t.TempDir()
	files := make([]string, 5)
	for i := range files {
		files[i] = writeSearchFile(t, dir, fmt.Sprintf("%c.txt", 'a'+i), fmt.Sprintf("MATCH %d ", i)+strings.Repeat("x", maxMatchLineBytes)+"\n")
	}
	ctx := WithSearchStore(context.Background(), "session-1", search.NewRegistry())
	args := fmt.Sprintf(`{"pattern":"MATCH","path":%q,"max_results":5}`, dir)
	page := ExecuteResult(ctx, All(), "grep", json.RawMessage(args))
	if len(page.Preview) > searchPreviewBytes {
		t.Fatalf("first search preview exceeded byte ceiling: %d", len(page.Preview))
	}
	var displayed, remaining int
	fmt.Sscan(page.Metadata["search_displayed"], &displayed)
	fmt.Sscan(page.Metadata["search_remaining"], &remaining)
	if displayed <= 0 || displayed+remaining != 5 || remaining == 0 {
		t.Fatalf("first page accounting = %+v", page.Metadata)
	}
	if !strings.Contains(page.Preview, files[0]) {
		t.Fatalf("first page rendered the wrong results: %q", page.Preview)
	}
	cursor := page.Metadata["search_cursor"]
	if cursor == "" {
		t.Fatal("first page should retain the unseen long result behind a cursor")
	}
	page2 := ExecuteResult(ctx, All(), "grep", json.RawMessage(fmt.Sprintf(`{"cursor":%q,"max_results":5}`, cursor)))
	if len(page2.Preview) > searchPreviewBytes {
		t.Fatalf("second search preview exceeded byte ceiling: %d", len(page2.Preview))
	}
	var displayed2, remaining2 int
	fmt.Sscan(page2.Metadata["search_displayed"], &displayed2)
	fmt.Sscan(page2.Metadata["search_remaining"], &remaining2)
	if displayed+displayed2 != 5 || remaining2 != 0 {
		t.Fatalf("second page accounting = %+v, displayed1=%d", page2.Metadata, displayed)
	}
	if !strings.Contains(page2.Preview, files[displayed]) {
		t.Fatalf("second page skipped or repeated a result: %q", page2.Preview)
	}
}

func TestUngroupedSearchRemainingUsesCursorOffset(t *testing.T) {
	dir := t.TempDir()
	for _, name := range []string{"one.txt", "two.txt", "three.txt"} {
		writeSearchFile(t, dir, name, "")
	}
	ctx := WithSearchStore(context.Background(), "session-1", search.NewRegistry())
	page := ExecuteResult(ctx, All(), "glob", json.RawMessage(fmt.Sprintf(`{"pattern":"*.txt","path":%q,"max_results":1}`, dir)))
	if page.Metadata["search_displayed"] != "1" || page.Metadata["search_remaining"] != "2" {
		t.Fatalf("first ungrouped page accounting = %+v", page.Metadata)
	}
	for wantRemaining := 1; wantRemaining >= 0; wantRemaining-- {
		cursor := page.Metadata["search_cursor"]
		if cursor == "" {
			t.Fatalf("page with %d remaining results lost its cursor", wantRemaining+1)
		}
		page = ExecuteResult(ctx, All(), "glob", json.RawMessage(fmt.Sprintf(`{"cursor":%q,"max_results":1}`, cursor)))
		if page.Metadata["search_displayed"] != "1" || page.Metadata["search_remaining"] != fmt.Sprint(wantRemaining) {
			t.Fatalf("page remaining = %d, metadata = %+v", wantRemaining, page.Metadata)
		}
	}
	if page.Metadata["search_cursor"] != "" {
		t.Fatalf("last ungrouped page should not expose a cursor: %+v", page.Metadata)
	}
}

func TestFindFilesUsesSharedFuzzyIndex(t *testing.T) {
	dir := t.TempDir()
	writeSearchFile(t, dir, "archive/road-notes.txt", "")
	want := writeSearchFile(t, dir, "src/roadmap.md", "")
	writeSearchFile(t, dir, "README.md", "")
	out := run(t, "find_files", fmt.Sprintf(`{"query":"roadmap","path":%q,"max_results":5}`, dir))
	if !strings.Contains(out, want) {
		t.Fatalf("fuzzy path search missed best candidate %q: %q", want, out)
	}
}

func nonEmptySearchLines(s string) []string {
	var lines []string
	for _, line := range strings.Split(strings.TrimSpace(s), "\n") {
		if line != "" && !strings.HasPrefix(line, "glob: ") && !strings.HasPrefix(line, "[cursor=") && !strings.HasPrefix(line, "[incomplete") {
			lines = append(lines, line)
		}
	}
	return lines
}

func TestGitStatusCacheCoalescingAndExpiry(t *testing.T) {
	var (
		mu          sync.Mutex
		loadCount   int
		currentTime = time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	)
	nowFn := func() time.Time {
		mu.Lock()
		defer mu.Unlock()
		return currentTime
	}
	advance := func(d time.Duration) {
		mu.Lock()
		defer mu.Unlock()
		currentTime = currentTime.Add(d)
	}

	started := make(chan struct{})
	unblock := make(chan struct{})

	loader := func(ctx context.Context, root string) map[string]struct{} {
		mu.Lock()
		loadCount++
		currentCount := loadCount
		mu.Unlock()

		if currentCount == 1 {
			close(started)
			<-unblock
		}

		return map[string]struct{}{
			fmt.Sprintf("%s/file%d.go", root, currentCount): {},
		}
	}

	cache := newGitStatusCache(2*time.Second, loader, nowFn)

	// 1. Issue concurrent requests for one root
	const concurrent = 5
	var wg sync.WaitGroup
	results := make([]map[string]struct{}, concurrent)
	for i := 0; i < concurrent; i++ {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			if idx > 0 {
				<-started
			}
			results[idx] = cache.get(context.Background(), "/repo")
		}(i)
	}

	<-started
	close(unblock)
	wg.Wait()

	mu.Lock()
	initialLoads := loadCount
	mu.Unlock()
	if initialLoads != 1 {
		t.Fatalf("expected 1 loader execution for concurrent requests, got %d", initialLoads)
	}
	for i, res := range results {
		if _, ok := res["/repo/file1.go"]; !ok {
			t.Fatalf("result[%d] missing expected path: %+v", i, res)
		}
	}

	// 2. Assert another request inside the TTL uses the cached value
	advance(1 * time.Second)
	cachedRes := cache.get(context.Background(), "/repo")
	if _, ok := cachedRes["/repo/file1.go"]; !ok {
		t.Fatalf("cached result missing expected path: %+v", cachedRes)
	}
	mu.Lock()
	afterCachedLoads := loadCount
	mu.Unlock()
	if afterCachedLoads != 1 {
		t.Fatalf("expected 1 total loader execution within TTL, got %d", afterCachedLoads)
	}

	// 3. Assert a request after expiry refreshes it
	advance(2 * time.Second)
	refreshedRes := cache.get(context.Background(), "/repo")
	if _, ok := refreshedRes["/repo/file2.go"]; !ok {
		t.Fatalf("refreshed result missing new path: %+v", refreshedRes)
	}
	mu.Lock()
	afterExpiryLoads := loadCount
	mu.Unlock()
	if afterExpiryLoads != 2 {
		t.Fatalf("expected 2 total loader executions after expiry, got %d", afterExpiryLoads)
	}

	// 4. Assert different repository roots do not share results
	diffRootRes := cache.get(context.Background(), "/other-repo")
	if _, ok := diffRootRes["/other-repo/file3.go"]; !ok {
		t.Fatalf("different root result missing its own path: %+v", diffRootRes)
	}
	mu.Lock()
	diffRootLoads := loadCount
	mu.Unlock()
	if diffRootLoads != 3 {
		t.Fatalf("expected 3 total loader executions for different root, got %d", diffRootLoads)
	}
}

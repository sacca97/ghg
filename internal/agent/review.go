package agent

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/sacca97/ghg/internal/models"
	"github.com/sacca97/ghg/internal/search"
	"github.com/sacca97/ghg/internal/tools"
)

// reviewModePrompt is injected as a transient system message on every model
// round while the agent is in Review mode.
const reviewModePrompt = `You are reviewing in a read-only collaboration mode. Inspect only the repository evidence needed to evaluate the user's request.

Report only actionable, evidence-backed findings. Order findings by severity and expected impact. Distinguish confirmed defects from opportunities that require measurement. Do not invent findings to fill categories. If no material problems are found, say so in the summary and submit an empty findings list.

Budget is an estimate; sufficient semantic coverage is the completion criterion, not line-by-line completeness. Broad reviews may extend when meaningful requested areas remain unreviewed. Focused reviews should extend mainly when unresolved evidence could change a finding. Do not extend merely because the remaining code is unread if it adds no meaningful semantic coverage; disclose any material requested area that remains unreviewed in the summary.

When finished, call submit_review exactly once. Do not implement fixes or return an implementation plan.

The first request includes a deterministic <review_preflight> inventory with the authoritative scope and file list. Use it as scope context and choose the currently exposed read-only tools that best answer the remaining questions. The set_next_reasoning_effort tool is available when the model advertises multiple efforts; use it when a different advertised effort would be useful for the next model call, otherwise keep the configured effort. Select only an advertised effort.

At a review budget checkpoint, either call request_review_extension with reason (incomplete_coverage or unresolved_finding), one meaningful remaining area, one concise sentence explaining why it matters, and the exact remaining lookup, or use one final evidence batch. Do not request an extension merely because some lines remain unread. After that batch, only submit_review and set_next_reasoning_effort are available.`

type reviewContinuation struct {
	target            string
	budget            *ReviewBudget
	checkpointPending bool
	closed            bool
}

const (
	defaultReviewBudget     = 4
	minReviewBudget         = 5
	maxReviewBudget         = 64
	maxReviewInventoryFiles = 4096
	maxReviewInventoryBytes = 32 << 20
	maxReviewFocusFiles     = 5
	maxReviewPreflightFiles = 128
)

// ReviewInventory is the bounded, deterministic scope summary used to size a
// review. It intentionally contains metadata only; source contents never go
// into the model request during preflight.
type ReviewInventory struct {
	Scope           []string                      `json:"scope"`
	Files           []string                      `json:"files,omitempty"`
	ProductionFiles int                           `json:"production_files"`
	TestFiles       int                           `json:"test_files"`
	ProductionLOC   int                           `json:"production_loc"`
	LanguageStats   map[string]ReviewLanguageStat `json:"language_stats,omitempty"`
	LargeFiles      []string                      `json:"large_files,omitempty"`
	LargestFiles    []string                      `json:"largest_files,omitempty"`
	Partial         bool                          `json:"partial,omitempty"`
	productionDirs  []string                      `json:"-"`
	explicitScope   bool                          `json:"-"`
}

type ReviewLanguageStat struct {
	Files int `json:"files"`
	LOC   int `json:"loc"`
}

// ReviewBudget is the ReviewMode exploration state. Allocation is a
// renewable boundary; HardLimit is absolute and includes every exploration
// lease, while the final evidence batch is separate.
type ReviewBudget struct {
	Baseline       int
	Allocation     int
	CurrentRound   int
	HardLimit      int
	TargetHash     string
	Focus          []string
	Rationale      string
	Inventory      ReviewInventory
	checkpointOpen bool
}

// ReviewProgress is emitted for the TUI and headless telemetry. The payload
// is deliberately small and safe to send over the worker wire.
type ReviewProgress struct {
	TargetHash     string           `json:"target_hash,omitempty"`
	Phase          string           `json:"phase"`
	CurrentRound   int              `json:"current_round"`
	Allocation     int              `json:"allocation"`
	HardLimit      int              `json:"hard_limit"`
	Baseline       int              `json:"baseline,omitempty"`
	Inventory      *ReviewInventory `json:"inventory,omitempty"`
	Focus          []string         `json:"focus,omitempty"`
	Rationale      string           `json:"rationale,omitempty"`
	Reason         string           `json:"reason,omitempty"`
	FromAllocation int              `json:"from_allocation,omitempty"`
	ToAllocation   int              `json:"to_allocation,omitempty"`
}

// RestoreReviewContinuation rebuilds an interrupted review from its persisted
// progress events. A worker restart must not turn an existing review into a
// fresh lease with a fresh tool budget.
func (a *Agent) RestoreReviewContinuation(target string, progress []ReviewProgress) bool {
	if a == nil || strings.TrimSpace(target) == "" || len(progress) == 0 {
		return false
	}
	targetHash := reviewTargetHash(target)
	var budget ReviewBudget
	phase := ""
	seen := false
	for _, value := range progress {
		// Progress without an identity predates resumable reviews. Do not
		// attach it to a later prompt: stale telemetry is safer to discard than
		// to resume under the wrong user request.
		if value.TargetHash != targetHash {
			continue
		}
		// This is an informational event emitted when an in-memory continuation
		// resumes; it must not erase the durable phase that preceded it.
		if value.Reason == "resumed" {
			continue
		}
		seen = true
		phase = value.Phase
		if value.Baseline > 0 {
			budget.Baseline = value.Baseline
		}
		if value.Allocation > 0 {
			budget.Allocation = value.Allocation
		}
		if value.CurrentRound >= 0 {
			budget.CurrentRound = value.CurrentRound
		}
		if value.HardLimit > 0 {
			budget.HardLimit = value.HardLimit
		}
		if value.Inventory != nil {
			budget.Inventory = *value.Inventory
		}
		if value.Focus != nil {
			budget.Focus = append([]string(nil), value.Focus...)
		}
		if value.Rationale != "" {
			budget.Rationale = value.Rationale
		}
	}
	if !seen || phase == "completed" {
		return false
	}
	if (phase == "final_evidence" || phase == "exploration_closed") && a.reviewWasAcceptedAfterLatestPrompt() {
		return false
	}
	if budget.Baseline <= 0 {
		budget.Baseline = defaultReviewBudget
	}
	budget.Baseline = clampReviewBudget(budget.Baseline)
	if budget.Allocation <= 0 {
		budget.Allocation = budget.Baseline
	}
	if budget.HardLimit <= 0 {
		budget.HardLimit = 2 * max(budget.Baseline, minReviewBudget)
	}
	budget.HardLimit = min(max(budget.HardLimit, budget.Baseline), 2*maxReviewBudget)
	budget.Allocation = min(max(budget.Allocation, 1), budget.HardLimit)
	budget.TargetHash = targetHash
	a.reviewContinuation = &reviewContinuation{
		target:            target,
		budget:            &budget,
		checkpointPending: phase == "budget_boundary",
		closed:            phase == "final_evidence" || phase == "exploration_closed",
	}
	return true
}

func reviewTargetHash(target string) string {
	sum := sha256.Sum256([]byte(strings.TrimSpace(target)))
	return hex.EncodeToString(sum[:])
}

func (a *Agent) reviewWasAcceptedAfterLatestPrompt() bool {
	messages := a.MessagesSnapshot()
	start := 0
	for i := len(messages) - 1; i >= 0; i-- {
		if messages[i].Role == "user" && messages[i].Authored {
			start = i + 1
			break
		}
	}
	for _, message := range messages[start:] {
		if message.Role == "tool" && message.Name == "submit_review" && strings.TrimSpace(message.Content) == "Review accepted." {
			return true
		}
	}
	return false
}

type reviewExtensionRequest struct {
	Reason          string `json:"reason"`
	RemainingArea   string `json:"remaining_area"`
	WhyItMatters    string `json:"why_it_matters"`
	RemainingLookup string `json:"remaining_lookup"`

	// Legacy fields keep persisted requests and older model responses readable.
	UnresolvedIssue string `json:"unresolved_issue"`
	Evidence        string `json:"evidence"`
	Rounds          int    `json:"rounds"`
}

func (r reviewExtensionRequest) displayReason() string {
	label := strings.ReplaceAll(strings.TrimSpace(r.Reason), "_", " ")
	return fmt.Sprintf("%s: %s — %s", label, r.RemainingArea, r.WhyItMatters)
}

// reviewInventoryAt resolves only existing paths below workspace. A review
// request is prose, so path-looking tokens are opportunistic; an unresolved
// request simply reviews the workspace root.
func reviewInventoryAt(workspace, target string) ReviewInventory {
	workspace = reviewCanonicalDir(workspace)
	if workspace == "" {
		return ReviewInventory{Partial: true}
	}
	scopes, explicitScope := reviewScopes(workspace, target)
	inventory := ReviewInventory{Scope: make([]string, 0, len(scopes)), LanguageStats: make(map[string]ReviewLanguageStat)}
	inventory.explicitScope = explicitScope
	files := make([]string, 0)
	seen := make(map[string]struct{})
	var inventoryPathBytes int
	for _, scope := range scopes {
		rel, _ := filepath.Rel(workspace, scope)
		if rel == "" {
			rel = "."
		}
		inventory.Scope = append(inventory.Scope, filepath.ToSlash(rel))
		info, err := os.Stat(scope)
		if err != nil {
			inventory.Partial = true
			continue
		}
		if !info.IsDir() {
			if relPath, err := filepath.Rel(workspace, scope); err == nil {
				relPath = filepath.ToSlash(relPath)
				if reviewExcludedGeneratedPath(relPath) || !reviewSourceLanguageKnown(relPath) {
					continue
				}
				if _, ok := seen[relPath]; ok {
					continue
				}
				if len(files) >= maxReviewInventoryFiles || inventoryPathBytes+len(relPath)+1 > maxReviewInventoryBytes {
					inventory.Partial = true
					continue
				}
				seen[relPath] = struct{}{}
				files = append(files, relPath)
				inventoryPathBytes += len(relPath) + 1
			}
			continue
		}
		candidates := search.FuzzyFiles(scope, "", maxReviewInventoryFiles+1)
		if len(candidates) > maxReviewInventoryFiles {
			inventory.Partial = true
		}
		for _, relPath := range candidates {
			if len(files) >= maxReviewInventoryFiles {
				inventory.Partial = true
				break
			}
			path := filepath.Join(scope, filepath.FromSlash(relPath))
			workspaceRel, err := filepath.Rel(workspace, path)
			if err != nil || workspaceRel == ".." || strings.HasPrefix(workspaceRel, ".."+string(filepath.Separator)) {
				continue
			}
			workspaceRel = filepath.ToSlash(workspaceRel)
			if reviewExcludedGeneratedPath(workspaceRel) || !reviewSourceLanguageKnown(workspaceRel) {
				continue
			}
			if _, ok := seen[workspaceRel]; ok {
				continue
			}
			if inventoryPathBytes+len(workspaceRel)+1 > maxReviewInventoryBytes {
				inventory.Partial = true
				break
			}
			seen[workspaceRel] = struct{}{}
			files = append(files, workspaceRel)
			inventoryPathBytes += len(workspaceRel) + 1
		}
	}
	sort.Strings(files)
	inventory.Files = files
	stats := make([]reviewFileStat, 0, len(files))
	readableFiles := make([]string, 0, len(files))
	var totalBytes int64
	for _, relPath := range files {
		language, ok := reviewSourceLanguage(relPath)
		if !ok {
			continue
		}
		if len(stats) >= maxReviewInventoryFiles || totalBytes >= maxReviewInventoryBytes {
			inventory.Partial = true
			break
		}
		path := filepath.Join(workspace, filepath.FromSlash(relPath))
		info, err := os.Stat(path)
		if err != nil || !info.Mode().IsRegular() {
			inventory.Partial = true
			continue
		}
		if totalBytes+info.Size() > maxReviewInventoryBytes {
			inventory.Partial = true
			break
		}
		lines, bytesRead, err := reviewFileLines(path)
		if err != nil {
			if errors.Is(err, errReviewBinary) {
				continue
			}
			inventory.Partial = true
			continue
		}
		totalBytes += bytesRead
		readableFiles = append(readableFiles, relPath)
		stat := reviewFileStat{Path: relPath, Lines: lines, Test: reviewIsTestFile(relPath)}
		stats = append(stats, stat)
		if stat.Test {
			inventory.TestFiles++
			continue
		}
		inventory.ProductionFiles++
		inventory.ProductionLOC += lines
		languageStat := inventory.LanguageStats[language]
		languageStat.Files++
		languageStat.LOC += lines
		inventory.LanguageStats[language] = languageStat
		if lines > 500 {
			inventory.LargeFiles = append(inventory.LargeFiles, relPath)
		}
		inventory.productionDirs = append(inventory.productionDirs, filepath.ToSlash(filepath.Dir(relPath)))
	}
	inventory.Files = readableFiles
	sort.SliceStable(stats, func(i, j int) bool {
		if stats[i].Lines != stats[j].Lines {
			return stats[i].Lines > stats[j].Lines
		}
		return stats[i].Path < stats[j].Path
	})
	for _, stat := range stats {
		if stat.Test {
			continue
		}
		inventory.LargestFiles = append(inventory.LargestFiles, stat.Path)
		if len(inventory.LargestFiles) >= maxReviewFocusFiles {
			break
		}
	}
	sort.Strings(inventory.LargeFiles)
	inventory.productionDirs = uniqueStrings(inventory.productionDirs)
	return inventory
}

func reviewExcludedGeneratedPath(path string) bool {
	parts := strings.Split(strings.ToLower(filepath.ToSlash(filepath.Clean(path))), "/")
	for _, part := range parts[:max(0, len(parts)-1)] {
		switch part {
		case ".git", ".hg", ".svn", "node_modules", "vendor", "third_party", "third-party", "deps", "dependencies":
			return true
		case "doc", "docs", "documentation", "build", "dist", "gen", "generated", "out", "target", "coverage", "__pycache__", ".venv", "venv":
			return true
		}
		if strings.HasPrefix(part, ".") {
			return true
		}
	}
	base := strings.ToLower(filepath.Base(path))
	return strings.Contains(base, "generated") || strings.Contains(base, ".gen.")
}

type reviewFileStat struct {
	Path  string
	Lines int
	Test  bool
}

func reviewSourceLanguageKnown(path string) bool {
	_, ok := reviewSourceLanguage(path)
	return ok
}

func reviewSourceLanguage(path string) (string, bool) {
	switch strings.ToLower(filepath.Ext(path)) {
	case ".go":
		return "Go", true
	case ".js", ".jsx", ".mjs", ".cjs":
		return "JavaScript", true
	case ".ts", ".tsx", ".mts", ".cts":
		return "TypeScript", true
	case ".html", ".htm":
		return "HTML", true
	case ".css", ".scss", ".sass", ".less":
		return "CSS", true
	case ".m", ".mm":
		return "Objective-C", true
	case ".c":
		return "C", true
	case ".cc", ".cpp", ".cxx", ".h", ".hh", ".hpp", ".hxx":
		return "C/C++", true
	case ".py":
		return "Python", true
	case ".rs":
		return "Rust", true
	case ".java":
		return "Java", true
	case ".kt", ".kts":
		return "Kotlin", true
	case ".swift":
		return "Swift", true
	case ".cs":
		return "C#", true
	case ".rb":
		return "Ruby", true
	case ".php":
		return "PHP", true
	case ".vue":
		return "Vue", true
	case ".svelte":
		return "Svelte", true
	case ".sh", ".bash", ".zsh", ".fish":
		return "Shell", true
	case ".sql":
		return "SQL", true
	default:
		return "", false
	}
}

func reviewIsTestFile(path string) bool {
	parts := strings.Split(strings.ToLower(filepath.ToSlash(path)), "/")
	for _, part := range parts[:max(0, len(parts)-1)] {
		switch part {
		case "test", "tests", "testdata", "spec", "specs", "__tests__":
			return true
		}
	}
	base := parts[len(parts)-1]
	return strings.HasPrefix(base, "test.") || strings.HasPrefix(base, "test_") ||
		strings.HasPrefix(base, "test-") || strings.HasPrefix(base, "spec.") ||
		strings.HasPrefix(base, "spec_") || strings.Contains(base, ".test.") ||
		strings.Contains(base, ".spec.") || strings.Contains(base, "_test.") ||
		strings.Contains(base, "_spec.")
}

var errReviewBinary = errors.New("binary review file")

func reviewCanonicalDir(path string) string {
	path = strings.TrimSpace(path)
	if path == "" {
		return ""
	}
	abs, err := filepath.Abs(path)
	if err != nil {
		return ""
	}
	info, err := os.Stat(abs)
	if err != nil || !info.IsDir() {
		return ""
	}
	resolved, err := filepath.EvalSymlinks(abs)
	if err != nil {
		return ""
	}
	return resolved
}

func reviewScopes(workspace, target string) ([]string, bool) {
	var candidates []string
	for _, token := range strings.Fields(target) {
		for _, raw := range reviewPathCandidates(token) {
			path := raw
			if !filepath.IsAbs(path) {
				path = filepath.Join(workspace, filepath.FromSlash(path))
			}
			info, err := os.Lstat(path)
			if err != nil || (!info.IsDir() && !info.Mode().IsRegular()) {
				continue
			}
			resolved, err := filepath.EvalSymlinks(path)
			if err != nil || !reviewWithin(workspace, resolved) {
				continue
			}
			candidates = append(candidates, resolved)
			break
		}
	}
	if len(candidates) == 0 {
		return []string{workspace}, false
	}
	sort.Slice(candidates, func(i, j int) bool {
		if len(candidates[i]) != len(candidates[j]) {
			return len(candidates[i]) < len(candidates[j])
		}
		return candidates[i] < candidates[j]
	})
	var scopes []string
	for _, candidate := range candidates {
		duplicate := false
		for _, scope := range scopes {
			if reviewWithin(scope, candidate) {
				duplicate = true
				break
			}
		}
		if !duplicate {
			scopes = append(scopes, candidate)
		}
	}
	return scopes, true
}

func reviewPathCandidates(token string) []string {
	token = strings.Trim(token, " \t\r\n\"'`.,;()[]{}<>")
	if token == "" {
		return nil
	}
	paths := []string{token}
	if colon := strings.LastIndexByte(token, ':'); colon > 0 {
		line := token[colon+1:]
		if line != "" {
			valid := true
			for _, r := range line {
				if r < '0' || r > '9' {
					valid = false
					break
				}
			}
			if valid {
				paths = append(paths, token[:colon])
			}
		}
	}
	return paths
}

func reviewWithin(root, path string) bool {
	rel, err := filepath.Rel(root, path)
	return err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}

func reviewFileLines(path string) (int, int64, error) {
	file, err := os.Open(path)
	if err != nil {
		return 0, 0, err
	}
	defer file.Close()
	var lines int
	var total int64
	var last byte
	buffer := make([]byte, 256*1024)
	newline := []byte{'\n'}
	for {
		n, readErr := file.Read(buffer)
		if n > 0 {
			chunk := buffer[:n]
			if bytes.IndexByte(chunk, 0) >= 0 {
				return 0, 0, errReviewBinary
			}
			lines += bytes.Count(chunk, newline)
			total += int64(n)
			last = chunk[n-1]
		}
		if readErr == io.EOF {
			break
		}
		if readErr != nil {
			return 0, 0, readErr
		}
	}
	if total > 0 && last != '\n' {
		lines++
	}
	return lines, total, nil
}

func uniqueStrings(values []string) []string {
	seen := make(map[string]struct{}, len(values))
	out := make([]string, 0, len(values))
	for _, value := range values {
		if _, ok := seen[value]; ok {
			continue
		}
		seen[value] = struct{}{}
		out = append(out, value)
	}
	sort.Strings(out)
	return out
}

func reviewBudgetBaseline(target string, inventory ReviewInventory) int {
	budget := defaultReviewBudget + reviewCeilDiv(inventory.ProductionFiles, 5) + reviewCeilDiv(inventory.ProductionLOC, 2000)
	if broadReviewRequest(target) {
		budget += 2
	}
	if !inventory.explicitScope {
		budget += 2
	}
	if len(inventory.LanguageStats) > 1 {
		budget += 2
	}
	if len(inventory.productionDirs) > 1 {
		budget += 2
	}
	largeBonus := (len(inventory.LargeFiles) + 2) / 3
	if largeBonus > 2 {
		largeBonus = 2
	}
	budget += largeBonus
	fileCount := len(inventory.Files)
	if fileCount == 0 {
		fileCount = inventory.ProductionFiles + inventory.TestFiles
	}
	budget = max(budget, fileCount)
	return clampReviewBudget(budget)
}

func reviewCeilDiv(value, divisor int) int {
	if value <= 0 {
		return 0
	}
	return (value + divisor - 1) / divisor
}

func broadReviewRequest(target string) bool {
	terms := map[string]struct{}{
		"bug": {}, "bugs": {}, "performance": {}, "cleanup": {}, "security": {},
		"quality": {}, "optimization": {}, "optimize": {}, "speed": {}, "efficiency": {},
		"frontend": {}, "backend": {}, "ui": {}, "ux": {}, "desktop": {}, "authentication": {},
		"entire": {}, "codebase": {}, "all": {},
	}
	count := 0
	seen := make(map[string]struct{})
	for _, field := range strings.Fields(strings.ToLower(target)) {
		field = strings.Trim(field, " \t\r\n\"'`.,;:()[]{}<>")
		if _, ok := terms[field]; ok {
			if _, already := seen[field]; already {
				continue
			}
			seen[field] = struct{}{}
			count++
		}
	}
	return count >= 2 || strings.Contains(strings.ToLower(target), "review everything")
}

func clampReviewBudget(value int) int {
	if value < minReviewBudget {
		return minReviewBudget
	}
	return min(value, maxReviewBudget)
}

func reviewExtensionAllocation(current, hardLimit, baseline int) int {
	if current >= hardLimit {
		return 0
	}
	grant := reviewCeilDiv(max(baseline, 1), 4)
	return min(max(grant, 1), hardLimit-current)
}

func reviewWorkspace(a *Agent) string {
	if a != nil && a.Runtime != nil && a.Runtime.Policy != nil {
		if workspace := reviewCanonicalDir(a.Runtime.Policy.Workspace()); workspace != "" {
			return workspace
		}
	}
	workspace, err := os.Getwd()
	if err != nil {
		return ""
	}
	return reviewCanonicalDir(workspace)
}

func (a *Agent) newReviewBudget(_ context.Context, target string, ev Events) *ReviewBudget {
	workspace := reviewWorkspace(a)
	inventory := reviewInventoryAt(workspace, target)
	baseline := reviewBudgetBaseline(target, inventory)
	focus := append([]string(nil), inventory.LargestFiles...)
	budget := &ReviewBudget{
		Baseline: baseline, Allocation: baseline, HardLimit: 2 * baseline,
		TargetHash: reviewTargetHash(target),
		Focus:      focus, Inventory: inventory,
		Rationale: fmt.Sprintf("deterministic scope baseline: %d production files, %d production LOC, %d large files", inventory.ProductionFiles, inventory.ProductionLOC, len(inventory.LargeFiles)),
	}
	a.emitReviewProgress(ev, budget, "inventory", "")
	a.emitReviewProgress(ev, budget, "assessment", "")
	return budget
}

func (a *Agent) emitReviewProgress(ev Events, budget *ReviewBudget, phase, reason string) {
	if ev.OnReviewProgress == nil || budget == nil {
		return
	}
	var inventory *ReviewInventory
	if phase == "inventory" || phase == "assessment" {
		copyInventory := budget.Inventory
		copyInventory.Scope = append([]string(nil), copyInventory.Scope...)
		copyInventory.Files = append([]string(nil), copyInventory.Files...)
		copyInventory.LargeFiles = append([]string(nil), copyInventory.LargeFiles...)
		copyInventory.LargestFiles = append([]string(nil), copyInventory.LargestFiles...)
		if copyInventory.LanguageStats != nil {
			stats := make(map[string]ReviewLanguageStat, len(copyInventory.LanguageStats))
			for language, stat := range copyInventory.LanguageStats {
				stats[language] = stat
			}
			copyInventory.LanguageStats = stats
		}
		copyInventory.productionDirs = nil
		inventory = &copyInventory
	}
	progress := ReviewProgress{
		TargetHash: budget.TargetHash,
		Phase:      phase, CurrentRound: budget.CurrentRound, Allocation: budget.Allocation,
		HardLimit: budget.HardLimit, Baseline: budget.Baseline,
		Inventory: inventory, Focus: append([]string(nil), budget.Focus...), Rationale: budget.Rationale, Reason: reason,
	}
	ev.OnReviewProgress(progress)
}

func reviewPreflightPrompt(budget *ReviewBudget) string {
	if budget == nil {
		return ""
	}
	inventory := budget.Inventory
	var b strings.Builder
	b.WriteString("<review_preflight>\n")
	b.WriteString("This deterministic inventory is authoritative for the review scope. Treat it as scope context; it does not prescribe an inspection workflow.\n")
	b.WriteString("review policy: budget is an estimate; finish when semantic coverage is sufficient, not when every line is read.\n")
	fmt.Fprintf(&b, "scope: %s\nproduction files: %d\ntest files: %d\nproduction LOC: %d\n", strings.Join(inventory.Scope, ", "), inventory.ProductionFiles, inventory.TestFiles, inventory.ProductionLOC)
	if inventory.Partial {
		b.WriteString("inventory: partial; the scope ceiling was reached\n")
	}
	if len(budget.Focus) > 0 {
		fmt.Fprintf(&b, "focus: %s\n", strings.Join(budget.Focus, ", "))
	}
	if len(inventory.LargeFiles) > 0 {
		fmt.Fprintf(&b, "large production files: %s\n", strings.Join(inventory.LargeFiles, ", "))
	}
	if len(inventory.LanguageStats) > 0 {
		languages := make([]string, 0, len(inventory.LanguageStats))
		for language := range inventory.LanguageStats {
			languages = append(languages, language)
		}
		sort.Strings(languages)
		b.WriteString("production by language:\n")
		for _, language := range languages {
			stat := inventory.LanguageStats[language]
			fmt.Fprintf(&b, "- %s: %d files, %d LOC\n", language, stat.Files, stat.LOC)
		}
	}
	b.WriteString("files:\n")
	fileLimit := min(len(inventory.Files), maxReviewPreflightFiles)
	for _, file := range inventory.Files[:fileLimit] {
		b.WriteString("- ")
		b.WriteString(file)
		b.WriteByte('\n')
	}
	if fileLimit < len(inventory.Files) {
		fmt.Fprintf(&b, "- ... (%d additional files omitted from preflight)\n", len(inventory.Files)-fileLimit)
	}
	b.WriteString("</review_preflight>")
	return b.String()
}

func (a *Agent) emitReviewExtensionProgress(ev Events, budget *ReviewBudget, from, to int, reason string) {
	if ev.OnReviewProgress == nil || budget == nil {
		return
	}
	ev.OnReviewProgress(ReviewProgress{
		TargetHash: budget.TargetHash,
		Phase:      "extension", CurrentRound: budget.CurrentRound, Allocation: budget.Allocation,
		HardLimit: budget.HardLimit, Baseline: budget.Baseline,
		Focus: append([]string(nil), budget.Focus...), Rationale: budget.Rationale, Reason: reason,
		FromAllocation: from, ToAllocation: to,
	})
}

func reviewBudgetCheckpointReminder(budget *ReviewBudget) string {
	if budget == nil {
		return ""
	}
	if budget.Allocation >= budget.HardLimit {
		return fmt.Sprintf("<review_budget_checkpoint>\nYou have completed %d review exploration rounds, the absolute limit. Synthesis is mandatory now; disclose any explicitly requested area that remains unreviewed in the summary. Do not request an extension. Use one final evidence batch if one bounded lookup remains, or call submit_review. set_next_reasoning_effort remains available if you prefer a different effort for the next call.\n</review_budget_checkpoint>", budget.CurrentRound)
	}
	return fmt.Sprintf("<review_budget_checkpoint>\nYou have completed %d of %d allocated review exploration rounds (absolute limit %d). Budget is an estimate; sufficient semantic coverage is the completion criterion. For a broad review, request an extension when a meaningful requested area remains unreviewed. For a focused review, request one mainly when unresolved evidence could change a finding. Do not extend merely for line-by-line completeness when the remaining code adds no meaningful semantic coverage. If requesting one, call request_review_extension exactly once with reason (incomplete_coverage or unresolved_finding), one meaningful remaining area, one concise why_it_matters sentence, and the exact remaining lookup; otherwise use one final evidence batch. An explicitly requested but material area may justify an extension when you name the area and next files or entry points. The final evidence batch is only for one narrow confirmation after requested areas have been sampled; it does not replace missing meaningful coverage. After that batch, only submit_review and set_next_reasoning_effort are available.\n</review_budget_checkpoint>", budget.CurrentRound, budget.Allocation, budget.HardLimit)
}

// ReviewFinding describes one structured issue or observation in a code review.
type ReviewFinding struct {
	Title          string `json:"title"`
	Severity       string `json:"severity"` // "critical" | "high" | "medium" | "low" | "info"
	File           string `json:"file,omitempty"`
	Line           int    `json:"line,omitempty"`
	Evidence       string `json:"evidence,omitempty"`
	Recommendation string `json:"recommendation,omitempty"`
}

// Review is the structured result produced by the review workflow.
type Review struct {
	Summary         string          `json:"summary"`
	Verdict         string          `json:"verdict"` // "approve" | "request_changes" | "comment"
	Findings        []ReviewFinding `json:"findings"`
	ChecksPerformed []string        `json:"checks_performed,omitempty"`
}

// UnmarshalJSON accepts the two encodings emitted by review models for the
// optional checks list: an array, or a JSON string containing that array.
// Keeping this normalization on Review avoids a general-purpose JSON repair
// path and leaves Validate responsible for the existing content limits.
func (r *Review) UnmarshalJSON(data []byte) error {
	type reviewWire struct {
		Summary         string          `json:"summary"`
		Verdict         string          `json:"verdict"`
		Findings        json.RawMessage `json:"findings"`
		ChecksPerformed json.RawMessage `json:"checks_performed"`
	}
	var wire reviewWire
	if err := json.Unmarshal(data, &wire); err != nil {
		return err
	}
	var findings []ReviewFinding
	rawFindings := bytes.TrimSpace(wire.Findings)
	if len(rawFindings) > 0 && !bytes.Equal(rawFindings, []byte("null")) {
		if rawFindings[0] == '"' {
			var encoded string
			if err := json.Unmarshal(rawFindings, &encoded); err != nil {
				return fmt.Errorf("findings must be an array or JSON-encoded array: %w", err)
			}
			rawFindings = bytes.TrimSpace([]byte(encoded))
			if len(rawFindings) == 0 || rawFindings[0] != '[' {
				return errors.New("findings string is not a JSON array")
			}
		}
		if err := json.Unmarshal(rawFindings, &findings); err != nil {
			return fmt.Errorf("findings must contain review objects: %w", err)
		}
	}
	*r = Review{Summary: wire.Summary, Verdict: wire.Verdict, Findings: findings}
	raw := bytes.TrimSpace(wire.ChecksPerformed)
	if len(raw) == 0 || bytes.Equal(raw, []byte("null")) {
		return nil
	}
	var checks []string
	if raw[0] == '"' {
		var encoded string
		if err := json.Unmarshal(raw, &encoded); err != nil {
			return fmt.Errorf("checks_performed must be an array or JSON-encoded array: %w", err)
		}
		encodedJSON := bytes.TrimSpace([]byte(encoded))
		if len(encodedJSON) == 0 || encodedJSON[0] != '[' {
			return errors.New("checks_performed string is not a JSON array")
		}
		if err := json.Unmarshal(encodedJSON, &checks); err != nil {
			return fmt.Errorf("checks_performed string is not a JSON array: %w", err)
		}
	} else if err := json.Unmarshal(raw, &checks); err != nil {
		return fmt.Errorf("checks_performed must contain only strings: %w", err)
	}
	r.ChecksPerformed = checks
	return nil
}

const (
	maxReviewSummaryLen = 4096
	maxReviewFindings   = 50
	maxFindingFieldLen  = 2048
	maxReviewChecks     = 50
)

var validVerdicts = map[string]struct{}{
	"approve":         {},
	"request_changes": {},
	"comment":         {},
}

var validSeverities = map[string]struct{}{
	"critical": {},
	"high":     {},
	"medium":   {},
	"low":      {},
	"info":     {},
}

// Validate checks the review against structural constraints.
func (r Review) Validate() error {
	if strings.TrimSpace(r.Summary) == "" {
		return errors.New("review has no summary")
	}
	if len(r.Summary) > maxReviewSummaryLen {
		return fmt.Errorf("review summary exceeds %d characters", maxReviewSummaryLen)
	}
	verdict := strings.ToLower(strings.TrimSpace(r.Verdict))
	if _, ok := validVerdicts[verdict]; !ok {
		return fmt.Errorf("invalid verdict %q (want approve, request_changes, or comment)", r.Verdict)
	}
	if len(r.Findings) > maxReviewFindings {
		return fmt.Errorf("review findings exceed limit of %d", maxReviewFindings)
	}
	for i, f := range r.Findings {
		if strings.TrimSpace(f.Title) == "" {
			return fmt.Errorf("finding %d has no title", i+1)
		}
		if len(f.Title) > maxFindingFieldLen {
			return fmt.Errorf("finding %d title exceeds %d characters", i+1, maxFindingFieldLen)
		}
		sev := strings.ToLower(strings.TrimSpace(f.Severity))
		if _, ok := validSeverities[sev]; !ok {
			return fmt.Errorf("finding %d has invalid severity %q (want critical, high, medium, low, or info)", i+1, f.Severity)
		}
		if f.Line < 0 {
			return fmt.Errorf("finding %d has negative line number %d", i+1, f.Line)
		}
	}
	if len(r.ChecksPerformed) > maxReviewChecks {
		return fmt.Errorf("checks performed exceed limit of %d", maxReviewChecks)
	}
	for i, check := range r.ChecksPerformed {
		if strings.TrimSpace(check) == "" {
			return fmt.Errorf("check %d is empty", i+1)
		}
	}
	return nil
}

// ParseReview accepts the JSON object returned by the reviewer.
func ParseReview(response string) (Review, error) {
	response = strings.TrimSpace(response)
	var r Review
	if err := json.Unmarshal([]byte(response), &r); err != nil {
		return Review{}, fmt.Errorf("reviewer returned invalid JSON: %w", err)
	}
	if err := r.Validate(); err != nil {
		return Review{}, err
	}
	r.Summary = strings.TrimSpace(r.Summary)
	r.Verdict = strings.ToLower(strings.TrimSpace(r.Verdict))
	for i := range r.Findings {
		r.Findings[i].Title = strings.TrimSpace(r.Findings[i].Title)
		r.Findings[i].Severity = strings.ToLower(strings.TrimSpace(r.Findings[i].Severity))
		r.Findings[i].File = strings.TrimSpace(r.Findings[i].File)
		r.Findings[i].Evidence = strings.TrimSpace(r.Findings[i].Evidence)
		r.Findings[i].Recommendation = strings.TrimSpace(r.Findings[i].Recommendation)
	}
	for i := range r.ChecksPerformed {
		r.ChecksPerformed[i] = strings.TrimSpace(r.ChecksPerformed[i])
	}
	return r, nil
}

func submitReviewTool() tools.Tool {
	return tools.Tool{
		Def: models.NewTool("submit_review",
			"Submit the validated code review. This is the reviewer's terminal tool; call it once when inspection is complete.",
			`{"type":"object","properties":{"summary":{"type":"string","description":"Executive summary of the review"},"verdict":{"type":"string","enum":["approve","request_changes","comment"],"description":"Review verdict"},"findings":{"oneOf":[{"type":"array","description":"Structured findings","items":{"type":"object","properties":{"title":{"type":"string"},"severity":{"type":"string","enum":["critical","high","medium","low","info"]},"file":{"type":"string"},"line":{"type":"integer"},"evidence":{"type":"string"},"recommendation":{"type":"string"}},"required":["title","severity"]}},{"type":"string","description":"JSON-encoded array of structured findings"}]},"checks_performed":{"oneOf":[{"type":"array","items":{"type":"string"}},{"type":"string","description":"JSON-encoded array of check strings"}]}},"required":["summary","verdict","findings"]}`),
		Run: func(_ context.Context, args json.RawMessage) (string, error) {
			if _, err := ParseReview(string(args)); err != nil {
				return "", err
			}
			return "Review accepted.", nil
		},
	}
}

func requestReviewExtensionTool() tools.Tool {
	return tools.Tool{
		Def: models.NewTool("request_review_extension",
			"At a review budget checkpoint, request the next exploration lease of one quarter of the initial review allocation, capped by the absolute limit. Give a meaningful remaining area, why it matters, and the exact lookup; unread lines alone are not sufficient.",
			`{"type":"object","properties":{"reason":{"type":"string","enum":["incomplete_coverage","unresolved_finding"]},"remaining_area":{"type":"string"},"why_it_matters":{"type":"string"},"remaining_lookup":{"type":"string"}},"required":["reason","remaining_area","why_it_matters","remaining_lookup"],"additionalProperties":false}`),
		Run: func(_ context.Context, args json.RawMessage) (string, error) {
			if _, err := parseReviewExtension(args); err != nil {
				return "", err
			}
			return "Review extension request received.", nil
		},
	}
}

func parseReviewExtension(args json.RawMessage) (reviewExtensionRequest, error) {
	var request reviewExtensionRequest
	decoder := json.NewDecoder(bytes.NewReader(args))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&request); err != nil {
		return reviewExtensionRequest{}, fmt.Errorf("invalid review extension: %w", err)
	}
	var trailing any
	if err := decoder.Decode(&trailing); err != io.EOF {
		if err == nil {
			return reviewExtensionRequest{}, errors.New("invalid review extension: multiple JSON values")
		}
		return reviewExtensionRequest{}, fmt.Errorf("invalid review extension: %w", err)
	}
	request.Reason = strings.ToLower(strings.TrimSpace(request.Reason))
	request.RemainingArea = strings.TrimSpace(request.RemainingArea)
	request.WhyItMatters = strings.TrimSpace(request.WhyItMatters)
	request.RemainingLookup = strings.TrimSpace(request.RemainingLookup)
	request.UnresolvedIssue = strings.TrimSpace(request.UnresolvedIssue)
	request.Evidence = strings.TrimSpace(request.Evidence)
	if request.Reason == "" && request.RemainingArea == "" && request.WhyItMatters == "" && request.UnresolvedIssue != "" && request.Evidence != "" {
		request.Reason = "unresolved_finding"
		request.RemainingArea = request.UnresolvedIssue
		request.WhyItMatters = request.Evidence
	}
	if request.Reason != "incomplete_coverage" && request.Reason != "unresolved_finding" {
		return reviewExtensionRequest{}, errors.New("review extension reason must be incomplete_coverage or unresolved_finding")
	}
	if request.RemainingArea == "" || request.WhyItMatters == "" || request.RemainingLookup == "" {
		return reviewExtensionRequest{}, errors.New("review extension requires reason, remaining_area, why_it_matters, and remaining_lookup")
	}
	if len(request.RemainingArea) > maxFindingFieldLen || len(request.WhyItMatters) > maxFindingFieldLen || len(request.RemainingLookup) > maxFindingFieldLen {
		return reviewExtensionRequest{}, errors.New("review extension fields are too long")
	}
	return request, nil
}

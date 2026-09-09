package agent

import (
	"bytes"
	"context"
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

Before submitting, sample every area explicitly requested by the user. If the hard limit prevents that, disclose the unreviewed area in the summary.

When finished, call submit_review exactly once. Do not implement fixes or return an implementation plan.

The first request includes a deterministic <review_preflight> inventory with the authoritative scope and file list. Start from it; do not call glob or find_files to rediscover the scope. Use read.ranges as the default shape even for one range. Never construct or infer an opaque pagination cursor: pass cursor only when that same tool explicitly returned one, copied exactly.

For files over 500 lines, locate the relevant symbol or range with structural_search, lsp, or grep before reading. Do not paginate sequentially through an entire large file; if broad inspection is genuinely necessary, batch independent ranges in one read.ranges call.

At a review budget checkpoint, either call request_review_extension with one unresolved issue, the evidence that would resolve it, and the exact remaining lookup, or use one final evidence batch. After that batch, only submit_review is available.`

type reviewContinuation struct {
	target            string
	budget            *ReviewBudget
	checkpointPending bool
	closed            bool
}

const (
	defaultReviewBudget      = 4
	minReviewBudget          = 5
	maxReviewBudget          = 24
	reviewExplorationHardMax = 38
	reviewLeaseRounds        = 4
	maxReviewInventoryFiles  = 4096
	maxReviewInventoryBytes  = 32 << 20
	maxReviewFocusFiles      = 5
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
	Focus          []string
	Rationale      string
	Inventory      ReviewInventory
	checkpointOpen bool
}

// ReviewProgress is emitted for the TUI and headless telemetry. The payload
// is deliberately small and safe to send over the worker wire.
type ReviewProgress struct {
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

type reviewExtensionRequest struct {
	UnresolvedIssue string `json:"unresolved_issue"`
	Evidence        string `json:"evidence"`
	RemainingLookup string `json:"remaining_lookup"`
	Rounds          int    `json:"rounds"`
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
	if inventory.Partial {
		budget = maxReviewBudget
	}
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
	if value > maxReviewBudget {
		return maxReviewBudget
	}
	return value
}

func reviewExtensionAllocation(current, hardLimit int) int {
	if current >= hardLimit {
		return 0
	}
	return min(reviewLeaseRounds, hardLimit-current)
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
		Baseline: baseline, Allocation: baseline, HardLimit: reviewExplorationHardMax,
		Focus: focus, Inventory: inventory,
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
		Phase: phase, CurrentRound: budget.CurrentRound, Allocation: budget.Allocation,
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
	b.WriteString("This deterministic inventory is authoritative for the review scope. Do not use glob or find_files to rediscover these files; inspect the listed paths directly.\n")
	fmt.Fprintf(&b, "scope: %s\nproduction files: %d\ntest files: %d\nproduction LOC: %d\nallocated exploration rounds: %d\nhard limit: %d\n", strings.Join(inventory.Scope, ", "), inventory.ProductionFiles, inventory.TestFiles, inventory.ProductionLOC, budget.Allocation, budget.HardLimit)
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
	for _, file := range inventory.Files {
		b.WriteString("- ")
		b.WriteString(file)
		b.WriteByte('\n')
	}
	b.WriteString("</review_preflight>")
	return b.String()
}

func (a *Agent) emitReviewExtensionProgress(ev Events, budget *ReviewBudget, from, to int, reason string) {
	if ev.OnReviewProgress == nil || budget == nil {
		return
	}
	ev.OnReviewProgress(ReviewProgress{
		Phase: "extension", CurrentRound: budget.CurrentRound, Allocation: budget.Allocation,
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
		return fmt.Sprintf("<review_budget_checkpoint>\nYou have completed %d review exploration rounds, the absolute limit. Synthesis is mandatory now; disclose any explicitly requested area that remains unreviewed in the summary. Do not request an extension. Use one final evidence batch if one bounded lookup remains, or call submit_review.\n</review_budget_checkpoint>", budget.CurrentRound)
	}
	return fmt.Sprintf("<review_budget_checkpoint>\nYou have completed %d of %d allocated review exploration rounds (absolute limit %d). The pending navigation calls were withheld. Either call request_review_extension exactly once with one concrete unresolved issue, evidence already gathered, and the exact remaining lookup, or use one final evidence batch. An explicitly requested but untouched area, such as frontend, backend, desktop, authentication, or UI, is a valid reason for an extension; name that area and the next files or entry points you will inspect. Use the final evidence batch only for one narrow confirmation after requested areas have been sampled; it does not replace missing requested coverage. After that batch, only submit_review is available.\n</review_budget_checkpoint>", budget.CurrentRound, budget.Allocation, budget.HardLimit)
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
			`{"type":"object","properties":{"summary":{"type":"string","description":"Executive summary of the review"},"verdict":{"type":"string","enum":["approve","request_changes","comment"],"description":"Review verdict"},"findings":{"type":"array","description":"Structured findings","items":{"type":"object","properties":{"title":{"type":"string"},"severity":{"type":"string","enum":["critical","high","medium","low","info"]},"file":{"type":"string"},"line":{"type":"integer"},"evidence":{"type":"string"},"recommendation":{"type":"string"}},"required":["title","severity"]}},"checks_performed":{"type":"array","items":{"type":"string"}}},"required":["summary","verdict","findings"]}`),
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
			"At a review budget checkpoint, request up to four more exploration rounds. Name the unresolved issue, evidence needed, and exact remaining lookup.",
			`{"type":"object","properties":{"unresolved_issue":{"type":"string"},"evidence":{"type":"string"},"remaining_lookup":{"type":"string"},"rounds":{"type":"integer","minimum":1,"maximum":4}},"required":["unresolved_issue","evidence","remaining_lookup","rounds"],"additionalProperties":false}`),
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
	request.UnresolvedIssue = strings.TrimSpace(request.UnresolvedIssue)
	request.Evidence = strings.TrimSpace(request.Evidence)
	request.RemainingLookup = strings.TrimSpace(request.RemainingLookup)
	if request.UnresolvedIssue == "" || request.Evidence == "" || request.RemainingLookup == "" {
		return reviewExtensionRequest{}, errors.New("review extension requires unresolved_issue, evidence, and remaining_lookup")
	}
	if len(request.UnresolvedIssue) > maxFindingFieldLen || len(request.Evidence) > maxFindingFieldLen || len(request.RemainingLookup) > maxFindingFieldLen {
		return reviewExtensionRequest{}, errors.New("review extension fields are too long")
	}
	if request.Rounds < 1 || request.Rounds > reviewLeaseRounds {
		return reviewExtensionRequest{}, fmt.Errorf("review extension rounds must be between 1 and %d", reviewLeaseRounds)
	}
	return request, nil
}

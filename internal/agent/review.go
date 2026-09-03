package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/sacca97/ghg/internal/models"
	"github.com/sacca97/ghg/internal/tools"
)

// reviewModePrompt is injected as a transient system message on every model
// round while the agent is in Review mode.
const reviewModePrompt = `You are reviewing in a read-only collaboration mode. Inspect only the repository evidence needed to evaluate the user's request.

Report only actionable, evidence-backed findings. Order findings by severity and expected impact. Distinguish confirmed defects from opportunities that require measurement. Do not invent findings to fill categories. If no material problems are found, say so in the summary and submit an empty findings list.

When finished, call submit_review exactly once. Do not implement fixes or return an implementation plan.`

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

// buildReviewCheckpoint constructs a deterministic markdown checkpoint containing
// the review target, verdict, summary, findings, and checks performed.
func buildReviewCheckpoint(target string, rev Review) string {
	var b strings.Builder
	b.WriteString("# Review Checkpoint\n\n")
	if strings.TrimSpace(target) != "" {
		b.WriteString("## Target / Instructions\n\n")
		b.WriteString(strings.TrimSpace(target))
		b.WriteString("\n\n")
	}
	b.WriteString(fmt.Sprintf("## Verdict: %s\n\n", rev.Verdict))
	b.WriteString("### Summary\n\n")
	b.WriteString(rev.Summary)
	b.WriteString("\n\n")

	if len(rev.Findings) > 0 {
		b.WriteString("### Findings\n\n")
		for _, f := range rev.Findings {
			loc := ""
			if f.File != "" {
				if f.Line > 0 {
					loc = fmt.Sprintf(" (%s:%d)", f.File, f.Line)
				} else {
					loc = fmt.Sprintf(" (%s)", f.File)
				}
			}
			b.WriteString(fmt.Sprintf("- **[%s] %s%s**\n", strings.ToUpper(f.Severity), f.Title, loc))
			if f.Evidence != "" {
				b.WriteString(fmt.Sprintf("  - Evidence: %s\n", f.Evidence))
			}
			if f.Recommendation != "" {
				b.WriteString(fmt.Sprintf("  - Recommendation: %s\n", f.Recommendation))
			}
		}
		b.WriteString("\n")
	}
	if len(rev.ChecksPerformed) > 0 {
		b.WriteString("### Checks Performed\n\n")
		for _, c := range rev.ChecksPerformed {
			b.WriteString(fmt.Sprintf("- %s\n", c))
		}
	}
	return strings.TrimSpace(b.String())
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

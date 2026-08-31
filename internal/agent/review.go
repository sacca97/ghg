package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
)

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

const maxReviewAttempts = 2

// ProposeReview runs the built-in reviewer definition.
func ProposeReview(ctx context.Context, reviewer *Agent, target string, ev Events) (Review, error) {
	return ProposeReviewWithDefinition(ctx, reviewer, target, BuiltInReviewerDefinition(), ev)
}

// ProposeReviewWithDefinition runs the review workflow with a specific definition.
func ProposeReviewWithDefinition(ctx context.Context, reviewer *Agent, target string, def Definition, ev Events) (Review, error) {
	var lastErr error
	for attempt := 0; attempt < maxReviewAttempts; attempt++ {
		input := reviewerInput(target, attempt > 0)
		result, err := reviewer.RunDefinition(ctx, input, def, ev)
		if err != nil {
			return Review{}, err
		}
		if result.TerminalName != "submit_review" {
			lastErr = errors.New("reviewer did not call submit_review")
			continue
		}
		review, err := ParseReview(string(result.TerminalArgs))
		if err == nil {
			return review, nil
		}
		lastErr = err
	}
	if lastErr == nil {
		lastErr = errors.New("reviewer returned no review")
	}
	return Review{}, fmt.Errorf("invalid reviewer output after %d attempts: %w", maxReviewAttempts, lastErr)
}

func reviewerInput(target string, retry bool) string {
	input := fmt.Sprintf("Review the following changes or target:\n\n%s\n\nInspect the repository as needed with read-only tools, then submit the review with submit_review.", strings.TrimSpace(target))
	if retry {
		input += "\n\nYour previous review was invalid or did not use the terminal tool. Correct it and call submit_review now."
	}
	return input
}

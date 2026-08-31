package export

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/sacca97/ghg/internal/agent"
	"github.com/sacca97/ghg/internal/llm"
	"github.com/sacca97/ghg/internal/session"
)

var (
	ErrDestinationExists = errors.New("destination file already exists")
	ErrUnsupportedKind   = errors.New("unsupported workflow result kind")
	ErrUnsupportedFormat = errors.New("unsupported export format")
)

// Format names
const (
	FormatMarkdown = "markdown"
	FormatJSON     = "json"
)

// DefaultExportFilename derives a sanitized default file name for export.
func DefaultExportFilename(kind string, t time.Time, format string) string {
	if t.IsZero() {
		t = time.Now()
	}
	kind = strings.ToLower(strings.TrimSpace(kind))
	if kind == "" {
		kind = "result"
	}
	stamp := t.UTC().Format("20060102-150405")
	ext := ".md"
	if strings.ToLower(format) == FormatJSON {
		ext = ".json"
	}
	return fmt.Sprintf("%s-%s%s", kind, stamp, ext)
}

// RenderPlanMarkdown renders a structured plan to human-readable Markdown.
func RenderPlanMarkdown(p agent.Plan) string {
	var b strings.Builder
	b.WriteString("# Plan: " + p.Goal + "\n\n")
	if len(p.Assumptions) > 0 {
		b.WriteString("## Assumptions\n\n")
		for _, a := range p.Assumptions {
			b.WriteString("- " + a + "\n")
		}
		b.WriteString("\n")
	}
	b.WriteString("## Steps\n\n")
	for i, step := range p.Steps {
		fmt.Fprintf(&b, "%d. %s\n", i+1, step)
	}
	b.WriteString("\n## Acceptance checks\n\n")
	for _, check := range p.AcceptanceChecks {
		b.WriteString("- " + check + "\n")
	}
	if len(p.Risks) > 0 {
		b.WriteString("\n## Risks\n\n")
		for _, r := range p.Risks {
			b.WriteString("- " + r + "\n")
		}
	}
	return strings.TrimRight(b.String(), "\n") + "\n"
}

var severityOrder = map[string]int{
	"critical": 0,
	"high":     1,
	"medium":   2,
	"low":      3,
	"info":     4,
}

// RenderReviewMarkdown renders a structured review to human-readable Markdown.
func RenderReviewMarkdown(r agent.Review) string {
	var b strings.Builder
	b.WriteString("# Review: " + strings.ToUpper(r.Verdict) + "\n\n")
	b.WriteString(r.Summary + "\n\n")
	if len(r.ChecksPerformed) > 0 {
		b.WriteString("## Checks performed\n\n")
		for _, check := range r.ChecksPerformed {
			b.WriteString("- " + check + "\n")
		}
		b.WriteString("\n")
	}
	if len(r.Findings) > 0 {
		b.WriteString("## Findings\n\n")
		// Stable sort findings by severity (critical first)
		findings := append([]agent.ReviewFinding(nil), r.Findings...)
		sort.SliceStable(findings, func(i, j int) bool {
			oi, oki := severityOrder[findings[i].Severity]
			if !oki {
				oi = 99
			}
			oj, okj := severityOrder[findings[j].Severity]
			if !okj {
				oj = 99
			}
			return oi < oj
		})
		for _, f := range findings {
			fmt.Fprintf(&b, "### [%s] %s\n\n", strings.ToUpper(f.Severity), f.Title)
			if f.File != "" {
				if f.Line > 0 {
					fmt.Fprintf(&b, "- **Location**: `%s:%d`\n", f.File, f.Line)
				} else {
					fmt.Fprintf(&b, "- **Location**: `%s`\n", f.File)
				}
			}
			if f.Evidence != "" {
				fmt.Fprintf(&b, "- **Evidence**: %s\n", f.Evidence)
			}
			if f.Recommendation != "" {
				fmt.Fprintf(&b, "- **Recommendation**: %s\n", f.Recommendation)
			}
			b.WriteString("\n")
		}
	}
	return strings.TrimRight(b.String(), "\n") + "\n"
}

// RenderChat formats a session conversation into clean Markdown / text.
func RenderChat(sessionID string, msgs []llm.Message) string {
	var b strings.Builder
	if sessionID != "" {
		b.WriteString("# Conversation: " + sessionID + "\n\n")
	} else {
		b.WriteString("# Conversation\n\n")
	}
	for _, m := range msgs {
		if m.Role == "system" {
			// Skip initial default system prompt, render if it is a compaction summary
			if strings.Contains(m.Content, "Summary") || strings.Contains(m.Content, "summary") {
				b.WriteString("### System (Summary)\n\n" + strings.TrimSpace(m.Content) + "\n\n")
			}
			continue
		}
		switch m.Role {
		case "user":
			b.WriteString("### User\n\n" + strings.TrimSpace(m.TextContent()) + "\n\n")
		case "assistant", "model":
			b.WriteString("### Assistant\n\n")
			if text := strings.TrimSpace(m.TextContent()); text != "" {
				b.WriteString(text + "\n\n")
			}
			for _, tc := range m.ToolCalls {
				b.WriteString("`Tool Call: " + tc.Function.Name + "`\n```json\n" + strings.TrimSpace(tc.Function.Arguments) + "\n```\n\n")
			}
		case "tool":
			name := m.ToolCallID
			if name == "" {
				name = "result"
			}
			b.WriteString("`Tool Result (" + name + ")`\n```\n" + strings.TrimSpace(m.Content) + "\n```\n\n")
		default:
			roleName := m.Role
			if len(roleName) > 0 {
				roleName = strings.ToUpper(roleName[:1]) + roleName[1:]
			}
			b.WriteString("### " + roleName + "\n\n" + strings.TrimSpace(m.Content) + "\n\n")
		}
	}
	return strings.TrimRight(b.String(), "\n") + "\n"
}

// RenderResult renders a persisted workflow result record to the requested format.
func RenderResult(record session.WorkflowResultRecord, format string) ([]byte, error) {
	switch strings.ToLower(format) {
	case FormatJSON, "raw":
		// Format the structured JSON payload indented
		var raw any
		if err := json.Unmarshal([]byte(record.Payload), &raw); err != nil {
			return nil, fmt.Errorf("decode result payload: %w", err)
		}
		out := map[string]any{
			"id":         record.ResultID,
			"session_id": record.SessionID,
			"kind":       record.Kind,
			"version":    record.Version,
			"created_at": record.CreatedAt.UTC().Format(time.RFC3339Nano),
			"role":       record.Role,
			"provider":   record.Provider,
			"model":      record.Model,
			"payload":    raw,
		}
		return json.MarshalIndent(out, "", "  ")
	case FormatMarkdown, "md", "text", "txt", "":
		switch record.Kind {
		case "plan":
			plan, err := agent.ParsePlan(record.Payload)
			if err != nil {
				return nil, fmt.Errorf("parse plan payload: %w", err)
			}
			return []byte(RenderPlanMarkdown(plan)), nil
		case "review":
			review, err := agent.ParseReview(record.Payload)
			if err != nil {
				return nil, fmt.Errorf("parse review payload: %w", err)
			}
			return []byte(RenderReviewMarkdown(review)), nil
		case "message", "text":
			var rawText string
			if err := json.Unmarshal([]byte(record.Payload), &rawText); err == nil {
				return []byte(strings.TrimRight(rawText, "\n") + "\n"), nil
			}
			return []byte(strings.TrimRight(record.Payload, "\n") + "\n"), nil
		case "chat", "log", "transcript":
			var msgs []llm.Message
			if err := json.Unmarshal([]byte(record.Payload), &msgs); err != nil {
				return nil, fmt.Errorf("parse chat payload: %w", err)
			}
			return []byte(RenderChat(record.SessionID, msgs)), nil
		default:
			return nil, fmt.Errorf("%w: %q", ErrUnsupportedKind, record.Kind)
		}
	default:
		return nil, fmt.Errorf("%w: %q", ErrUnsupportedFormat, format)
	}
}

// WriteExportFile writes exported data atomically with permissions 0600.
func WriteExportFile(dest string, data []byte, force bool, workspace string) (string, error) {
	dest = strings.TrimSpace(dest)
	if dest == "" {
		return "", errors.New("destination path cannot be empty")
	}
	if workspace == "" {
		var err error
		workspace, err = os.Getwd()
		if err != nil {
			return "", fmt.Errorf("resolve workspace: %w", err)
		}
	}
	absWorkspace, err := filepath.Abs(workspace)
	if err != nil {
		return "", fmt.Errorf("resolve workspace path: %w", err)
	}

	var targetPath string
	if filepath.IsAbs(dest) {
		targetPath = filepath.Clean(dest)
	} else {
		targetPath = filepath.Clean(filepath.Join(absWorkspace, dest))
	}

	targetDir := filepath.Dir(targetPath)
	if err := os.MkdirAll(targetDir, 0755); err != nil {
		return "", fmt.Errorf("create destination directory: %w", err)
	}

	if _, err := os.Stat(targetPath); err == nil {
		if !force {
			return targetPath, fmt.Errorf("%w: %s", ErrDestinationExists, targetPath)
		}
	}

	tmpFile, err := os.CreateTemp(targetDir, ".export-*.tmp")
	if err != nil {
		return "", fmt.Errorf("create temporary export file: %w", err)
	}
	tmpPath := tmpFile.Name()
	cleanup := true
	defer func() {
		if cleanup {
			_ = os.Remove(tmpPath)
		}
	}()

	if err := tmpFile.Chmod(0600); err != nil {
		_ = tmpFile.Close()
		return "", fmt.Errorf("set export file permissions: %w", err)
	}
	if _, err := tmpFile.Write(data); err != nil {
		_ = tmpFile.Close()
		return "", fmt.Errorf("write export data: %w", err)
	}
	if err := tmpFile.Sync(); err != nil {
		_ = tmpFile.Close()
		return "", fmt.Errorf("sync export file: %w", err)
	}
	if err := tmpFile.Close(); err != nil {
		return "", fmt.Errorf("close export file: %w", err)
	}

	if err := os.Rename(tmpPath, targetPath); err != nil {
		return "", fmt.Errorf("commit export file: %w", err)
	}
	cleanup = false
	return targetPath, nil
}

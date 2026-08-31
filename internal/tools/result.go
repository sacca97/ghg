package tools

import (
	"bytes"
	"errors"
	"fmt"
	"strconv"
	"strings"

	"github.com/sacca97/ghg/internal/artifact"
)

const maxArtifactBytes = artifact.DefaultMaxBytes

// ToolResult is the internal result of one tool invocation. Preview is the
// bounded text sent to the model; Retained is the bounded evidence available
// to the artifact writer before the preview is shortened. Legacy tools may
// leave the extra fields empty and are normalized by ExecuteResult.
type ToolResult struct {
	Preview       string
	Retained      string
	OriginalBytes int64
	Complete      bool
	Artifact      *artifact.Ref
	ExitCode      int
	// Source identifies the tool/integration that produced the bytes. It is
	// assigned by ExecuteResult so MCP and future network tools share the same
	// untrusted-content boundary.
	Source   string
	Metadata map[string]string
}

// textResult retains bounded evidence and applies the caller's model preview.
// The raw string is already bounded by callers that read external streams;
// retainText applies the common hard ceiling for file/MCP/tool strings.
func textResult(raw, preview string, exitCode int) ToolResult {
	retained, complete := retainText(raw)
	if preview == "" && raw != "" {
		preview = Truncate(raw)
	}
	return ToolResult{
		Preview:       preview,
		Retained:      retained,
		OriginalBytes: int64(len(raw)),
		Complete:      complete,
		ExitCode:      exitCode,
	}
}

// TextResult builds a structured result for an integration tool whose raw
// output is already available as a string. preview is the text sent to the
// model; an empty preview for non-empty raw output uses the standard head cap.
func TextResult(raw, preview string) ToolResult {
	return textResult(raw, preview, 0)
}

// capturedResult adapts a stream capture that already retained a bounded
// representation while preserving the producer's original byte count.
func capturedResult(retained, preview string, original int64, complete bool, exitCode int) ToolResult {
	if original <= 0 {
		original = int64(len(retained))
	}
	if preview == "" && retained != "" {
		preview = TruncateTail(retained)
	}
	return ToolResult{
		Preview:       preview,
		Retained:      retained,
		OriginalBytes: original,
		Complete:      complete && original == int64(len(retained)),
		ExitCode:      exitCode,
	}
}

func retainText(s string) (string, bool) {
	if int64(len(s)) <= artifact.DefaultMaxBytes {
		return s, true
	}
	data := retainBytes([]byte(s), artifact.DefaultMaxBytes)
	return string(data), false
}

func retainBytes(data []byte, limit int64) []byte {
	if limit <= 0 || int64(len(data)) <= limit {
		return bytes.Clone(data)
	}
	head := int(limit / 2)
	tail := int(limit) - head
	out := make([]byte, 0, int(limit))
	out = append(out, data[:head]...)
	out = append(out, data[len(data)-tail:]...)
	return out
}

// TextCapture is the string-side equivalent of the bash runner's bounded
// capture. It lets file/MCP adapters count a large result without retaining
// more than the artifact ceiling in memory.
type TextCapture struct {
	limit     int
	total     int64
	data      []byte
	head      []byte
	tail      []byte
	truncated bool
}

// NewTextCapture creates a bounded capture. A non-positive limit uses the
// default artifact ceiling.
func NewTextCapture(limit int64) *TextCapture {
	if limit <= 0 {
		limit = maxArtifactBytes
	}
	return &TextCapture{limit: int(limit)}
}

func (c *TextCapture) WriteString(s string) {
	c.total += int64(len(s))
	if c.truncated {
		c.appendTailString(s)
		return
	}
	if len(c.data)+len(s) <= c.limit {
		c.data = append(c.data, s...)
		return
	}
	c.truncated = true
	headLen := c.limit / 2
	c.head = make([]byte, 0, headLen)
	if len(c.data) >= headLen {
		c.head = append(c.head, c.data[:headLen]...)
	} else {
		c.head = append(c.head, c.data...)
		c.head = append(c.head, s[:headLen-len(c.data)]...)
	}
	c.tail = lastBytes(c.data, s, c.limit-headLen)
	c.data = nil
}

func (c *TextCapture) appendTailString(s string) {
	tailLen := c.limit - c.limit/2
	if len(s) >= tailLen {
		c.tail = append(c.tail[:0], s[len(s)-tailLen:]...)
		return
	}
	keep := tailLen - len(s)
	if len(c.tail) < keep {
		keep = len(c.tail)
	}
	start := len(c.tail) - keep
	out := make([]byte, 0, keep+len(s))
	out = append(out, c.tail[start:]...)
	out = append(out, s...)
	c.tail = out
}

func lastBytes(prefix []byte, suffix string, limit int) []byte {
	if len(suffix) >= limit {
		return append([]byte(nil), suffix[len(suffix)-limit:]...)
	}
	keep := limit - len(suffix)
	if len(prefix) > keep {
		prefix = prefix[len(prefix)-keep:]
	}
	out := make([]byte, 0, len(prefix)+len(suffix))
	out = append(out, prefix...)
	out = append(out, suffix...)
	return out
}

func (c *TextCapture) String() string {
	if !c.truncated {
		return string(c.data)
	}
	data := make([]byte, 0, len(c.head)+len(c.tail))
	data = append(data, c.head...)
	data = append(data, c.tail...)
	return string(data)
}

// OriginalBytes returns the total number of bytes written, including bytes
// omitted from the retained representation.
func (c *TextCapture) OriginalBytes() int64 { return c.total }

// Complete reports whether every written byte is retained.
func (c *TextCapture) Complete() bool { return !c.truncated }

// CapturedTextResult converts a bounded capture into a structured result.
func CapturedTextResult(c *TextCapture, preview string, exitCode int) ToolResult {
	if c == nil {
		return ToolResult{}
	}
	return capturedResult(c.String(), preview, c.OriginalBytes(), c.Complete(), exitCode)
}

// TextResultWithSize builds a result from an already-bounded representation
// while preserving the producer's original byte count.
func TextResultWithSize(retained, preview string, original int64, complete bool, exitCode int) ToolResult {
	return capturedResult(retained, preview, original, complete, exitCode)
}

// MarkUntrusted records that result's bytes came from an external or
// user-controlled source. The marker is metadata rather than a change to
// Preview so legacy Execute callers and the TUI can keep their existing text;
// the agent applies ModelText when it builds the provider-facing message.
func MarkUntrusted(result ToolResult, source string) ToolResult {
	if strings.TrimSpace(source) == "" {
		source = result.Source
	}
	if strings.TrimSpace(source) == "" {
		source = "tool"
	}
	result.Source = source
	if result.Metadata == nil {
		result.Metadata = map[string]string{}
	}
	result.Metadata["source"] = source
	result.Metadata["untrusted"] = "true"
	return result
}

// IsUntrusted reports whether a result should be delimited before it is sent
// to the model. Callers use MarkUntrusted at the producer boundary, so a
// future network or MCP adapter cannot accidentally forget the policy while
// reusing the structured result type.
func IsUntrusted(result ToolResult) bool {
	return strings.EqualFold(result.Metadata["untrusted"], "true")
}

// ModelText renders the model-facing form of a tool result. Untrusted bytes
// are explicitly delimited and the source is quoted as data. A trusted
// artifact reference, when present, stays outside the delimiters so the
// recovery instruction cannot be confused with bytes returned by the tool.
func ModelText(result ToolResult) string {
	if !IsUntrusted(result) {
		return result.Preview
	}
	source := result.Source
	if strings.TrimSpace(source) == "" {
		source = "tool"
	}
	body, reference := result.Preview, ""
	if result.Artifact != nil {
		candidate := ArtifactReference(*result.Artifact)
		if strings.HasSuffix(body, candidate) {
			body = strings.TrimSuffix(body, candidate)
			reference = candidate
		}
	}
	return fmt.Sprintf("<untrusted_tool_output source=%s>\n%s\n</untrusted_tool_output>%s",
		strconv.Quote(source), body, reference)
}

func normalizeResult(result ToolResult) ToolResult {
	if result.OriginalBytes <= 0 {
		result.OriginalBytes = int64(len(result.Retained))
	}
	if result.Retained == "" && result.Preview != "" && result.Preview != "(no output)" {
		result.Retained, result.Complete = retainText(result.Preview)
		if result.OriginalBytes < int64(len(result.Retained)) {
			result.OriginalBytes = int64(len(result.Retained))
		}
	}
	if result.Preview == "" {
		result.Preview = "(no output)"
	}
	result.Preview = truncate(result.Preview)
	return result
}

func errorToolResult(err error) ToolResult {
	if err == nil {
		err = errors.New("tool failed")
	}
	message := "Error: " + err.Error()
	result := textResult(message, message, 1)
	result.Complete = true
	return result
}

// ArtifactReference is appended to a model-facing preview after the artifact is
// durably written. The reference deliberately contains no filesystem path.
func ArtifactReference(ref artifact.Ref) string {
	retention := "full result retained"
	if !ref.Complete {
		retention = "only deterministic head/tail retained; middle omitted"
	}
	return fmt.Sprintf("\n[artifact %s: %d bytes original, %d bytes stored; %s; use artifact_read with this id]",
		ref.ID, ref.OriginalBytes, ref.StoredBytes, retention)
}

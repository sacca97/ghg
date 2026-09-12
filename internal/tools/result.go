package tools

import (
	"bytes"
	"errors"
	"fmt"
	"strconv"
	"strings"

	"github.com/sacca97/ghg/internal/models"
	"github.com/sacca97/ghg/internal/session"
)

const maxOutputBytes int64 = session.DefaultMaxBytes
const maxOutput = 16 << 10

func Truncate(s string) string {
	return truncateWithMarkerLimit(s, maxOutput, func(omitted int) string {
		return fmt.Sprintf("\n... [truncated %d bytes]", omitted)
	}, false)
}

func TruncateWithSuffix(s, suffix string) string {
	if len(s)+len(suffix) <= maxOutput {
		return s + suffix
	}
	if len(suffix) >= maxOutput {
		return suffix[len(suffix)-maxOutput:]
	}
	return truncateWithMarkerLimit(s, maxOutput-len(suffix), func(omitted int) string {
		return fmt.Sprintf("\n... [truncated %d bytes]", omitted)
	}, false) + suffix
}

func TruncateTail(s string) string {
	return truncateWithMarkerLimit(s, maxOutput, func(omitted int) string {
		return fmt.Sprintf("[... first %d bytes truncated]\n", omitted)
	}, true)
}

// TruncateTailWithLimit keeps the end of s within limit bytes and identifies
// the omitted prefix.
func TruncateTailWithLimit(s string, limit int) string {
	if limit <= 0 {
		return ""
	}
	return truncateWithMarkerLimit(s, limit, func(omitted int) string {
		return fmt.Sprintf("[... first %d bytes truncated]\n", omitted)
	}, true)
}

func truncateWithMarkerLimit(s string, limit int, marker func(omitted int) string, tail bool) string {
	if len(s) <= limit {
		return s
	}
	keep := limit
	for {
		text := marker(len(s) - keep)
		next := limit - len(text)
		if next <= 0 {
			return text[:limit]
		}
		if next == keep {
			if tail {
				return text + s[len(s)-keep:]
			}
			return s[:keep] + text
		}
		keep = next
	}
}

// ToolResult contains bounded output data from a tool execution.
// Tracks preview text, retained output for storage, and source completeness.
type ToolResult struct {
	Preview       string
	Retained      string
	OriginalBytes int64
	Complete      bool
	Output        *models.OutputRef
	ExitCode      int
	// Source identifies the tool producing the result for content boundaries.
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
	if int64(len(s)) <= maxOutputBytes {
		return s, true
	}
	data := retainBytes([]byte(s), maxOutputBytes)
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

// OutputCapture bounds output while retaining a deterministic representation
// and an optional rolling preview.
type OutputCapture struct {
	limit     int
	total     int64
	data      []byte
	head      []byte
	tail      []byte
	rolling   []byte
	truncated bool
	preview   bool
}

// NewOutputCapture creates a bounded capture. A non-positive limit uses the
// default output ceiling. preview enables the rolling live-preview buffer.
func NewOutputCapture(limit int64, preview ...bool) *OutputCapture {
	if limit <= 0 {
		limit = maxOutputBytes
	}
	return &OutputCapture{limit: int(limit), preview: len(preview) > 0 && preview[0]}
}

func (c *OutputCapture) Write(p []byte) (int, error) {
	c.write(p)
	return len(p), nil
}

func (c *OutputCapture) WriteString(s string) (int, error) {
	c.write([]byte(s))
	return len(s), nil
}

func (c *OutputCapture) write(p []byte) {
	c.total += int64(len(p))
	if c.preview {
		c.appendRolling(p, 16<<10)
	}
	if c.truncated {
		c.appendTail(p)
		return
	}
	c.data = append(c.data, p...)
	if len(c.data) <= c.limit {
		return
	}
	c.truncated = true
	headLen := c.limit / 2
	if headLen > len(c.data) {
		headLen = len(c.data)
	}
	c.head = append([]byte(nil), c.data[:headLen]...)
	c.tail = append([]byte(nil), c.data[len(c.data)-(c.limit-headLen):]...)
	c.data = nil
}

func (c *OutputCapture) appendRolling(p []byte, limit int) {
	if len(p) >= limit {
		c.rolling = append(c.rolling[:0], p[len(p)-limit:]...)
		return
	}
	c.rolling = append(c.rolling, p...)
	if len(c.rolling) > limit {
		c.rolling = c.rolling[len(c.rolling)-limit:]
	}
}

func (c *OutputCapture) appendTail(p []byte) {
	tailLen := c.limit - c.limit/2
	if len(p) >= tailLen {
		c.tail = append(c.tail[:0], p[len(p)-tailLen:]...)
		return
	}
	keep := tailLen - len(p)
	if len(c.tail) < keep {
		keep = len(c.tail)
	}
	start := len(c.tail) - keep
	out := make([]byte, 0, keep+len(p))
	out = append(out, c.tail[start:]...)
	out = append(out, p...)
	c.tail = out
}

// Preview returns the latest output within limit bytes.
func (c *OutputCapture) Preview(limit int) string {
	if limit <= 0 {
		limit = maxOutput
	}
	data := c.rolling
	if len(data) == 0 {
		if c.truncated {
			data = c.tail
		} else {
			data = c.data
		}
	}
	if len(data) > limit {
		data = data[len(data)-limit:]
	}
	return validUTF8Tail(data)
}

func validUTF8Tail(data []byte) string {
	for len(data) > 0 && (data[0]&0xc0) == 0x80 {
		data = data[1:]
	}
	return string(data)
}

func (c *OutputCapture) String() string {
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
func (c *OutputCapture) OriginalBytes() int64 { return c.total }

// Complete reports whether every written byte is retained.
func (c *OutputCapture) Complete() bool { return !c.truncated }

// TextCapture remains an alias for integration callers that use the older
// name.
type TextCapture = OutputCapture

// NewTextCapture creates a bounded capture for integration output.
func NewTextCapture(limit int64) *TextCapture { return NewOutputCapture(limit) }

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
// output reference, when present, stays outside the delimiters so the
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
	if result.Output != nil {
		candidate := OutputReference(*result.Output)
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
	result.Preview = Truncate(result.Preview)
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

// OutputReference is appended to a model-facing preview after the output is
// durably written. The reference deliberately contains no filesystem path.
func OutputReference(ref models.OutputRef) string {
	retention := "full result retained"
	if !ref.Complete {
		retention = "only deterministic head/tail retained; middle omitted"
	}
	return fmt.Sprintf("\n[output %s: %d bytes original, %d bytes stored; %s; use output_read with this id]",
		ref.ID, ref.OriginalBytes, ref.StoredBytes, retention)
}

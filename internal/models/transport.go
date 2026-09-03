package models

import (
	"bufio"
	"bytes"
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"
)

type transport struct {
	BaseURL    string
	APIKey     string
	HTTP       *http.Client
	Headers    map[string]string
	AuthKind   string
	AuthHeader string
	MaxRetries int
	OnRetry    func(RetryEvent)
}

func newTransport(baseURL, apiKey string) transport {
	return transport{
		BaseURL: strings.TrimRight(baseURL, "/"),
		APIKey:  apiKey,
		HTTP:    &http.Client{Timeout: 10 * time.Minute},
	}
}

func (t transport) attempts() int {
	if t.MaxRetries > 0 {
		return t.MaxRetries
	}
	return DefaultMaxAttempts
}

func (t transport) httpClient() *http.Client {
	if t.HTTP != nil {
		return t.HTTP
	}
	return http.DefaultClient
}

var (
	ephemeralSessionMu sync.Mutex
	ephemeralSessionID string
)

func fallbackSessionID() string {
	ephemeralSessionMu.Lock()
	defer ephemeralSessionMu.Unlock()
	if ephemeralSessionID == "" {
		b := make([]byte, 8)
		if _, err := rand.Read(b); err != nil {
			ephemeralSessionID = fmt.Sprintf("ghg-%x", time.Now().UnixNano())
		} else {
			ephemeralSessionID = fmt.Sprintf("ghg-%x", b)
		}
	}
	return ephemeralSessionID
}

type sessionIDKey struct{}

// WithSessionID returns a context carrying the conversation's session ID.
func WithSessionID(ctx context.Context, sessionID string) context.Context {
	if sessionID == "" {
		return ctx
	}
	return context.WithValue(ctx, sessionIDKey{}, sessionID)
}

// SessionIDFromContext returns the session ID stored in ctx, or "".
func SessionIDFromContext(ctx context.Context) string {
	if ctx == nil {
		return ""
	}
	id, _ := ctx.Value(sessionIDKey{}).(string)
	return id
}

func (t transport) setRequestHeaders(req *http.Request) error {
	return applyRequestHeaders(req, t.Headers, t.APIKey, t.AuthKind, t.AuthHeader)
}

func applyRequestHeaders(req *http.Request, headers map[string]string, apiKey, authKind, authHeader string) error {
	if req.Header.Get("User-Agent") == "" {
		req.Header.Set("User-Agent", "ghg")
	}
	for name, value := range headers {
		req.Header.Set(name, value)
	}
	if req.Header.Get("X-Opencode-Session") == "" {
		sid := SessionIDFromContext(req.Context())
		if sid == "" {
			sid = fallbackSessionID()
		}
		req.Header.Set("X-Opencode-Session", sid)
	}
	kind := authKind
	if kind == "" {
		kind = "bearer"
	}
	if kind == "none" || apiKey == "" {
		if kind != "bearer" && kind != "header" && kind != "none" {
			return fmt.Errorf("models: unsupported auth kind %q", authKind)
		}
		return nil
	}
	header := authHeader
	if header == "" {
		header = "Authorization"
	}
	switch kind {
	case "bearer":
		req.Header.Set(header, "Bearer "+apiKey)
	case "header":
		req.Header.Set(header, apiKey)
	default:
		return fmt.Errorf("models: unsupported auth kind %q", authKind)
	}
	return nil
}

// HTTPError retains the status and bounded response body from a failed call.
type HTTPError struct {
	Status string
	Body   string
}

func (e *HTTPError) Error() string { return e.Status + ": " + e.Body }

func readHTTPError(resp *http.Response) *HTTPError {
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
	return &HTTPError{Status: resp.Status, Body: strings.TrimSpace(string(body))}
}

const DefaultMaxAttempts = 3

type RetryEvent struct {
	Attempt int
	Max     int
	Delay   time.Duration
	Err     error
}

func retryableStatus(code int) bool {
	return code == http.StatusTooManyRequests || code >= 500
}

func isTransientErrorMessage(msg string) bool {
	low := strings.ToLower(msg)
	return strings.Contains(low, "upstream") ||
		strings.Contains(low, "server error") ||
		strings.Contains(low, "service unavailable") ||
		strings.Contains(low, "overloaded") ||
		strings.Contains(low, "rate limit") ||
		strings.Contains(low, "temporarily unavailable") ||
		strings.Contains(low, "stream ended") ||
		strings.Contains(low, "connection reset") ||
		strings.Contains(low, "gateway") ||
		strings.Contains(low, "timeout")
}

type nonRetryable struct{ err error }

func (n nonRetryable) Error() string { return n.err.Error() }
func (n nonRetryable) Unwrap() error { return n.err }

func retryable(err error) bool {
	if err == nil || errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return false
	}
	var nr nonRetryable
	if errors.As(err, &nr) {
		return false
	}
	var he *HTTPError
	if errors.As(err, &he) {
		code, _ := strconv.Atoi(strings.Fields(he.Status)[0])
		return retryableStatus(code)
	}
	return true
}

func backoff(attempt int) time.Duration {
	return 1 * time.Second
}

var sleep = func(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

var contextLimitMarkers = []string{
	"context_length_exceeded",
	"maximum context length",
	"prompt_too_long",
	"prompt is too long",
	"context window",
	"model_context_window_exceeded",
}

func IsContextLimit(err error) bool {
	if err == nil {
		return false
	}
	var he *HTTPError
	if errors.As(err, &he) {
		if strings.HasPrefix(he.Status, "400") || strings.HasPrefix(he.Status, "413") {
			b := strings.ToLower(he.Body)
			for _, marker := range contextLimitMarkers {
				if strings.Contains(b, marker) {
					return true
				}
			}
		}
		return false
	}
	s := strings.ToLower(err.Error())
	for _, marker := range contextLimitMarkers {
		if strings.Contains(s, marker) {
			return true
		}
	}
	return false
}

type RequestAuthorizer interface {
	Authorize(*http.Request) error
}

func authenticatedProbe(ctx context.Context, client *http.Client, endpoint string, body []byte, setHeaders func(*http.Request) error) error {
	if client == nil {
		client = http.DefaultClient
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	if err := setHeaders(req); err != nil {
		return err
	}
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode >= http.StatusOK && resp.StatusCode < http.StatusMultipleChoices {
		return nil
	}
	body, _ = io.ReadAll(io.LimitReader(resp.Body, 4096))
	if !isAuthenticationError(body) {
		return nil
	}
	return &HTTPError{Status: resp.Status, Body: string(bytes.TrimSpace(body))}
}

func isAuthenticationError(body []byte) bool {
	var envelope struct {
		Error struct {
			Type string `json:"type"`
			Code any    `json:"code"`
		} `json:"error"`
		Type string `json:"type"`
	}
	if err := json.Unmarshal(body, &envelope); err != nil {
		return false
	}
	errType := strings.ToLower(strings.TrimSpace(envelope.Error.Type))
	if errType == "" {
		errType = strings.ToLower(strings.TrimSpace(envelope.Type))
	}
	switch errType {
	case "autherror", "authentication_error", "invalid_api_key", "unauthorized", "auth_error":
		return true
	}
	if code, ok := envelope.Error.Code.(string); ok && strings.ToLower(strings.TrimSpace(code)) == "invalid_api_key" {
		return true
	}
	return false
}

func scanSSE(r io.Reader, maxLine int, handle func(string, []byte) error, malformed func(string) error) error {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 64*1024), maxLine)
	var data []string
	eventName := ""
	dispatch := func() error {
		if len(data) == 0 {
			eventName = ""
			return nil
		}
		payload := strings.Join(data, "\n")
		name := eventName
		data = nil
		eventName = ""
		return handle(name, []byte(payload))
	}
	for sc.Scan() {
		line := strings.TrimSuffix(sc.Text(), "\r")
		if line == "" {
			if err := dispatch(); err != nil {
				return err
			}
			continue
		}
		if strings.HasPrefix(line, ":") {
			continue
		}
		field, value, ok := strings.Cut(line, ":")
		if !ok {
			if malformed != nil {
				return malformed(line)
			}
			return fmt.Errorf("malformed SSE line %q", line)
		}
		value = strings.TrimPrefix(value, " ")
		switch field {
		case "event":
			eventName = value
		case "data":
			data = append(data, value)
		}
	}
	if err := sc.Err(); err != nil {
		return err
	}
	return dispatch()
}

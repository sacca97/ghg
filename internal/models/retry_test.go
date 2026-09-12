package models

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// noSleep swaps the backoff sleep for a no-op so retry tests run instantly.
func noSleep(t *testing.T) {
	t.Helper()
	orig := sleep
	sleep = func(context.Context, time.Duration) error { return nil }
	t.Cleanup(func() { sleep = orig })
}

func retryClient(t *testing.T, transport http.RoundTripper) *Client {
	t.Helper()
	c := testChatClient(t, "http://provider.test", "k")
	c.HTTP = &http.Client{Transport: transport}
	return c
}

func TestRetryableClassification(t *testing.T) {
	cases := []struct {
		err  error
		want bool
	}{
		{nil, false},
		{context.Canceled, false},
		{context.DeadlineExceeded, false},
		{&HTTPError{Status: "400 Bad Request", Body: "bad"}, false},
		{&HTTPError{Status: "401 Unauthorized", Body: "nope"}, false},
		{&HTTPError{Status: "403 Forbidden", Body: "nope"}, false},
		{&HTTPError{Status: "429 Too Many Requests", Body: "slow down"}, true},
		{&HTTPError{Status: "500 Internal Server Error", Body: "boom"}, true},
		{&HTTPError{Status: "524", Body: "origin timeout"}, true},
		{errors.New("dial tcp: connection refused"), true},
		{io.ErrUnexpectedEOF, true},
	}
	for _, c := range cases {
		if got := retryable(c.err); got != c.want {
			t.Errorf("retryable(%v) = %v, want %v", c.err, got, c.want)
		}
	}
}

// A transient 524 then a good stream must succeed without the caller seeing
// the failure (and the OnRetry hook must fire with a delay).
func TestStreamRetriesTransientStatus(t *testing.T) {
	noSleep(t)
	var calls atomic.Int32
	c := retryClient(t, testRoundTripper(func(*http.Request) (*http.Response, error) {
		if calls.Add(1) < 3 {
			return testHTTPResponse(524, "error code: 524"), nil
		}
		resp := testHTTPResponse(http.StatusOK, "data: {\"choices\":[{\"delta\":{\"content\":\"ok\"}}]}\n\ndata: [DONE]\n\n")
		resp.Header.Set("Content-Type", "text/event-stream")
		return resp, nil
	}))
	var retries []RetryEvent

	msg, _, err := runStreamWithRetry(c, context.Background(), Request{Model: "m"}, func(ev RetryEvent) { retries = append(retries, ev) })
	if err != nil {
		t.Fatal(err)
	}
	if msg.Content != "ok" {
		t.Fatalf("content: %q", msg.Content)
	}
	if calls.Load() != 3 {
		t.Fatalf("attempts: %d, want 3", calls.Load())
	}
	if len(retries) != 2 {
		t.Fatalf("OnRetry fired %d times, want 2", len(retries))
	}
	if retries[0].Attempt != 1 || retries[0].Err == nil {
		t.Fatalf("retry event: %+v", retries[0])
	}
}

type errorAfterBody struct {
	data []byte
	err  error
}

func (r *errorAfterBody) Read(p []byte) (int, error) {
	if len(r.data) == 0 {
		return 0, r.err
	}
	n := copy(p, r.data)
	r.data = r.data[n:]
	return n, nil
}

func (r *errorAfterBody) Close() error { return nil }

const http2GoAwayError = `http2: server sent GOAWAY and closed the connection; LastStreamID=27, ErrCode=NO_ERROR, debug=""`
const http2InternalStreamError = `stream error: stream ID 25; INTERNAL_ERROR; received from peer`

func TestStreamRetriesHTTP2GoAwayAfterReasoning(t *testing.T) {
	noSleep(t)
	var calls atomic.Int32
	client := testChatClient(t, "http://provider.test", "k")
	client.HTTP = &http.Client{Transport: testRoundTripper(func(*http.Request) (*http.Response, error) {
		if calls.Add(1) == 1 {
			return &http.Response{
				StatusCode: http.StatusOK,
				Header:     make(http.Header),
				Body: &errorAfterBody{
					data: []byte("data: {\"choices\":[{\"delta\":{\"reasoning_content\":\"partial reasoning\"}}]}\n\n"),
					err:  errors.New(http2GoAwayError),
				},
			}, nil
		}
		return &http.Response{
			StatusCode: http.StatusOK,
			Header:     make(http.Header),
			Body:       io.NopCloser(strings.NewReader("data: {\"choices\":[{\"delta\":{\"content\":\"recovered\"}}]}\n\ndata: [DONE]\n\n")),
		}, nil
	})}

	var thinking, text strings.Builder
	msg, _, err := client.Stream(context.Background(), Request{Model: "m"}, EventSink{
		OnThink: func(delta string) { thinking.WriteString(delta) },
		OnText:  func(delta string) { text.WriteString(delta) },
	})
	if err != nil {
		t.Fatal(err)
	}
	if got := thinking.String(); got != "partial reasoning" {
		t.Fatalf("thinking = %q, want partial reasoning", got)
	}
	if got := text.String(); got != "recovered" || msg.Content != got {
		t.Fatalf("text = %q, message content = %q, want recovered", got, msg.Content)
	}
	if got := calls.Load(); got != 2 {
		t.Fatalf("attempts = %d, want 2", got)
	}
}

func TestStreamRetriesHTTP2InternalErrorAfterReasoning(t *testing.T) {
	noSleep(t)
	var calls atomic.Int32
	client := testChatClient(t, "http://provider.test", "k")
	client.HTTP = &http.Client{Transport: testRoundTripper(func(*http.Request) (*http.Response, error) {
		if calls.Add(1) == 1 {
			return &http.Response{
				StatusCode: http.StatusOK,
				Header:     make(http.Header),
				Body: &errorAfterBody{
					data: []byte("data: {\"choices\":[{\"delta\":{\"reasoning_content\":\"partial reasoning\"}}]}\n\n"),
					err:  errors.New(http2InternalStreamError),
				},
			}, nil
		}
		return &http.Response{
			StatusCode: http.StatusOK,
			Header:     make(http.Header),
			Body:       io.NopCloser(strings.NewReader("data: {\"choices\":[{\"delta\":{\"content\":\"recovered\"}}]}\n\ndata: [DONE]\n\n")),
		}, nil
	})}

	var thinking, text strings.Builder
	msg, _, err := client.Stream(context.Background(), Request{Model: "m"}, EventSink{
		OnThink: func(delta string) { thinking.WriteString(delta) },
		OnText:  func(delta string) { text.WriteString(delta) },
	})
	if err != nil {
		t.Fatal(err)
	}
	if got := thinking.String(); got != "partial reasoning" {
		t.Fatalf("thinking = %q, want partial reasoning", got)
	}
	if got := text.String(); got != "recovered" || msg.Content != got {
		t.Fatalf("text = %q, message content = %q, want recovered", got, msg.Content)
	}
	if got := calls.Load(); got != 2 {
		t.Fatalf("attempts = %d, want 2", got)
	}
}

func TestStreamRetriesHTTP2GoAwayPastConfiguredBudget(t *testing.T) {
	noSleep(t)
	var calls atomic.Int32
	client := testChatClient(t, "http://provider.test", "k")
	client.HTTP = &http.Client{Transport: testRoundTripper(func(*http.Request) (*http.Response, error) {
		if calls.Add(1) <= DefaultMaxAttempts {
			return nil, errors.New(http2GoAwayError)
		}
		return &http.Response{
			StatusCode: http.StatusOK,
			Header:     make(http.Header),
			Body:       io.NopCloser(strings.NewReader("data: {\"choices\":[{\"delta\":{\"content\":\"recovered\"}}]}\n\ndata: [DONE]\n\n")),
		}, nil
	})}

	msg, _, err := client.Stream(context.Background(), Request{Model: "m"}, EventSink{})
	if err != nil {
		t.Fatal(err)
	}
	if msg.Content != "recovered" {
		t.Fatalf("content = %q, want recovered", msg.Content)
	}
	if got := calls.Load(); got != DefaultMaxAttempts+1 {
		t.Fatalf("attempts = %d, want %d", got, DefaultMaxAttempts+1)
	}
}

// Context-limit errors must NOT be retried — the agent's compaction path
// depends on seeing them immediately.
func TestStreamDoesNotRetryContextLimit(t *testing.T) {
	noSleep(t)
	var calls atomic.Int32
	c := retryClient(t, testRoundTripper(func(*http.Request) (*http.Response, error) {
		calls.Add(1)
		return testHTTPResponse(http.StatusBadRequest, `{"error":{"code":"context_length_exceeded"}}`), nil
	}))

	_, _, err := runStream(c, context.Background(), Request{Model: "m"}, nil, nil)
	if !IsContextLimit(err) {
		t.Fatalf("expected context-limit error, got %v", err)
	}
	if calls.Load() != 1 {
		t.Fatalf("attempts: %d, want 1", calls.Load())
	}
}

// Once visible text has streamed, a mid-stream failure must surface rather
// than retry — a retry would replay the already-rendered text.
func TestStreamDoesNotRetryAfterEmission(t *testing.T) {
	noSleep(t)
	c := retryClient(t, testRoundTripper(func(*http.Request) (*http.Response, error) {
		return &http.Response{
			StatusCode: http.StatusOK,
			Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
			Body: &errorAfterBody{
				data: []byte("data: {\"choices\":[{\"delta\":{\"content\":\"partial\"}}]}\n\ndata: {\"error\":{\"message\":\"stream died\"}}\n\n"),
				err:  io.ErrUnexpectedEOF,
			},
		}, nil
	}))

	var streamed strings.Builder
	var retried int
	_, _, err := c.Stream(context.Background(), Request{Model: "m"}, EventSink{
		OnText:  func(d string) { streamed.WriteString(d) },
		OnRetry: func(RetryEvent) { retried++ },
	})
	if err == nil {
		t.Fatal("expected error")
	}
	if retried != 0 {
		t.Fatalf("retried %d times after emission, want 0", retried)
	}
	if streamed.String() != "partial" {
		t.Fatalf("streamed: %q", streamed.String())
	}
}

// MaxRetries overrides the default budget; 1 means a single attempt.
func TestMaxRetriesConfigurable(t *testing.T) {
	noSleep(t)
	var calls atomic.Int32
	c := retryClient(t, testRoundTripper(func(*http.Request) (*http.Response, error) {
		calls.Add(1)
		return testHTTPResponse(http.StatusInternalServerError, ""), nil
	}))
	c.MaxRetries = 2
	if _, _, err := runStream(c, context.Background(), Request{Model: "m"}, nil, nil); err == nil {
		t.Fatal("expected error")
	}
	if calls.Load() != 2 {
		t.Fatalf("attempts: %d, want 2 (MaxRetries=2)", calls.Load())
	}

	calls.Store(0)
	c.MaxRetries = 1
	if _, _, err := runStream(c, context.Background(), Request{Model: "m"}, nil, nil); err == nil {
		t.Fatal("expected error")
	}
	if calls.Load() != 1 {
		t.Fatalf("attempts: %d, want 1 (MaxRetries=1 disables retries)", calls.Load())
	}
}

// Caller cancellation during backoff must abort the retry loop promptly.
func TestRetryRespectsCancellation(t *testing.T) {
	c := retryClient(t, testRoundTripper(func(*http.Request) (*http.Response, error) {
		return testHTTPResponse(http.StatusInternalServerError, ""), nil
	}))

	ctx, cancel := context.WithCancel(context.Background())
	go func() { time.Sleep(50 * time.Millisecond); cancel() }()

	start := time.Now()
	_, _, err := runStream(c, ctx, Request{Model: "m"}, nil, nil)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("expected context.Canceled, got %v", err)
	}
	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Fatalf("cancellation took %v; backoff should have been interrupted", elapsed)
	}
}

func TestStreamRetriesUpstreamStreamEndedError(t *testing.T) {
	noSleep(t)
	var calls atomic.Int32
	c := retryClient(t, testRoundTripper(func(*http.Request) (*http.Response, error) {
		if calls.Add(1) < 3 {
			resp := testHTTPResponse(http.StatusOK, "data: {\"error\":{\"message\":\"upstream stream ended before terminal chunk\"}}\n\n")
			resp.Header.Set("Content-Type", "text/event-stream")
			return resp, nil
		}
		resp := testHTTPResponse(http.StatusOK, "data: {\"choices\":[{\"delta\":{\"content\":\"recovered\"}}]}\n\ndata: [DONE]\n\n")
		resp.Header.Set("Content-Type", "text/event-stream")
		return resp, nil
	}))
	var retried int
	retry := func(ev RetryEvent) {
		retried++
		if ev.Delay != 1*time.Second {
			t.Errorf("delay = %v, want 1s", ev.Delay)
		}
	}

	msg, _, err := runStreamWithRetry(c, context.Background(), Request{Model: "m"}, retry)
	if err != nil {
		t.Fatal(err)
	}
	if msg.Content != "recovered" {
		t.Fatalf("content: %q, want 'recovered'", msg.Content)
	}
	if calls.Load() != 3 {
		t.Fatalf("attempts = %d, want 3", calls.Load())
	}
	if retried != 2 {
		t.Fatalf("retried %d times, want 2", retried)
	}
}

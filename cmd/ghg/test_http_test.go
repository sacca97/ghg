package main

import (
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
)

// testEndpoint routes HTTP clients through an in-memory RoundTripper.
type testEndpoint struct {
	URL  string
	once sync.Once
}

var testHTTPState struct {
	sync.Mutex
	handlers map[string]http.Handler
	previous http.RoundTripper
	active   int
}
var testHTTPSeq uint64

func testEndpointFor(t *testing.T, handler http.Handler) *testEndpoint {
	t.Helper()
	id := atomic.AddUint64(&testHTTPSeq, 1)
	s := &testEndpoint{URL: fmt.Sprintf("https://ghg-test-%d.invalid", id)}
	testHTTPState.Lock()
	if testHTTPState.active == 0 {
		testHTTPState.previous = http.DefaultTransport
		http.DefaultTransport = testRoundTripper{}
	}
	testHTTPState.active++
	if testHTTPState.handlers == nil {
		testHTTPState.handlers = make(map[string]http.Handler)
	}
	testHTTPState.handlers[s.URL] = handler
	testHTTPState.Unlock()
	t.Cleanup(s.Close)
	return s
}

func (s *testEndpoint) Close() {
	if s == nil {
		return
	}
	s.once.Do(func() {
		testHTTPState.Lock()
		delete(testHTTPState.handlers, s.URL)
		testHTTPState.active--
		if testHTTPState.active == 0 {
			http.DefaultTransport = testHTTPState.previous
			testHTTPState.previous = nil
		}
		testHTTPState.Unlock()
	})
}

type testRoundTripper struct{}

func (testRoundTripper) RoundTrip(req *http.Request) (*http.Response, error) {
	testHTTPState.Lock()
	handler := testHTTPState.handlers[req.URL.Scheme+"://"+req.URL.Host]
	fallback := testHTTPState.previous
	testHTTPState.Unlock()
	if handler == nil {
		if fallback == nil {
			return nil, errors.New("test endpoint is closed or unknown")
		}
		return fallback.RoundTrip(req)
	}
	response := make(chan *http.Response, 1)
	go func() {
		recorder := httptest.NewRecorder()
		handler.ServeHTTP(recorder, req)
		response <- recorder.Result()
	}()
	select {
	case result := <-response:
		return result, nil
	case <-req.Context().Done():
		return nil, req.Context().Err()
	}
}

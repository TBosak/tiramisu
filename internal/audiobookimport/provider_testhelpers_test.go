package audiobookimport

import (
	"io"
	"net/http"
	"net/url"
	"sync"
	"testing"
	"time"
)

// boundedWait is the fixed upper bound every cross-goroutine synchronization
// point in this package's tests waits before failing instead of hanging. It
// exists only as a backstop against code under test - including a zero-value
// production stub - that never performs the interaction a test is waiting to
// observe (e.g. a client that never issues the HTTP request it was asked to
// make). It is never used to pace, sleep, or poll: every wait below is a
// single select on the real signal plus this one timeout arm, so a passing
// implementation returns as soon as the real signal arrives and only a
// genuinely stuck implementation ever waits the full duration.
const boundedWait = 2 * time.Second

// awaitSignal receives once from ch, or fails the calling test the moment
// boundedWait elapses, so a test can never hang indefinitely waiting for a
// signal (an observed request, a completed call) that the code under test
// never produces. It must be called from the test's own goroutine, not a
// spawned one, per testing.T's rules for FailNow.
func awaitSignal[T any](t *testing.T, ch <-chan T, msg string) T {
	t.Helper()
	select {
	case v := <-ch:
		return v
	case <-time.After(boundedWait):
		t.Fatalf("%s: timed out after %s waiting for a signal that never arrived", msg, boundedWait)
		var zero T
		return zero
	}
}

// recordedRequest captures everything a test needs to assert about one HTTP
// request the client under test sent, without giving the test access to the
// client's internals.
type recordedRequest struct {
	Method string
	Path   string
	// EscapedPath preserves percent-encoding exactly as the client sent it,
	// unlike Path (which net/http decodes), so tests can distinguish a
	// literal "/" in an id from an encoded "%2F".
	EscapedPath string
	RawQuery    string
	Query       url.Values
	Header      http.Header
}

// requestRecorder is an in-process fake that records every request an
// httptest.Server receives, so tests can assert request shape (method, path,
// query, headers) without a live network call.
type requestRecorder struct {
	mu       sync.Mutex
	requests []recordedRequest
}

func (r *requestRecorder) record(req *http.Request) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.requests = append(r.requests, recordedRequest{
		Method:      req.Method,
		Path:        req.URL.Path,
		EscapedPath: req.URL.EscapedPath(),
		RawQuery:    req.URL.RawQuery,
		Query:       req.URL.Query(),
		Header:      req.Header.Clone(),
	})
}

func (r *requestRecorder) count() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.requests)
}

func (r *requestRecorder) all() []recordedRequest {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]recordedRequest, len(r.requests))
	copy(out, r.requests)
	return out
}

func (r *requestRecorder) last() recordedRequest {
	r.mu.Lock()
	defer r.mu.Unlock()
	if len(r.requests) == 0 {
		return recordedRequest{}
	}
	return r.requests[len(r.requests)-1]
}

// countingBody wraps a response body so bodyCloseTracker can prove every
// response body was closed, on every status path, without altering the
// bytes a caller reads.
type countingBody struct {
	io.ReadCloser
	tracker *bodyCloseTracker
}

func (b countingBody) Close() error {
	b.tracker.mu.Lock()
	b.tracker.closes++
	b.tracker.mu.Unlock()
	return b.ReadCloser.Close()
}

// bodyCloseTracker wraps an http.RoundTripper so tests can assert response
// bodies are always closed and, separately, that requests actually flow
// through the *http.Client the test injected (I1) rather than some client
// the production code builds for itself.
type bodyCloseTracker struct {
	next http.RoundTripper

	mu       sync.Mutex
	requests int
	closes   int
}

func (t *bodyCloseTracker) RoundTrip(req *http.Request) (*http.Response, error) {
	t.mu.Lock()
	t.requests++
	t.mu.Unlock()
	resp, err := t.next.RoundTrip(req)
	if err != nil || resp == nil {
		return resp, err
	}
	resp.Body = countingBody{ReadCloser: resp.Body, tracker: t}
	return resp, nil
}

func (t *bodyCloseTracker) requestCount() int {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.requests
}

func (t *bodyCloseTracker) closeCount() int {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.closes
}

// trackedClient returns an *http.Client whose requests are counted and whose
// response bodies are tracked for Close calls. Redirects are NOT auto-followed
// by default (CheckRedirect returns ErrUseLastResponse), matching what a
// careful HTTP client author would hand to code that must inspect redirects
// itself (E2) - tests that want stdlib auto-follow behavior construct their
// own client instead.
func trackedClient() (*http.Client, *bodyCloseTracker) {
	tracker := &bodyCloseTracker{next: http.DefaultTransport}
	return &http.Client{
		Transport: tracker,
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}, tracker
}

// slowBody lets a handler release response bytes in controlled steps, driven
// by a channel rather than time.Sleep, so cancellation-during-body-read tests
// are deterministic.
type stepWriter struct {
	w      http.ResponseWriter
	flush  func()
	signal <-chan struct{}
}

func newStepWriter(w http.ResponseWriter, signal <-chan struct{}) stepWriter {
	f, _ := w.(http.Flusher)
	var flush func()
	if f != nil {
		flush = f.Flush
	} else {
		flush = func() {}
	}
	return stepWriter{w: w, flush: flush, signal: signal}
}

// write flushes chunk to the client immediately without waiting on the
// signal channel, so a caller can write, then signal an observer that bytes
// are on the wire, then block - giving a precise happens-before edge between
// "the client has data to read" and "the test may act on it".
func (s stepWriter) write(chunk string) {
	_, _ = s.w.Write([]byte(chunk))
	s.flush()
}

func (s stepWriter) writeAndWait(chunk string) {
	s.write(chunk)
	<-s.signal
}

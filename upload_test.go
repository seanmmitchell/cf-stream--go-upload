package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/eventials/go-tus"
	"github.com/eventials/go-tus/memorystore"
)

var fastRetry = retryPolicy{maxRetries: 3, baseDelay: time.Millisecond, maxDelay: time.Millisecond}

// patchOutcome says how fakeTUS answers a PATCH. The zero value stores the
// chunk and replies 204.
type patchOutcome struct {
	discard bool // Drop the chunk instead of storing it.
	status  int  // Reply with this status instead of 204.
	hangup  bool // Close the connection instead of replying.
}

// fakeTUS is a minimal TUS server for one upload.
type fakeTUS struct {
	mu      sync.Mutex
	data    []byte
	posts   int
	patches int
	heads   int
	patch   func(n int) patchOutcome // Picks the outcome of PATCH number n (from 1). Nil means always succeed.
	post    func(n int) int          // Status to fail POST number n with, or 0 to succeed. Nil means always succeed.
	head    func(n int) int          // Status to fail HEAD number n with, or 0 to succeed. Nil means always succeed.
}

func (s *fakeTUS) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	defer s.mu.Unlock()

	switch r.Method {
	case http.MethodPost:
		s.posts++
		if s.post != nil && s.post(s.posts) != 0 {
			w.WriteHeader(s.post(s.posts))
			return
		}
		w.Header().Set("Location", "/files/1")
		w.WriteHeader(http.StatusCreated)
	case http.MethodHead:
		s.heads++
		if s.head != nil && s.head(s.heads) != 0 {
			w.WriteHeader(s.head(s.heads))
			return
		}
		w.Header().Set("Upload-Offset", strconv.Itoa(len(s.data)))
		w.WriteHeader(http.StatusOK)
	case http.MethodPatch:
		s.patches++
		body, _ := io.ReadAll(r.Body)
		if r.Header.Get("Upload-Offset") != strconv.Itoa(len(s.data)) {
			w.WriteHeader(http.StatusConflict)
			return
		}

		var out patchOutcome
		if s.patch != nil {
			out = s.patch(s.patches)
		}
		if !out.discard {
			s.data = append(s.data, body...)
		}
		switch {
		case out.hangup:
			conn, _, _ := w.(http.Hijacker).Hijack()
			conn.Close()
		case out.status != 0:
			w.WriteHeader(out.status)
		default:
			w.Header().Set("Upload-Offset", strconv.Itoa(len(s.data)))
			w.WriteHeader(http.StatusNoContent)
		}
	}
}

// newTestUpload returns a client for srv that sends content in 4-byte chunks.
func newTestUpload(t *testing.T, srv *fakeTUS, content []byte) (*tus.Client, *tus.Upload) {
	t.Helper()
	ts := httptest.NewServer(srv)
	t.Cleanup(ts.Close)

	store, _ := memorystore.NewMemoryStore()
	client, err := tus.NewClient(ts.URL+"/files", &tus.Config{ChunkSize: 4, Resume: true, Store: store, HttpClient: newHTTPClient(context.Background())})
	if err != nil {
		t.Fatal(err)
	}
	return client, tus.NewUpload(bytes.NewReader(content), int64(len(content)), nil, "fingerprint")
}

// runUpload uploads content to srv with fastRetry and returns every progress
// report and uploadFile's error.
func runUpload(t *testing.T, srv *fakeTUS, content []byte) ([]progress, error) {
	t.Helper()
	client, upload := newTestUpload(t, srv, content)
	var reports []progress
	err := uploadFile(context.Background(), client, upload, fastRetry, func(p progress) { reports = append(reports, p) })
	return reports, err
}

func TestUploadFileReturnsWhenComplete(t *testing.T) {
	content := []byte("0123456789")
	srv := &fakeTUS{}
	reports, err := runUpload(t, srv, content)

	if err != nil {
		t.Fatalf("uploadFile: %v", err)
	}
	if !bytes.Equal(srv.data, content) {
		t.Errorf("server has %q, want %q", srv.data, content)
	}
	if srv.patches != 3 {
		t.Errorf("sent %d PATCH requests, want 3", srv.patches)
	}
	if last := reports[len(reports)-1]; last.offset != int64(len(content)) || last.err != nil {
		t.Errorf("last report = %+v, want offset %d and no error", last, len(content))
	}
}

func TestUploadFileResyncsAfterLostResponse(t *testing.T) {
	content := []byte("0123456789")
	// The server stores the second chunk but the response never arrives.
	srv := &fakeTUS{patch: func(n int) patchOutcome { return patchOutcome{hangup: n == 2} }}
	_, err := runUpload(t, srv, content)

	if err != nil {
		t.Fatalf("uploadFile: %v", err)
	}
	if !bytes.Equal(srv.data, content) {
		t.Errorf("server has %q, want %q", srv.data, content)
	}
	// Without the HEAD resync, the resent second chunk would get 409 forever.
	if srv.heads != 1 || srv.patches != 3 {
		t.Errorf("sent %d HEAD and %d PATCH requests, want 1 and 3", srv.heads, srv.patches)
	}
}

func TestUploadFileResyncsAfterOffsetMismatch(t *testing.T) {
	content := []byte("0123456789")
	// The server already has the first chunk, so the first PATCH gets 409.
	srv := &fakeTUS{data: []byte("0123")}
	reports, err := runUpload(t, srv, content)

	if err != nil {
		t.Fatalf("uploadFile: %v", err)
	}
	if !bytes.Equal(srv.data, content) {
		t.Errorf("server has %q, want %q", srv.data, content)
	}
	if !errors.Is(reports[1].err, tus.ErrOffsetMismatch) {
		t.Errorf("second report err = %v, want %v", reports[1].err, tus.ErrOffsetMismatch)
	}
}

func TestUploadFileRetriesServerErrors(t *testing.T) {
	content := []byte("0123456789")
	srv := &fakeTUS{patch: func(n int) patchOutcome {
		if n <= 2 {
			return patchOutcome{discard: true, status: http.StatusServiceUnavailable}
		}
		return patchOutcome{}
	}}
	reports, err := runUpload(t, srv, content)

	if err != nil {
		t.Fatalf("uploadFile: %v", err)
	}
	if !bytes.Equal(srv.data, content) {
		t.Errorf("server has %q, want %q", srv.data, content)
	}
	var attempts []int
	for i, p := range reports {
		if p.retryIn > 0 {
			attempts = append(attempts, p.attempt)
			// Once the wait is over, the countdown is cleared while the resync runs.
			if next := reports[i+1]; next.retryIn != 0 || next.attempt != p.attempt {
				t.Errorf("report after waiting = %+v, want attempt %d with no countdown", next, p.attempt)
			}
		}
	}
	if len(attempts) != 2 || attempts[0] != 1 || attempts[1] != 2 {
		t.Errorf("retry attempts reported = %v, want [1 2]", attempts)
	}
}

func TestUploadFileStopsOnPermanentError(t *testing.T) {
	srv := &fakeTUS{patch: func(int) patchOutcome {
		return patchOutcome{discard: true, status: http.StatusUnauthorized}
	}}
	_, err := runUpload(t, srv, []byte("0123456789"))

	var clientErr tus.ClientError
	if !errors.As(err, &clientErr) || clientErr.Code != http.StatusUnauthorized {
		t.Fatalf("uploadFile err = %v, want status 401", err)
	}
	if srv.patches != 1 || srv.heads != 0 {
		t.Errorf("sent %d PATCH and %d HEAD requests, want 1 and 0", srv.patches, srv.heads)
	}
}

func TestUploadFileGivesUpAfterMaxRetries(t *testing.T) {
	srv := &fakeTUS{patch: func(int) patchOutcome {
		return patchOutcome{discard: true, status: http.StatusServiceUnavailable}
	}}
	_, err := runUpload(t, srv, []byte("0123456789"))

	if err == nil || !strings.Contains(err.Error(), "gave up after 3 retries") {
		t.Fatalf("uploadFile err = %v, want it to give up after 3 retries", err)
	}
	if srv.patches != fastRetry.maxRetries+1 {
		t.Errorf("sent %d PATCH requests, want %d", srv.patches, fastRetry.maxRetries+1)
	}
}

func TestUploadFileStopsWhenCancelled(t *testing.T) {
	srv := &fakeTUS{patch: func(int) patchOutcome {
		return patchOutcome{discard: true, status: http.StatusServiceUnavailable}
	}}
	client, upload := newTestUpload(t, srv, []byte("0123456789"))
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	slow := retryPolicy{maxRetries: 3, baseDelay: time.Hour, maxDelay: time.Hour}

	err := uploadFile(ctx, client, upload, slow, func(p progress) {
		if p.retryIn > 0 {
			cancel()
		}
	})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("uploadFile err = %v, want %v", err, context.Canceled)
	}
}

func TestUploadFileStopsWhenCancelledBetweenChunks(t *testing.T) {
	srv := &fakeTUS{}
	client, upload := newTestUpload(t, srv, []byte("0123456789"))
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	err := uploadFile(ctx, client, upload, fastRetry, func(p progress) {
		if p.offset == 4 {
			cancel()
		}
	})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("uploadFile err = %v, want %v", err, context.Canceled)
	}
	if srv.patches != 1 {
		t.Errorf("sent %d PATCH requests, want 1", srv.patches)
	}
}

func TestUploadFileRetriesWhenOffsetDoesNotAdvance(t *testing.T) {
	// The server answers 204 but keeps none of the chunk.
	srv := &fakeTUS{patch: func(int) patchOutcome { return patchOutcome{discard: true} }}
	_, err := runUpload(t, srv, []byte("0123456789"))

	if !errors.Is(err, errNoProgress) || !strings.Contains(err.Error(), "gave up after 3 retries") {
		t.Fatalf("uploadFile err = %v, want it to give up with %v", err, errNoProgress)
	}
	if srv.patches != fastRetry.maxRetries+1 {
		t.Errorf("sent %d PATCH requests, want %d", srv.patches, fastRetry.maxRetries+1)
	}
}

func TestUploadFileRetriesCreate(t *testing.T) {
	content := []byte("0123456789")
	srv := &fakeTUS{post: func(n int) int {
		if n <= 2 {
			return http.StatusServiceUnavailable
		}
		return 0
	}}
	_, err := runUpload(t, srv, content)

	if err != nil {
		t.Fatalf("uploadFile: %v", err)
	}
	if !bytes.Equal(srv.data, content) || srv.posts != 3 {
		t.Errorf("server has %q after %d POST requests, want %q after 3", srv.data, srv.posts, content)
	}
}

func TestUploadFileRetriesFailedResync(t *testing.T) {
	content := []byte("0123456789")
	srv := &fakeTUS{
		patch: func(n int) patchOutcome { return patchOutcome{hangup: n == 2} },
		head: func(n int) int {
			if n == 1 {
				return http.StatusServiceUnavailable
			}
			return 0
		},
	}
	_, err := runUpload(t, srv, content)

	if err != nil {
		t.Fatalf("uploadFile: %v", err)
	}
	if !bytes.Equal(srv.data, content) || srv.heads != 2 {
		t.Errorf("server has %q after %d HEAD requests, want %q after 2", srv.data, srv.heads, content)
	}
}

func TestUploadFileClearsCountdownBeforeResync(t *testing.T) {
	var countdown atomic.Bool // Whether the UI was last told to show "Retrying in Ns".
	srv := &fakeTUS{
		patch: func(n int) patchOutcome { return patchOutcome{hangup: n == 2} },
		head: func(int) int {
			if countdown.Load() {
				t.Error("resync HEAD sent while the UI still shows the retry countdown")
			}
			return 0
		},
	}
	client, upload := newTestUpload(t, srv, []byte("0123456789"))
	err := uploadFile(context.Background(), client, upload, fastRetry, func(p progress) { countdown.Store(p.retryIn > 0) })

	if err != nil || srv.heads != 1 {
		t.Fatalf("uploadFile = %v after %d HEAD requests, want nil after 1", err, srv.heads)
	}
}

func TestUploadFileStopsWhenResyncFindsNoUpload(t *testing.T) {
	srv := &fakeTUS{
		patch: func(n int) patchOutcome { return patchOutcome{hangup: n == 2} },
		head:  func(int) int { return http.StatusNotFound },
	}
	_, err := runUpload(t, srv, []byte("0123456789"))

	if !errors.Is(err, tus.ErrUploadNotFound) {
		t.Fatalf("uploadFile err = %v, want %v", err, tus.ErrUploadNotFound)
	}
	if srv.heads != 1 {
		t.Errorf("sent %d HEAD requests, want 1", srv.heads)
	}
}

func TestUploadFileCountsResyncFailuresTowardsRetries(t *testing.T) {
	srv := &fakeTUS{
		patch: func(n int) patchOutcome { return patchOutcome{hangup: n == 2} },
		head:  func(int) int { return http.StatusServiceUnavailable },
	}
	_, err := runUpload(t, srv, []byte("0123456789"))

	if err == nil || !strings.Contains(err.Error(), "gave up after 3 retries") {
		t.Fatalf("uploadFile err = %v, want it to give up after 3 retries", err)
	}
	// One failed PATCH and two failed HEADs use 3 retries; the third HEAD failure is one too many.
	if srv.patches != 2 || srv.heads != 3 {
		t.Errorf("sent %d PATCH and %d HEAD requests, want 2 and 3", srv.patches, srv.heads)
	}
}

func TestRetryableRejectsUntrustedCertificate(t *testing.T) {
	ts := httptest.NewTLSServer(http.NotFoundHandler())
	defer ts.Close()

	_, err := http.Get(ts.URL)
	if err == nil || retryable(err) {
		t.Fatalf("retryable(%v) = true, want false", err)
	}
}

func TestRetryable(t *testing.T) {
	tests := []struct {
		err  error
		want bool
	}{
		{tus.ErrOffsetMismatch, true},
		{tus.ClientError{Code: http.StatusInternalServerError}, true},
		{tus.ClientError{Code: http.StatusServiceUnavailable}, true},
		{tus.ClientError{Code: http.StatusRequestTimeout}, true},
		{tus.ClientError{Code: http.StatusTooManyRequests}, true},
		{tus.ClientError{Code: http.StatusLocked}, true},
		{errNoProgress, true},
		{&url.Error{Op: "Patch", URL: "https://example.com", Err: io.ErrUnexpectedEOF}, true},
		{&url.Error{Op: "Patch", URL: "https://example.com", Err: &net.OpError{Op: "write", Net: "tcp", Err: syscall.ECONNRESET}}, true},
		{&url.Error{Op: "Patch", URL: "https://example.com", Err: errStalled}, true},
		{&url.Error{Op: "parse", URL: "::", Err: errors.New("missing protocol scheme")}, false},
		{&url.Error{Op: "Post", URL: "https://example.com", Err: errors.New("Proxy Authentication Required")}, false},
		{&url.Error{Op: "Patch", URL: "https://example.com", Err: context.Canceled}, false},
		{tus.ClientError{Code: http.StatusUnauthorized}, false},
		{tus.ClientError{Code: http.StatusForbidden}, false},
		{tus.ErrLargeUpload, false},
		{tus.ErrVersionMismatch, false},
		{tus.ErrUploadNotFound, false},
		{io.EOF, false},
	}
	for _, tt := range tests {
		if got := retryable(tt.err); got != tt.want {
			t.Errorf("retryable(%v) = %v, want %v", tt.err, got, tt.want)
		}
	}
}

func TestRetryPolicyDelay(t *testing.T) {
	want := []time.Duration{1, 2, 4, 8, 16, 32, 60, 60}
	for i, w := range want {
		if got := defaultRetry.delay(i + 1); got != w*time.Second {
			t.Errorf("delay(%d) = %s, want %s", i+1, got, w*time.Second)
		}
	}
}

func TestDescribe(t *testing.T) {
	long := strings.Repeat("x", 400)
	tests := []struct {
		err  error
		want string
	}{
		{tus.ClientError{Code: 400, Body: []byte(" {\"errors\":[\"bad chunk size\"]}\n")}, `unexpected status code: 400: {"errors":["bad chunk size"]}`},
		{tus.ClientError{Code: 401}, "unexpected status code: 401"},
		{fmt.Errorf("gave up after 3 retries: %w", tus.ClientError{Code: 503, Body: []byte(long)}), "gave up after 3 retries: unexpected status code: 503: " + long[:300] + "..."},
		{io.EOF, "EOF"},
	}
	for _, tt := range tests {
		if got := describe(tt.err); got != tt.want {
			t.Errorf("describe(%v) = %q, want %q", tt.err, got, tt.want)
		}
	}
}

// roundTripFunc adapts a function to http.RoundTripper.
type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(req *http.Request) (*http.Response, error) { return f(req) }

func TestStallGuardFailsStalledBody(t *testing.T) {
	// The transport sends one byte of the body, then the connection black-holes.
	next := roundTripFunc(func(req *http.Request) (*http.Response, error) {
		req.Body.Read(make([]byte, 1))
		<-req.Context().Done()
		return nil, req.Context().Err()
	})
	client := &http.Client{Transport: stallGuard{ctx: context.Background(), idle: 20 * time.Millisecond, next: next}}

	_, err := client.Post("https://example.com", "application/offset+octet-stream", strings.NewReader("0123456789"))
	if !errors.Is(err, errStalled) || !retryable(err) {
		t.Fatalf("Post err = %v, want a retryable %v", err, errStalled)
	}
}

func TestStallGuardCancelsWithCtx(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	next := roundTripFunc(func(req *http.Request) (*http.Response, error) {
		cancel()
		<-req.Context().Done()
		return nil, req.Context().Err()
	})
	client := &http.Client{Transport: stallGuard{ctx: ctx, idle: time.Hour, next: next}}

	_, err := client.Post("https://example.com", "text/plain", strings.NewReader("x"))
	if !errors.Is(err, context.Canceled) || retryable(err) {
		t.Fatalf("Post err = %v, want a permanent %v", err, context.Canceled)
	}
}

func TestStallGuardKeepsReplyReadable(t *testing.T) {
	var reqCtx context.Context
	next := roundTripFunc(func(req *http.Request) (*http.Response, error) {
		reqCtx = req.Context()
		io.Copy(io.Discard, req.Body)
		req.Body.Close()
		return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader("ok")), Request: req}, nil
	})
	client := &http.Client{Transport: stallGuard{ctx: context.Background(), idle: 20 * time.Millisecond, next: next}}

	resp, err := client.Post("https://example.com", "text/plain", strings.NewReader("0123456789"))
	if err != nil {
		t.Fatal(err)
	}
	// Longer than idle: a body that was sent in full must not trip the guard.
	time.Sleep(50 * time.Millisecond)
	if body, err := io.ReadAll(resp.Body); err != nil || string(body) != "ok" || reqCtx.Err() != nil {
		t.Fatalf("reply = %q, %v (request ctx err %v), want \"ok\" with a live ctx", body, err, reqCtx.Err())
	}
	resp.Body.Close()
	if reqCtx.Err() == nil {
		t.Error("request ctx still live after the reply was closed")
	}
}

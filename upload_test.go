package main

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"sync"
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
	patches int
	heads   int
	patch   func(n int) patchOutcome // Picks the outcome of PATCH number n (from 1). Nil means always succeed.
}

func (s *fakeTUS) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	defer s.mu.Unlock()

	switch r.Method {
	case http.MethodPost:
		w.Header().Set("Location", "/files/1")
		w.WriteHeader(http.StatusCreated)
	case http.MethodHead:
		s.heads++
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
	client, err := tus.NewClient(ts.URL+"/files", &tus.Config{ChunkSize: 4, Resume: true, Store: store})
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
	for _, p := range reports {
		if p.retryIn > 0 {
			attempts = append(attempts, p.attempt)
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
		{&url.Error{Op: "Patch", URL: "https://example.com", Err: io.ErrUnexpectedEOF}, true},
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
	want := []time.Duration{1, 2, 4, 8, 16, 30, 30, 30}
	for i, w := range want {
		if got := defaultRetry.delay(i + 1); got != w*time.Second {
			t.Errorf("delay(%d) = %s, want %s", i+1, got, w*time.Second)
		}
	}
}

package main

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
	"unicode"

	"github.com/eventials/go-tus"
)

// progress is a snapshot of the upload, sent from the upload worker to the UI.
type progress struct {
	url     string        // Upload URL. Empty until the upload is created.
	offset  int64         // Bytes the server has confirmed.
	err     error         // Last failure. Nil once a chunk succeeds again.
	attempt int           // Consecutive failures so far.
	retryIn time.Duration // Wait before the next attempt. Zero while sending.
}

// retryPolicy bounds how long a failing upload keeps retrying.
type retryPolicy struct {
	maxRetries int           // Consecutive failures allowed before giving up.
	baseDelay  time.Duration // Wait after the first failure. Doubles each time.
	maxDelay   time.Duration // Cap on a single wait.
}

// defaultRetry rides out about 10 minutes of consecutive failures
// (1+2+4+8+16+32s, then 60s each), long enough for a router reboot or a short
// outage, since a failed upload cannot be resumed by a later run.
var defaultRetry = retryPolicy{maxRetries: 15, baseDelay: time.Second, maxDelay: time.Minute}

// delay returns the wait before retry number attempt (starting at 1).
func (r retryPolicy) delay(attempt int) time.Duration {
	d := r.baseDelay
	for i := 1; i < attempt && d < r.maxDelay; i++ {
		d *= 2
	}
	return min(d, r.maxDelay)
}

var (
	// errNoProgress means the server accepted a chunk without moving the offset.
	errNoProgress = errors.New("server did not advance the upload offset")
	// errStalled means a request body stopped being sent (see stallGuard).
	errStalled = errors.New("upload stalled: no data sent")
	// errBadOffset means the server reported an offset outside the file.
	errBadOffset = errors.New("server reported an impossible upload offset")
)

// checkOffset fails with errBadOffset unless 0 <= offset <= size. Without it,
// an offset past the end of the file would end the upload as if it were complete.
func checkOffset(offset, size int64) error {
	if offset < 0 || offset > size {
		return fmt.Errorf("%w: %d for a %d-byte file", errBadOffset, offset, size)
	}
	return nil
}

// retryable reports whether err may clear up on its own: a server error, rate
// limiting, a locked upload, an offset mismatch (which a resync fixes), or a
// transport failure (timeout, reset, DNS or dial error, connection closed
// early, stall). Everything else (bad token, upload too large, untrusted TLS
// certificate, proxy or URL errors, cancellation, local file errors) is permanent.
func retryable(err error) bool {
	if errors.Is(err, tus.ErrOffsetMismatch) || errors.Is(err, errNoProgress) {
		return true
	}
	var clientErr tus.ClientError
	if errors.As(err, &clientErr) {
		switch clientErr.Code {
		case http.StatusRequestTimeout, http.StatusLocked, http.StatusTooManyRequests:
			return true
		}
		return clientErr.Code >= 500
	}
	var certErr *tls.CertificateVerificationError
	if errors.As(err, &certErr) {
		return false
	}
	var urlErr *url.Error
	if !errors.As(err, &urlErr) {
		return false
	}
	var netErr net.Error
	return errors.As(urlErr.Err, &netErr) || errors.Is(urlErr.Err, io.EOF) ||
		errors.Is(urlErr.Err, io.ErrUnexpectedEOF) || errors.Is(urlErr.Err, errStalled)
}

// describe returns err's message, plus the start of the server's reply for a
// status-code error, since go-tus only says "unexpected status code: N".
func describe(err error) string {
	var clientErr tus.ClientError
	if !errors.As(err, &clientErr) {
		return err.Error()
	}
	body := strings.TrimSpace(string(clientErr.Body))
	if body == "" {
		return err.Error()
	}
	if len(body) > 300 {
		body = strings.ToValidUTF8(body[:300], "") + "..."
	}
	return fmt.Sprintf("%s: %s", err, body)
}

// printable makes text that may come from the server safe to show in a
// terminal: invalid UTF-8 and control characters (C0, DEL and C1, which can
// start escape sequences) are dropped, and line breaks and tabs become spaces.
func printable(s string) string {
	return strings.Map(func(r rune) rune {
		switch {
		case r == '\n' || r == '\r' || r == '\t':
			return ' '
		case unicode.IsControl(r):
			return -1
		}
		return r
	}, strings.ToValidUTF8(s, ""))
}

// newHTTPClient returns the client go-tus uses. stallGuard gives requests ctx
// and fails a stalled body, and ResponseHeaderTimeout bounds the wait for a
// reply; both are retried like any other network error. HTTP/1.1 only, so
// transport failures surface as the net errors retryable() checks for rather
// than HTTP/2 stream errors.
func newHTTPClient(ctx context.Context) *http.Client {
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.ResponseHeaderTimeout = 2 * time.Minute
	transport.ForceAttemptHTTP2 = false
	transport.TLSNextProto = map[string]func(string, *tls.Conn) http.RoundTripper{}
	return &http.Client{Transport: stallGuard{ctx: ctx, idle: time.Minute, next: transport}}
}

// stallGuard is an http.RoundTripper that ties every request to ctx and
// fails one with errStalled when its body is not read for idle, which is how
// a black-holed connection shows up while a chunk is being sent. go-tus builds
// requests without a context, so this is the only place to add one. The wait
// for the reply after the body is sent is bounded by ResponseHeaderTimeout.
type stallGuard struct {
	ctx  context.Context
	idle time.Duration
	next http.RoundTripper
}

func (g stallGuard) RoundTrip(req *http.Request) (*http.Response, error) {
	ctx, cancel := context.WithCancelCause(g.ctx)
	req = req.WithContext(ctx)
	var body *watchedBody
	if req.Body != nil && req.Body != http.NoBody {
		body = &watchedBody{ReadCloser: req.Body, idle: g.idle}
		body.timer = time.AfterFunc(g.idle, func() { cancel(errStalled) })
		req.Body = body
	}

	resp, err := g.next.RoundTrip(req)
	if body != nil {
		body.stop()
	}
	if err != nil {
		cancel(nil)
		if errors.Is(context.Cause(ctx), errStalled) {
			return nil, errStalled
		}
		return nil, err
	}
	// The reply is read under ctx, so only cancel it once the reply is closed.
	resp.Body = cancelOnClose{ReadCloser: resp.Body, cancel: func() { cancel(nil) }}
	return resp, nil
}

// watchedBody restarts its timer on every read and stops it at the end of the body.
type watchedBody struct {
	io.ReadCloser
	idle  time.Duration
	mu    sync.Mutex
	timer *time.Timer
	done  bool
}

func (b *watchedBody) Read(p []byte) (int, error) {
	n, err := b.ReadCloser.Read(p)
	if err != nil {
		b.stop()
		return n, err
	}
	b.mu.Lock()
	if !b.done {
		b.timer.Reset(b.idle)
	}
	b.mu.Unlock()
	return n, err
}

func (b *watchedBody) Close() error {
	b.stop()
	return b.ReadCloser.Close()
}

func (b *watchedBody) stop() {
	b.mu.Lock()
	b.done = true
	b.timer.Stop()
	b.mu.Unlock()
}

type cancelOnClose struct {
	io.ReadCloser
	cancel func()
}

func (c cancelOnClose) Close() error {
	err := c.ReadCloser.Close()
	c.cancel()
	return err
}

// uploadFile creates the upload on the server and sends it chunk by chunk
// until the server has every byte. After a retryable failure it waits, then
// re-reads the server's offset before resending, since the server may have
// stored the chunk even though the response was lost. The client must have
// Resume enabled so ResumeUpload can look the upload up again.
func uploadFile(ctx context.Context, client *tus.Client, upload *tus.Upload, retry retryPolicy, report func(progress)) error {
	var p progress
	failures := 0
	// backoff counts a failure and waits before the next attempt. It returns
	// an error instead if err is permanent, retries are used up, or ctx ends.
	backoff := func(err error) error {
		if !retryable(err) {
			return err
		}
		failures++
		if failures > retry.maxRetries {
			return fmt.Errorf("gave up after %d retries: %w", retry.maxRetries, err)
		}
		p.err, p.attempt, p.retryIn = err, failures, retry.delay(failures)
		report(p)
		if err := sleep(ctx, p.retryIn); err != nil {
			return err
		}
		// The wait is over; show that the retry is in progress.
		p.retryIn = 0
		report(p)
		return nil
	}

	// Repeating this is safe; at worst a lost response leaves an empty upload behind.
	uploader, err := client.CreateUpload(upload)
	for err != nil {
		if stop := backoff(err); stop != nil {
			return fmt.Errorf("failed to create upload: %w", stop)
		}
		uploader, err = client.CreateUpload(upload)
	}

	p, failures = progress{url: uploader.Url()}, 0
	for uploader.Offset() < upload.Size() {
		p.offset, p.retryIn = uploader.Offset(), 0
		report(p)
		// report returns early once ctx is cancelled, so check before sending more.
		if err := ctx.Err(); err != nil {
			return err
		}

		err := uploader.UploadChunck()
		if err == nil && uploader.Offset() <= p.offset {
			err = errNoProgress
		}
		if err == nil {
			if err := checkOffset(uploader.Offset(), upload.Size()); err != nil {
				return err
			}
			failures, p.err, p.attempt = 0, nil, 0
			continue
		}

		// Keep retrying until the offset resync succeeds; the outer loop then resends from there.
		for err != nil {
			if stop := backoff(err); stop != nil {
				return stop
			}

			var resumed *tus.Uploader
			if resumed, err = client.ResumeUpload(upload); err == nil {
				uploader = resumed
			}
		}
		if err := checkOffset(uploader.Offset(), upload.Size()); err != nil {
			return err
		}
		// The server stored data although the reply was lost, so the upload is
		// still moving: count retries afresh, as after a successful chunk.
		if uploader.Offset() > p.offset {
			failures, p.err, p.attempt = 0, nil, 0
		}
	}

	report(progress{url: p.url, offset: uploader.Offset()})
	return nil
}

// sleep waits for d, or returns early with ctx's error if ctx is cancelled.
func sleep(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-t.C:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

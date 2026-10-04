package main

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"time"

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

var defaultRetry = retryPolicy{maxRetries: 8, baseDelay: time.Second, maxDelay: 30 * time.Second}

// delay returns the wait before retry number attempt (starting at 1).
func (r retryPolicy) delay(attempt int) time.Duration {
	d := r.baseDelay
	for i := 1; i < attempt && d < r.maxDelay; i++ {
		d *= 2
	}
	return min(d, r.maxDelay)
}

// errNoProgress means the server accepted a chunk without moving the offset.
var errNoProgress = errors.New("server did not advance the upload offset")

// retryable reports whether err may clear up on its own: a network failure,
// a server error, rate limiting, a locked upload, or an offset mismatch, which
// a resync fixes. Everything else (bad token, upload too large, untrusted TLS
// certificate, malformed URL, local file errors) is permanent.
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
	return errors.As(err, &urlErr) && urlErr.Op != "parse"
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
		return sleep(ctx, p.retryIn)
	}

	// Repeating this is safe; at worst a lost response leaves an empty upload behind.
	uploader, err := client.CreateUpload(upload)
	for err != nil {
		if stop := backoff(err); stop != nil {
			return fmt.Errorf("failed to create upload: %w", stop)
		}
		p.retryIn = 0
		report(p)
		uploader, err = client.CreateUpload(upload)
	}

	p, failures = progress{url: uploader.Url()}, 0
	for uploader.Offset() < upload.Size() {
		if err := ctx.Err(); err != nil {
			return err
		}
		p.offset, p.retryIn = uploader.Offset(), 0
		report(p)

		err := uploader.UploadChunck()
		if err == nil && uploader.Offset() <= p.offset {
			err = errNoProgress
		}
		if err == nil {
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

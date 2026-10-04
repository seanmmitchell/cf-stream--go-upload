package main

import (
	"context"
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

// retryable reports whether err may clear up on its own: a network failure,
// a server error, rate limiting, or an offset mismatch, which a resync fixes.
// Everything else (bad token, upload too large, local file errors) is permanent.
func retryable(err error) bool {
	if errors.Is(err, tus.ErrOffsetMismatch) {
		return true
	}
	var clientErr tus.ClientError
	if errors.As(err, &clientErr) {
		return clientErr.Code == http.StatusRequestTimeout || clientErr.Code == http.StatusTooManyRequests || clientErr.Code >= 500
	}
	var urlErr *url.Error
	return errors.As(err, &urlErr)
}

// uploadFile creates the upload on the server and sends it chunk by chunk
// until the server has every byte. After a retryable failure it waits, then
// re-reads the server's offset before resending, since the server may have
// stored the chunk even though the response was lost. The client must have
// Resume enabled so ResumeUpload can look the upload up again.
func uploadFile(ctx context.Context, client *tus.Client, upload *tus.Upload, retry retryPolicy, report func(progress)) error {
	uploader, err := client.CreateUpload(upload)
	if err != nil {
		return fmt.Errorf("failed to create upload: %w", err)
	}

	p := progress{url: uploader.Url()}
	failures := 0
	for uploader.Offset() < upload.Size() {
		p.offset, p.retryIn = uploader.Offset(), 0
		report(p)

		err := uploader.UploadChunck()
		if err == nil {
			failures, p.err, p.attempt = 0, nil, 0
			continue
		}

		// Keep retrying until the offset resync succeeds; the outer loop then resends from there.
		for err != nil {
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

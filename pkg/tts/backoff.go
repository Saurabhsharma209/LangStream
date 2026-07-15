package tts

import (
	"context"
	"errors"
	"math/rand"
	"time"
)

// retryBackoff returns the delay to wait before retry attempt n (0-indexed)
// of a transient TTS vendor request failure, using capped exponential
// backoff with jitter. This mirrors pkg/asr/backoff.go's reconnectBackoff
// policy and pkg/translate/backoff.go's retryBackoff (same shape:
// base * 2^attempt, capped, plus up to ~20% jitter), reimplemented locally
// rather than imported from either package so pkg/tts doesn't take on a
// cross-workstream dependency for a few lines of arithmetic.
func retryBackoff(attempt int, base, max time.Duration) time.Duration {
	if attempt < 0 {
		attempt = 0
	}
	if attempt > 20 {
		attempt = 20 // guard against overflow from base << attempt
	}
	d := base << uint(attempt) // base * 2^attempt
	if d <= 0 || d > max {
		d = max
	}
	// Add up to 20% jitter so many concurrent calls retrying at once
	// don't all hammer the vendor at the same instant.
	jitter := time.Duration(rand.Int63n(int64(d)/5 + 1))
	return d + jitter
}

// isRetryableStatusCode reports whether an HTTP/WebSocket-handshake status
// code represents a transient vendor-side condition worth retrying: 429
// (rate limited) or any 5xx (server error). Other 4xx codes (bad auth, bad
// request, not found, ...) are permanent client errors a retry cannot fix,
// so callers should fail fast on those instead.
func isRetryableStatusCode(code int) bool {
	return code == 429 || (code >= 500 && code <= 599)
}

// isRetryableConnErr classifies a connection-level failure (dial, TLS
// handshake, HTTP round trip, WebSocket frame write) that occurred before
// -- or independent of -- any vendor status code. Context cancellation/
// deadline errors are never retried, since retrying would just ignore the
// caller's own cancellation; anything else observed at this layer
// (connection refused, connection reset, timeouts, ...) is exactly the
// "transient network blip" case this retry logic exists for.
func isRetryableConnErr(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return false
	}
	return true
}

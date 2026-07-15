package translate

import (
	"math/rand"
	"time"
)

// retryBackoff returns the delay to wait before retry attempt n (0-indexed)
// of a transient GPT-4o request failure, using capped exponential backoff
// with jitter. This mirrors pkg/asr/backoff.go's reconnectBackoff policy
// (same shape: base * 2^attempt, capped, plus up to ~20% jitter),
// reimplemented locally rather than imported from pkg/asr so this package
// doesn't take on a cross-workstream dependency on pkg/asr's internals for
// a few lines of arithmetic.
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
	// (e.g. after a shared upstream blip) don't all hammer the vendor at
	// the same instant.
	jitter := time.Duration(rand.Int63n(int64(d)/5 + 1))
	return d + jitter
}

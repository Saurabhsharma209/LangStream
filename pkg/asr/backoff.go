package asr

import (
	"math/rand"
	"time"
)

// reconnectBackoff returns the delay to wait before reconnect attempt n
// (0-indexed), using capped exponential backoff with jitter. It is shared
// by the Deepgram and Sarvam backends so both apply the same basic,
// documented reconnect policy after a mid-stream WebSocket disconnect.
func reconnectBackoff(attempt int, base, max time.Duration) time.Duration {
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
	// Add up to 20% jitter so many sessions reconnecting simultaneously
	// (e.g. after a regional outage) don't all hammer the vendor at once.
	jitter := time.Duration(rand.Int63n(int64(d)/5 + 1))
	return d + jitter
}

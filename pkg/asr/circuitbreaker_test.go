package asr

import (
	"testing"
	"time"
)

// --- direct unit tests on circuitBreaker, mirroring
// pkg/translate/circuitbreaker_test.go's unit-level coverage -------------

func TestASRCircuitBreaker_AllowRecordSuccessFailureUnit(t *testing.T) {
	b := newCircuitBreaker(2, 20*time.Millisecond)

	if !b.allow() {
		t.Fatal("expected allow() to be true on a fresh closed breaker")
	}
	b.recordFailure()
	if b.open {
		t.Fatal("breaker should still be closed after 1 of 2 failures")
	}

	if !b.allow() {
		t.Fatal("expected allow() to be true before threshold reached")
	}
	b.recordFailure()
	if !b.open {
		t.Fatal("breaker should be open after reaching the threshold")
	}

	if b.allow() {
		t.Fatal("expected allow() to be false immediately after opening (still cooling down)")
	}

	time.Sleep(30 * time.Millisecond)
	if !b.allow() {
		t.Fatal("expected allow() to let exactly one probe through after cooldown elapses")
	}
	// A concurrent caller during the probe must be rejected.
	if b.allow() {
		t.Fatal("expected allow() to reject a second concurrent probe attempt")
	}

	b.recordSuccess()
	if b.open {
		t.Fatal("breaker should be closed after a successful probe")
	}
	if !b.allow() {
		t.Fatal("expected allow() to be true after breaker closed")
	}
}

func TestASRCircuitBreaker_FailedProbeReopensCooldownUnit(t *testing.T) {
	b := newCircuitBreaker(1, 20*time.Millisecond)
	b.recordFailure() // trips the breaker (threshold 1)
	if !b.open {
		t.Fatal("expected breaker to be open after tripping")
	}

	time.Sleep(30 * time.Millisecond)
	if !b.allow() {
		t.Fatal("expected the cooldown-elapsed probe to be allowed")
	}
	b.recordFailure()
	if !b.open {
		t.Fatal("breaker should still be open after a failed probe")
	}
	if b.allow() {
		t.Fatal("expected allow() to be false immediately after the failed probe reopened the cooldown")
	}
}

func TestASRCircuitBreaker_DefaultsAppliedForNonPositiveArgs(t *testing.T) {
	b := newCircuitBreaker(0, 0)
	if b.threshold != defaultBreakerFailureThreshold {
		t.Errorf("threshold = %d, want default %d", b.threshold, defaultBreakerFailureThreshold)
	}
	if b.cooldown != defaultBreakerCooldown {
		t.Errorf("cooldown = %v, want default %v", b.cooldown, defaultBreakerCooldown)
	}

	b2 := newCircuitBreaker(-1, -1*time.Second)
	if b2.threshold != defaultBreakerFailureThreshold {
		t.Errorf("threshold = %d, want default %d", b2.threshold, defaultBreakerFailureThreshold)
	}
	if b2.cooldown != defaultBreakerCooldown {
		t.Errorf("cooldown = %v, want default %v", b2.cooldown, defaultBreakerCooldown)
	}
}

func TestASRCircuitBreaker_ConcurrentAccessIsRaceFree(t *testing.T) {
	b := newCircuitBreaker(5, 5*time.Millisecond)
	done := make(chan struct{})
	for i := 0; i < 20; i++ {
		go func(n int) {
			defer func() { done <- struct{}{} }()
			for j := 0; j < 50; j++ {
				if b.allow() {
					if (n+j)%2 == 0 {
						b.recordSuccess()
					} else {
						b.recordFailure()
					}
				}
			}
		}(i)
	}
	for i := 0; i < 20; i++ {
		<-done
	}
}

// TestASRCircuitBreaker_AbortReleasesStuckProbe is a regression test
// mirroring pkg/translate/circuitbreaker_test.go's
// TestCircuitBreaker_AbortReleasesStuckProbe: if a cooldown-elapsed probe
// call ends without ever calling recordSuccess or recordFailure -- e.g.
// a session was torn down (Close()) before its first connect attempt
// ever resolved -- probeInFlight must not get stuck forever. abort()
// fixes this by clearing probeInFlight without touching open/closed
// state or the failure count.
func TestASRCircuitBreaker_AbortReleasesStuckProbe(t *testing.T) {
	b := newCircuitBreaker(1, 5*time.Millisecond)
	b.recordFailure() // trips the breaker (threshold 1)
	if !b.open {
		t.Fatal("expected breaker to be open after tripping")
	}

	// Simulate the cooldown elapsing, then a probe being let through.
	b.now = func() time.Time { return time.Now().Add(time.Hour) }
	if !b.allow() {
		t.Fatal("expected the cooldown-elapsed probe to be allowed")
	}
	if !b.probeInFlight {
		t.Fatal("expected probeInFlight to be true after the probe was let through")
	}
	if b.allow() {
		t.Fatal("expected a second concurrent allow() to be rejected while the probe is in flight")
	}

	// Simulate the probe's session being torn down without ever settling
	// the breaker (the bug scenario).
	b.abort()

	if b.probeInFlight {
		t.Fatal("abort() should have cleared probeInFlight")
	}
	b.now = func() time.Time { return time.Now().Add(24 * time.Hour) }
	if !b.allow() {
		t.Fatal("breaker should allow a fresh probe after abort() released the stuck one, even much later")
	}
}

func TestASRCircuitBreaker_AbortIsNoOpWhenClosed(t *testing.T) {
	b := newCircuitBreaker(5, 5*time.Millisecond)
	if !b.allow() {
		t.Fatal("expected allow() true on a fresh closed breaker")
	}
	b.abort() // should not panic or affect closed-state behavior
	if b.open {
		t.Fatal("abort() must never open a closed breaker")
	}
	if !b.allow() {
		t.Fatal("breaker should remain closed and allow calls after abort() on a closed breaker")
	}
}

func TestASRCircuitBreaker_AbortDoesNotCountAsFailure(t *testing.T) {
	b := newCircuitBreaker(2, 5*time.Millisecond)
	if !b.allow() {
		t.Fatal("expected allow() true on a fresh closed breaker")
	}
	b.abort()
	if b.open {
		t.Fatal("abort() must not trip the breaker")
	}
	if b.consecutiveFails != 0 {
		t.Errorf("consecutiveFails = %d after abort(), want 0 (abort is not a failure)", b.consecutiveFails)
	}
}

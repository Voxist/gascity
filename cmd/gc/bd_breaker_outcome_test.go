package main

import (
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/resilience"
)

// bdOutcomeErr is a minimal error whose text drives classification, matching
// how bd surfaces failures (formatted strings, not typed sentinels).
type bdOutcomeErr string

func (e bdOutcomeErr) Error() string { return string(e) }

// TestBdBreakerOutcomeSeparatesNoAnswerFromApplicationError is the regression
// for the breaker never tripping on the failure it exists to catch.
//
// The outcome was derived from bdTransportRetryableError alone, so anything
// without a stderr marker counted as a SUCCESS and reset the consecutive
// failure count. A hung backend killed by the command budget produces exactly
// that shape — no marker, because bd never got a reply to print — so a
// timeout-shaped failure stream never reached the threshold and bd subprocesses
// kept piling up at ~2 minutes each.
//
// Three states are required, not two: "cannot classify" must not be reported as
// healthy, or an unclassifiable stream hides a wedged backend just as well.
func TestBdBreakerOutcomeSeparatesNoAnswerFromApplicationError(t *testing.T) {
	city, scope := t.TempDir(), t.TempDir()

	t.Run("bd answered", func(t *testing.T) {
		failure, conclusive := bdBreakerOutcomeFor(city, scope, nil, nil)
		if failure || !conclusive {
			t.Fatalf("nil error = (failure %v, conclusive %v), want (false, true): success is the only thing that may reset the failure count", failure, conclusive)
		}
	})

	for _, msg := range []string{
		"bd list: timed out after 2m0s",
		"bd list: context deadline exceeded",
		"bd ready: signal: killed",
	} {
		t.Run("no answer/"+msg, func(t *testing.T) {
			failure, conclusive := bdBreakerOutcomeFor(city, scope, nil, bdOutcomeErr(msg))
			if !failure || !conclusive {
				t.Fatalf("%q = (failure %v, conclusive %v), want (true, true): a backend that never answered is a transport failure, whatever the marker table says", msg, failure, conclusive)
			}
		})
	}

	t.Run("application-class error is inconclusive", func(t *testing.T) {
		failure, conclusive := bdBreakerOutcomeFor(city, scope, nil, bdOutcomeErr("bd show: bead not found"))
		if failure || conclusive {
			t.Fatalf("application error = (failure %v, conclusive %v), want (false, false): neither a transport fault nor proof of health", failure, conclusive)
		}
	})
}

// TestRecordBdBreakerOutcomeResolvesHalfOpenProbe pins that an inconclusive
// verdict still releases a probe the breaker already consumed.
//
// Once the breaker is open, every cached read short-circuits, so the reconcile
// probe is the only bd traffic left on the scope. Allow() moves the breaker to
// half-open to admit it. If that probe fails inconclusively and nothing is
// recorded, the breaker neither closes (no success) nor re-arms its backoff (no
// failure): it stays half-open forever, pinning the scope in degraded mode and
// hiding the real error from every caller.
func TestRecordBdBreakerOutcomeResolvesHalfOpenProbe(t *testing.T) {
	reg := resilience.NewRegistry(resilience.Settings{Enabled: true, ConsecutiveFailures: 1})
	// Zero jitter puts the open-state deadline at "now", so the very next Allow
	// admits the recovery probe and the half-open state is reached
	// deterministically rather than by racing a backoff.
	reg.SetJitterForTest(func(time.Duration) time.Duration { return 0 })
	b := reg.Breaker("/city/rig", resilience.OpClassBd)

	b.Trip()
	if !b.Allow() {
		t.Fatal("Allow() rejected with a zero backoff deadline; the probe should have been admitted")
	}
	if got := b.State(); got != resilience.StateHalfOpen {
		t.Fatalf("state after admitted probe = %v, want %v", got, resilience.StateHalfOpen)
	}

	// Inconclusive: must not be silently dropped while a probe is outstanding.
	recordBdBreakerOutcome(b, false, false)
	if got := b.State(); got == resilience.StateHalfOpen {
		t.Fatal("breaker still half-open after an inconclusive outcome: the consumed probe was never resolved, so it can neither close nor re-arm its backoff, pinning the scope in degraded mode")
	}
}

// TestRecordBdBreakerOutcomeInconclusiveDoesNotResetClosedBreaker keeps the
// other half of the contract: while closed, an unclassifiable error must not
// count as a success and clear an accumulating failure count.
func TestRecordBdBreakerOutcomeInconclusiveDoesNotResetClosedBreaker(t *testing.T) {
	reg := resilience.NewRegistry(resilience.Settings{Enabled: true, ConsecutiveFailures: 3})
	b := reg.Breaker("/city/rig", resilience.OpClassBd)

	recordBdBreakerOutcome(b, true, true)   // one real transport failure
	recordBdBreakerOutcome(b, false, false) // inconclusive: must be a no-op
	recordBdBreakerOutcome(b, true, true)
	recordBdBreakerOutcome(b, true, true)

	if b.Allow() {
		t.Fatal("breaker still admitting after 3 transport failures: an inconclusive outcome reset the count it must have left alone")
	}
}

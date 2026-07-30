package main

import "testing"

// bdOutcomeErr is a minimal error whose text drives classification, matching how
// bd surfaces failures (formatted strings, not typed sentinels).
type bdOutcomeErr string

func (e bdOutcomeErr) Error() string { return string(e) }

// TestBdBreakerOutcomeSeparatesNoAnswerFromApplicationError is the regression
// for the breaker never tripping on the failure it exists to catch.
//
// The outcome was previously derived from bdTransportRetryableError alone, so
// anything without a stderr marker counted as a SUCCESS and reset the
// consecutive-failure count. A hung backend killed by the command budget
// produces exactly that shape: no marker to match, because bd never got a reply
// to print. With a timeout-shaped failure stream the breaker therefore never
// reached its threshold and bd subprocesses kept piling up at ~2 minutes each —
// the pile-up the breaker was added to prevent.
//
// Three states are required, not two. "Cannot classify" must not be reported as
// healthy, or an unclassifiable error stream hides a wedged backend just as
// effectively.
func TestBdBreakerOutcomeSeparatesNoAnswerFromApplicationError(t *testing.T) {
	city, scope := t.TempDir(), t.TempDir()

	t.Run("bd answered", func(t *testing.T) {
		failure, conclusive := bdBreakerOutcomeFor(city, scope, nil, nil)
		if failure || !conclusive {
			t.Fatalf("nil error = (failure %v, conclusive %v), want (false, true): a successful call is the only thing that may reset the failure count", failure, conclusive)
		}
	})

	// bd never got a reply. No stderr marker exists precisely because the
	// backend never answered, so marker matching cannot see these.
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

	// bd answered with a complaint about the request. The transport is fine, but
	// this is not positive evidence of health either — it must not reset the
	// count, and it must not trip the breaker.
	t.Run("application-class error is inconclusive", func(t *testing.T) {
		failure, conclusive := bdBreakerOutcomeFor(city, scope, nil, bdOutcomeErr("bd show: bead not found"))
		if failure || conclusive {
			t.Fatalf("application error = (failure %v, conclusive %v), want (false, false): neither a transport fault nor proof the transport is healthy", failure, conclusive)
		}
	})
}

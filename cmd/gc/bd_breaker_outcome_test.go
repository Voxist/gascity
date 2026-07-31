package main

import "testing"

// bdOutcomeErr is a minimal error whose text drives classification, matching
// how bd surfaces failures (formatted strings, not typed sentinels).
type bdOutcomeErr string

func (e bdOutcomeErr) Error() string { return string(e) }

// TestBdTransportFailureCountsNoAnswerNotApplicationErrors is the regression for
// the breaker never tripping on the failure it exists to catch.
//
// The outcome used to be derived from bdTransportRetryableError alone, so
// anything without a stderr marker counted as a SUCCESS and reset the
// consecutive-failure count. A backend that hangs until the command budget
// kills it produces exactly that shape — there is no marker because bd never
// got a reply to print — so a timeout-shaped failure stream never reached the
// threshold and bd subprocesses kept piling up at ~2 minutes each.
//
// The other half matters just as much: an application-class error means bd
// REACHED the store and answered, which is evidence the transport is healthy.
// Classifying it as a failure re-trips breakers whose backend has demonstrably
// recovered — Breaker.RecordFailure's own doc forbids recording application
// errors.
func TestBdTransportFailureCountsNoAnswerNotApplicationErrors(t *testing.T) {
	city, scope := t.TempDir(), t.TempDir()

	t.Run("no error is not a failure", func(t *testing.T) {
		if bdTransportFailure(city, scope, nil, nil) {
			t.Fatal("nil error classified as a transport failure")
		}
	})

	// bd never got a reply. Marker matching cannot see these, because there is
	// no stderr to match.
	for _, msg := range []string{
		"bd list: timed out after 2m0s",
		"bd list: context deadline exceeded",
		"bd ready: signal: killed",
	} {
		t.Run("no answer", func(t *testing.T) {
			if !bdTransportFailure(city, scope, nil, bdOutcomeErr(msg)) {
				t.Fatalf("%q not classified as a transport failure: a backend that never answered is a transport fault whatever the marker table says", msg)
			}
		})
	}

	// bd answered, with a complaint about the request. The transport worked.
	for _, msg := range []string{
		"bd show: bead not found",
		"bd update: invalid status",
	} {
		t.Run("application error", func(t *testing.T) {
			if bdTransportFailure(city, scope, nil, bdOutcomeErr(msg)) {
				t.Fatalf("%q classified as a transport failure: bd reached the store and answered, so this must reset the failure count, not add to it", msg)
			}
		})
	}
}

package beads

import (
	"context"
	"fmt"
	"testing"
)

// outageRunner serves a fixed bead set until it is wedged, after which every
// command fails the way a hung backend actually fails: the per-command deadline
// expires and bd is killed, producing the "timed out after" error shape.
type outageRunner struct {
	wedged bool
	calls  int
}

func (r *outageRunner) run(_, name string, args ...string) ([]byte, error) {
	r.calls++
	if r.wedged {
		return nil, fmt.Errorf("timed out after 2m0s")
	}
	if name != "bd" || len(args) == 0 {
		return nil, fmt.Errorf("unexpected command %q %v", name, args)
	}
	switch args[0] {
	case "list":
		return []byte(`[{"id":"ga-1","title":"one","status":"open"},
		                {"id":"ga-2","title":"two","status":"in_progress"}]`), nil
	case "ready":
		return []byte(`[]`), nil
	case "version":
		return []byte("bd version 1.0.4\n"), nil
	}
	return []byte(`[]`), nil
}

// stubGate is an AvailabilityGate whose answer the test controls, standing in
// for the per-scope resilience.Breaker that bd_resilience.go wires in
// production.
type stubGate struct{ available bool }

func (g *stubGate) Available() bool { return g.available }
func (g *stubGate) ProbeDue() bool  { return false }

// TestCachingStoreServesLastGoodWhenTheGateIsOpen tests the hypothesis behind
// the proposed classification fix.
//
// Production wires a breaker into the cache (cmd/gc/bd_resilience.go:123), and
// reads consult it before anything else: caching_store_reads.go:40 routes to
// listLastGood when the gate reports unavailable, and listLastGood ignores
// cacheServableLocked entirely — it refuses only an unprimed cache.
//
// So IF the breaker tripped on a wedged backend, reads would survive. The
// reason it does not trip is that bdTransportRetryableMarkers carries no
// timeout marker, so a "timed out after" failure is never classified as
// transport.
//
// This test pins that hypothesis. If it passes, making the breaker trip is a
// sufficient production fix and the cache's own defect stays latent behind it.
// If it fails, the classification fix is not enough on its own.
func TestCachingStoreServesLastGoodWhenTheGateIsOpen(t *testing.T) {
	t.Parallel()

	runner := &outageRunner{}
	cache := NewCachingStore(NewBdStore("/city", runner.run), nil)
	if err := cache.Prime(context.Background()); err != nil {
		t.Fatalf("Prime: %v", err)
	}

	before, err := cache.List(ListQuery{Status: "open"})
	if err != nil {
		t.Fatalf("List before the outage: %v", err)
	}

	// Same outage as the previous test: backend wedges, reconcile cycles fail
	// until the cache degrades itself.
	runner.wedged = true
	for i := 0; i < maxCacheSyncFailures+1; i++ {
		cache.runReconciliation()
	}

	// Now the breaker notices the transport is down — the step that does NOT
	// happen today, because a timeout matches no transport marker.
	cache.SetAvailabilityGate(&stubGate{available: false})

	after, err := cache.List(ListQuery{Status: "open"})
	if err != nil {
		t.Fatalf("gate open, but List still failed: %v", err)
	}
	if len(after) != len(before) {
		t.Fatalf("gate open: List returned %d beads, want the %d still cached", len(after), len(before))
	}
}

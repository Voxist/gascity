package main

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/resilience"
)

// TestBdInvocationTimedOutScoping pins both axes this classification can get
// wrong. Each false case is a real over-match that would trip a scope's
// transport breaker — and so fail-fast every bd call in that scope for the
// backoff window — on evidence that says nothing about the bd transport.
func TestBdInvocationTimedOutScoping(t *testing.T) {
	perCommand := fmt.Errorf("bd list: timed out after %s", 2*time.Minute)

	for _, tc := range []struct {
		name    string
		cmd     string
		err     error
		want    bool
		because string
	}{
		{
			"bd killed at its deadline", "bd", perCommand, true,
			"the wedged-backend shape: bd never answered, so no stderr marker exists to classify",
		},
		{
			"embedded dolt sql killed at its deadline", "dolt", perCommand, false,
			"BdStore drives dolt sql through the same runner; a slow local fallback is not a bd transport failure",
		},
		{
			"caller budget expired", "bd", fmt.Errorf("timed out after 50ms (caller deadline)"), false,
			"the caller's own short budget says nothing about the store's health",
		},
		{
			"application-class failure", "bd", errors.New("bead ga-1 not found"), false,
			"bd reached the store and answered",
		},
		{"no error", "bd", nil, false, "success"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := bdInvocationTimedOut(tc.cmd, tc.err); got != tc.want {
				t.Errorf("bdInvocationTimedOut(%q, %v) = %v, want %v — %s",
					tc.cmd, tc.err, got, tc.want, tc.because)
			}
		})
	}
}

// TestWedgedBackendTripsTheBreakerThroughTheRealRunner is the end-to-end
// property ga-2bo4m needs, driven through bdCommandRunnerWithManagedRetry
// rather than by reconstructing its call site — so dropping the wiring fails
// this test rather than leaving it green.
//
// The cache's rescue path (serve last-good instead of dialing a dead store)
// already exists and is already wired in production; it activates only when the
// breaker reports the transport unavailable. Before this change a wedged
// backend recorded a HEALTHY transport on every call, so the breaker never
// tripped, the rescue never activated, and reads failed against a store known
// to be down while the cache still held the data.
func TestWedgedBackendTripsTheBreakerThroughTheRealRunner(t *testing.T) {
	t.Setenv("GC_BEADS", "bd")
	cityPath := writeBreakerTestCity(t, "")
	bdResilienceRegistryForCity(cityPath).SetJitterForTest(func(c time.Duration) time.Duration { return c })
	calls := installFakeBdExec(t, func(_, _ string, _ ...string) ([]byte, error) {
		return nil, fmt.Errorf("timed out after %s", 2*time.Minute)
	})
	runner := breakerTestRunner(cityPath)
	scope := t.TempDir()

	for i := 0; i < 3; i++ {
		if _, err := runner(scope, "bd", "list", "--json"); err == nil {
			t.Fatalf("call %d: err = nil, want the timeout", i)
		} else if errors.Is(err, beads.ErrStoreUnavailable) {
			t.Fatalf("call %d: tripped before the threshold", i)
		}
	}

	// A timeout must NOT take the managed-retry path: retrying a command that
	// just burned its full per-command budget doubles every call's latency
	// against a store already known to be unresponsive. One exec per call.
	if *calls != 3 {
		t.Errorf("exec calls = %d after 3 invocations, want 3 — a timeout must not be retried", *calls)
	}

	if _, err := runner(scope, "bd", "list", "--json"); !errors.Is(err, beads.ErrStoreUnavailable) {
		t.Fatalf("after 3 consecutive wedged-backend timeouts err = %v, want ErrStoreUnavailable; "+
			"the breaker never trips, so the cache's last-good read path never activates", err)
	}
}

// TestCompleteBindingScopesStayBehindTheBreaker pins that a scope with a
// complete external storage binding — which bdCommandRunnerForCity routes to
// the context runner instead of the managed-retry path — still gets the scope
// transport breaker. The external-endpoint direction is exactly what the
// breaker was built for; an early return that skipped it would let a wedged
// endpoint pile up bd subprocesses unbounded.
func TestCompleteBindingScopesStayBehindTheBreaker(t *testing.T) {
	t.Setenv("GC_BEADS", "bd")
	cityPath := writeBreakerTestCity(t, "")
	if err := os.MkdirAll(filepath.Join(cityPath, ".beads"), 0o755); err != nil {
		t.Fatal(err)
	}
	metadata := `{"backend":"dolt","storage_endpoint":"db.example.com:3306","storage_database":"beads_prod"}`
	if err := os.WriteFile(scopeMetadataJSONPath(cityPath), []byte(metadata), 0o644); err != nil {
		t.Fatal(err)
	}
	bdResilienceRegistryForCity(cityPath).SetJitterForTest(func(c time.Duration) time.Duration { return c })
	calls := installFakeBdExec(t, func(_, _ string, _ ...string) ([]byte, error) {
		return nil, fmt.Errorf("timed out after %s", 2*time.Minute)
	})

	runner := bdCommandRunnerForCity(cityPath)
	scope := t.TempDir()
	for i := 0; i < 3; i++ {
		if _, err := runner(scope, "bd", "list", "--json"); err == nil {
			t.Fatalf("call %d: err = nil, want the timeout", i)
		} else if errors.Is(err, beads.ErrStoreUnavailable) {
			t.Fatalf("call %d: tripped before the threshold", i)
		}
	}
	if *calls != 3 {
		t.Fatalf("exec calls = %d after 3 invocations, want 3", *calls)
	}
	if _, err := runner(scope, "bd", "list", "--json"); !errors.Is(err, beads.ErrStoreUnavailable) {
		t.Fatalf("after 3 consecutive wedged-endpoint timeouts err = %v, want ErrStoreUnavailable; "+
			"the context runner bypasses the breaker", err)
	}
	if *calls != 3 {
		t.Fatalf("exec calls = %d after the breaker opened, want still 3 — an open breaker must spawn zero subprocesses", *calls)
	}
}

// TestApplicationFailuresDoNotTripTheBreaker is the counterweight: a store that
// answers is healthy transport however unhappy the answer. Without this, any
// run of ordinary "not found" errors would fail-fast the whole scope.
func TestApplicationFailuresDoNotTripTheBreaker(t *testing.T) {
	reg := resilience.NewRegistry(resilience.Settings{
		Enabled: true, ConsecutiveFailures: 3, OpenBase: time.Minute, OpenMax: time.Minute,
	})
	breaker := reg.Breaker("/scope", resilience.OpClassBd)

	for i := 0; i < 20; i++ {
		recordBdBreakerOutcome(breaker, bdInvocationTimedOut("bd", errors.New("bead ga-1 not found")))
	}
	if !breaker.Available() {
		t.Fatal("application-class failures tripped the transport breaker")
	}
}

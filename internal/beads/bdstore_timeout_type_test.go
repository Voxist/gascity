package beads

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"
)

// TestBDExecTimeoutErrorCarriesSentinel pins that both branches of
// bdExecTimeoutError — the per-command timer and the caller-deadline
// attribution — produce an error callers can identify by type.
func TestBDExecTimeoutErrorCarriesSentinel(t *testing.T) {
	start := time.Now()

	perCommand := bdExecTimeoutError(context.Background(), 120*time.Second, start)
	if !IsBDTimeout(perCommand) {
		t.Errorf("per-command timeout not identifiable as a bd timeout: %v", perCommand)
	}

	ctx, cancel := context.WithDeadline(context.Background(), start.Add(50*time.Millisecond))
	defer cancel()
	callerDeadline := bdExecTimeoutError(ctx, 120*time.Second, start)
	if !IsBDTimeout(callerDeadline) {
		t.Errorf("caller-deadline timeout not identifiable as a bd timeout: %v", callerDeadline)
	}
}

// TestBDTimeoutMessageUnchanged pins the operator-visible text. The whole point
// of the typed error is to be *additive*: existing substring matchers across the
// tree, and the stderr text operators grep for, must keep working byte for byte
// while the type is introduced. If this test fails, the type stopped being
// additive and every un-migrated caller silently changed behavior.
func TestBDTimeoutMessageUnchanged(t *testing.T) {
	start := time.Now()

	if got, want := bdExecTimeoutError(context.Background(), 120*time.Second, start).Error(), "timed out after 2m0s"; got != want {
		t.Errorf("per-command message changed:\n got %q\nwant %q", got, want)
	}

	ctx, cancel := context.WithDeadline(context.Background(), start.Add(50*time.Millisecond))
	defer cancel()
	got := bdExecTimeoutError(ctx, 120*time.Second, start).Error()
	if want := "timed out after 50ms (caller deadline)"; got != want {
		t.Errorf("caller-deadline message changed:\n got %q\nwant %q", got, want)
	}
}

// TestBDTimeoutIsNarrowerThanTheString is the assertion that justifies the type
// existing at all.
//
// At least five sites in the tree format "timed out after" for reasons that are
// not a bd transport failure: waiting for a bead to appear, waiting for the
// supervisor to exit, waiting for managed Dolt to become TCP-ready, and two in
// the Dolt SQL health path. A substring matcher cannot tell any of those from a
// wedged backend, so every classifier built on the string over-matches. The
// sentinel must not.
func TestBDTimeoutIsNarrowerThanTheString(t *testing.T) {
	impostors := []error{
		fmt.Errorf("timed out after %s waiting for bead %s; check `gc bd show %s`", time.Minute, "ga-1", "ga-1"),
		fmt.Errorf("timed out after %s waiting for supervisor at %s to exit", time.Minute, "/tmp/s.sock"),
		fmt.Errorf("timed out after %s waiting for managed Dolt on port %d to become TCP-ready", time.Minute, 3306),
		fmt.Errorf("timed out after %s waiting for managed Dolt runtime state to be published", time.Minute),
	}
	for _, err := range impostors {
		if IsBDTimeout(err) {
			t.Errorf("non-transport timeout misidentified as a bd timeout: %v", err)
		}
	}
}

// TestBDTimeoutSurvivesWrapping pins that callers may add context with %w
// without destroying the identity — the failure mode that makes a sentinel
// useless in practice, since every layer between exec and the classifier wraps.
func TestBDTimeoutSurvivesWrapping(t *testing.T) {
	base := bdExecTimeoutError(context.Background(), 30*time.Second, time.Now())
	wrapped := fmt.Errorf("listing beads: %w", fmt.Errorf("bd list: %w", base))
	if !IsBDTimeout(wrapped) {
		t.Errorf("sentinel lost through two layers of %%w: %v", wrapped)
	}
}

// TestBDTimeoutIsNotContextDeadlineExceeded guards the hazard documented on
// ErrBDTimeout. dispatch.IsTransientControllerError short-circuits on
// context.DeadlineExceeded, so if a bd timeout also satisfied that sentinel it
// would silently reclassify every bd timeout as a transient controller error
// across the control dispatcher. Reusing the stdlib sentinel is the obvious
// shortcut here and it is wrong; this test makes taking it fail.
func TestBDTimeoutIsNotContextDeadlineExceeded(t *testing.T) {
	err := bdExecTimeoutError(context.Background(), 30*time.Second, time.Now())
	if errors.Is(err, context.DeadlineExceeded) {
		t.Error("bd timeout also satisfies context.DeadlineExceeded; this silently reclassifies bd timeouts inside the control dispatcher")
	}
}

// TestBDTimeoutDoesNotMatchUnrelatedSentinels pins that the new identity is
// specific: it must not collide with the store-layer sentinels that already
// exist, or callers switching on them would mis-route.
func TestBDTimeoutDoesNotMatchUnrelatedSentinels(t *testing.T) {
	err := bdExecTimeoutError(context.Background(), 30*time.Second, time.Now())
	for name, other := range map[string]error{
		"ErrStoreUnavailable": ErrStoreUnavailable,
		"ErrNotFound":         ErrNotFound,
	} {
		if errors.Is(err, other) {
			t.Errorf("bd timeout unexpectedly satisfies %s", name)
		}
	}
	if errors.Is(ErrStoreUnavailable, ErrBDTimeout) {
		t.Error("ErrStoreUnavailable unexpectedly satisfies ErrBDTimeout")
	}
}

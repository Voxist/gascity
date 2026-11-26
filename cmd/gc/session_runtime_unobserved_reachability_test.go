package main

import (
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/clock"
	"github.com/gastownhall/gascity/internal/runtime"
	sessionpkg "github.com/gastownhall/gascity/internal/session"
	"github.com/gastownhall/gascity/internal/session/sessiontest"
)

// TestHealStatePatchWithRollbackInfoObservesUnobservedRuntimeFromARealProbeFailure
// is ADR-0096 D6 / P4b: at least one production code path must be SHOWN to
// produce the NOT-OBSERVED projection by driving the REAL constructor chain
// from a stubbed non-completing probe — never by constructing
// session.RuntimeFacts{Observed: false} (or any direct analogue) by hand,
// which is a fixture that encodes its own premise (ADR-0096 audit finding,
// vc-15f6rc).
//
// The chain this test drives, with NO test-side RuntimeFacts/LifecycleInput
// construction anywhere:
//
//	a stubbed provider whose ObserveLivenessWithError never completes
//	  -> workerSessionTargetRunningWithConfig (the real liveness probe
//	     cmd/gc/session_reconciler.go's reconcile loop calls)
//	  -> healStatePatchWithRollbackInfo (the real heal constructor;
//	     session_reconciler.go:2664 calls it the identical way, passing
//	     observed = livenessErr == nil)
//	  -> sessionpkg.ProjectLifecycle (internal; see projectRuntimeProjection's
//	     "!input.Runtime.Observed" branch)
//
// What's observed at the far end is the heal BATCH, which is the only thing
// healStatePatchWithRollbackInfo exposes — but it is not an indirect proxy:
// projectRuntimeProjection's unobserved branch returns (RuntimeProjectionUnknown,
// compat, false), i.e. the CURRENT state unchanged, which is exactly why the
// batch comes back nil here. The confirmed-dead contrast case below proves
// that nil batch is attributable to Observed, not to this fixture's other
// inputs: same Info, same alive=false outcome, only the probe's completion
// differs, and the confirmed-dead case DOES produce a transition (to asleep)
// via the SAME unchanged code path.
func TestHealStatePatchWithRollbackInfoObservesUnobservedRuntimeFromARealProbeFailure(t *testing.T) {
	const target = "worker-reachability-probe"
	clk := &clock.Fake{Time: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)}
	info := sessiontest.InfoFromMeta(t, map[string]string{
		"session_name": target,
		"state":        "active",
	})

	t.Run("stubbed non-completing probe yields NOT-OBSERVED and a fail-closed no-op heal", func(t *testing.T) {
		stub := &nonCompletingLivenessProvider{Fake: runtime.NewFake()}
		alive, livenessErr := workerSessionTargetRunningWithConfig("", nil, stub, nil, target)
		if livenessErr == nil {
			t.Fatalf("stubbed probe returned no error — the stub itself is broken, this run proves nothing")
		}

		// observed is computed exactly as cmd/gc/session_reconciler.go:2664 computes
		// it for its own call to this same function — never hand-set to false.
		observed := livenessErr == nil
		batch := healStatePatchWithRollbackInfo(info, alive, observed, clk, 5*time.Minute, true)
		if batch != nil {
			t.Fatalf("heal batch = %v, want nil — an unobserved runtime must fail closed (no destructive heal), per projectRuntimeProjection's \"!input.Runtime.Observed\" branch returning the unchanged current state", batch)
		}
	})

	t.Run("contrast: a completing probe confirming the runtime is gone DOES heal to asleep", func(t *testing.T) {
		healthy := runtime.NewFake() // target was never Start()ed: IsRunning/liveness report false, no error.
		alive, livenessErr := workerSessionTargetRunningWithConfig("", nil, healthy, nil, target)
		if livenessErr != nil {
			t.Fatalf("healthy probe returned an error: %v — the contrast case must observe successfully", livenessErr)
		}
		if alive {
			t.Fatalf("alive = true for a session that was never started")
		}

		observed := livenessErr == nil
		batch := healStatePatchWithRollbackInfo(info, alive, observed, clk, 5*time.Minute, true)
		if batch["state"] != string(sessionpkg.StateAsleep) {
			t.Fatalf("heal batch state = %q, want %q — a CONFIRMED-dead runtime (Observed=true) must still heal normally; this is the control that proves the nil batch above is attributable to Observed, not to some other difference in the fixture", batch["state"], sessionpkg.StateAsleep)
		}
	})
}

// nonCompletingLivenessProvider's ObserveLivenessWithError always returns an
// error, modeling a probe that could not complete (transport failure, process
// exec error, etc.) rather than one that completed and found nothing running.
// This is the "stubbed non-completing probe" ADR-0096 D6's acceptance
// criterion names — distinct from a healthy probe's negative answer, and it is
// what workerSessionTargetRunningWithConfig surfaces as a non-nil error.
type nonCompletingLivenessProvider struct {
	*runtime.Fake
}

func (p *nonCompletingLivenessProvider) ObserveLivenessWithError(name string, processNames []string) (runtime.Liveness, error) {
	return runtime.Liveness{}, runtime.ErrRuntimeUnavailable
}

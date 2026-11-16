package session

import (
	"testing"
)

// ADR-0096 D4: NOT-OBSERVED must be OUT OF BAND. RuntimeProjectionUnknown must
// not be the zero value (an in-band unknown sentinel occupying "" is
// indistinguishable from unset), and the view must carry runtime observation
// as a separate channel so a consumer ignoring the sentinel cannot silently
// read "unknown" as "unset".
func TestRuntimeProjectionUnknownIsNotTheZeroValue(t *testing.T) {
	if RuntimeProjectionUnknown == "" {
		t.Fatalf("RuntimeProjectionUnknown is the zero value: ADR-0096 D4 forbids an in-band unknown sentinel at %q", RuntimeProjectionUnknown)
	}
}

func TestLifecycleViewCarriesRuntimeObservedOutOfBand(t *testing.T) {
	unobserved := ProjectLifecycle(LifecycleInput{
		Status:  "open",
		Runtime: RuntimeFacts{Observed: false},
	})
	if unobserved.RuntimeProjection != RuntimeProjectionUnknown {
		t.Fatalf("unobserved runtime: want %q, got %q", RuntimeProjectionUnknown, unobserved.RuntimeProjection)
	}
	if unobserved.RuntimeObserved {
		t.Fatalf("unobserved runtime: RuntimeObserved must be false")
	}
	if unobserved.RuntimeProjection == "" {
		t.Fatalf("unobserved runtime: projection must not be the zero value")
	}

	observed := ProjectLifecycle(LifecycleInput{
		Status:  "open",
		Runtime: RuntimeFacts{Observed: true, Alive: true},
	})
	if observed.RuntimeProjection != RuntimeProjectionAlive {
		t.Fatalf("observed alive runtime: want %q, got %q", RuntimeProjectionAlive, observed.RuntimeProjection)
	}
	if !observed.RuntimeObserved {
		t.Fatalf("observed runtime: RuntimeObserved must be true")
	}
}

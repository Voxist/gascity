package session

import "testing"

// TestRuntimeFactsZeroValueIsNotObserved pins the ADR-0096 D1 safety property:
// a construction site that forgets to set Runtime at all must fail inert
// (not-observed), never silently alive or dead.
func TestRuntimeFactsZeroValueIsNotObserved(t *testing.T) {
	var zero RuntimeFacts
	got := Fold(zero,
		func(attached, pending bool) string { return "live" },
		func() string { return "dead" },
		func(reason string) string { return "not-observed:" + reason },
	)
	if got != "not-observed:" {
		t.Errorf("zero value folded to %q, want not-observed with empty reason", got)
	}
}

// TestRuntimeFactsFoldDispatchesByConstructor exercises every branch Fold can
// take, keyed off which constructor built the value — the three-way split
// ADR-0096 D2 requires a reader to handle exhaustively.
func TestRuntimeFactsFoldDispatchesByConstructor(t *testing.T) {
	type result struct {
		branch            string
		attached, pending bool
		reason            string
	}
	fold := func(f RuntimeFacts) result {
		return Fold(f,
			func(attached, pending bool) result {
				return result{branch: "live", attached: attached, pending: pending}
			},
			func() result { return result{branch: "dead"} },
			func(reason string) result { return result{branch: "not-observed", reason: reason} },
		)
	}

	if got := fold(Observed(true, true, false)); got.branch != "live" || !got.attached || got.pending {
		t.Errorf("Observed(true, true, false) folded to %+v, want live/attached=true/pending=false", got)
	}
	if got := fold(Observed(true, false, true)); got.branch != "live" || got.attached || !got.pending {
		t.Errorf("Observed(true, false, true) folded to %+v, want live/attached=false/pending=true", got)
	}
	if got := fold(Observed(false, false, false)); got.branch != "dead" {
		t.Errorf("Observed(false, false, false) folded to %+v, want dead", got)
	}
	if got := fold(NotObserved("gc session list: rc=124")); got.branch != "not-observed" || got.reason != "gc session list: rc=124" {
		t.Errorf("NotObserved folded to %+v, want not-observed with the given reason", got)
	}
}

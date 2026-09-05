package beads

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// TestDepsCompleteHasASingleWriter pins the ADR-0094 D6 gauge to the flag it
// reports.
//
// setDepsCompleteLocked is what maintains depsIncompleteSince, the latching
// driver and the transition counters. A direct assignment anywhere else moves
// the flag without moving the gauge, and the dwell the ADR's trigger condition
// reads ("depsComplete latched false for > 1h") would then describe a
// transition that never happened — a silently wrong answer to exactly the
// question this bead exists to make answerable. That failure is invisible at
// review time, because the assignment itself looks correct; only this test
// makes it loud.
//
// Test files are exempt: they set the field directly to construct states.
func TestDepsCompleteHasASingleWriter(t *testing.T) {
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatalf("read package dir: %v", err)
	}

	assign := regexp.MustCompile(`c\.depsComplete\s*=[^=]`)
	setterStart := regexp.MustCompile(`func \(c \*CachingStore\) setDepsCompleteLocked\(`)

	insideSetter := 0
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		raw, err := os.ReadFile(filepath.Join(".", name))
		if err != nil {
			t.Fatalf("read %s: %v", name, err)
		}
		src := string(raw)

		// Byte range of setDepsCompleteLocked's body, if this file declares it.
		lo, hi := -1, -1
		if loc := setterStart.FindStringIndex(src); loc != nil {
			lo = loc[0]
			if end := strings.Index(src[lo:], "\n}\n"); end >= 0 {
				hi = lo + end
			} else {
				hi = len(src)
			}
		}

		for _, m := range assign.FindAllStringIndex(src, -1) {
			if lo >= 0 && m[0] >= lo && m[0] < hi {
				insideSetter++
				continue
			}
			line := 1 + strings.Count(src[:m[0]], "\n")
			t.Errorf("%s:%d assigns c.depsComplete directly; route it through "+
				"setDepsCompleteLocked so the ADR-0094 D6 gauge stays exact", name, line)
		}
	}

	if insideSetter != 1 {
		t.Fatalf("want exactly 1 direct assignment (the one inside setDepsCompleteLocked), got %d", insideSetter)
	}
}

// TestDepsCompleteGaugeReportsDwellAndLatchingDriver covers the three
// acceptance criteria as behavior: the flag is readable through Stats (no
// debugger), its dwell is reported, and the driver names the site that latched
// the current degradation rather than the last one to re-assert it.
func TestDepsCompleteGaugeReportsDwellAndLatchingDriver(t *testing.T) {
	c := newCachingStore(nil, "vc", nil)

	// A fresh cache has no dep projection, and says so — dwell is measured
	// from construction, not from the first flip.
	if s := c.Stats(); s.DepsComplete {
		t.Fatal("a newly constructed cache must not claim a complete dep projection")
	} else if s.DepsIncompleteSince.IsZero() {
		t.Fatal("DepsIncompleteSince must be stamped at construction so dwell is measurable from process start")
	}

	c.mu.Lock()
	c.setDepsCompleteLocked(true, "")
	c.mu.Unlock()

	s := c.Stats()
	if !s.DepsComplete {
		t.Fatal("DepsComplete = false after the flag was set true")
	}
	if !s.DepsIncompleteSince.IsZero() || s.DepsIncompleteFor != 0 || s.DepsIncompleteDriver != "" {
		t.Fatalf("a complete projection must report no live degradation; got since=%v for=%v driver=%q",
			s.DepsIncompleteSince, s.DepsIncompleteFor, s.DepsIncompleteDriver)
	}
	if s.DepsRestorations != 1 {
		t.Fatalf("DepsRestorations = %d, want 1", s.DepsRestorations)
	}

	c.mu.Lock()
	c.setDepsCompleteLocked(false, "event-updated-deps-unknown")
	latchedAt := c.depsIncompleteSince
	// Re-assert false from a different site: this must not restart the dwell
	// or re-attribute the latch.
	c.setDepsCompleteLocked(false, "reconcile-row-degraded")
	c.mu.Unlock()

	s = c.Stats()
	if s.DepsComplete {
		t.Fatal("DepsComplete = true after degradation")
	}
	if s.DepsIncompleteDriver != "event-updated-deps-unknown" {
		t.Fatalf("DepsIncompleteDriver = %q, want the site that LATCHED the degradation", s.DepsIncompleteDriver)
	}
	if !s.DepsIncompleteSince.Equal(latchedAt) {
		t.Fatalf("re-asserting false restarted the dwell clock: since=%v want=%v", s.DepsIncompleteSince, latchedAt)
	}
	if s.DepsDegradations != 1 {
		t.Fatalf("DepsDegradations = %d, want 1 — a no-op re-assert must not count as a transition", s.DepsDegradations)
	}
	if s.DepsIncompleteFor <= 0 {
		t.Fatalf("DepsIncompleteFor = %v, want a positive dwell", s.DepsIncompleteFor)
	}
}

// TestDepsIncompleteSelectsWholeCacheBranchAndCountsIt is the mechanism ADR-0094
// D6 names: with depsComplete false, invalidating ONE bead's dependents
// invalidates the entire cache. The counter is what makes that blast radius
// reportable next to the flag.
func TestDepsIncompleteSelectsWholeCacheBranchAndCountsIt(t *testing.T) {
	c := newCachingStore(nil, "vc", nil)

	blocked := false
	c.mu.Lock()
	c.state = cacheLive
	for _, id := range []string{"vc-1", "vc-2", "vc-3"} {
		c.beads[id] = Bead{ID: id, Status: "open", IsBlocked: &blocked}
	}
	// vc-1 is the only row with a recorded edge on vc-999.
	c.deps["vc-1"] = []Dep{{IssueID: "vc-1", DependsOnID: "vc-999", Type: "blocks"}}
	c.setDepsCompleteLocked(true, "")
	c.clearDependentReadyProjectionsLocked("vc-999")

	narrowlyKept := 0
	for _, id := range []string{"vc-2", "vc-3"} {
		if c.beads[id].IsBlocked != nil {
			narrowlyKept++
		}
	}
	wipesAfterNarrow := c.depsWholeCacheWipes
	c.mu.Unlock()

	if narrowlyKept != 2 {
		t.Fatalf("with a complete dep projection only vc-1's dependents should be invalidated; %d/2 unrelated rows kept their verdict", narrowlyKept)
	}
	if wipesAfterNarrow != 0 {
		t.Fatalf("depsWholeCacheWipes = %d after a narrow invalidation, want 0", wipesAfterNarrow)
	}

	c.mu.Lock()
	for _, id := range []string{"vc-1", "vc-2", "vc-3"} {
		b := c.beads[id]
		b.IsBlocked = &blocked
		c.beads[id] = b
	}
	c.setDepsCompleteLocked(false, "event-updated-deps-unknown")
	c.clearDependentReadyProjectionsLocked("vc-999")

	stillHeld := 0
	for _, id := range []string{"vc-1", "vc-2", "vc-3"} {
		if c.beads[id].IsBlocked != nil {
			stillHeld++
		}
	}
	wipes := c.depsWholeCacheWipes
	c.mu.Unlock()

	if stillHeld != 0 {
		t.Fatalf("degraded projection must invalidate every row; %d/3 rows kept a verdict", stillHeld)
	}
	if wipes != 1 {
		t.Fatalf("depsWholeCacheWipes = %d, want 1", wipes)
	}
	if s := c.Stats(); s.DepsWholeCacheWipes != 1 {
		t.Fatalf("Stats().DepsWholeCacheWipes = %d, want 1", s.DepsWholeCacheWipes)
	}
}

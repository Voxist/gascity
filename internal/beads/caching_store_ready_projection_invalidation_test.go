package beads

import (
	"context"
	"encoding/json"
	"sort"
	"testing"
)

// floodFixture builds a cache whose rows carry a settled is_blocked verdict
// that AGREES with the dependency graph behind it. Agreement is the point: it
// makes falling back to the dependency predicate a no-op, so any change in
// Ready across an invalidation is a real regression rather than the fixture
// contradicting itself.
//
// The shape covers the three row kinds ADR-0094 acceptance 4 names: blocked
// (verdict true, open blocking edge), unblocked (verdict false, no edge), and
// unprojected (no verdict at all, the row the invalidation cannot mark).
type floodFixture struct {
	cache     *CachingStore
	blockerID string
	events    *[]string
}

func newFloodFixture(t *testing.T) floodFixture {
	t.Helper()
	// Four of each kind: enough that a whole-store re-emit is unmistakable in
	// the event log, small enough to read. No test needs a different width.
	const perKind = 4

	mem := NewMemStore()
	unblocked := false
	blocker, err := mem.Create(Bead{Title: "blocker", IsBlocked: &unblocked})
	if err != nil {
		t.Fatalf("Create blocker: %v", err)
	}
	for i := 0; i < perKind; i++ {
		blocked := true
		dep, err := mem.Create(Bead{Title: "blocked", IsBlocked: &blocked})
		if err != nil {
			t.Fatalf("Create blocked %d: %v", i, err)
		}
		if err := mem.DepAdd(dep.ID, blocker.ID, "blocks"); err != nil {
			t.Fatalf("DepAdd %s: %v", dep.ID, err)
		}

		free := false
		if _, err := mem.Create(Bead{Title: "unblocked", IsBlocked: &free}); err != nil {
			t.Fatalf("Create unblocked %d: %v", i, err)
		}
		if _, err := mem.Create(Bead{Title: "unprojected"}); err != nil {
			t.Fatalf("Create unprojected %d: %v", i, err)
		}
	}

	events := make([]string, 0, 64)
	cache := NewCachingStoreForTest(mem, func(eventType, beadID string, _ json.RawMessage) {
		events = append(events, eventType+":"+beadID)
	})
	if err := cache.Prime(context.Background()); err != nil {
		t.Fatalf("Prime: %v", err)
	}
	return floodFixture{cache: cache, blockerID: blocker.ID, events: &events}
}

// tickAndCollectUpdates runs one reconcile pass over an unchanged backing and
// returns the ids that re-emitted bead.updated.
func (f floodFixture) tickAndCollectUpdates() []string {
	*f.events = (*f.events)[:0]
	f.cache.runReconciliation()
	updated := make([]string, 0, len(*f.events))
	for _, e := range *f.events {
		if len(e) > len("bead.updated:") && e[:len("bead.updated:")] == "bead.updated:" {
			updated = append(updated, e[len("bead.updated:"):])
		}
	}
	sort.Strings(updated)
	return updated
}

// TestReconcileEmitsNoUpdatesOnUnchangedStore is ADR-0094 D4(a): a reconcile
// pass over a store nothing has touched must emit no bead.updated at all.
//
// This is the quiescent half of the guard. It is necessary but, on its own,
// insufficient — it is the shape of guard that already existed when ga-ocypq2
// shipped, and it stayed green through the flood this bead fixes. D4(b) below
// is the half with teeth.
func TestReconcileEmitsNoUpdatesOnUnchangedStore(t *testing.T) {
	t.Parallel()

	f := newFloodFixture(t)
	for tick := 1; tick <= 10; tick++ {
		if updated := f.tickAndCollectUpdates(); len(updated) != 0 {
			t.Fatalf("tick %d over an unchanged store emitted bead.updated for %v, want none", tick, updated)
		}
	}
}

// TestReconcileEmitsNoUpdatesAfterWholeCacheInvalidation is ADR-0094 D4(b) —
// the case that would have caught both this defect and ga-ocypq2.
//
// With depsComplete false, clearDependentReadyProjectionsLocked takes its
// whole-cache branch and invalidates every resident row's is_blocked verdict.
// The old code recorded that by writing nil into c.beads[id].IsBlocked, which
// the reconcile differ could not distinguish from the backing genuinely
// reporting nil: the next pass compared a settled fresh verdict against the
// wiped cached one, beadChanged fired, and every row in the store re-emitted
// bead.updated — the ~450-661/tick flood. Invalidation is not a value, so the
// row keeps what the backing last reported and the pass stays silent.
//
// The assertion runs over consecutive ticks on purpose. A single tick cannot
// see D3's latch: stamping localBeadAt for the whole store made every row look
// recently locally mutated, which drove mergeSkipRecentLocal →
// degradeDepsComplete → depsComplete latched false, so the next tick took the
// whole-cache branch again and the flood sustained itself rather than decaying.
func TestReconcileEmitsNoUpdatesAfterWholeCacheInvalidation(t *testing.T) {
	t.Parallel()

	f := newFloodFixture(t)

	// Drain the settled state first: a dirty pass here would mask the flood.
	if updated := f.tickAndCollectUpdates(); len(updated) != 0 {
		t.Fatalf("fixture was not quiescent before invalidation: %v", updated)
	}

	f.cache.mu.Lock()
	f.cache.depsComplete = false
	beforeObservation := f.cache.observationRevision
	if !f.cache.clearDependentReadyProjectionsLocked(f.blockerID) {
		f.cache.mu.Unlock()
		t.Fatal("clearDependentReadyProjectionsLocked reported no invalidation; the fixture no longer exercises the whole-cache branch")
	}
	// Non-vacuity probe, re-expressed for upstream's observation fence
	// (2026-08-31 resync). The fork recorded invalidation as a per-id set
	// (readyProjectionInvalid) and this line counted it; upstream solves the
	// same ADR-0094 problem — keeping invalidation out of the mutation fences
	// the reconcile differ reads — with a global observationRevision counter.
	// The counter is strictly coarser (it invalidates every observation, not
	// just the affected ids), so it satisfies this guard's premise a fortiori.
	// Restoring the fork's set alongside it would double-fence the same event.
	// The ten-tick assertion below is mechanism-independent and unchanged.
	afterObservation := f.cache.observationRevision
	f.cache.mu.Unlock()

	if afterObservation == beforeObservation {
		t.Fatal("observationRevision did not advance, so nothing was invalidated and this guard would pass vacuously")
	}

	for tick := 1; tick <= 10; tick++ {
		if updated := f.tickAndCollectUpdates(); len(updated) != 0 {
			t.Fatalf("tick %d after whole-cache invalidation emitted bead.updated for %v, want none (ADR-0094: invalidation must not read as a store-side change)", tick, updated)
		}
	}
}

// TestWholeCacheInvalidationDoesNotFenceEveryRow is ADR-0094 D3. A
// cache-internal invalidation is not a mutation of the rows, so it must not
// stamp them.
//
// The stamp that mattered is beadSeq, the reconcile fence.
// noteMutationLocked(cleared...) bumped mutationSeq once and wrote that seq to
// every resident row; on the next pass every row then satisfied
// beadAtSeq > startSeq, so reconcileMergeDecision returned mergeSkipFenced
// with degradeDepsComplete for any row lacking cached deps. depsComplete
// latched false, clearDependentReadyProjectionsLocked kept selecting its
// whole-cache branch, and the flood sustained itself instead of decaying —
// which is why 13:00Z was a step rather than a spike.
//
// Asserting the absence of the stamp names that latch directly, so a
// reintroduction fails here rather than as a slow flood downstream.
func TestWholeCacheInvalidationDoesNotFenceEveryRow(t *testing.T) {
	t.Parallel()

	f := newFloodFixture(t)

	f.cache.mu.Lock()
	f.cache.depsComplete = false
	seqBefore := f.cache.mutationSeq
	fenceBefore := cloneU64Map(f.cache.beadSeq)
	if !f.cache.clearAllReadyProjectionsLocked() {
		f.cache.mu.Unlock()
		t.Fatal("clearAllReadyProjectionsLocked invalidated nothing; the guard would pass vacuously")
	}
	seqAfter := f.cache.mutationSeq
	fenceAfter := cloneU64Map(f.cache.beadSeq)
	f.cache.mu.Unlock()

	if seqAfter != seqBefore {
		t.Fatalf("mutationSeq advanced %d -> %d across a whole-cache invalidation; invalidation is not a mutation (ADR-0094 D3)", seqBefore, seqAfter)
	}
	for id, after := range fenceAfter {
		if before, ok := fenceBefore[id]; !ok || before != after {
			t.Fatalf("beadSeq[%s] = %d (was %d, present=%v): a whole-cache invalidation fenced the row, which latches depsComplete false on the next pass (ADR-0094 D3)", id, after, before, ok)
		}
	}
}

// TestWholeCacheInvalidationPreservesReadyResults is ADR-0094 acceptance 4:
// moving the invalidation out of band must not change which beads are ready.
// The readers consult the mark exactly where they used to observe the nil
// sentinel, so an invalidated row still falls back to the dependency predicate.
func TestWholeCacheInvalidationPreservesReadyResults(t *testing.T) {
	t.Parallel()

	f := newFloodFixture(t)

	before := readyIDs(t, f.cache)

	f.cache.mu.Lock()
	f.cache.depsComplete = false
	f.cache.clearDependentReadyProjectionsLocked(f.blockerID)
	f.cache.mu.Unlock()

	if after := readyIDs(t, f.cache); !slicesEqualStr(after, before) {
		t.Fatalf("Ready ids after invalidation = %v, want unchanged %v", after, before)
	}
}

// projectionStrippingStore drops the is_blocked column from List results on
// demand, standing in for the bounded ready-projection query failing or
// racing while the plain list still succeeds — the exact condition
// preserveCachedReadyProjectionLocked exists to smooth over.
type projectionStrippingStore struct {
	Store
	strip bool
}

func (s *projectionStrippingStore) List(query ListQuery) ([]Bead, error) {
	rows, err := s.Store.List(query)
	if err != nil || !s.strip {
		return rows, err
	}
	out := make([]Bead, 0, len(rows))
	for _, b := range rows {
		b.IsBlocked = nil
		out = append(out, b)
	}
	return out, nil
}

// TestProjectionPreservationDoesNotResurrectInvalidatedVerdict guards the
// interaction ADR-0094 D1 widens.
//
// preserveCachedReadyProjectionLocked restores a cached is_blocked whenever the
// projection did not return the row, so a transient projection failure does not
// flip the verdict to nil and emit a spurious update. It skips that restore
// when the row's deps moved or a blocking target's status changed — but the
// local write that closes a blocker updates the blocker's cached status
// *before* reconcile runs, so by then cached and fresh agree and neither guard
// fires.
//
// Under the old in-band sentinel that was harmless: invalidation had already
// nil'd the dependent's cached verdict, so preservation found nothing to
// restore and readiness fell back to the dependency predicate. Now that the row
// retains what the backing last reported, preservation can put a verdict
// computed against the *pre-close* graph back into service and — because an
// absorb discharges the invalidation — mark it observed. Preservation is not
// observation, so a row awaiting re-observation must be left alone.
func TestProjectionPreservationDoesNotResurrectInvalidatedVerdict(t *testing.T) {
	t.Parallel()

	mem := NewMemStore()
	blocker, err := mem.Create(Bead{Title: "blocker"})
	if err != nil {
		t.Fatalf("Create blocker: %v", err)
	}
	blocked := true
	dependent, err := mem.Create(Bead{Title: "dependent", IsBlocked: &blocked})
	if err != nil {
		t.Fatalf("Create dependent: %v", err)
	}
	if err := mem.DepAdd(dependent.ID, blocker.ID, "blocks"); err != nil {
		t.Fatalf("DepAdd: %v", err)
	}

	backing := &projectionStrippingStore{Store: mem}
	cache := NewCachingStoreForTest(backing, func(string, string, json.RawMessage) {})
	if err := cache.Prime(context.Background()); err != nil {
		t.Fatalf("Prime: %v", err)
	}

	// Close the blocker through the cache: this invalidates the dependent's
	// verdict and updates the blocker's cached status in the same critical
	// section, which is what defeats the status-change guard below.
	closed := "closed"
	if err := cache.Update(blocker.ID, UpdateOpts{Status: &closed}); err != nil {
		t.Fatalf("close blocker: %v", err)
	}
	if ready := readyIDs(t, cache); !slicesContains(ready, dependent.ID) {
		t.Fatalf("dependent %s not ready immediately after its blocker closed; ready = %v", dependent.ID, ready)
	}

	// Now the projection fails for a cycle, so preservation is reached.
	backing.strip = true
	cache.runReconciliation()

	if ready := readyIDs(t, cache); !slicesContains(ready, dependent.ID) {
		t.Fatalf("dependent %s dropped out of ready after a projection-failure reconcile; the stale pre-close verdict was preserved back into service (ADR-0094: preservation is not observation). ready = %v", dependent.ID, ready)
	}
}

func slicesContains(haystack []string, needle string) bool {
	for _, s := range haystack {
		if s == needle {
			return true
		}
	}
	return false
}

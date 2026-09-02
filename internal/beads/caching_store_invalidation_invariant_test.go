package beads

import (
	"context"
	"encoding/json"
	"fmt"
	"testing"
)

// This file is the "once and for all" guard for ADR-0094's served-verdict
// contract. Three consecutive review rounds each found another read path that
// served an invalidated is_blocked verdict, because the previous design kept
// the disowned value IN the row and relied on a per-exit scrub that had to be
// enumerated by hand — an executed inventory then showed ten of eleven escape
// surfaces leaked (the fork's own production code included).
//
// The current design inverts that: invalidation NILS the row (upstream's
// sentinel contract, which every reader inside and outside this repo was
// written against) and retains the disowned value in readyProjectionInvalid
// for the reconcile differ alone. Under that design, "no reader leaks" is not
// a property of each reader — it is a property of c.beads itself. So the guard
// is an INVARIANT on the store, not a checklist of readers:
//
//	every id in readyProjectionInvalid has c.beads[id].IsBlocked == nil
//
// If that holds, no read path present or future can serve a disowned verdict,
// because there is nothing in the row to serve. The surface sweep below is the
// belt on top: it exercises the public read methods end to end so a future
// path that serves from somewhere OTHER than c.beads is still caught.

// assertInvalidatedRowsAreNil walks the invariant under lock.
func assertInvalidatedRowsAreNil(t *testing.T, c *CachingStore, when string) {
	t.Helper()
	c.mu.RLock()
	defer c.mu.RUnlock()
	if len(c.readyProjectionInvalid) == 0 {
		t.Fatalf("%s: nothing is invalidated — this scenario no longer exercises the invariant and passes vacuously", when)
	}
	for id := range c.readyProjectionInvalid {
		row, ok := c.beads[id]
		if !ok {
			continue // evicted rows carry nothing to serve
		}
		if row.IsBlocked != nil {
			t.Fatalf("%s: row %s is marked invalid but still carries IsBlocked=%v in c.beads; "+
				"every reader serving from the row will leak the disowned verdict", when, id, *row.IsBlocked)
		}
	}
}

func newInvalidatedFixture(t *testing.T) (*CachingStore, *MemStore, string, string) {
	t.Helper()
	blocked := true
	backing := NewMemStore()
	blocker, err := backing.Create(Bead{Title: "blocker", Status: "open", Type: "task"})
	if err != nil {
		t.Fatalf("create blocker: %v", err)
	}
	dep, err := backing.Create(Bead{
		Title: "dep", Status: "open", Type: "task",
		Needs: []string{blocker.ID}, IsBlocked: &blocked,
	})
	if err != nil {
		t.Fatalf("create dep: %v", err)
	}
	cache := NewCachingStoreForTest(backing, nil)
	if err := cache.Prime(context.Background()); err != nil {
		t.Fatalf("prime: %v", err)
	}
	return cache, backing, blocker.ID, dep.ID
}

// TestInvalidationNilsTheRowAcrossMutationFamilies pins the invariant on every
// mutation family that raises an invalidation.
func TestInvalidationNilsTheRowAcrossMutationFamilies(t *testing.T) {
	t.Parallel()

	t.Run("close event, targeted branch", func(t *testing.T) {
		cache, backing, blockerID, _ := newInvalidatedFixture(t)
		if err := backing.Close(blockerID); err != nil {
			t.Fatalf("close: %v", err)
		}
		payload, _ := json.Marshal(map[string]string{"id": blockerID, "status": "closed"})
		cache.ApplyEvent("bead.closed", payload)
		assertInvalidatedRowsAreNil(t, cache, "after bead.closed")
	})

	t.Run("whole-cache branch (depsComplete=false)", func(t *testing.T) {
		cache, _, blockerID, _ := newInvalidatedFixture(t)
		cache.mu.Lock()
		cache.depsComplete = false
		changed := cache.clearDependentReadyProjectionsLocked(blockerID)
		cache.mu.Unlock()
		if !changed {
			t.Fatal("whole-cache branch invalidated nothing; scenario is vacuous")
		}
		assertInvalidatedRowsAreNil(t, cache, "after whole-cache invalidation")
	})

	t.Run("local write via Update", func(t *testing.T) {
		cache, _, blockerID, _ := newInvalidatedFixture(t)
		closed := "closed"
		if err := cache.Update(blockerID, UpdateOpts{Status: &closed}); err != nil {
			t.Fatalf("update: %v", err)
		}
		assertInvalidatedRowsAreNil(t, cache, "after local status close")
	})
}

// TestNoReadSurfaceServesADisownedVerdict is the belt: every public read
// method, exercised end to end against an invalidated row, must hand back
// IsBlocked == nil for it.
func TestNoReadSurfaceServesADisownedVerdict(t *testing.T) {
	t.Parallel()

	cache, backing, blockerID, depID := newInvalidatedFixture(t)
	if err := backing.Close(blockerID); err != nil {
		t.Fatalf("close: %v", err)
	}
	payload, _ := json.Marshal(map[string]string{"id": blockerID, "status": "closed"})
	cache.ApplyEvent("bead.closed", payload)
	assertInvalidatedRowsAreNil(t, cache, "fixture")

	check := func(t *testing.T, surface string, rows []Bead, err error) {
		t.Helper()
		if err != nil {
			t.Fatalf("%s: %v", surface, err)
		}
		seen := false
		for _, b := range rows {
			if b.ID != depID {
				continue
			}
			seen = true
			if b.IsBlocked != nil {
				t.Fatalf("%s served the disowned verdict (IsBlocked=%v) for %s", surface, *b.IsBlocked, depID)
			}
		}
		if !seen {
			t.Logf("%s: row %s not in result (surface may legitimately exclude it)", surface, depID)
		}
	}

	checkOne := func(t *testing.T, surface string, b Bead, err error) {
		t.Helper()
		check(t, surface, []Bead{b}, err)
	}

	{
		b, err := cache.Get(depID)
		checkOne(t, "Get", b, err)
	}
	{
		rows, err := listAll(cache.List)
		check(t, "List", rows, err)
	}
	{
		rows, err := listAll(func(q ListQuery) ([]Bead, error) { return cache.ListCtx(context.Background(), q) })
		check(t, "ListCtx", rows, err)
	}
	{
		rows, err := cache.ListOpen()
		check(t, "ListOpen", rows, err)
	}
	{
		rows, err := cache.ListByAssignee("", "open", 0)
		check(t, "ListByAssignee", rows, err)
	}
	{
		rows, err := cache.Ready()
		check(t, "Ready", rows, err)
	}
	if rows, ok := cache.CachedReady(); ok {
		check(t, "CachedReady", rows, nil)
	}
	h := cache.Handles()
	{
		b, err := h.Cached.Get(depID)
		checkOne(t, "Handles.Cached.Get", b, err)
	}
	{
		rows, err := listAll(h.Cached.List)
		check(t, "Handles.Cached.List", rows, err)
	}
	{
		rows, err := h.Cached.Ready()
		check(t, "Handles.Cached.Ready", rows, err)
	}
	// Live surfaces bypass the cache entirely (liveStoreReader.Get is
	// backing.Get verbatim), so "no disowned verdict" is not their contract —
	// backing truth is, and against bd/Dolt the backing recomputes is_blocked
	// itself. What the cache owes a live read is NON-INTERFERENCE: it must
	// serve exactly what the backing serves, injecting nothing. (This fixture's
	// MemStore does not maintain the projection, which is precisely why
	// asserting nil here would test the fixture rather than the cache.)
	{
		live, err := h.Live.Get(depID)
		if err != nil {
			t.Fatalf("Handles.Live.Get: %v", err)
		}
		raw, err := backing.Get(depID)
		if err != nil {
			t.Fatalf("backing.Get: %v", err)
		}
		liveV, rawV := "nil", "nil"
		if live.IsBlocked != nil {
			liveV = fmt.Sprint(*live.IsBlocked)
		}
		if raw.IsBlocked != nil {
			rawV = fmt.Sprint(*raw.IsBlocked)
		}
		if liveV != rawV {
			t.Fatalf("Handles.Live.Get IsBlocked=%s but backing says %s; the live surface must be a pass-through, not a cache projection", liveV, rawV)
		}
	}
}

func listAll(list func(ListQuery) ([]Bead, error)) ([]Bead, error) {
	rows, err := list(ListQuery{AllowScan: true})
	if err != nil {
		return nil, fmt.Errorf("listAll: %w", err)
	}
	return rows, nil
}

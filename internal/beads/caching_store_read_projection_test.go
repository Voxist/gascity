package beads

import (
	"context"
	"encoding/json"
	"testing"
	"time"
)

// TestGetWithholdsInvalidatedReadyProjection pins the ADR-0094 read boundary.
//
// The invalidated verdict is deliberately KEPT in c.beads so the reconcile
// differ compares what the backing last reported (that is what stops the
// bead.updated flood). It must not be SERVED: readers outside this package take
// Bead.IsBlocked as the backing's answer, and several treat nil as their
// fail-open case — bindNamedSessionTriggerBead's staleness test,
// computeAwakeBridge's blocked test, and gc bead state. Serving a verdict the
// cache has already disowned makes them act on a value the cache does not
// trust.
//
// Without the scrub this test observes IsBlocked == true after the only blocker
// closed, where the pre-ADR-0094 in-band sentinel returned nil.
func TestGetWithholdsInvalidatedReadyProjection(t *testing.T) {
	t.Parallel()

	blockedProjection := true
	backing := NewMemStore()
	blocker, err := backing.Create(Bead{Title: "blocker", Status: "open", Type: "task"})
	if err != nil {
		t.Fatalf("create blocker: %v", err)
	}
	blocked, err := backing.Create(Bead{
		Title: "blocked", Status: "open", Type: "task",
		Needs: []string{blocker.ID}, IsBlocked: &blockedProjection,
	})
	if err != nil {
		t.Fatalf("create blocked: %v", err)
	}

	cache := NewCachingStoreForTest(backing, nil)
	if err := cache.Prime(context.Background()); err != nil {
		t.Fatalf("prime: %v", err)
	}
	before, err := cache.Get(blocked.ID)
	if err != nil {
		t.Fatalf("get before: %v", err)
	}
	if before.IsBlocked == nil || !*before.IsBlocked {
		t.Fatalf("fixture did not seat a blocked projection (IsBlocked=%v); this guard would pass vacuously", before.IsBlocked)
	}

	if err := backing.Close(blocker.ID); err != nil {
		t.Fatalf("close blocker: %v", err)
	}
	payload, err := json.Marshal(map[string]string{"id": blocker.ID, "status": "closed"})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	cache.ApplyEvent("bead.closed", payload)

	after, err := cache.Get(blocked.ID)
	if err != nil {
		t.Fatalf("get after: %v", err)
	}
	if after.IsBlocked != nil {
		t.Fatalf("Get served an invalidated is_blocked verdict (%v) after the only blocker closed; "+
			"readers outside this package treat nil as fail-open and will act on a value the cache has disowned",
			*after.IsBlocked)
	}

	// The verdict must still be RESIDENT, or the differ loses its like-for-like
	// comparison and the bead.updated flood returns.
	cache.mu.RLock()
	stored, ok := cache.beads[blocked.ID]
	invalid := cache.readyProjectionInvalidLocked(blocked.ID)
	cache.mu.RUnlock()
	if !ok {
		t.Fatal("row evicted; cannot assert the stored verdict")
	}
	// OPTION 2: residency moved to the side map — the ROW is nil'd (safe by
	// default for every reader) and the differ substitutes the disowned value.
	if stored.IsBlocked != nil {
		t.Fatal("row verdict was not nil-ed; under Option 2 the row is the sentinel")
	}
	if v, ok2 := cache.readyProjectionInvalid[blocked.ID]; !ok2 || !v {
		t.Fatal("disowned value not retained in readyProjectionInvalid; the reconcile differ would see nil->set and flood")
	}
	if !invalid {
		t.Fatal("row is not marked invalid; the scrub above would be untested")
	}
}

// TestVerdictlessAbsorbDoesNotDischargeInvalidation pins the ADR-0094
// discharge rule under the nil-the-row design: an absorb discharges the
// invalidation ONLY when the incoming row actually carries an is_blocked
// verdict.
//
// This matters because an invalidated row is nil'd, and every cache-seeded
// merge (mergeCacheEventPatch on an event that lacks is_blocked) therefore
// arrives verdict-LESS — it observed nothing and must leave both the mark and
// the disowned value in place, or unrelated traffic silently cancels a pending
// re-ask and the differ loses the value it substitutes. Only a row that
// genuinely carries the field (a backing read, a graph apply, an event with
// is_blocked) may discharge.
func TestVerdictlessAbsorbDoesNotDischargeInvalidation(t *testing.T) {
	t.Parallel()

	seed := func(t *testing.T) (*CachingStore, string) {
		t.Helper()
		blockedProjection := true
		backing := NewMemStore()
		b, err := backing.Create(Bead{
			Title: "blocked", Status: "open", Type: "task", IsBlocked: &blockedProjection,
		})
		if err != nil {
			t.Fatalf("create: %v", err)
		}
		cache := NewCachingStoreForTest(backing, nil)
		if err := cache.Prime(context.Background()); err != nil {
			t.Fatalf("prime: %v", err)
		}
		cache.mu.Lock()
		if !cache.clearReadyProjectionLocked(b.ID) {
			cache.mu.Unlock()
			t.Fatal("fixture row had no verdict to invalidate; this guard would pass vacuously")
		}
		cache.mu.Unlock()
		return cache, b.ID
	}

	t.Run("verdict-less absorb keeps the mark", func(t *testing.T) {
		cache, id := seed(t)
		// What a cache-seeded merge produces for an invalidated row: nil.
		cache.mu.RLock()
		row := cloneBead(cache.beads[id])
		cache.mu.RUnlock()
		if row.IsBlocked != nil {
			t.Fatal("invalidated row still carries a verdict; the nil-the-row invariant is broken and this guard is mistargeted")
		}
		cache.mu.Lock()
		cache.absorbFreshLocked(id, row, time.Now(), absorbOpts{
			depsMode: depsKeepCached, seqMode: seqKeep, clearDirty: true,
		})
		_, still := cache.readyProjectionInvalid[id]
		cache.mu.Unlock()
		if !still {
			t.Fatal("a verdict-less absorb discharged the invalidation; unrelated traffic can now cancel a pending re-ask")
		}
	})

	t.Run("verdict-carrying absorb discharges", func(t *testing.T) {
		cache, id := seed(t)
		observed := false
		cache.mu.Lock()
		cache.absorbFreshLocked(id, Bead{ID: id, Title: "blocked", Status: "open", Type: "task", IsBlocked: &observed},
			time.Now(), absorbOpts{depsMode: depsKeepCached, seqMode: seqKeep, clearDirty: true})
		_, still := cache.readyProjectionInvalid[id]
		cache.mu.Unlock()
		if still {
			t.Fatal("a genuine observation did not discharge the invalidation; the mark would never clear")
		}
	})
}

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
	if stored.IsBlocked == nil {
		t.Fatal("stored verdict was nil-ed; ADR-0094 requires it to stay resident so the reconcile differ compares like with like")
	}
	if !invalid {
		t.Fatal("row is not marked invalid; the scrub above would be untested")
	}
}

// TestUnobservedAbsorbDoesNotDischargeInvalidation pins the second half of the
// ADR-0094 discharge rule: only an absorb that OBSERVED an is_blocked value may
// clear the invalidation mark.
//
// This is a unit test on absorbFreshLocked rather than an ApplyEvent scenario
// deliberately. The hazard is structural: mergeCacheEventPatch seeds the merged
// row from the CACHED row and overwrites IsBlocked only when the event carried
// is_blocked, so an absorb can receive a non-nil, authoritative-LOOKING verdict
// that observed nothing. Inferring observation from the value alone therefore
// lets unrelated traffic silently cancel a pending re-ask. Driving that through
// ApplyEvent is unreliable (the conflict guards drop most such events before
// they reach the absorb), and a test that cannot steer the path is a test that
// passes vacuously — so the rule is asserted where it lives.
func TestUnobservedAbsorbDoesNotDischargeInvalidation(t *testing.T) {
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
		cache.markReadyProjectionInvalidLocked(b.ID)
		cache.mu.Unlock()
		return cache, b.ID
	}

	// A cache-seeded verdict: non-nil, but copied rather than observed.
	cacheSeeded := func(c *CachingStore, id string) Bead {
		c.mu.RLock()
		defer c.mu.RUnlock()
		return cloneBead(c.beads[id])
	}

	t.Run("unobserved absorb keeps the mark", func(t *testing.T) {
		cache, id := seed(t)
		row := cacheSeeded(cache, id)
		if row.IsBlocked == nil {
			t.Fatal("fixture row carries no verdict; this guard would pass vacuously")
		}
		cache.mu.Lock()
		cache.absorbFreshLocked(id, row, time.Now(), absorbOpts{
			depsMode: depsKeepCached, seqMode: seqKeep, clearDirty: true,
			readyProjectionUnobserved: true,
		})
		still := cache.readyProjectionInvalidLocked(id)
		cache.mu.Unlock()
		if !still {
			t.Fatal("an absorb that observed nothing discharged the invalidation; " +
				"a cache-seeded verdict must not read as an observation")
		}
	})

	t.Run("observed absorb discharges the mark", func(t *testing.T) {
		cache, id := seed(t)
		row := cacheSeeded(cache, id)
		cache.mu.Lock()
		cache.absorbFreshLocked(id, row, time.Now(), absorbOpts{
			depsMode: depsKeepCached, seqMode: seqKeep, clearDirty: true,
		})
		still := cache.readyProjectionInvalidLocked(id)
		cache.mu.Unlock()
		if still {
			t.Fatal("a genuine observation did not discharge the invalidation; the mark would never clear")
		}
	})
}

package beads

import (
	"context"
	"testing"
	"time"
)

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

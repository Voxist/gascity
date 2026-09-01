package beads

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"
)

// Round-4 findings #3/#4: the resync dropped error-path pass-throughs from the
// degraded-read fallbacks, so budget expiry, typed partials and cancellation
// were silently converted into stale-but-nil-error answers — a wedged backend
// presenting as a healthy store. These guards pin the fork's original
// contracts.

type blockUntilCtxStore struct {
	*MemStore
}

func (s *blockUntilCtxStore) ListCtx(ctx context.Context, q ListQuery) ([]Bead, error) {
	<-ctx.Done()
	return nil, ctx.Err()
}

// TestListCtxDeadlineExpiryIsNotMaskedByLastGood: a deadline that expires
// DURING the backing read must surface, not be answered from the snapshot with
// a nil error. Count kept this guard through the resync; List lost it —
// reproduced as rows=1 err=nil before the fix.
func TestListCtxDeadlineExpiryIsNotMaskedByLastGood(t *testing.T) {
	t.Parallel()

	mem := NewMemStore()
	b, err := mem.Create(Bead{Title: "row", Status: "open", Type: "task"})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	backing := &blockUntilCtxStore{MemStore: mem}
	cache := NewCachingStoreForTest(backing, nil)
	if err := cache.Prime(context.Background()); err != nil {
		t.Fatalf("prime: %v", err)
	}
	_ = b
	// Primed-but-degraded, in the one shape where the snapshot serves CLEAN
	// (no partial tag): the last-good is intact, but more rows are dirty than
	// the per-read overlay budget (dirtyOverlayMaxGets), so the cache refuses
	// the overlay, the read goes to the backing, the deadline expires there —
	// and the fallback must now choose between the deadline error and clean
	// stale rows with a nil error. (A partial-prime fixture does NOT isolate
	// this: its snapshot arrives partial-tagged, so an error surfaces with or
	// without the guard — that variant of this test passed against the broken
	// code.) This is the statusListStoreWithTimeout budget shape.
	cache.mu.Lock()
	for i := 0; i <= dirtyOverlayMaxGets; i++ {
		cache.dirty[fmt.Sprintf("gc-dirty-%d", i)] = struct{}{}
	}
	cache.mu.Unlock()

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	rows, err := cache.ListCtx(ctx, ListQuery{AllowScan: true})
	if err == nil {
		t.Fatalf("deadline-expired ListCtx returned %d stale rows with a NIL error; "+
			"the caller's budget fired and was swallowed — a wedged backend presents as healthy", len(rows))
	}
	if !errors.Is(err, context.DeadlineExceeded) && ctx.Err() == nil {
		t.Fatalf("err = %v; want the caller's deadline surfaced", err)
	}
}

type getFailStore struct {
	*MemStore
	err error
}

func (s *getFailStore) Get(id string) (Bead, error) {
	if s.err != nil {
		return Bead{}, s.err
	}
	return s.MemStore.Get(id)
}

// TestGetFallbackPassesThroughCancellationAndMapsTombstones pins the fork
// semantics of getBackingOrLastGood (authored in the resync as a rename that
// silently dropped them):
//   - context.Canceled passes through — the caller's own budget, never
//     converted into a stale-with-nil answer
//   - a last-good tombstone answers ErrNotFound, not the raw transport error
func TestGetFallbackPassesThroughCancellationAndMapsTombstones(t *testing.T) {
	t.Parallel()

	t.Run("cancellation passes through", func(t *testing.T) {
		mem := NewMemStore()
		b, _ := mem.Create(Bead{Title: "row", Status: "open", Type: "task"})
		backing := &getFailStore{MemStore: mem, err: context.Canceled}
		cache := NewCachingStoreForTest(backing, nil)
		if err := cache.Prime(context.Background()); err != nil {
			t.Fatalf("prime: %v", err)
		}
		_, err := cache.getBackingOrLastGood(b.ID)
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("err = %v; a cancelled backing read was answered from last-good — the cancellation budget is swallowed", err)
		}
	})

	t.Run("tombstoned id answers ErrNotFound during an outage", func(t *testing.T) {
		mem := NewMemStore()
		b, err := mem.Create(Bead{Title: "doomed", Status: "open", Type: "task"})
		if err != nil {
			t.Fatalf("create: %v", err)
		}
		backing := &getFailStore{MemStore: mem}
		cache := NewCachingStoreForTest(backing, nil)
		if err := cache.Prime(context.Background()); err != nil {
			t.Fatalf("prime: %v", err)
		}
		// Tombstone THROUGH the cache while the store is healthy, so last-good
		// authoritatively knows the id is gone...
		if err := cache.Delete(b.ID); err != nil {
			t.Fatalf("delete: %v", err)
		}
		// ...then the outage begins.
		backing.err = errors.New("dial tcp: connection refused")
		if _, err := cache.getBackingOrLastGood(b.ID); !errors.Is(err, ErrNotFound) {
			t.Fatalf("err = %v; a locally-tombstoned id must answer the authoritative ErrNotFound during an outage, "+
				"not the raw transport error that spins ErrNotFound-terminal callers", err)
		}
	})
}

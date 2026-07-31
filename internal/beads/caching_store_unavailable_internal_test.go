package beads

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"
)

// fakeAvailabilityGate is a controllable AvailabilityGate for tests.
type fakeAvailabilityGate struct {
	mu        sync.Mutex
	available bool
	probeDue  bool
}

func (g *fakeAvailabilityGate) Available() bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.available
}

func (g *fakeAvailabilityGate) ProbeDue() bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.probeDue
}

func (g *fakeAvailabilityGate) set(available, probeDue bool) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.available = available
	g.probeDue = probeDue
}

// callCountingStore counts backing List/Get calls so tests can assert the
// breaker-open path performs zero backing operations.
type callCountingStore struct {
	Store
	mu    sync.Mutex
	lists int
	gets  int
}

func (s *callCountingStore) List(query ListQuery) ([]Bead, error) {
	s.mu.Lock()
	s.lists++
	s.mu.Unlock()
	return s.Store.List(query)
}

func (s *callCountingStore) Get(id string) (Bead, error) {
	s.mu.Lock()
	s.gets++
	s.mu.Unlock()
	return s.Store.Get(id)
}

func (s *callCountingStore) counts() (int, int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.lists, s.gets
}

func newPrimedGatedCache(t *testing.T, beadsIn ...Bead) (*CachingStore, *callCountingStore, *fakeAvailabilityGate) {
	t.Helper()
	backing := &callCountingStore{Store: NewMemStore()}
	for _, b := range beadsIn {
		if _, err := backing.Create(b); err != nil {
			t.Fatalf("Create: %v", err)
		}
	}
	cache := NewCachingStoreForTest(backing, nil)
	if err := cache.Prime(context.Background()); err != nil {
		t.Fatalf("Prime: %v", err)
	}
	gate := &fakeAvailabilityGate{available: true}
	cache.SetAvailabilityGate(gate)
	return cache, backing, gate
}

// TestCachingStoreWarmCacheReadsSurviveAnOpenGate is the regression for the
// third and worst version of this feature.
//
// The availability gate is an input to the RECONCILER, not to reads. Two
// earlier revisions short-circuited above the cached read path — first to serve
// last-good data, then to return ErrStoreUnavailable — and both broke reads
// that needed no backing store at all. A cache primed by PrimeActive answers
// active-bead queries entirely from memory, so gating them converted working
// reads into failures: worse than the previous version AND worse than the
// pre-breaker baseline.
//
// The gate must therefore be invisible here. Reads that the cache can answer
// keep working no matter what the breaker thinks of the transport.
func TestCachingStoreWarmCacheReadsSurviveAnOpenGate(t *testing.T) {
	t.Parallel()
	cache, backing, gate := newPrimedGatedCache(t, Bead{Title: "task-1"}, Bead{Title: "task-2"})

	gate.set(false, false) // breaker open: transport believed down
	listsBefore, getsBefore := backing.counts()

	got, err := cache.List(ListQuery{AllowScan: true})
	if err != nil {
		t.Fatalf("List under an open gate: %v — the active-bead path answers from memory, so the gate must not touch it", err)
	}
	if len(got) != 2 {
		t.Fatalf("List returned %d beads, want 2 from the warm cache", len(got))
	}
	if listsAfter, _ := backing.counts(); listsAfter != listsBefore {
		t.Fatalf("warm-cache List made %d backing call(s); it should need none", listsAfter-listsBefore)
	}

	if _, err := cache.Get(got[0].ID); err != nil {
		t.Fatalf("Get of a cached bead under an open gate: %v — this is a map lookup, not a backing call", err)
	}
	if _, getsAfter := backing.counts(); getsAfter != getsBefore {
		t.Fatalf("cached Get made %d backing call(s); it should need none", getsAfter-getsBefore)
	}
}

// TestCachingStoreGateDoesNotMaskLocallyKnownDeletion pins the other half: a
// bead the cache has already established is gone must still report ErrNotFound,
// not an unknown-state error. Callers branch on ErrNotFound to finish cleanup;
// reporting "unavailable" for a fact we hold locally makes them retry forever.
func TestCachingStoreGateDoesNotMaskLocallyKnownDeletion(t *testing.T) {
	t.Parallel()
	cache, _, gate := newPrimedGatedCache(t, Bead{Title: "task-1"})
	all, err := cache.List(ListQuery{AllowScan: true})
	if err != nil || len(all) == 0 {
		t.Fatalf("prime: List = (%d, %v), want beads", len(all), err)
	}
	id := all[0].ID
	if err := cache.Delete(id); err != nil {
		t.Skipf("backing delete unsupported in this harness: %v", err)
	}

	gate.set(false, false)
	if _, err := cache.Get(id); !errors.Is(err, ErrNotFound) {
		t.Fatalf("Get of a locally-deleted bead under an open gate = %v, want ErrNotFound: the cache already knows this answer", err)
	}
}

func TestCachingStoreReconcileSkipsCycleWhileUnavailable(t *testing.T) {
	t.Parallel()
	cache, backing, gate := newPrimedGatedCache(t, Bead{Title: "task-1"})
	gate.set(false, false)

	listsBefore, _ := backing.counts()
	cache.runReconciliation()
	if listsAfter, _ := backing.counts(); listsAfter != listsBefore {
		t.Fatal("runReconciliation reached the backing store under an open breaker with no probe due")
	}
}

func TestCachingStoreReconcileRunsWhenProbeDue(t *testing.T) {
	t.Parallel()
	cache, backing, gate := newPrimedGatedCache(t, Bead{Title: "task-1"})
	gate.set(false, true) // open, but a recovery probe is due

	listsBefore, _ := backing.counts()
	cache.runReconciliation()
	if listsAfter, _ := backing.counts(); listsAfter == listsBefore {
		t.Fatal("runReconciliation skipped the cycle although a probe was due — the breaker could never recover")
	}
}

func TestCachingStoreReconcileSkipLogsProblemOncePerEpisode(t *testing.T) {
	t.Parallel()
	cache, _, gate := newPrimedGatedCache(t, Bead{Title: "task-1"})
	gate.set(false, false)

	cache.runReconciliation()
	first := cache.Stats().ProblemCount
	if first == 0 {
		t.Fatal("ProblemCount = 0 after an unavailable-skip, want one recorded problem")
	}
	cache.runReconciliation()
	cache.runReconciliation()
	if got := cache.Stats().ProblemCount; got != first {
		t.Fatalf("ProblemCount = %d after repeated skips in one episode, want %d (emit once)", got, first)
	}

	// Recovery closes the episode; the next outage logs again.
	gate.set(true, false)
	cache.runReconciliation()
	gate.set(false, false)
	cache.runReconciliation()
	if got := cache.Stats().ProblemCount; got != first+1 {
		t.Fatalf("ProblemCount = %d after a second episode, want %d", got, first+1)
	}
}

func TestCachingStoreNextReconcileDelaySkipsWhileUnavailable(t *testing.T) {
	t.Parallel()
	cache, _, gate := newPrimedGatedCache(t, Bead{Title: "task-1"})
	gate.set(false, false)
	// Force "due now" conditions, then verify the gate overrides them.
	cache.mu.Lock()
	cache.lastFreshAt = time.Time{}
	cache.mu.Unlock()
	if got := cache.nextReconcileDelay(time.Now()); got <= 0 {
		t.Fatalf("nextReconcileDelay = %v under open breaker, want a positive skip delay", got)
	}
	gate.set(false, true)
	if got := cache.nextReconcileDelay(time.Now()); got != 0 {
		t.Fatalf("nextReconcileDelay = %v with probe due, want 0 (run the probing cycle)", got)
	}
}

func TestErrStoreUnavailableIsDistinct(t *testing.T) {
	t.Parallel()
	for _, other := range []error{ErrNotFound, ErrCacheUnavailable, ErrStoreClosed} {
		if errors.Is(ErrStoreUnavailable, other) || errors.Is(other, ErrStoreUnavailable) {
			t.Fatalf("ErrStoreUnavailable must be distinct from %v", other)
		}
	}
}

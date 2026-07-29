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

func TestCachingStoreListUnavailableServesLastGoodCache(t *testing.T) {
	t.Parallel()
	cache, backing, gate := newPrimedGatedCache(t, Bead{Title: "task-1"}, Bead{Title: "task-2"})

	gate.set(false, false)
	listsBefore, _ := backing.counts()

	got, err := cache.List(ListQuery{AllowScan: true})
	if err != nil {
		t.Fatalf("List under open breaker: %v, want last-good cache", err)
	}
	if len(got) != 2 {
		t.Fatalf("List returned %d beads, want 2 from last-good cache", len(got))
	}
	listsAfter, _ := backing.counts()
	if listsAfter != listsBefore {
		t.Fatalf("backing List called %d times under open breaker, want 0", listsAfter-listsBefore)
	}
	if !cache.Degraded() {
		t.Fatal("Degraded() = false while serving under an open breaker, want true")
	}
	if got := cache.Stats().DegradedReads; got == 0 {
		t.Fatal("Stats().DegradedReads = 0 after a degraded read, want > 0")
	}
}

func TestCachingStoreListUnavailableLiveQueryStaysOnCache(t *testing.T) {
	t.Parallel()
	cache, backing, gate := newPrimedGatedCache(t, Bead{Title: "task-1"})

	gate.set(false, false)
	listsBefore, _ := backing.counts()
	got, err := cache.List(ListQuery{AllowScan: true, Live: true})
	if err != nil {
		t.Fatalf("List(Live) under open breaker: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("List(Live) returned %d beads, want 1", len(got))
	}
	if listsAfter, _ := backing.counts(); listsAfter != listsBefore {
		t.Fatal("Live query reached the backing store under an open breaker")
	}
}

func TestCachingStoreListUnavailableUnprimedReturnsTypedError(t *testing.T) {
	t.Parallel()
	backing := &callCountingStore{Store: NewMemStore()}
	cache := NewCachingStoreForTest(backing, nil)
	gate := &fakeAvailabilityGate{}
	cache.SetAvailabilityGate(gate)

	_, err := cache.List(ListQuery{AllowScan: true})
	if !errors.Is(err, ErrStoreUnavailable) {
		t.Fatalf("List on unprimed cache under open breaker: err = %v, want ErrStoreUnavailable (unavailable must never read as empty)", err)
	}
	if lists, _ := backing.counts(); lists != 0 {
		t.Fatalf("backing List called %d times, want 0", lists)
	}
}

func TestCachingStoreGetUnavailableServesCachedBead(t *testing.T) {
	t.Parallel()
	cache, backing, gate := newPrimedGatedCache(t, Bead{Title: "task-1"})
	all, err := cache.List(ListQuery{AllowScan: true})
	if err != nil || len(all) != 1 {
		t.Fatalf("List: %v (len %d)", err, len(all))
	}

	gate.set(false, false)
	_, getsBefore := backing.counts()
	got, err := cache.Get(all[0].ID)
	if err != nil {
		t.Fatalf("Get under open breaker: %v", err)
	}
	if got.Title != "task-1" {
		t.Fatalf("Get Title = %q, want task-1", got.Title)
	}
	if _, getsAfter := backing.counts(); getsAfter != getsBefore {
		t.Fatal("Get reached the backing store under an open breaker")
	}
}

func TestCachingStoreGetUnavailableUncachedReturnsTypedError(t *testing.T) {
	t.Parallel()
	cache, _, gate := newPrimedGatedCache(t, Bead{Title: "task-1"})
	gate.set(false, false)
	_, err := cache.Get("missing-id")
	if !errors.Is(err, ErrStoreUnavailable) {
		t.Fatalf("Get(missing) under open breaker: err = %v, want ErrStoreUnavailable (cannot distinguish missing from unreachable)", err)
	}
}

func TestCachingStoreAvailableGateLeavesReadsUntouched(t *testing.T) {
	t.Parallel()
	cache, _, gate := newPrimedGatedCache(t, Bead{Title: "task-1"})
	gate.set(true, false)
	got, err := cache.List(ListQuery{AllowScan: true})
	if err != nil || len(got) != 1 {
		t.Fatalf("List with available gate: %v (len %d)", err, len(got))
	}
	if cache.Degraded() {
		t.Fatal("Degraded() = true with an available gate and healthy cache, want false")
	}
}

func TestCachingStoreNilGateLeavesReadsUntouched(t *testing.T) {
	t.Parallel()
	backing := NewMemStore()
	if _, err := backing.Create(Bead{Title: "task-1"}); err != nil {
		t.Fatalf("Create: %v", err)
	}
	cache := NewCachingStoreForTest(backing, nil)
	if err := cache.Prime(context.Background()); err != nil {
		t.Fatalf("Prime: %v", err)
	}
	got, err := cache.List(ListQuery{AllowScan: true})
	if err != nil || len(got) != 1 {
		t.Fatalf("List without gate: %v (len %d)", err, len(got))
	}
	if cache.Degraded() {
		t.Fatal("Degraded() = true without a gate and with a healthy cache")
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

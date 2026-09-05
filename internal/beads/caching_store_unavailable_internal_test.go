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

// TestCachingStoreListUnavailableLiveQueryRefusesStaleAnswer replaces the
// former ...LiveQueryStaysOnCache, which asserted the opposite: that a Live
// query under an open breaker returns cached rows with a nil error. That
// pinned the pool-release hazard — ListQuery.Live's documented contract is
// "must observe external mutations immediately", and a lifecycle gate that
// treats absence as authoritative would release a running session's pool
// assignment on a stale short list. The half the old test got right is kept:
// the backing store must not be dialed while the breaker is open. A non-Live
// query in the same state proves the refusal is scoped, not a retreat from
// serving last-good.
func TestCachingStoreListUnavailableLiveQueryRefusesStaleAnswer(t *testing.T) {
	t.Parallel()
	cache, backing, gate := newPrimedGatedCache(t, Bead{Title: "task-1"})

	gate.set(false, false)
	listsBefore, _ := backing.counts()
	got, err := cache.List(ListQuery{AllowScan: true, Live: true})
	if !errors.Is(err, ErrStoreUnavailable) {
		t.Fatalf("List(Live) under open breaker = (%d beads, %v), want ErrStoreUnavailable", len(got), err)
	}
	if len(got) != 0 {
		t.Fatalf("List(Live) under open breaker returned %d beads alongside the refusal, want 0", len(got))
	}
	if listsAfter, _ := backing.counts(); listsAfter != listsBefore {
		t.Fatal("Live query reached the backing store under an open breaker")
	}

	nonLive, err := cache.List(ListQuery{AllowScan: true})
	if err != nil || len(nonLive) != 1 {
		t.Fatalf("non-Live List under open breaker = (%d beads, %v), want the last-good bead — the refusal must be scoped to Live", len(nonLive), err)
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

// newPartialPrimedGatedCache builds a cache in exactly the state PrimeActive
// leaves behind on a clean run: state == cachePartial with primePartialErr
// nil, so the snapshot holds open + in_progress rows only. The backing store
// implements Counter so the degraded Count path is reachable (BdStore does
// not; Doltlite-backed stores do).
func newPartialPrimedGatedCache(t *testing.T, beadsIn ...Bead) (*CachingStore, *fakeAvailabilityGate) {
	t.Helper()
	backing := &memCounterStore{Store: NewMemStore()}
	for _, b := range beadsIn {
		// MemStore.Create forces status "open"; the status a caller asked for
		// is applied as a follow-up transition.
		created, err := backing.Create(b)
		if err != nil {
			t.Fatalf("Create: %v", err)
		}
		if b.Status != "" && b.Status != created.Status {
			status := b.Status
			if err := backing.Update(created.ID, UpdateOpts{Status: &status}); err != nil {
				t.Fatalf("Update %s to %q: %v", created.ID, status, err)
			}
		}
	}
	cache := NewCachingStoreForTest(backing, nil)
	if err := cache.PrimeActive(); err != nil {
		t.Fatalf("PrimeActive: %v", err)
	}
	cache.mu.RLock()
	state, partialErr := cache.state, cache.primePartialErr
	cache.mu.RUnlock()
	if state != cachePartial || partialErr != nil {
		t.Fatalf("after PrimeActive: state=%v primePartialErr=%v, want cachePartial with a nil partial error", state, partialErr)
	}
	gate := &fakeAvailabilityGate{available: true}
	cache.SetAvailabilityGate(gate)
	return cache, gate
}

// TestCachingStoreListUnavailablePartialPrimeTagsBroadNonclosedQuery pins the
// degraded path to the same servability rule cacheServableForListQueryLocked
// applies on the healthy path. A PrimeActive snapshot holds open +
// in_progress only, so a Status:"" query — which ListQuery.Matches reads as
// "every non-closed status" — cannot be answered completely from it. The
// healthy path falls back to the backing store for that shape; with the
// breaker open there is nothing to fall back to, so the answer must arrive
// tagged partial rather than presenting a short list as fact.
func TestCachingStoreListUnavailablePartialPrimeTagsBroadNonclosedQuery(t *testing.T) {
	t.Parallel()
	cache, gate := newPartialPrimedGatedCache(t,
		Bead{Title: "open-1", Status: "open"},
		Bead{Title: "running-1", Status: "in_progress"},
		Bead{Title: "blocked-1", Status: "blocked"},
	)

	gate.set(false, false)

	got, err := cache.List(ListQuery{AllowScan: true})
	if !IsPartialResult(err) {
		t.Fatalf("List(Status:\"\") under open breaker on a partial prime = (%d beads, %v), want a PartialResultError: the blocked bead is missing from the snapshot", len(got), err)
	}
	if len(got) != 2 {
		t.Fatalf("List returned %d beads alongside the partial tag, want the 2 primed rows", len(got))
	}
}

// TestCachingStoreListUnavailablePartialPrimeServesPrimedStatuses is the
// scoping half: the two statuses PrimeActive actually loaded are complete in
// the snapshot, so they must keep the path's long-standing rows+nil contract
// (beadmail's session caches discard rows on any non-nil error).
func TestCachingStoreListUnavailablePartialPrimeServesPrimedStatuses(t *testing.T) {
	t.Parallel()
	cache, gate := newPartialPrimedGatedCache(t,
		Bead{Title: "open-1", Status: "open"},
		Bead{Title: "running-1", Status: "in_progress"},
		Bead{Title: "blocked-1", Status: "blocked"},
	)

	gate.set(false, false)

	for _, status := range partialPrimeStatuses {
		got, err := cache.List(ListQuery{Status: status})
		if err != nil {
			t.Fatalf("List(Status:%q) under open breaker = %v, want the primed rows untagged", status, err)
		}
		if len(got) != 1 {
			t.Fatalf("List(Status:%q) returned %d beads, want 1", status, len(got))
		}
	}
}

// TestCachingStoreCountUnavailablePartialPrimeRefusesBroadNonclosedQuery is
// finding 2's counterpart. A count carries no partial tag, so the only honest
// answer for a shape the snapshot cannot cover is a refusal — the same
// ok=false cachedCountContext returns on the healthy path.
func TestCachingStoreCountUnavailablePartialPrimeRefusesBroadNonclosedQuery(t *testing.T) {
	t.Parallel()
	cache, gate := newPartialPrimedGatedCache(t,
		Bead{Title: "open-1", Status: "open"},
		Bead{Title: "running-1", Status: "in_progress"},
		Bead{Title: "blocked-1", Status: "blocked"},
	)

	gate.set(false, false)

	n, err := cache.Count(context.Background(), ListQuery{AllowScan: true})
	if !errors.Is(err, ErrStoreUnavailable) {
		t.Fatalf("Count(Status:\"\") under open breaker on a partial prime = (%d, %v), want ErrStoreUnavailable rather than a confident undercount", n, err)
	}

	for _, status := range partialPrimeStatuses {
		got, err := cache.Count(context.Background(), ListQuery{Status: status})
		if err != nil {
			t.Fatalf("Count(Status:%q) under open breaker = %v, want the primed count", status, err)
		}
		if got != 1 {
			t.Fatalf("Count(Status:%q) = %d, want 1", status, got)
		}
	}
}

// TestCachingStoreLastGoodScopeSurvivesSelfDegrade pins that snapshot scope is
// tracked separately from liveness. Reconcile failures push a partial-primed
// cache to cacheDegraded without widening what the snapshot holds, so a broad
// nonclosed query must still be refused a confident answer; keying the rule on
// c.state alone would silently pass here.
func TestCachingStoreLastGoodScopeSurvivesSelfDegrade(t *testing.T) {
	t.Parallel()
	cache, gate := newPartialPrimedGatedCache(t,
		Bead{Title: "open-1", Status: "open"},
		Bead{Title: "blocked-1", Status: "blocked"},
	)

	cache.mu.Lock()
	cache.state = cacheDegraded
	cache.mu.Unlock()
	gate.set(false, false)

	if got, err := cache.List(ListQuery{AllowScan: true}); !IsPartialResult(err) {
		t.Fatalf("List(Status:\"\") on a self-degraded partial prime = (%d beads, %v), want a PartialResultError", len(got), err)
	}
	if n, err := cache.Count(context.Background(), ListQuery{AllowScan: true}); !errors.Is(err, ErrStoreUnavailable) {
		t.Fatalf("Count(Status:\"\") on a self-degraded partial prime = (%d, %v), want ErrStoreUnavailable", n, err)
	}
}

// TestCachingStoreLastGoodRegainsFullScopeAfterLivePromotion is the release
// half: once a full prime has loaded the complete nonclosed set, the snapshot
// answers every nonclosed shape, and a later outage must not start tagging
// answers that were complete before it.
func TestCachingStoreLastGoodRegainsFullScopeAfterLivePromotion(t *testing.T) {
	t.Parallel()
	cache, gate := newPartialPrimedGatedCache(t,
		Bead{Title: "open-1", Status: "open"},
		Bead{Title: "blocked-1", Status: "blocked"},
	)

	if err := cache.Prime(context.Background()); err != nil {
		t.Fatalf("Prime: %v", err)
	}
	gate.set(false, false)

	got, err := cache.List(ListQuery{AllowScan: true})
	if err != nil {
		t.Fatalf("List(Status:\"\") after a full prime = %v, want the complete snapshot untagged", err)
	}
	if len(got) != 2 {
		t.Fatalf("List(Status:\"\") after a full prime returned %d beads, want 2", len(got))
	}
	n, err := cache.Count(context.Background(), ListQuery{AllowScan: true})
	if err != nil {
		t.Fatalf("Count(Status:\"\") after a full prime = %v, want the complete count", err)
	}
	if n != 2 {
		t.Fatalf("Count(Status:\"\") after a full prime = %d, want 2", n)
	}
}

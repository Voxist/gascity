package beads

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
)

// tierRunner drives a REAL BdStore through the same two-subprocess tier merge
// production uses. This is the fixture correction ga-2p81g demanded: seven
// prior attempts faked the reconcile failure as one bare error, but the
// reconciler's scan is TierBoth (cacheFullScanQuery), which issues `bd list`
// AND `bd query` as separate subprocesses and merges them in
// mergeListTierResults. Under the realistic outage — a loaded store whose
// unbounded full scan times out while small point queries still answer — the
// merge returns a *PartialResultError, not a bare error, and any design built
// on the bare-error shape never fires. These tests construct both real shapes
// through the real merge code:
//
//	modeOverloaded  bd list times out, bd query answers  -> PartialResultError
//	modeDown        both time out                        -> bare joined error
type tierRunner struct {
	mu        sync.Mutex
	mode      string // "healthy", "overloaded", "down"
	listTitle string // title served for ga-1, to distinguish fresh vs snapshot
	listCalls int
}

func newTierRunner() *tierRunner {
	return &tierRunner{mode: "healthy", listTitle: "one"}
}

func (r *tierRunner) setMode(mode string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.mode = mode
}

func (r *tierRunner) setListTitle(title string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.listTitle = title
}

func (r *tierRunner) listCallCount() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.listCalls
}

func (r *tierRunner) run(_, name string, args ...string) ([]byte, error) {
	r.mu.Lock()
	mode, title := r.mode, r.listTitle
	if len(args) > 0 && args[0] == "list" {
		r.listCalls++
	}
	r.mu.Unlock()

	if name != "bd" || len(args) == 0 {
		return nil, fmt.Errorf("unexpected command %q %v", name, args)
	}
	// A down store answers nothing at all; an overloaded store fails only
	// the unbounded scan (`bd list`) while small commands still answer.
	if mode == "down" {
		return nil, fmt.Errorf("timed out after 2m0s")
	}
	switch args[0] {
	case "list":
		if mode == "overloaded" {
			return nil, fmt.Errorf("timed out after 2m0s")
		}
		return []byte(fmt.Sprintf(`[{"id":"ga-1","title":%q,"status":"open"},
		                {"id":"ga-2","title":"two","status":"in_progress"}]`, title)), nil
	case "query":
		return []byte(`[{"id":"wisp-1","title":"w","status":"open"}]`), nil
	case "ready":
		return []byte(`[]`), nil
	case "version":
		return []byte("bd version 1.0.4\n"), nil
	}
	return []byte(`[]`), nil
}

// primeTierCache builds a CachingStore over a real BdStore and primes it while
// the runner is healthy, asserting the baseline works so later failures cannot
// be fixture artifacts.
func primeTierCache(t *testing.T, r *tierRunner) *CachingStore {
	t.Helper()
	cache := NewCachingStore(NewBdStore("/city", r.run), nil)
	if err := cache.Prime(t.Context()); err != nil {
		t.Fatalf("Prime: %v", err)
	}
	before, err := cache.List(ListQuery{Status: "open"})
	if err != nil {
		t.Fatalf("healthy List: %v", err)
	}
	if len(before) == 0 {
		t.Fatal("fixture served no beads while healthy; the test would prove nothing")
	}
	return cache
}

func degradeCache(t *testing.T, cache *CachingStore) {
	t.Helper()
	for i := 0; i < maxCacheSyncFailures+1; i++ {
		cache.runReconciliation()
	}
	if cache.state != cacheDegraded {
		t.Fatalf("state = %v after %d failing cycles, want cacheDegraded", cache.state, maxCacheSyncFailures+1)
	}
}

// TestCachingStoreOverloadedScanKeepsServingActiveReads is the corrected
// ga-2p81g scenario. Under overload the reconciler receives PartialResultError
// (the wisp tier answered), so the cache sits in cacheDegraded — the state
// seven prior attempts believed unreachable-branch logic would cover. The
// required behavior: an active-shape read dials the backing store first
// (reality wins when it answers), and when that dial fails, serves the
// last-good snapshot instead of amplifying the outage with an error.
func TestCachingStoreOverloadedScanKeepsServingActiveReads(t *testing.T) {
	t.Parallel()

	r := newTierRunner()
	cache := primeTierCache(t, r)
	r.setMode("overloaded")
	degradeCache(t, cache)

	dialsBefore := r.listCallCount()
	got, err := cache.List(ListQuery{Status: "open"})
	if err != nil {
		t.Fatalf("List during overload = error %v; the snapshot still holds the data and must be served", err)
	}
	if !hasBead(got, "ga-1") {
		t.Fatalf("List during overload = %d beads without ga-1; want the last-good snapshot", len(got))
	}
	if r.listCallCount() == dialsBefore {
		t.Fatal("reality-first: the backing store must be dialed before falling back to the snapshot")
	}
}

// TestCachingStoreDegradedReadServesFreshWhenBackingRecovers is the control
// against the predict-and-route error made in all seven prior attempts: while
// degraded, a read whose backing dial SUCCEEDS must return the fresh backing
// data, never the stale snapshot.
func TestCachingStoreDegradedReadServesFreshWhenBackingRecovers(t *testing.T) {
	t.Parallel()

	r := newTierRunner()
	cache := primeTierCache(t, r)
	r.setMode("overloaded")
	degradeCache(t, cache)

	r.setListTitle("fresh")
	r.setMode("healthy")
	got, err := cache.List(ListQuery{Status: "open"})
	if err != nil {
		t.Fatalf("List after recovery: %v", err)
	}
	for _, b := range got {
		if b.ID == "ga-1" {
			if b.Title != "fresh" {
				t.Fatalf("ga-1 title = %q, want the fresh backing answer %q — degraded routing must not pin reads to the snapshot", b.Title, "fresh")
			}
			return
		}
	}
	t.Fatal("ga-1 missing from recovered List")
}

// TestCachingStoreDownServesLastGoodForActiveShapes is the original ga-2p81g
// repro through the real merge: both tiers fail (bare joined error), the cache
// degrades on the failure counter, and an active-shape read serves last-good
// after the backing dial fails.
func TestCachingStoreDownServesLastGoodForActiveShapes(t *testing.T) {
	t.Parallel()

	r := newTierRunner()
	cache := primeTierCache(t, r)
	r.setMode("down")
	degradeCache(t, cache)

	got, err := cache.List(ListQuery{Status: "open"})
	if err != nil {
		t.Fatalf("List during outage = error %v; want last-good snapshot", err)
	}
	if !hasBead(got, "ga-1") {
		t.Fatalf("List during outage = %d beads without ga-1", len(got))
	}
}

// TestCachingStoreDownLiveQueryStillFails pins the honesty boundary: Live
// declares staleness unacceptable (a lifecycle gate treating absence as
// authoritative must never see a stale short list — it would release live
// pool assignments), so a Live read during an outage surfaces the failure.
func TestCachingStoreDownLiveQueryStillFails(t *testing.T) {
	t.Parallel()

	r := newTierRunner()
	cache := primeTierCache(t, r)
	r.setMode("down")
	degradeCache(t, cache)

	if got, err := cache.List(ListQuery{AllowScan: true, Live: true}); err == nil {
		t.Fatalf("Live List during outage = (%d beads, nil); a stale answer to a Live query is the pool-release hazard", len(got))
	}
}

// TestCachingStoreDownClosedOnlyQueryStillFails pins the second honesty
// boundary: the snapshot holds active beads only, so a closed-only query
// cannot be answered from it — a plausible-looking empty list with a nil
// error would read as "no closed beads exist".
func TestCachingStoreDownClosedOnlyQueryStillFails(t *testing.T) {
	t.Parallel()

	r := newTierRunner()
	cache := primeTierCache(t, r)
	r.setMode("down")
	degradeCache(t, cache)

	if got, err := cache.List(ListQuery{Status: "closed", AllowScan: true}); err == nil {
		t.Fatalf("closed-only List during outage = (%d beads, nil); the active-only snapshot cannot answer this shape", len(got))
	}
}

// TestCachingStoreDownIncludeClosedServesPartial: an IncludeClosed query can
// be half-answered — the active snapshot is served, tagged with the package's
// existing partial-result convention so callers know history is missing.
func TestCachingStoreDownIncludeClosedServesPartial(t *testing.T) {
	t.Parallel()

	r := newTierRunner()
	cache := primeTierCache(t, r)
	r.setMode("down")
	degradeCache(t, cache)

	got, err := cache.List(ListQuery{AllowScan: true, IncludeClosed: true})
	if !IsPartialResult(err) {
		t.Fatalf("IncludeClosed List during outage err = %v, want a PartialResultError carrying the active snapshot", err)
	}
	if !hasBead(got, "ga-1") {
		t.Fatalf("IncludeClosed List during outage = %d beads without ga-1", len(got))
	}
}

// TestCachingStoreDownGetServesLastGoodAndPropagatesMisses: Get serves the
// cached clone when the backing dial fails, and an uncached id propagates the
// backing failure — the cache cannot distinguish "missing" from "unreachable".
func TestCachingStoreDownGetServesLastGoodAndPropagatesMisses(t *testing.T) {
	t.Parallel()

	r := newTierRunner()
	cache := primeTierCache(t, r)
	r.setMode("down")
	degradeCache(t, cache)

	got, err := cache.Get("ga-1")
	if err != nil {
		t.Fatalf("Get(cached id) during outage = %v; want the last-good clone", err)
	}
	if got.ID != "ga-1" {
		t.Fatalf("Get(cached id) = %q, want ga-1", got.ID)
	}
	if _, err := cache.Get("never-seen"); err == nil {
		t.Fatal("Get(uncached id) during outage = nil error; absence cannot be proven while the store is down")
	}
}

// notFoundThenFailStore exercises the Get fallback's ErrNotFound boundary with
// a Store-level fake: ErrNotFound is an ANSWER (the store was reached, the
// bead is absent) and must propagate — falling back to the snapshot would
// resurrect deleted beads.
type notFoundThenFailStore struct {
	Store
	getErr error
}

func (s *notFoundThenFailStore) Get(id string) (Bead, error) {
	if s.getErr != nil {
		return Bead{}, s.getErr
	}
	return s.Store.Get(id)
}

func TestCachingStoreGetDoesNotResurrectOnNotFound(t *testing.T) {
	t.Parallel()

	mem := NewMemStore()
	created, err := mem.Create(Bead{Title: "will-vanish", Status: "open"})
	if err != nil {
		t.Fatalf("seed: %v", err)
	}
	backing := &notFoundThenFailStore{Store: mem}
	cache := NewCachingStore(backing, nil)
	if err := cache.Prime(t.Context()); err != nil {
		t.Fatalf("Prime: %v", err)
	}

	// Degraded state with a reachable store answering ErrNotFound: the state
	// transition realism is covered by the tierRunner tests above; this unit
	// pins only the fallback's NotFound boundary.
	cache.mu.Lock()
	cache.state = cacheDegraded
	cache.mu.Unlock()

	backing.getErr = ErrNotFound
	if _, err := cache.Get(created.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("Get with backing ErrNotFound = %v; the snapshot must not resurrect a bead the store says is gone", err)
	}
}

// memCounterStore exercises the Count paths with a Counter-implementing
// backing (BdStore does not implement Counter, so production bd cities never
// reach counter.Count; Doltlite-backed stores do).
type memCounterStore struct {
	Store
	mu         sync.Mutex
	countErr   error
	countCalls int
}

func (s *memCounterStore) Count(_ context.Context, query ListQuery, excludeTypes ...string) (int, error) {
	s.mu.Lock()
	s.countCalls++
	err := s.countErr
	s.mu.Unlock()
	if err != nil {
		return 0, err
	}
	items, listErr := s.List(query)
	if listErr != nil {
		return 0, listErr
	}
	n := 0
	for _, b := range items {
		exclude := false
		for _, t := range excludeTypes {
			if b.Type == t {
				exclude = true
				break
			}
		}
		if !exclude {
			n++
		}
	}
	return n, nil
}

func (s *memCounterStore) setCountErr(err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.countErr = err
}

func (s *memCounterStore) countCallCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.countCalls
}

func TestCachingStoreCountFallsBackToSnapshotOnBackingFailure(t *testing.T) {
	t.Parallel()

	mem := NewMemStore()
	if _, err := mem.Create(Bead{Title: "a", Status: "open"}); err != nil {
		t.Fatalf("seed: %v", err)
	}
	backing := &memCounterStore{Store: mem}
	cache := NewCachingStore(backing, nil)
	if err := cache.Prime(t.Context()); err != nil {
		t.Fatalf("Prime: %v", err)
	}
	cache.mu.Lock()
	cache.state = cacheDegraded
	cache.mu.Unlock()

	backing.setCountErr(fmt.Errorf("timed out after 2m0s"))
	n, err := cache.Count(t.Context(), ListQuery{Status: "open", AllowScan: true})
	if err != nil {
		t.Fatalf("Count during outage = %v; want the snapshot count", err)
	}
	if n != 1 {
		t.Fatalf("Count during outage = %d, want 1", n)
	}

	backing.setCountErr(ErrCountUnsupported)
	if _, err := cache.Count(t.Context(), ListQuery{Status: "open", AllowScan: true}); !errors.Is(err, ErrCountUnsupported) {
		t.Fatalf("Count with ErrCountUnsupported = %v; capability errors must propagate so callers fall back to List", err)
	}
}

// TestCachingStoreGatedCountServesSnapshotWithoutDialing mirrors the List
// gated path: a proven-down store (breaker open) is not dialed for counts.
func TestCachingStoreGatedCountServesSnapshotWithoutDialing(t *testing.T) {
	t.Parallel()

	mem := NewMemStore()
	if _, err := mem.Create(Bead{Title: "a", Status: "open"}); err != nil {
		t.Fatalf("seed: %v", err)
	}
	backing := &memCounterStore{Store: mem}
	cache := NewCachingStore(backing, nil)
	if err := cache.Prime(t.Context()); err != nil {
		t.Fatalf("Prime: %v", err)
	}
	// A live clean cache answers counts from cachedCountContext without
	// dialing, gate or no gate. The hole opens when the cache is degraded:
	// cachedCountContext declines and Count used to dial the proven-down
	// store. Reproduce that state.
	cache.mu.Lock()
	cache.state = cacheDegraded
	cache.mu.Unlock()
	cache.SetAvailabilityGate(&stubGate{available: false})

	n, err := cache.Count(t.Context(), ListQuery{Status: "open", AllowScan: true})
	if err != nil {
		t.Fatalf("gated Count = %v; want the snapshot count", err)
	}
	if n != 1 {
		t.Fatalf("gated Count = %d, want 1", n)
	}
	if backing.countCallCount() != 0 {
		t.Fatalf("gated Count dialed the backing store %d time(s); the breaker's fail-fast purpose is defeated", backing.countCallCount())
	}
}

// TestCachingStoreGatedClosedOnlyQueryRefuses pins the gated-path half of the
// honesty boundary — a pre-existing hole: with the breaker open, a closed-only
// query used to return the snapshot's zero matching rows with a nil error,
// which reads as "no closed beads exist".
func TestCachingStoreGatedClosedOnlyQueryRefuses(t *testing.T) {
	t.Parallel()

	r := newTierRunner()
	cache := primeTierCache(t, r)
	cache.SetAvailabilityGate(&stubGate{available: false})

	if got, err := cache.List(ListQuery{Status: "closed", AllowScan: true}); !errors.Is(err, ErrStoreUnavailable) {
		t.Fatalf("gated closed-only List = (%d beads, %v), want ErrStoreUnavailable", len(got), err)
	}
}

// TestCachingStoreGatedIncludeClosedServesSnapshotCompat pins the gated
// path's long-standing IncludeClosed contract: rows with a NIL error.
// Error-intolerant gated consumers (beadmail's sessionBeadCache and
// historical-alias routing discard rows on ANY non-nil error) previously
// kept working from the snapshot during breaker-open episodes; tagging the
// answer partial here would silently break mail routing for the outage's
// duration. The dial-first fallback path DOES tag
// (TestCachingStoreDownIncludeClosedServesPartial): its callers previously
// received a hard error, so a tagged answer cannot regress anyone. A
// partially-primed snapshot is tagged on both paths — that data is
// known-incomplete at the active level, which is a different claim.
func TestCachingStoreGatedIncludeClosedServesSnapshotCompat(t *testing.T) {
	t.Parallel()

	r := newTierRunner()
	cache := primeTierCache(t, r)
	cache.SetAvailabilityGate(&stubGate{available: false})

	got, err := cache.List(ListQuery{AllowScan: true, IncludeClosed: true})
	if err != nil {
		t.Fatalf("gated IncludeClosed List err = %v, want nil (compat with error-intolerant gated consumers)", err)
	}
	if !hasBead(got, "ga-1") {
		t.Fatalf("gated IncludeClosed List = %d beads without ga-1", len(got))
	}
}

// TestCachingStoreFallbackTagsPartialPrimedSnapshot: a snapshot built from a
// partial prime is known-incomplete at the active level (it can hold wisps
// only), so the fallback must never present it as a complete nil-error
// answer — "no work" must not be fabricated from half a prime.
func TestCachingStoreFallbackTagsPartialPrimedSnapshot(t *testing.T) {
	t.Parallel()

	r := newTierRunner()
	r.setMode("overloaded") // prime's TierBoth scan -> PartialResultError
	cache := NewCachingStore(NewBdStore("/city", r.run), nil)
	if err := cache.Prime(t.Context()); err != nil {
		t.Fatalf("Prime under overload: %v", err)
	}
	cache.mu.RLock()
	partial := cache.primePartialErr
	cache.mu.RUnlock()
	if partial == nil {
		t.Fatal("fixture: primePartialErr not set after overloaded prime; test would prove nothing")
	}

	got, err := cache.List(ListQuery{Status: "open"})
	if !IsPartialResult(err) {
		t.Fatalf("List on partial-primed snapshot = (%d beads, %v), want a PartialResultError tag", len(got), err)
	}
}

// TestCachingStoreGatedParentIDServesActiveChildren pins the gated path's
// pre-existing child-listing behavior: active children live in the snapshot,
// and convergence/molecule progress evaluation must keep advancing during a
// breaker-open episode.
func TestCachingStoreGatedParentIDServesActiveChildren(t *testing.T) {
	t.Parallel()

	mem := NewMemStore()
	parent, err := mem.Create(Bead{Title: "root", Status: "open"})
	if err != nil {
		t.Fatalf("seed parent: %v", err)
	}
	if _, err := mem.Create(Bead{Title: "step", Status: "open", ParentID: parent.ID}); err != nil {
		t.Fatalf("seed child: %v", err)
	}
	cache := NewCachingStore(mem, nil)
	if err := cache.Prime(t.Context()); err != nil {
		t.Fatalf("Prime: %v", err)
	}
	cache.SetAvailabilityGate(&stubGate{available: false})

	got, err := cache.List(ListQuery{ParentID: parent.ID})
	if err != nil {
		t.Fatalf("gated ParentID List = %v, want the cached active children", err)
	}
	if len(got) != 1 {
		t.Fatalf("gated ParentID List = %d beads, want 1", len(got))
	}
}

// TestCachingStoreGatedCountWithoutCounterKeepsContract: the capability
// contract outranks the gate. Backing stores without a Counter must report
// ErrCountUnsupported in every state so callers keep their documented List
// fallback (store-health depends on it during outages).
func TestCachingStoreGatedCountWithoutCounterKeepsContract(t *testing.T) {
	t.Parallel()

	mem := NewMemStore()
	if _, err := mem.Create(Bead{Title: "a", Status: "open"}); err != nil {
		t.Fatalf("seed: %v", err)
	}
	cache := NewCachingStore(mem, nil)
	if err := cache.Prime(t.Context()); err != nil {
		t.Fatalf("Prime: %v", err)
	}
	cache.mu.Lock()
	cache.state = cacheDegraded
	cache.mu.Unlock()
	cache.SetAvailabilityGate(&stubGate{available: false})

	if _, err := cache.Count(t.Context(), ListQuery{Status: "open", AllowScan: true}); !errors.Is(err, ErrCountUnsupported) {
		t.Fatalf("gated Count without Counter = %v, want ErrCountUnsupported (callers fall back to List)", err)
	}
}

// cancelingListerStore expires the caller's ctx mid-dial and returns the ctx
// error, modeling a CtxLister backing whose query outlives the caller's
// deadline.
type cancelingListerStore struct {
	Store
	cancel context.CancelFunc
}

func (s *cancelingListerStore) ListCtx(ctx context.Context, _ ListQuery) ([]Bead, error) {
	s.cancel()
	<-ctx.Done()
	return nil, ctx.Err()
}

// TestCachingStoreFallbackPropagatesCallerDeadline: once the CALLER's budget
// is spent, serving a stale snapshot with a nil error would mask the very
// slowness the deadline exists to surface — the dial's error stands.
func TestCachingStoreFallbackPropagatesCallerDeadline(t *testing.T) {
	t.Parallel()

	mem := NewMemStore()
	if _, err := mem.Create(Bead{Title: "a", Status: "open"}); err != nil {
		t.Fatalf("seed: %v", err)
	}
	backing := &cancelingListerStore{Store: mem}
	cache := NewCachingStore(backing, nil)
	if err := cache.Prime(t.Context()); err != nil {
		t.Fatalf("Prime: %v", err)
	}
	cache.mu.Lock()
	cache.state = cacheDegraded
	cache.mu.Unlock()

	ctx, cancel := context.WithCancel(t.Context())
	backing.cancel = cancel
	got, err := cache.ListCtx(ctx, ListQuery{Status: "open", AllowScan: true})
	if err == nil {
		t.Fatalf("ListCtx past the caller's deadline = (%d beads, nil); stale data after budget exhaustion masks the slowness", len(got))
	}
}

// TestCachingStoreDegradedDialSuccessRefreshesSnapshot: a successful
// degraded-path dial folds its rows into the snapshot, so a fallback moments
// later serves the freshest observed truth, not the pre-outage state —
// consecutive reads must not travel backwards in time by hours.
func TestCachingStoreDegradedDialSuccessRefreshesSnapshot(t *testing.T) {
	t.Parallel()

	r := newTierRunner()
	cache := primeTierCache(t, r)
	r.setMode("overloaded")
	degradeCache(t, cache)

	r.setListTitle("fresh")
	r.setMode("healthy")
	if _, err := cache.List(ListQuery{Status: "open"}); err != nil {
		t.Fatalf("List during recovery window: %v", err)
	}

	r.setMode("down")
	got, err := cache.List(ListQuery{Status: "open"})
	if err != nil {
		t.Fatalf("List after store went down = %v, want last-good snapshot", err)
	}
	for _, b := range got {
		if b.ID == "ga-1" {
			if b.Title != "fresh" {
				t.Fatalf("ga-1 title = %q, want %q — the successful dial's rows must be absorbed into the snapshot", b.Title, "fresh")
			}
			return
		}
	}
	t.Fatal("ga-1 missing from last-good snapshot")
}

// wispGetRunner: `bd show` reports not-found (wisps are invisible to it)
// while the supplemental wisp `bd query` fails with a transport error.
type wispGetRunner struct{ queryErr error }

func (r *wispGetRunner) run(_, name string, args ...string) ([]byte, error) {
	if name != "bd" || len(args) == 0 {
		return nil, fmt.Errorf("unexpected command %q %v", name, args)
	}
	switch args[0] {
	case "show":
		return nil, fmt.Errorf("exit status 1: Error fetching %s: no issue found matching %q", args[len(args)-1], args[len(args)-1])
	case "query":
		if r.queryErr != nil {
			return nil, r.queryErr
		}
		return []byte(`[]`), nil
	}
	return []byte(`[]`), nil
}

// TestBdStoreGetWispLookupFailureIsNotNotFound: when bd show says not-found
// (wisps are always invisible to it) and the supplemental wisp query FAILS,
// the bead may still exist and be unobservable — fabricating ErrNotFound
// teaches NotFound-is-authoritative callers to drop live beads (auto-handoff
// mail) during exactly the overload ga-2p81g exists to fix. A wisp query
// that ANSWERS not-found (or empty) is confirmed absence and stays
// ErrNotFound.
func TestBdStoreGetWispLookupFailureIsNotNotFound(t *testing.T) {
	t.Parallel()

	r := &wispGetRunner{queryErr: fmt.Errorf("timed out after 2m0s")}
	store := NewBdStore("/city", r.run)
	if _, err := store.Get("wisp-1"); errors.Is(err, ErrNotFound) || err == nil {
		t.Fatalf("Get with failed wisp lookup = %v; a transport failure must not be reported as absence", err)
	}

	r.queryErr = nil // wisp query answers: empty result = confirmed absence
	if _, err := store.Get("wisp-1"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("Get with empty wisp answer = %v, want ErrNotFound", err)
	}
}

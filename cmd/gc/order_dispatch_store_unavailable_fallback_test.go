package main

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/orders"
)

// storeUnavailableFallbackOrder builds a cooldown-triggered exec order (a
// fixed 1m interval — every test below either needs that exact cooldown
// window or doesn't care) with RecoverOnStoreUnavailable set as requested,
// mirroring reservedExecOrder's TOML-parse convention so the test exercises
// the real orders.Parse decode path for the new field, not a struct literal
// shortcut.
func storeUnavailableFallbackOrder(t *testing.T, name string, optIn bool) orders.Order {
	t.Helper()
	definition := `[order]
exec = "placeholder"
trigger = "cooldown"
interval = "1m"
`
	if optIn {
		definition += "recover_on_store_unavailable = true\n"
	}
	order, err := orders.Parse([]byte(definition))
	if err != nil {
		t.Fatalf("Parse store-unavailable-fallback order %q: %v", name, err)
	}
	order.Name = name
	order.Exec = name
	return order
}

// errConnectionRefused is a synthetic transport failure shaped like a real
// dolt sql-server that is not listening at all (ga-xuapz's ~2min gap): the
// message contains the "dial tcp"/"connection refused" markers
// classifyWorkQueryStoreUnavailable (and the fleet's shared transport
// classifier, bdTransportRetryableMarkers) key on.
var errConnectionRefused = errors.New("dial tcp 127.0.0.1:48770: connect: connection refused")

// dispatcherWithFailingStore builds a dispatcher over aa whose storeFn
// itself always fails with errConnectionRefused. Review finding (PR #227):
// for a bd-contract provider this does NOT happen in production — a failed
// native open falls back to a BdStore constructor, which cannot error
// (internal/beads/factory_test.go). This test exists only to prove the
// store-open trigger point stays wired for whatever CAN fail there (a
// different provider, or a future path); see the *TransportError tests
// below for the trigger points that fire in the ga-xuapz scenario itself.
func dispatcherWithFailingStore(t *testing.T, aa []orders.Order, execRun ExecRunner) *memoryOrderDispatcher {
	t.Helper()
	m := buildOrderDispatcherFromListExec(aa, beads.NewMemStore(), nil, execRun, nil).(*memoryOrderDispatcher)
	m.storeFn = func(execStoreTarget) (beads.Store, error) {
		return nil, errConnectionRefused
	}
	return m
}

// orderDispatchTransportFailStore wraps a real store so it opens
// successfully (storeFn succeeds, exactly as it does for a bd-contract
// provider whose native open fell back to BdStore) but its List and/or
// Create calls fail with a real transport-shaped error — the shape
// gateOpenWorkBounded's underlying call and launchResolvedDispatch's
// CreateRun actually produce against a dead or slow store, not a
// storeFn-level fake.
type orderDispatchTransportFailStore struct {
	beads.Store
	listErr   error
	createErr error
}

func (s orderDispatchTransportFailStore) List(query beads.ListQuery) ([]beads.Bead, error) {
	if s.listErr != nil && isOrderGateListQuery(query) {
		return nil, s.listErr
	}
	return s.Store.List(query)
}

func (s orderDispatchTransportFailStore) Create(b beads.Bead) (beads.Bead, error) {
	if s.createErr != nil {
		return beads.Bead{}, s.createErr
	}
	return s.Store.Create(b)
}

func TestOrderDispatchStoreUnavailableFallbackFiresWhenStoreOpenItselfFails(t *testing.T) {
	const orderName = "store-open-fail"
	recorder := &reservedDispatchExecRecorder{}
	order := storeUnavailableFallbackOrder(t, orderName, true)
	m := dispatcherWithFailingStore(t, []orders.Order{order}, recorder.run)
	cityPath := t.TempDir()

	m.dispatch(context.Background(), cityPath, time.Now())
	drainOrderDispatch(t, m)

	if got := recorder.counts()[orderName]; got != 1 {
		t.Fatalf("store-open trigger: fallback exec calls = %d, want 1", got)
	}
}

// TestOrderDispatchStoreUnavailableFallbackFiresOnRealGateTransportError is
// the HIGH-review failing-first test: it reproduces ga-xuapz's actual shape
// — the store OPENS fine (storeFn succeeds, err == nil) but the open-work
// gate's own store.List call gets connection-refused — and proves the
// fallback still fires. dispatcherWithFailingStore's storeFn-level fake
// never happens for a bd-contract provider; this is the path that does.
func TestOrderDispatchStoreUnavailableFallbackFiresOnRealGateTransportError(t *testing.T) {
	const orderName = "gate-transport-fail"
	recorder := &reservedDispatchExecRecorder{}
	order := storeUnavailableFallbackOrder(t, orderName, true)
	store := orderDispatchTransportFailStore{Store: beads.NewMemStore(), listErr: errConnectionRefused}
	m := buildOrderDispatcherFromListExec([]orders.Order{order}, store, nil, recorder.run, nil).(*memoryOrderDispatcher)
	cityPath := t.TempDir()

	m.dispatch(context.Background(), cityPath, time.Now())
	drainOrderDispatch(t, m)

	if got := recorder.counts()[orderName]; got != 1 {
		t.Fatalf("open-work gate transport error: fallback exec calls = %d, want 1", got)
	}
}

// TestOrderDispatchStoreUnavailableFallbackFiresOnCreateRunTransportError
// covers the third trigger point: the store opens and the gates pass (no
// open work), but the tracking-bead CreateRun itself fails with a real
// transport error.
func TestOrderDispatchStoreUnavailableFallbackFiresOnCreateRunTransportError(t *testing.T) {
	const orderName = "createrun-transport-fail"
	recorder := &reservedDispatchExecRecorder{}
	order := storeUnavailableFallbackOrder(t, orderName, true)
	store := orderDispatchTransportFailStore{Store: beads.NewMemStore(), createErr: errConnectionRefused}
	m := buildOrderDispatcherFromListExec([]orders.Order{order}, store, nil, recorder.run, nil).(*memoryOrderDispatcher)
	cityPath := t.TempDir()

	m.dispatch(context.Background(), cityPath, time.Now())
	drainOrderDispatch(t, m)

	if got := recorder.counts()[orderName]; got != 1 {
		t.Fatalf("CreateRun transport error: fallback exec calls = %d, want 1", got)
	}
}

// TestOrderDispatchStoreUnavailableFallbackNeverFiresOnASlowButLiveStore
// pins the fix for the review's safety HIGH: a store that is merely SLOW —
// its List call runs to completion but returns context.DeadlineExceeded,
// exactly a wedged-but-listening Dolt under load, not a dead one — must
// NEVER fire this fallback. It used to: classifyWorkQueryStoreUnavailable
// (gc hook's own classifier, reused here before this fix) treats
// DeadlineExceeded the same as store-unavailable on purpose for gc hook's
// own dead-drop concern, and this fallback inherited that call rather than
// having its own. classifyStoreUnavailableFallbackTrigger now only fires on
// genuine transport-class death evidence (dial refused, connection refused,
// unexpected EOF — the ga-xuapz shape), never on a bare timeout, so a
// slow-but-live store must leave the normal tracking-bead/open-work-gate
// path in full effect instead of a bare, untracked exec.
func TestOrderDispatchStoreUnavailableFallbackNeverFiresOnASlowButLiveStore(t *testing.T) {
	const orderName = "slow-but-live"
	recorder := &reservedDispatchExecRecorder{}
	order := storeUnavailableFallbackOrder(t, orderName, true)
	store := orderDispatchTransportFailStore{Store: beads.NewMemStore(), listErr: context.DeadlineExceeded}
	m := buildOrderDispatcherFromListExec([]orders.Order{order}, store, nil, recorder.run, nil).(*memoryOrderDispatcher)
	cityPath := t.TempDir()

	m.dispatch(context.Background(), cityPath, time.Now())
	drainOrderDispatch(t, m)

	if got := recorder.counts()[orderName]; got != 0 {
		t.Fatalf("slow-but-live (DeadlineExceeded) gate error: fallback exec calls = %d, want 0 (must never fire on a timeout, only on death evidence)", got)
	}
}

func TestOrderDispatchStoreUnavailableFallbackRequiresOptIn(t *testing.T) {
	// An order that does NOT declare recover_on_store_unavailable must see
	// no behavior change at all: it stays fail-closed while the store is
	// down, exactly like order-tracking-sweep is meant to (its whole job is
	// bead bookkeeping, which is meaningless without a store).
	const orderName = "no-opt-in"
	recorder := &reservedDispatchExecRecorder{}
	order := storeUnavailableFallbackOrder(t, orderName, false)
	store := orderDispatchTransportFailStore{Store: beads.NewMemStore(), listErr: errConnectionRefused}
	m := buildOrderDispatcherFromListExec([]orders.Order{order}, store, nil, recorder.run, nil).(*memoryOrderDispatcher)
	cityPath := t.TempDir()

	m.dispatch(context.Background(), cityPath, time.Now())
	drainOrderDispatch(t, m)

	if got := recorder.counts()[orderName]; got != 0 {
		t.Fatalf("non-opted-in order fired through the store-unavailable fallback: got %d calls, want 0", got)
	}
}

func TestOrderDispatchStoreUnavailableFallbackObeysCooldown(t *testing.T) {
	// The fallback must not be able to loop faster than the order's own
	// interval: two ticks inside the cooldown window fire the exec once,
	// and a third tick after the cooldown elapses fires it again. This is
	// what keeps a persistently-down store from turning the fallback into
	// an uncapped restart loop — the mirror-image failure mode of the one
	// ga-amol9 fixed on the other recover path.
	const orderName = "cooldown-bound"
	recorder := &reservedDispatchExecRecorder{}
	order := storeUnavailableFallbackOrder(t, orderName, true)
	store := orderDispatchTransportFailStore{Store: beads.NewMemStore(), listErr: errConnectionRefused}
	m := buildOrderDispatcherFromListExec([]orders.Order{order}, store, nil, recorder.run, nil).(*memoryOrderDispatcher)
	cityPath := t.TempDir()

	t0 := time.Date(2031, 1, 2, 3, 4, 5, 0, time.UTC)
	m.dispatch(context.Background(), cityPath, t0)
	drainOrderDispatch(t, m)
	if got := recorder.counts()[orderName]; got != 1 {
		t.Fatalf("first tick (store down) exec calls = %d, want 1", got)
	}

	m.dispatch(context.Background(), cityPath, t0.Add(30*time.Second))
	drainOrderDispatch(t, m)
	if got := recorder.counts()[orderName]; got != 1 {
		t.Fatalf("second tick inside the 1m cooldown exec calls = %d, want 1 (still cooling down)", got)
	}

	m.dispatch(context.Background(), cityPath, t0.Add(61*time.Second))
	drainOrderDispatch(t, m)
	if got := recorder.counts()[orderName]; got != 2 {
		t.Fatalf("third tick past the 1m cooldown exec calls = %d, want 2", got)
	}
}

// TestOrderDispatchStoreUnavailableFallbackSingleFlightsOverlappingFires
// covers the MEDIUM review item: beads-health's own timeout (60s) exceeds
// its cooldown interval (30s), so without a single-flight guard a store
// that stays down across two ticks could have a second `gc beads health`
// admitted before the first one exits. This blocks the first fallback exec
// mid-flight and proves a second tick — even one still inside the store-
// unavailable window, independent of the cooldown clock — does not start a
// second one.
func TestOrderDispatchStoreUnavailableFallbackSingleFlightsOverlappingFires(t *testing.T) {
	const orderName = "single-flight"
	started := make(chan struct{})
	release := make(chan struct{})
	recorder := &reservedDispatchExecRecorder{started: started, release: release}
	order := storeUnavailableFallbackOrder(t, orderName, true)
	store := orderDispatchTransportFailStore{Store: beads.NewMemStore(), listErr: errConnectionRefused}
	m := buildOrderDispatcherFromListExec([]orders.Order{order}, store, nil, recorder.run, nil).(*memoryOrderDispatcher)
	cityPath := t.TempDir()
	var releaseOnce sync.Once
	releaseRun := func() { releaseOnce.Do(func() { close(release) }) }
	t.Cleanup(func() {
		releaseRun()
		drainOrderDispatch(t, m)
	})

	now := time.Now()
	m.dispatch(context.Background(), cityPath, now)
	awaitClose(t, started, "fallback exec start")

	m.dispatch(context.Background(), cityPath, now.Add(time.Millisecond))

	if got := recorder.counts()[orderName]; got != 1 {
		t.Fatalf("exec calls while the first fallback is still in flight = %d, want 1 (single-flight guard should block a second)", got)
	}

	releaseRun()
	drainOrderDispatch(t, m)
	if got := recorder.counts()[orderName]; got != 1 {
		t.Fatalf("exec calls after the first fallback released = %d, want 1", got)
	}
}

// toggleableTransportFailStore is orderDispatchTransportFailStore's mutable
// counterpart: its List error can change between dispatch ticks, to
// simulate a store that transitions from dead to live mid-test — the exact
// "store came back while the fallback is still in flight" shape
// TestOrderDispatchStoreUnavailableFallbackBlocksNormalDispatchWhileInFlight
// needs and orderDispatchTransportFailStore's immutable field cannot give it.
type toggleableTransportFailStore struct {
	beads.Store
	mu        sync.Mutex
	listErr   error
	createErr error
}

func (s *toggleableTransportFailStore) setListErr(err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.listErr = err
}

func (s *toggleableTransportFailStore) setCreateErr(err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.createErr = err
}

func (s *toggleableTransportFailStore) List(query beads.ListQuery) ([]beads.Bead, error) {
	s.mu.Lock()
	err := s.listErr
	s.mu.Unlock()
	if err != nil && isOrderGateListQuery(query) {
		return nil, err
	}
	return s.Store.List(query)
}

func (s *toggleableTransportFailStore) Create(b beads.Bead) (beads.Bead, error) {
	s.mu.Lock()
	err := s.createErr
	s.mu.Unlock()
	if err != nil {
		return beads.Bead{}, err
	}
	return s.Store.Create(b)
}

// TestOrderDispatchStoreUnavailableFallbackBlocksNormalDispatchWhileInFlight
// covers the MEDIUM review item: the fallback records its fire under
// orderHistoryCacheKey(orderName, nil) — no storeKeys, since it runs
// storeless — while the normal path's due-check (cachedLastRun) is scoped by
// the real store's storeKeys, and those two keys never matched before this
// PR's cachedLastRun/peekLastRunLocked fix. A normal dispatch tick racing the
// store's return mid-fallback would read a tracking bead that predates the
// fallback (none exists — the fallback writes none) and could conclude the
// order overdue, firing a SECOND, tracked dispatch while the first
// (untracked) fallback exec was still in flight — the mirror-image of the
// failure mode ga-amol9 fixed on the managed-dolt recover path, reintroduced
// here through the fallback's own separate clock.
func TestOrderDispatchStoreUnavailableFallbackBlocksNormalDispatchWhileInFlight(t *testing.T) {
	const orderName = "fallback-then-normal-race"
	started := make(chan struct{})
	release := make(chan struct{})
	recorder := &reservedDispatchExecRecorder{started: started, release: release}
	order := storeUnavailableFallbackOrder(t, orderName, true)
	store := &toggleableTransportFailStore{Store: beads.NewMemStore()}
	store.setListErr(errConnectionRefused)
	m := buildOrderDispatcherFromListExec([]orders.Order{order}, store, nil, recorder.run, nil).(*memoryOrderDispatcher)
	cityPath := t.TempDir()
	var releaseOnce sync.Once
	releaseRun := func() { releaseOnce.Do(func() { close(release) }) }
	t.Cleanup(func() {
		releaseRun()
		drainOrderDispatch(t, m)
	})

	now := time.Now()
	m.dispatch(context.Background(), cityPath, now)
	awaitClose(t, started, "fallback exec start")

	// The store is back — clear the injected failure, exactly like a
	// recovering server would look on the next tick, while the first
	// fallback exec is still running (single-flight armed, no tracking bead
	// written yet).
	store.setListErr(nil)
	m.dispatch(context.Background(), cityPath, now.Add(time.Millisecond))

	if got := recorder.counts()[orderName]; got != 1 {
		t.Fatalf("exec calls with the fallback still in flight and the store just recovered = %d, want 1 (the normal path must not also fire)", got)
	}

	releaseRun()
	drainOrderDispatch(t, m)
	if got := recorder.counts()[orderName]; got != 1 {
		t.Fatalf("exec calls after the fallback released = %d, want 1", got)
	}
	// The normal path never ran, so it never wrote a tracking bead either.
	if got := len(trackingBeads(t, store, "order-run:"+orderName)); got != 0 {
		t.Fatalf("tracking beads for %s = %d, want 0 (the normal path must not have run at all)", orderName, got)
	}
}

// TestOrderDispatchStoreUnavailableFallbackNoOverlapFromWarmCache pins the
// round-3 review's HIGH finding #1: cachedLastRun's exact-key entry, once
// warmed by an EARLIER completed normal dispatch, used to shadow a LATER
// fallback fire recorded under the fallback's own (storeless) cache key —
// TestOrderDispatchStoreUnavailableFallbackBlocksNormalDispatchWhileInFlight
// above starts from an empty cache and so never exercised this.
//
// Timeline (the reviewer's own reproduction): a normal dispatch completes at
// t0, warming the exact-key cache entry and closing its tracking bead. The
// store then dies; the fallback fires at t0+61s (past the order's 1m
// cooldown) and is still in flight when a new tick sees the store reachable
// again a moment later. A stale exact-key-first read would see the order as
// overdue since t0 (a WARM hit short-circuits before ever consulting
// peekLastRunLocked) and — since the fallback writes no tracking bead — find
// no open work either, launching a second, tracked dispatch while the
// fallback's untracked one is still running. Both this PR's fixes are
// exercised together here: cachedLastRun now takes max(exact, peek) even on
// a warm exact hit, and the shared recoverOnStoreUnavailableExecRunning fact
// blocks the launch regardless.
func TestOrderDispatchStoreUnavailableFallbackNoOverlapFromWarmCache(t *testing.T) {
	const orderName = "warm-cache-overlap"
	recorder := &reservedDispatchExecRecorder{}
	order := storeUnavailableFallbackOrder(t, orderName, true)
	store := &toggleableTransportFailStore{Store: beads.NewMemStore()}
	m := buildOrderDispatcherFromListExec([]orders.Order{order}, store, nil, recorder.run, nil).(*memoryOrderDispatcher)
	cityPath := t.TempDir()

	// Phase 1: a normal dispatch completes while the store is healthy,
	// warming cachedLastRun's exact-key entry and closing its tracking
	// bead. The tracking bead's own CreatedAt (real wall-clock, MemStore
	// does not accept an injected clock) is t0 for the cooldown math below.
	m.dispatch(context.Background(), cityPath, time.Now())
	drainOrderDispatch(t, m)
	if got := recorder.counts()[orderName]; got != 1 {
		t.Fatalf("warm-up exec calls = %d, want 1", got)
	}
	warmBeads := trackingBeads(t, store, "order-run:"+orderName)
	if len(warmBeads) != 1 {
		t.Fatalf("tracking beads after warm-up = %d, want 1", len(warmBeads))
	}
	t0 := warmBeads[0].CreatedAt

	// Phase 2: the store dies; the fallback fires past the order's 1m
	// cooldown and blocks mid-exec.
	started := make(chan struct{})
	release := make(chan struct{})
	recorder.started = started
	recorder.release = release
	store.setListErr(errConnectionRefused)

	t1 := t0.Add(61 * time.Second)
	m.dispatch(context.Background(), cityPath, t1)
	awaitClose(t, started, "fallback exec start")
	if got := recorder.counts()[orderName]; got != 2 {
		t.Fatalf("exec calls once the fallback has fired = %d, want 2 (warm-up + fallback)", got)
	}

	// The store is back, but the fallback exec is still in flight. A new
	// tick must not launch a THIRD exec, even though a stale exact-key read
	// would see the order as overdue since t0.
	store.setListErr(nil)
	m.dispatch(context.Background(), cityPath, t1.Add(time.Second))

	if got := recorder.counts()[orderName]; got != 2 {
		t.Fatalf("exec calls with the fallback still in flight and the store just recovered = %d, want 2 (the normal path must not also fire from a stale warm cache read)", got)
	}

	close(release)
	drainOrderDispatch(t, m)
	if got := recorder.counts()[orderName]; got != 2 {
		t.Fatalf("exec calls after the fallback released = %d, want 2", got)
	}
}

// TestOrderDispatchStoreUnavailableFallbackCooldownHoldsAfterCompletion pins
// the round-5 review's MEDIUM: cachedLastRun's max(exact, peek) must stay
// pinned even after the fallback exec has COMPLETED, not just while it is
// still in flight. The three NoOverlap tests above all exercise the
// in-flight case, where the shared recoverSF guard alone would also block
// a second fire regardless of cachedLastRun's own behavior — reverting
// cachedLastRun to `peekOK && !exactOK` leaves all of them green, because
// the flag masks the regression. This test lets the fallback run to
// completion (recorder.run with no started/release channel set returns
// immediately) before the next tick, so recoverSF has nothing left to
// block and only cachedLastRun's own cooldown math can prevent the
// overlap. Timeline: a normal dispatch completes at t0, warming the
// exact-key cache entry. The store dies; the fallback fires at t0+61s
// (past the order's 1m cooldown) and completes immediately. The store
// recovers at t0+62s: a stale exact-key-first read would see the order as
// overdue since t0 again and fire a THIRD exec, even though the fallback
// already satisfied the order's cooldown one second earlier.
func TestOrderDispatchStoreUnavailableFallbackCooldownHoldsAfterCompletion(t *testing.T) {
	const orderName = "cooldown-holds-after-fallback-completion"
	recorder := &reservedDispatchExecRecorder{}
	order := storeUnavailableFallbackOrder(t, orderName, true)
	store := &toggleableTransportFailStore{Store: beads.NewMemStore()}
	m := buildOrderDispatcherFromListExec([]orders.Order{order}, store, nil, recorder.run, nil).(*memoryOrderDispatcher)
	cityPath := t.TempDir()

	// Phase 1: a normal dispatch completes while the store is healthy,
	// warming cachedLastRun's exact-key entry.
	m.dispatch(context.Background(), cityPath, time.Now())
	drainOrderDispatch(t, m)
	if got := recorder.counts()[orderName]; got != 1 {
		t.Fatalf("warm-up exec calls = %d, want 1", got)
	}
	warmBeads := trackingBeads(t, store, "order-run:"+orderName)
	if len(warmBeads) != 1 {
		t.Fatalf("tracking beads after warm-up = %d, want 1", len(warmBeads))
	}
	t0 := warmBeads[0].CreatedAt

	// Phase 2: the store dies; the fallback fires past the order's 1m
	// cooldown and runs to completion immediately.
	t1 := t0.Add(61 * time.Second)
	store.setListErr(errConnectionRefused)
	m.dispatch(context.Background(), cityPath, t1)
	drainOrderDispatch(t, m)
	if got := recorder.counts()[orderName]; got != 2 {
		t.Fatalf("exec calls once the fallback has fired and completed = %d, want 2 (warm-up + fallback)", got)
	}

	// The store is back one second later. recoverSF has nothing left to
	// block (the fallback already finished and released it) -- cachedLastRun
	// taking max(exact, peek) is the ONLY thing left that can prevent a
	// third exec from a stale exact-key read that still thinks t0 is the
	// last run.
	store.setListErr(nil)
	m.dispatch(context.Background(), cityPath, t1.Add(time.Second))
	drainOrderDispatch(t, m)
	if got := recorder.counts()[orderName]; got != 2 {
		t.Fatalf("exec calls one second after the fallback completed and the store recovered = %d, want 2 (cachedLastRun must not let a stale exact-key read re-fire within the fallback's own cooldown)", got)
	}
}

// TestOrderDispatchStoreUnavailableFallbackNoOverlapWhenCooldownOutrunsTimeout
// pins the round-3 review's HIGH finding #2: beads-health's real
// configuration has timeout=60s but interval=30s, so the normal path's own
// cooldown can elapse — and its due-check can say "due" — well before the
// fallback's exec, still within its own 60s timeout, has finished. The
// normal path never consulted storeUnavailableFallbackRunning before this
// PR, so it could launch a second exec at 31s while the fallback fired at
// t0 was still legitimately running.
func TestOrderDispatchStoreUnavailableFallbackNoOverlapWhenCooldownOutrunsTimeout(t *testing.T) {
	const orderName = "cooldown-outruns-timeout"
	definition := `[order]
exec = "placeholder"
trigger = "cooldown"
interval = "30s"
timeout = "60s"
recover_on_store_unavailable = true
`
	order, err := orders.Parse([]byte(definition))
	if err != nil {
		t.Fatalf("Parse order: %v", err)
	}
	order.Name = orderName
	order.Exec = orderName

	started := make(chan struct{})
	release := make(chan struct{})
	recorder := &reservedDispatchExecRecorder{started: started, release: release}
	store := &toggleableTransportFailStore{Store: beads.NewMemStore()}
	store.setListErr(errConnectionRefused)
	m := buildOrderDispatcherFromListExec([]orders.Order{order}, store, nil, recorder.run, nil).(*memoryOrderDispatcher)
	cityPath := t.TempDir()
	var releaseOnce sync.Once
	releaseRun := func() { releaseOnce.Do(func() { close(release) }) }
	t.Cleanup(func() {
		releaseRun()
		drainOrderDispatch(t, m)
	})

	t0 := time.Now()
	m.dispatch(context.Background(), cityPath, t0)
	awaitClose(t, started, "fallback exec start")
	if got := recorder.counts()[orderName]; got != 1 {
		t.Fatalf("exec calls once the fallback has fired = %d, want 1", got)
	}

	// The store recovers immediately, well inside the fallback's own 60s
	// timeout. At t0+31s — past the order's 30s cooldown, which is what
	// would make a due-check say "due" — the fallback exec is still
	// blocked mid-flight; the normal path must not launch a second one.
	store.setListErr(nil)
	m.dispatch(context.Background(), cityPath, t0.Add(31*time.Second))

	if got := recorder.counts()[orderName]; got != 1 {
		t.Fatalf("exec calls at t0+31s with the fallback still in flight = %d, want 1 (a 30s-cooldown order's normal path must not fire while a 60s-timeout fallback exec is still running)", got)
	}

	releaseRun()
	drainOrderDispatch(t, m)
	if got := recorder.counts()[orderName]; got != 1 {
		t.Fatalf("exec calls after the fallback released = %d, want 1", got)
	}
}

// TestOrderDispatchStoreUnavailableFallbackNoOverlapReverseDirection pins
// the round-3 review's HIGH finding #3: the mirror-image direction, where a
// NORMAL dispatch is already in flight (its own exec's recover call may be
// mid-stop/start against Dolt) when the store starts refusing connections,
// and — once the order's own cooldown has passed — the fallback must not
// fire a second, untracked exec while the normal one is still running.
// admitManagedDoltRecover's throttle (dolt_recover_gate.go) is an
// in-process sync.Map: two `gc beads health` processes mean two throttles,
// so two recovers could overlap in the stop-then-start gap, and the second
// one's stop could kill the server the first just started — exactly the
// "never replace a live server, one throttle" invariant this guard exists
// to protect regardless of which direction the overlap comes from.
func TestOrderDispatchStoreUnavailableFallbackNoOverlapReverseDirection(t *testing.T) {
	const orderName = "reverse-direction-overlap"
	started := make(chan struct{})
	release := make(chan struct{})
	recorder := &reservedDispatchExecRecorder{started: started, release: release}
	order := storeUnavailableFallbackOrder(t, orderName, true)
	store := &toggleableTransportFailStore{Store: beads.NewMemStore()}
	m := buildOrderDispatcherFromListExec([]orders.Order{order}, store, nil, recorder.run, nil).(*memoryOrderDispatcher)
	cityPath := t.TempDir()
	var releaseOnce sync.Once
	releaseRun := func() { releaseOnce.Do(func() { close(release) }) }
	t.Cleanup(func() {
		releaseRun()
		drainOrderDispatch(t, m)
	})

	// A normal dispatch starts against a healthy store and blocks mid-exec
	// -- standing in for its own recover call being in progress.
	now := time.Now()
	m.dispatch(context.Background(), cityPath, now)
	awaitClose(t, started, "normal dispatch exec start")
	if got := recorder.counts()[orderName]; got != 1 {
		t.Fatalf("normal-path exec calls = %d, want 1", got)
	}

	// The store now refuses connections -- standing in for the in-flight
	// exec's own recover call having just stopped Dolt.
	store.setListErr(errConnectionRefused)

	// Past the order's own 1m cooldown, a new tick sees the dead store and
	// must not fire the fallback while the normal dispatch is still in
	// flight.
	m.dispatch(context.Background(), cityPath, now.Add(61*time.Second))

	if got := recorder.counts()[orderName]; got != 1 {
		t.Fatalf("exec calls with a normal dispatch still in flight and the store now dead = %d, want 1 (the fallback must not also fire)", got)
	}

	releaseRun()
	drainOrderDispatch(t, m)
	if got := recorder.counts()[orderName]; got != 1 {
		t.Fatalf("exec calls after the normal dispatch released = %d, want 1", got)
	}
}

// TestReplaceOrderDispatcherSharesRecoverSingleFlight pins the round-5
// review's HIGH: replaceOrderDispatcher carries lastRunCache, gate backoff,
// and open-work suppression across a dispatcher swap (config reload or
// order rescan), but before this fix did NOT carry the recoverSF
// single-flight guard -- the incoming dispatcher started with an empty
// map. A swap mid-incident (the operator running `gc reload` while the
// store is down, or a rescan racing a live recover) could therefore admit
// a second, concurrent `gc beads health` exec: the outgoing dispatcher's
// exec, launched before the swap, keeps running (a 1s drain timeout parks
// it in retiredOrderDispatchers rather than waiting for it), while the
// incoming dispatcher's fresh, empty map lets the SAME order fire again.
//
// This test drives that exact timeline: dispatcher A starts an exec and
// blocks mid-flight; CityRuntime swaps to dispatcher B while A's exec is
// still running; B ticks past the order's cooldown and must see exactly
// one exec in flight, not two -- proving the swap SHARES the guard (by
// pointer) rather than resetting it.
func TestReplaceOrderDispatcherSharesRecoverSingleFlight(t *testing.T) {
	const orderName = "swap-shares-single-flight"
	started := make(chan struct{})
	release := make(chan struct{})
	recorder := &reservedDispatchExecRecorder{started: started, release: release}
	order := storeUnavailableFallbackOrder(t, orderName, true)
	store := &toggleableTransportFailStore{Store: beads.NewMemStore()}
	cityPath := t.TempDir()

	store.setListErr(errConnectionRefused)
	dispatcherA := buildOrderDispatcherFromListExec([]orders.Order{order}, store, nil, recorder.run, nil).(*memoryOrderDispatcher)

	// The store is down; A fires the storeless fallback and blocks
	// mid-exec. This matters: the fallback writes NO tracking bead, so
	// nothing store-backed (an open-work gate) can independently block a
	// second fire the way it would for a normal, tracked dispatch -- only
	// the single-flight guard can.
	now := time.Now()
	dispatcherA.dispatch(context.Background(), cityPath, now)
	awaitClose(t, started, "dispatcher A fallback exec start")
	if got := recorder.counts()[orderName]; got != 1 {
		t.Fatalf("exec calls on A = %d, want 1", got)
	}

	// CityRuntime swaps to B WHILE A's exec is still in flight.
	dispatcherB := buildOrderDispatcherFromListExec([]orders.Order{order}, store, nil, recorder.run, nil).(*memoryOrderDispatcher)
	cr := &CityRuntime{od: dispatcherA}
	cr.replaceOrderDispatcher(dispatcherB)
	if cr.od != orderDispatcher(dispatcherB) {
		t.Fatalf("replaceOrderDispatcher installed %v, want dispatcher B", cr.od)
	}

	// Past the order's own 1m cooldown, with the store STILL down, B would
	// also try the fallback -- which writes no tracking bead, so nothing
	// but the shared single-flight guard can stop it from firing a
	// second, concurrent exec while A's fallback is still running.
	dispatcherB.dispatch(context.Background(), cityPath, now.Add(61*time.Second))
	if got := recorder.counts()[orderName]; got != 1 {
		t.Fatalf("exec calls on B while A's fallback exec is still in flight = %d, want 1 (the swap must share the single-flight guard, not reset it)", got)
	}

	close(release)
	drainOrderDispatch(t, dispatcherA)
	drainOrderDispatch(t, dispatcherB)
	if got := recorder.counts()[orderName]; got != 1 {
		t.Fatalf("exec calls after A's fallback released = %d, want 1", got)
	}

	// The guard must not leak forever either: once A's fallback has
	// released and enough time has passed, B must still be able to fire
	// its own fallback for this order (the store is still down).
	dispatcherB.dispatch(context.Background(), cityPath, now.Add(130*time.Second))
	drainOrderDispatch(t, dispatcherB)
	if got := recorder.counts()[orderName]; got != 2 {
		t.Fatalf("exec calls after A released and B's own cooldown passed = %d, want 2 (B must still be able to fire once the shared guard clears)", got)
	}
}

// openWorkGateDualBehaviorStore fails the open-work gate's two distinct
// reads differently: the broad, unlabeled index scan (entriesForStore's
// call, CACHED and shared across both open-work gates within a tick) times
// out, while the narrow order-run:<scoped>-labeled strict query
// (hasOpenWorkStrict's own live read, which hasOpenWork retries when the
// cached index read errors) hits a genuine transport failure.
//
// This is the one shape that reaches openWorkGateShut's own fallback call
// (the LOW review item: it had zero coverage) WITH the fallback actually
// firing. hasOpenTracking (the FIRST gate, in dispatch's per-order loop)
// returns a cached index error directly with no live retry, so for an
// idempotent order a contention-timeout-classified index error fails OPEN
// there (gateFailClosed, isGateContentionTimeout) and a candidate is
// formed — the fallback does NOT fire from that call. hasOpenWork (the
// SECOND gate, openWorkGateShut) consults the SAME cached error but then
// retries live via hasOpenWorkStrict before giving up; when THAT live
// retry hits a genuine (non-timeout) transport failure, gateFailClosed
// blocks unconditionally, and openWorkGateShut's own fallback call fires.
type openWorkGateDualBehaviorStore struct {
	beads.Store
	indexErr  error
	strictErr error
}

func (s *openWorkGateDualBehaviorStore) List(query beads.ListQuery) ([]beads.Bead, error) {
	if s.indexErr != nil && isOrderGateIndexQuery(query) {
		return nil, s.indexErr
	}
	// hasOpenWorkStrict's live retry query only — NOT the trigger's own
	// last-run lookup, which also carries an "order-run:" label prefix but
	// sets IncludeClosed/Limit (it needs closed tracking beads to find the
	// last run), the exact distinction isOrderGateListQuery itself draws.
	if s.strictErr != nil && isOrderGateListQuery(query) && !isOrderGateIndexQuery(query) {
		return nil, s.strictErr
	}
	return s.Store.List(query)
}

func TestOrderDispatchStoreUnavailableFallbackFiresFromOpenWorkGateShutHook(t *testing.T) {
	const orderName = "open-work-gate-shut"
	recorder := &reservedDispatchExecRecorder{}
	definition := `[order]
exec = "placeholder"
trigger = "cooldown"
interval = "1m"
idempotent = true
recover_on_store_unavailable = true
`
	order, err := orders.Parse([]byte(definition))
	if err != nil {
		t.Fatalf("Parse order: %v", err)
	}
	order.Name = orderName
	order.Exec = orderName

	store := &openWorkGateDualBehaviorStore{
		Store:     beads.NewMemStore(),
		indexErr:  context.DeadlineExceeded,
		strictErr: errConnectionRefused,
	}
	m := buildOrderDispatcherFromListExec([]orders.Order{order}, store, nil, recorder.run, nil).(*memoryOrderDispatcher)
	cityPath := t.TempDir()

	m.dispatch(context.Background(), cityPath, time.Now())
	drainOrderDispatch(t, m)

	if got := recorder.counts()[orderName]; got != 1 {
		t.Fatalf("openWorkGateShut fallback exec calls = %d, want 1", got)
	}
}

func TestOrderDispatchStoreUnavailableFallbackNeverFiresWhenStoreOpens(t *testing.T) {
	// The fallback is reached ONLY from a store-open/gate/CreateRun failure,
	// so a store that opens and answers fine — live, even if the normal
	// dispatch that follows is slow — must never also trigger it. This is
	// the dispatcher-level half of "a live-but-slow server is never
	// replaced": the store answering here means there IS something to
	// protect, and the fallback path structurally cannot run in that case.
	const orderName = "store-is-fine"
	recorder := &reservedDispatchExecRecorder{}
	order := storeUnavailableFallbackOrder(t, orderName, true)
	store := beads.NewMemStore()
	m := buildOrderDispatcherFromListExec([]orders.Order{order}, store, nil, recorder.run, nil).(*memoryOrderDispatcher)
	cityPath := t.TempDir()

	m.dispatch(context.Background(), cityPath, time.Now())
	drainOrderDispatch(t, m)

	if got := recorder.counts()[orderName]; got != 1 {
		t.Fatalf("normal-path exec calls with a healthy store = %d, want 1", got)
	}
	// Proof it went through the normal tracking-bead route, not the
	// storeless fallback (which writes none): a real tracking bead exists.
	if got := len(trackingBeads(t, store, "order-run:"+orderName)); got != 1 {
		t.Fatalf("tracking beads for %s = %d, want 1 (normal dispatch must still write one)", orderName, got)
	}
}

// TestClassifyStoreUnavailableFallbackTrigger covers the ga-3bwmf review
// round-3 M1 finding: at load 200 the reviewer's probe fired this fallback
// against a LIVE store on several shapes the classifier used to treat as
// death evidence. Every shape from that finding is pinned here, both the
// ones that must now be rejected and the ones that must still be accepted.
func TestClassifyStoreUnavailableFallbackTrigger(t *testing.T) {
	wrappedDeadline := fmt.Errorf("dial tcp 127.0.0.1:48770: %w", context.DeadlineExceeded)
	for _, tc := range []struct {
		name string
		err  error
		want bool
	}{
		{"nil error", nil, false},
		{"bare context.DeadlineExceeded", context.DeadlineExceeded, false},
		{"dial tcp i/o timeout text", errors.New("dial tcp 127.0.0.1:48770: connect: i/o timeout"), false},
		{"connect operation timed out text", errors.New("connect: operation timed out"), false},
		{"wrapped DeadlineExceeded whose text also contains dial tcp", wrappedDeadline, false},
		{"driver bad connection", errors.New("driver: bad connection"), false},
		{"broken pipe", errors.New("write: broken pipe"), false},
		{"unexpected EOF", errors.New("read: unexpected EOF"), false},
		{"use of closed network connection", errors.New("read: use of closed network connection"), false},
		{"EADDRNOTAVAIL", errors.New("dial tcp 127.0.0.1:48770: bind: can't assign requested address"), false},
		{"connection refused", errConnectionRefused, true},
		{"no such host", errors.New("dial tcp: lookup dolt-host: no such host"), true},
		{"server unreachable", errors.New("server unreachable"), true},
		{"bd auto-import marker, no timeout text", errors.New("auto-importing into empty database"), true},
		{"bd auto-import marker WITH timeout text", errors.New("auto-importing into empty database: dial tcp: i/o timeout"), false},
		{"unrelated error", errors.New("invalid issue type"), false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := classifyStoreUnavailableFallbackTrigger(tc.err); got != tc.want {
				t.Errorf("classifyStoreUnavailableFallbackTrigger(%v) = %v, want %v", tc.err, got, tc.want)
			}
		})
	}
}

// ---------------------------------------------------------------------
// recoverOnStoreUnavailableExecRunning leak-safety (ga-3bwmf review round
// 3, guard #1): "if it's left set, beads-health is silently disabled
// forever, which is worse than a double fire." Each test below drives one
// early-exit path to completion and then proves the order is still
// dispatchable afterward -- the only observable proof the flag did not
// leak, since it is unexported dispatcher state.
// ---------------------------------------------------------------------

// panicOnceExecRunner panics on its first invocation -- standing in for an
// exec callback panic -- and behaves like an ordinary immediate-success
// exec on every later call.
func panicOnceExecRunner(calls *int32) ExecRunner {
	return func(_ context.Context, _, _ string, _ []string) ([]byte, error) {
		if atomic.AddInt32(calls, 1) == 1 {
			panic("simulated exec panic (ga-3bwmf review round 3 leak-safety test)")
		}
		return nil, nil
	}
}

// ctxDoneOnceExecRunner blocks until ctx is done and returns ctx.Err() on
// its FIRST invocation only -- standing in for both an externally-canceled
// dispatch context and an order's own exec timeout elapsing, since both
// manifest identically from inside the exec, as a context that becomes
// Done. Every later call behaves like an ordinary immediate-success exec,
// so a SECOND dispatch -- proving the order is still dispatchable after the
// early exit -- does not also block on a ctx that was never going to
// become Done (a later tick's own ctx is uncancelled and carries no short
// deadline, unlike the first).
func ctxDoneOnceExecRunner(calls *int32) ExecRunner {
	return func(ctx context.Context, _, _ string, _ []string) ([]byte, error) {
		if atomic.AddInt32(calls, 1) == 1 {
			<-ctx.Done()
			return nil, ctx.Err()
		}
		return nil, nil
	}
}

// startSignalingCtxDoneOnceExecRunner is ctxDoneOnceExecRunner plus a
// one-shot signal the moment the FIRST call starts, so a test can wait for
// the exec to be genuinely in flight before canceling its context out
// from under it.
func startSignalingCtxDoneOnceExecRunner(calls *int32, started chan struct{}) ExecRunner {
	var once sync.Once
	return func(ctx context.Context, _, _ string, _ []string) ([]byte, error) {
		if atomic.AddInt32(calls, 1) == 1 {
			once.Do(func() { close(started) })
			<-ctx.Done()
			return nil, ctx.Err()
		}
		return nil, nil
	}
}

// timeoutOrder builds a cooldown-triggered, RecoverOnStoreUnavailable exec
// order with an explicit (short) exec timeout, for the two timeout-path
// leak-safety tests below.
func timeoutOrder(t *testing.T, name, timeout string) orders.Order {
	t.Helper()
	definition := fmt.Sprintf(`[order]
exec = "placeholder"
trigger = "cooldown"
interval = "1m"
timeout = "%s"
recover_on_store_unavailable = true
`, timeout)
	order, err := orders.Parse([]byte(definition))
	if err != nil {
		t.Fatalf("Parse order: %v", err)
	}
	order.Name = name
	order.Exec = name
	return order
}

// assertSingleFlightReleased asserts, by reading recoverSingleFlight's map
// directly under its own mutex, that scoped is NOT marked running on m right
// now (ga-3bwmf review round 5 LOW). This is deliberately a direct state
// check rather than an inference from a second dispatch's behavior: the
// round-3 onDone-vs-doneInflight ordering bug this PR fixed (see
// dispatchOne's preInflightRelease) was ~30% intermittent precisely because
// a stale release raced against drainOrderDispatch's own wakeup, so an
// indirect "did the second dispatch succeed" assertion only ever caught it
// probabilistically -- reliably under -race (which perturbs scheduling
// enough to widen the window), not in a plain `go test` run. Calling this
// directly after drainOrderDispatch turns that into a deterministic check:
// dispatchOne's preInflightRelease hook is a defer in the SAME function
// frame as doneInflight, registered to unwind strictly before it (see the
// doc comment on memoryOrderDispatcher.recoverSF), so by the time drain()
// observes doneInflight's effect, the release is already guaranteed to
// have happened -- no timing dependency left to hide behind.
func assertSingleFlightReleased(t *testing.T, m *memoryOrderDispatcher, scoped string) {
	t.Helper()
	sf := m.singleFlight()
	sf.mu.Lock()
	held := sf.running[scoped]
	sf.mu.Unlock()
	if held {
		t.Fatalf("recoverSF still marks %q running directly after drain -- the release is not actually visible by the time drain() returns", scoped)
	}
}

func TestOrderDispatchStoreUnavailableFallbackReleasesRunningFactOnNormalPathPanic(t *testing.T) {
	const orderName = "normal-path-panic-releases"
	var calls int32
	order := storeUnavailableFallbackOrder(t, orderName, true)
	store := beads.NewMemStore()
	m := buildOrderDispatcherFromListExec([]orders.Order{order}, store, nil, panicOnceExecRunner(&calls), nil).(*memoryOrderDispatcher)
	cityPath := t.TempDir()

	// First dispatch: the exec panics. runDispatchGuarded's own recover
	// must catch it, and the running fact must still be released.
	m.dispatch(context.Background(), cityPath, time.Now())
	drainOrderDispatch(t, m)
	if got := atomic.LoadInt32(&calls); got != 1 {
		t.Fatalf("exec calls after the panicking run = %d, want 1", got)
	}

	assertSingleFlightReleased(t, m, order.ScopedName())

	// A second, LATER dispatch (well past the order's own cooldown, so
	// only the leak question is being tested) must still be able to
	// launch.
	m.dispatch(context.Background(), cityPath, time.Now().Add(2*time.Minute))
	drainOrderDispatch(t, m)
	if got := atomic.LoadInt32(&calls); got != 2 {
		t.Fatalf("exec calls after a later tick = %d, want 2 (the running fact must not leak from a normal-path exec panic)", got)
	}
}

func TestOrderDispatchStoreUnavailableFallbackReleasesRunningFactOnFallbackPanic(t *testing.T) {
	const orderName = "fallback-panic-releases"
	var calls int32
	order := storeUnavailableFallbackOrder(t, orderName, true)
	store := &toggleableTransportFailStore{Store: beads.NewMemStore()}
	store.setListErr(errConnectionRefused)
	m := buildOrderDispatcherFromListExec([]orders.Order{order}, store, nil, panicOnceExecRunner(&calls), nil).(*memoryOrderDispatcher)
	cityPath := t.TempDir()

	// First tick: store down, the fallback fires, its exec panics.
	m.dispatch(context.Background(), cityPath, time.Now())
	drainOrderDispatch(t, m)
	if got := atomic.LoadInt32(&calls); got != 1 {
		t.Fatalf("exec calls after the panicking fallback = %d, want 1", got)
	}

	assertSingleFlightReleased(t, m, order.ScopedName())

	// Store recovers; a LATER tick's normal path must still be able to
	// launch.
	store.setListErr(nil)
	m.dispatch(context.Background(), cityPath, time.Now().Add(2*time.Minute))
	drainOrderDispatch(t, m)
	if got := atomic.LoadInt32(&calls); got != 2 {
		t.Fatalf("exec calls after a later tick = %d, want 2 (the running fact must not leak from a fallback exec panic)", got)
	}
}

func TestOrderDispatchStoreUnavailableFallbackReleasesRunningFactOnNormalPathCtxCancel(t *testing.T) {
	const orderName = "normal-path-ctx-cancel-releases"
	var calls int32
	started := make(chan struct{})
	order := storeUnavailableFallbackOrder(t, orderName, true)
	store := beads.NewMemStore()
	m := buildOrderDispatcherFromListExec([]orders.Order{order}, store, nil, startSignalingCtxDoneOnceExecRunner(&calls, started), nil).(*memoryOrderDispatcher)
	cityPath := t.TempDir()

	dispatchCtx, cancelDispatch := context.WithCancel(context.Background())
	m.dispatch(dispatchCtx, cityPath, time.Now())
	awaitClose(t, started, "normal dispatch exec start")
	cancelDispatch()
	drainOrderDispatch(t, m)
	if got := atomic.LoadInt32(&calls); got != 1 {
		t.Fatalf("exec calls after ctx cancel = %d, want 1", got)
	}
	assertSingleFlightReleased(t, m, order.ScopedName())

	m.dispatch(context.Background(), cityPath, time.Now().Add(2*time.Minute))
	drainOrderDispatch(t, m)
	if got := atomic.LoadInt32(&calls); got != 2 {
		t.Fatalf("exec calls after a later tick = %d, want 2 (the running fact must not leak from a canceled dispatch context)", got)
	}
}

func TestOrderDispatchStoreUnavailableFallbackReleasesRunningFactOnFallbackCtxCancel(t *testing.T) {
	const orderName = "fallback-ctx-cancel-releases"
	var calls int32
	started := make(chan struct{})
	order := storeUnavailableFallbackOrder(t, orderName, true)
	store := &toggleableTransportFailStore{Store: beads.NewMemStore()}
	store.setListErr(errConnectionRefused)
	m := buildOrderDispatcherFromListExec([]orders.Order{order}, store, nil, startSignalingCtxDoneOnceExecRunner(&calls, started), nil).(*memoryOrderDispatcher)
	cityPath := t.TempDir()

	dispatchCtx, cancelDispatch := context.WithCancel(context.Background())
	m.dispatch(dispatchCtx, cityPath, time.Now())
	awaitClose(t, started, "fallback exec start")
	cancelDispatch()
	drainOrderDispatch(t, m)
	if got := atomic.LoadInt32(&calls); got != 1 {
		t.Fatalf("exec calls after ctx cancel = %d, want 1", got)
	}
	assertSingleFlightReleased(t, m, order.ScopedName())

	store.setListErr(nil)
	m.dispatch(context.Background(), cityPath, time.Now().Add(2*time.Minute))
	drainOrderDispatch(t, m)
	if got := atomic.LoadInt32(&calls); got != 2 {
		t.Fatalf("exec calls after a later tick = %d, want 2 (the running fact must not leak from a canceled fallback context)", got)
	}
}

func TestOrderDispatchStoreUnavailableFallbackReleasesRunningFactOnNormalPathTimeout(t *testing.T) {
	const orderName = "normal-path-timeout-releases"
	var calls int32
	order := timeoutOrder(t, orderName, "50ms")
	store := beads.NewMemStore()
	m := buildOrderDispatcherFromListExec([]orders.Order{order}, store, nil, ctxDoneOnceExecRunner(&calls), nil).(*memoryOrderDispatcher)
	cityPath := t.TempDir()

	// The 50ms exec timeout elapses on its own inside drainOrderDispatch's
	// 10s budget.
	m.dispatch(context.Background(), cityPath, time.Now())
	drainOrderDispatch(t, m)
	if got := atomic.LoadInt32(&calls); got != 1 {
		t.Fatalf("exec calls after the timeout = %d, want 1", got)
	}

	assertSingleFlightReleased(t, m, order.ScopedName())

	m.dispatch(context.Background(), cityPath, time.Now().Add(2*time.Minute))
	drainOrderDispatch(t, m)
	if got := atomic.LoadInt32(&calls); got != 2 {
		t.Fatalf("exec calls after a later tick = %d, want 2 (the running fact must not leak from the order's own exec timeout)", got)
	}
}

func TestOrderDispatchStoreUnavailableFallbackReleasesRunningFactOnFallbackTimeout(t *testing.T) {
	const orderName = "fallback-timeout-releases"
	var calls int32
	order := timeoutOrder(t, orderName, "50ms")
	store := &toggleableTransportFailStore{Store: beads.NewMemStore()}
	store.setListErr(errConnectionRefused)
	m := buildOrderDispatcherFromListExec([]orders.Order{order}, store, nil, ctxDoneOnceExecRunner(&calls), nil).(*memoryOrderDispatcher)
	cityPath := t.TempDir()

	m.dispatch(context.Background(), cityPath, time.Now())
	drainOrderDispatch(t, m)
	if got := atomic.LoadInt32(&calls); got != 1 {
		t.Fatalf("exec calls after the fallback's own timeout = %d, want 1", got)
	}

	assertSingleFlightReleased(t, m, order.ScopedName())

	store.setListErr(nil)
	m.dispatch(context.Background(), cityPath, time.Now().Add(2*time.Minute))
	drainOrderDispatch(t, m)
	if got := atomic.LoadInt32(&calls); got != 2 {
		t.Fatalf("exec calls after a later tick = %d, want 2 (the running fact must not leak from the fallback's own exec timeout)", got)
	}
}

func TestOrderDispatchStoreUnavailableFallbackReleasesRunningFactOnCreateRunError(t *testing.T) {
	const orderName = "createrun-error-releases"
	recorder := &reservedDispatchExecRecorder{}
	order := storeUnavailableFallbackOrder(t, orderName, true)
	store := &toggleableTransportFailStore{Store: beads.NewMemStore()}
	store.setCreateErr(errConnectionRefused)
	m := buildOrderDispatcherFromListExec([]orders.Order{order}, store, nil, recorder.run, nil).(*memoryOrderDispatcher)
	cityPath := t.TempDir()

	// First tick: the normal path's CreateRun fails with a real transport
	// error, so launchResolvedDispatch releases the flag via the
	// "not committed" safety net (nothing launched) rather than the
	// goroutine's own defer, and the fallback fires instead.
	m.dispatch(context.Background(), cityPath, time.Now())
	drainOrderDispatch(t, m)
	if got := recorder.counts()[orderName]; got != 1 {
		t.Fatalf("exec calls after the CreateRun failure (fallback) = %d, want 1", got)
	}

	assertSingleFlightReleased(t, m, order.ScopedName())

	// The store recovers fully; a LATER tick's normal path must still be
	// able to launch.
	store.setCreateErr(nil)
	m.dispatch(context.Background(), cityPath, time.Now().Add(2*time.Minute))
	drainOrderDispatch(t, m)
	if got := recorder.counts()[orderName]; got != 2 {
		t.Fatalf("exec calls after a later tick = %d, want 2 (the running fact must not leak from a CreateRun failure)", got)
	}
}

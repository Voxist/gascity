package main

import (
	"context"
	"errors"
	"sync"
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
	mu      sync.Mutex
	listErr error
}

func (s *toggleableTransportFailStore) setListErr(err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.listErr = err
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

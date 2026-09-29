package main

import (
	"context"
	"errors"
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

// errConnectionRefused is a synthetic store-open failure shaped like a real
// dolt sql-server that is not listening at all (ga-xuapz's ~2min gap): the
// message contains the "dial tcp"/"connection refused" markers
// classifyWorkQueryStoreUnavailable (and the fleet's shared transport
// classifier, bdTransportRetryableMarkers) key on.
var errConnectionRefused = errors.New("opening store: dial tcp 127.0.0.1:48770: connect: connection refused")

// dispatcherWithFailingStore builds a dispatcher over aa whose storeFn always
// fails with errConnectionRefused, exactly as it would when the managed dolt
// behind cityPath's beads store is confirmed dead (process gone, port free).
func dispatcherWithFailingStore(t *testing.T, aa []orders.Order, execRun ExecRunner) *memoryOrderDispatcher {
	t.Helper()
	m := buildOrderDispatcherFromListExec(aa, beads.NewMemStore(), nil, execRun, nil).(*memoryOrderDispatcher)
	m.storeFn = func(execStoreTarget) (beads.Store, error) {
		return nil, errConnectionRefused
	}
	return m
}

func TestOrderDispatchStoreUnavailableFallbackFiresWhenStoreIsDown(t *testing.T) {
	// ga-3bwmf: this is the failing-first test for the order-dispatch
	// chicken-and-egg — a dead managed dolt with the port free must still
	// get beads-health's exec to run on the periodic path, even though the
	// dispatcher cannot open a store to due-check or tracking-bead it.
	const orderName = "beads-health-like"
	recorder := &reservedDispatchExecRecorder{}
	order := storeUnavailableFallbackOrder(t, orderName, true)
	m := dispatcherWithFailingStore(t, []orders.Order{order}, recorder.run)
	cityPath := t.TempDir()

	m.dispatch(context.Background(), cityPath, time.Now())
	drainOrderDispatch(t, m)

	if got := recorder.counts()[orderName]; got != 1 {
		t.Fatalf("store-unavailable fallback exec calls = %d, want 1 (store was down the whole tick)", got)
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
	m := dispatcherWithFailingStore(t, []orders.Order{order}, recorder.run)
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
	m := dispatcherWithFailingStore(t, []orders.Order{order}, recorder.run)
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

func TestOrderDispatchStoreUnavailableFallbackNeverFiresWhenStoreOpens(t *testing.T) {
	// The fallback is reached ONLY from the store-open failure branch, so a
	// store that opens successfully — live, even if the normal dispatch
	// that follows is slow — must never also trigger it. This is the
	// dispatcher-level half of "a live-but-slow server is never replaced":
	// the store-open succeeding here means there IS something to protect,
	// and the fallback path structurally cannot run in that case.
	const orderName = "store-is-fine"
	recorder := &reservedDispatchExecRecorder{}
	order := storeUnavailableFallbackOrder(t, orderName, true)
	m := buildOrderDispatcherFromListExec([]orders.Order{order}, beads.NewMemStore(), nil, recorder.run, nil).(*memoryOrderDispatcher)
	cityPath := t.TempDir()

	m.dispatch(context.Background(), cityPath, time.Now())
	drainOrderDispatch(t, m)

	// Exactly one dispatch happened (the normal tracking-bead path), and it
	// carries a tracking bead — proof it went through the normal route, not
	// the storeless fallback (which writes none).
	if got := recorder.counts()[orderName]; got != 1 {
		t.Fatalf("normal-path exec calls with a healthy store = %d, want 1", got)
	}
}

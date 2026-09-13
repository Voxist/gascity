package main

import (
	"context"
	"io"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
)

// AC2 of the vc-ny00 store-warming plan, asserted directly on the tick path
// rather than argued from its parts.
//
// The plan's AC2 says: with one store answering slower than the wall, orders
// for unaffected stores still dispatch on cadence — the dispatcher stays
// alive. The 2026-09-06 incident is the counterexample: dispatchOrders runs
// inside the single serial tick body, ahead of the session reconcile, and
// when the order-tracking sweep serialized behind a store that had stopped
// answering, every one of 20 checked cooldown orders went stale at once.
//
// The earlier tests in this suite establish the PARTS — the breaker trips
// (TestBreakerTripsWithinNBoundExceededReads), the sweep drops a degraded
// store (TestDegradedStoresAreDroppedFromTheTickSweep), one store's breaker
// does not degrade another (TestOneStoresBreakerDoesNotDegradeAnother), and
// the worst-case tick is bounded (TestWorstCaseTickCostIsBoundedByTheTickRead).
// This test asserts the CONCLUSION those parts are meant to add up to, on the
// real dispatchOrders path, because an argument that the tick cannot
// serialize is not the same as a demonstration that dispatch still fires.

// wedgedStore is a store whose every read blocks until the test releases it.
// It stands in for the 2026-09-06 store: reachable, accepted the query, and
// never answered. If the vc-ny00 breaker filter fails to skip it, any sweep
// that touches it blocks the whole tick — which is precisely the failure
// under test, and shows up here as dispatchOrders never returning.
type wedgedStore struct {
	beads.Store
	prefix  string
	release chan struct{}
	touched chan struct{}
}

func newWedgedStore(prefix string) *wedgedStore {
	return &wedgedStore{
		Store:   beads.NewMemStore(),
		prefix:  prefix,
		release: make(chan struct{}),
		touched: make(chan struct{}, 64),
	}
}

func (s *wedgedStore) IDPrefix() string { return s.prefix }

func (s *wedgedStore) List(q beads.ListQuery) ([]beads.Bead, error) {
	select {
	case s.touched <- struct{}{}:
	default:
	}
	<-s.release // never answers until the test says so
	return s.Store.List(q)
}

func (s *wedgedStore) wasTouched() bool {
	select {
	case <-s.touched:
		return true
	default:
		return false
	}
}

func TestDegradedStoreDoesNotStallOrderDispatch(t *testing.T) {
	useWarmingTestWall(t)
	beads.ResetStoreWarmingRegistryForTest()
	t.Cleanup(beads.ResetStoreWarmingRegistryForTest)

	cityPath := t.TempDir()
	rigPath := t.TempDir()

	wedged := newWedgedStore("vcnywedge")
	defer close(wedged.release) // unblock anything still parked at teardown

	// The reconciler has already proven this store cannot answer inside the
	// wall, so its breaker is open before the tick runs.
	degradeStore(t, "vcnywedge")

	cfg := &config.City{}
	cfg.Rigs = []config.Rig{{Name: "wedgedrig", Path: rigPath, Prefix: "vcnywedge"}}

	od := &recordingOrderDispatcher{}
	cr := &CityRuntime{
		cfg:                 cfg,
		cityPath:            cityPath,
		cityName:            "test-city",
		logPrefix:           "gc start",
		stderr:              io.Discard,
		stdout:              io.Discard,
		od:                  od,
		standaloneCityStore: beads.NewMemStore(),
		standaloneRigStores: map[string]beads.Store{"wedgedrig": wedged},
	}

	done := make(chan struct{})
	go func() {
		defer close(done)
		cr.dispatchOrders(context.Background(), cityPath)
	}()

	select {
	case <-done:
	case <-time.After(20 * time.Second):
		t.Fatal("dispatchOrders never returned with one degraded store present — " +
			"the serial tick serialized behind a store that cannot answer. This is " +
			"the 2026-09-06 cliff: dispatchOrders runs inside the tick body, so a " +
			"sweep that blocks here takes order dispatch fleet-wide down with it.")
	}

	if !od.called.Load() {
		t.Fatal("the order dispatcher never ran; a degraded store must not stop " +
			"dispatch for the stores that are healthy (vc-ny00 AC2)")
	}
	if wedged.wasTouched() {
		t.Error("the degraded store was read during the tick; the breaker must " +
			"skip it outright rather than pay for a read that will not answer")
	}
}

// TestHealthyStoreStillDispatchesWhenNoStoreIsDegraded is the control: the
// same path with nothing degraded must still dispatch, so a passing result
// above cannot be produced by the dispatcher simply never being wired.
func TestHealthyStoreStillDispatchesWhenNoStoreIsDegraded(t *testing.T) {
	useWarmingTestWall(t)
	beads.ResetStoreWarmingRegistryForTest()
	t.Cleanup(beads.ResetStoreWarmingRegistryForTest)

	cityPath := t.TempDir()
	rigPath := t.TempDir()

	cfg := &config.City{}
	cfg.Rigs = []config.Rig{{Name: "healthyrig", Path: rigPath, Prefix: "vcnyok"}}

	od := &recordingOrderDispatcher{}
	cr := &CityRuntime{
		cfg:                 cfg,
		cityPath:            cityPath,
		cityName:            "test-city",
		logPrefix:           "gc start",
		stderr:              io.Discard,
		stdout:              io.Discard,
		od:                  od,
		standaloneCityStore: beads.NewMemStore(),
		standaloneRigStores: map[string]beads.Store{"healthyrig": newPrefixedStore("vcnyok")},
	}

	done := make(chan struct{})
	go func() {
		defer close(done)
		cr.dispatchOrders(context.Background(), cityPath)
	}()

	select {
	case <-done:
	case <-time.After(20 * time.Second):
		t.Fatal("dispatchOrders did not return on an all-healthy city")
	}
	if !od.called.Load() {
		t.Fatal("the order dispatcher never ran on an all-healthy city")
	}
}

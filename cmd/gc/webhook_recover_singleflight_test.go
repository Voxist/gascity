package main

import (
	"context"
	"sync"
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/orderdispatch"
	"github.com/gastownhall/gascity/internal/orders"
	"github.com/gastownhall/gascity/internal/runtime"
)

// ga-w3bkx. Every webhook delivery builds its own memoryOrderDispatcher
// (controllerWebhookDispatcher.dispatcher), so two concurrent deliveries of a
// webhook-triggered recover_on_store_unavailable order must still share ONE
// recoverSingleFlight guard, or both would exec.
func newWebhookGuardState(t *testing.T, guard *recoverSingleFlight) *controllerState {
	t.Helper()
	return &controllerState{
		cityPath:      t.TempDir(),
		cfg:           &config.City{Workspace: config.Workspace{Name: "test-city"}},
		storageRoutes: messagingSplitRoutes(beads.NewMemStore()),
		recoverSF:     guard,
	}
}

func TestWebhookDispatchersShareTheRuntimeRecoverGuard(t *testing.T) {
	guard := &recoverSingleFlight{}
	cs := newWebhookGuardState(t, guard)

	first := controllerWebhookDispatcher{cs: cs}.dispatcher()
	second := controllerWebhookDispatcher{cs: cs}.dispatcher()
	if first.singleFlight() != guard || second.singleFlight() != guard {
		t.Fatalf("webhook dispatchers use guards %p and %p, want the runtime's shared %p", first.singleFlight(), second.singleFlight(), guard)
	}
}

func TestConcurrentWebhookDeliveriesAdmitOneRecoverExec(t *testing.T) {
	cs := newWebhookGuardState(t, &recoverSingleFlight{})
	const deliveries = 8
	var (
		wg       sync.WaitGroup
		mu       sync.Mutex
		admitted int
		start    = make(chan struct{})
	)
	for range deliveries {
		wg.Add(1)
		go func() {
			defer wg.Done()
			md := controllerWebhookDispatcher{cs: cs}.dispatcher()
			<-start
			if md.singleFlight().tryAcquire("recover-order") {
				mu.Lock()
				admitted++
				mu.Unlock()
			}
		}()
	}
	close(start)
	wg.Wait()
	if admitted != 1 {
		t.Fatalf("%d concurrent webhook deliveries admitted %d recover execs, want exactly 1", deliveries, admitted)
	}
}

// TestWireControllerStateFromRuntimeSharesTheTickGuard pins the production
// wiring a resync could silently drop: for a runtime built by newCityRuntime,
// a webhook dispatcher minted from the wired controllerState must consult the
// SAME guard as the tick dispatcher (cr.od).
func TestWireControllerStateFromRuntimeSharesTheTickGuard(t *testing.T) {
	cr := serverLifecycleCityRuntime(t, runtime.NewFake())
	if cr.recoverSF == nil {
		t.Fatal("newCityRuntime left cr.recoverSF nil; the webhook seam would get no shared guard")
	}
	// A city with no orders boots with no dispatcher; the first one installed
	// (a reload or rescan) must adopt the runtime's guard, not replace it.
	filler := orders.Order{Name: "filler", Trigger: "cooldown", Interval: "1h", Exec: "true"}
	var rec memRecorder
	cr.replaceOrderDispatcher(buildOrderDispatcherFromListExec([]orders.Order{filler}, beads.NewMemStore(), nil, nil, &rec))
	tick, ok := cr.od.(*memoryOrderDispatcher)
	if !ok {
		t.Fatalf("cr.od = %T, want *memoryOrderDispatcher", cr.od)
	}
	cs := newWebhookGuardState(t, nil)
	wireControllerStateFromRuntime(cs, cr)

	if cs.controllerGeneration != cr.controllerGeneration {
		t.Fatalf("controllerGeneration = %q, want the runtime's %q", cs.controllerGeneration, cr.controllerGeneration)
	}
	got := controllerWebhookDispatcher{cs: cs}.dispatcher().singleFlight()
	if got != tick.singleFlight() {
		t.Fatalf("webhook guard %p != tick guard %p; concurrent webhook deliveries would not be serialized against the tick loop", got, tick.singleFlight())
	}
}

// TestWebhookDispatchYieldsWhenRecoverGuardHeld drives the real webhook
// Dispatch path: with the runtime's guard already held for the order, a
// delivery must yield quietly -- no error, not fired, no tracking bead.
func TestWebhookDispatchYieldsWhenRecoverGuardHeld(t *testing.T) {
	guard := &recoverSingleFlight{}
	cs := newWebhookGuardState(t, guard)
	order := orders.Order{
		Name:                      "recover-order",
		Trigger:                   "webhook",
		Exec:                      "true",
		RecoverOnStoreUnavailable: true,
	}
	if !guard.tryAcquire(order.ScopedName()) {
		t.Fatal("tryAcquire on a fresh guard must succeed")
	}
	t.Cleanup(func() { guard.release(order.ScopedName()) })

	res, err := controllerWebhookDispatcher{cs: cs}.Dispatch(context.Background(), orderdispatch.DispatchRequest{
		Order:  order,
		Source: orderdispatch.SourceWebhook,
	})
	if err != nil {
		t.Fatalf("Dispatch returned an error for the single-flight yield: %v", err)
	}
	if res.Fired || res.TrackingID != "" {
		t.Fatalf("Dispatch result = %+v, want not fired and no tracking bead", res)
	}
}

package main

import (
	"sync"
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
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

package main

import (
	"bytes"
	"context"
	"go/ast"
	"go/token"
	"go/types"
	"os"
	"path/filepath"
	"sync"
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/events"
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
	graph := beads.NewMemStore()
	cs.storageRoutes = messagingSplitRoutes(graph)
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
	all, err := graph.List(beads.ListQuery{AllowScan: true, IncludeClosed: true})
	if err != nil {
		t.Fatalf("list graph store: %v", err)
	}
	if len(all) != 0 {
		t.Fatalf("graph store holds %d beads after a yielded delivery, want 0", len(all))
	}
}

// TestBootOrderDispatcherAdoptsTheRuntimeRecoverGuard pins newCityRuntime's
// `mem.recoverSF = recoverSF`: a city with an order at boot gets a tick
// dispatcher that already consults the runtime's guard, so a webhook
// dispatcher wired from the same runtime shares it with no swap needed.
func TestBootOrderDispatcherAdoptsTheRuntimeRecoverGuard(t *testing.T) {
	stubManagedDoltStoreOpeners(t)
	cityPath := t.TempDir()
	tomlPath := filepath.Join(cityPath, "city.toml")
	writeCityRuntimeConfig(t, tomlPath, "fake")
	if err := os.MkdirAll(filepath.Join(cityPath, "orders"), 0o755); err != nil {
		t.Fatal(err)
	}
	orderTOML := "[order]\nexec = \"true\"\ntrigger = \"cooldown\"\ninterval = \"1h\"\n"
	if err := os.WriteFile(filepath.Join(cityPath, "orders", "boot-order.toml"), []byte(orderTOML), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.Load(osFS{}, tomlPath)
	if err != nil {
		t.Fatalf("load config: %v", err)
	}
	sp := runtime.NewFake()
	var stdout, stderr bytes.Buffer
	cr := newTestCityRuntime(t, CityRuntimeParams{
		CityPath: cityPath,
		CityName: "test-city",
		TomlPath: tomlPath,
		Cfg:      cfg,
		SP:       sp,
		BuildFn: func(*config.City, runtime.Provider, beads.Store) DesiredStateResult {
			return DesiredStateResult{State: map[string]TemplateParams{}}
		},
		Dops:   newDrainOps(sp),
		Rec:    events.Discard,
		Stdout: &stdout,
		Stderr: &stderr,
	})
	tick, ok := cr.od.(*memoryOrderDispatcher)
	if !ok {
		t.Fatalf("cr.od = %T, want *memoryOrderDispatcher (stderr: %s)", cr.od, stderr.String())
	}
	if cr.recoverSF == nil || tick.singleFlight() != cr.recoverSF {
		t.Fatalf("boot dispatcher guard %p != runtime guard %p", tick.singleFlight(), cr.recoverSF)
	}
}

// TestEverySetControllerStateIsWiredFromRuntime is the structural guard for
// the wiring a resync could silently drop: in every non-test function that
// calls X.setControllerState(Y), an earlier wireControllerStateFromRuntime(Y, X)
// must appear, so no production path hands the webhook seam an unwired
// controllerState.
func TestEverySetControllerStateIsWiredFromRuntime(t *testing.T) {
	fset, files := parseNonTestCmdGC(t)
	sites := 0
	for _, f := range files {
		for _, decl := range f.Decls {
			fn, ok := decl.(*ast.FuncDecl)
			if !ok || fn.Body == nil || fn.Name.Name == "setControllerState" {
				continue
			}
			type pair struct{ rt, cs string }
			var sets []struct {
				p   pair
				pos token.Pos
			}
			var wires []struct {
				p   pair
				pos token.Pos
			}
			ast.Inspect(fn.Body, func(n ast.Node) bool {
				call, ok := n.(*ast.CallExpr)
				if !ok {
					return true
				}
				switch fun := call.Fun.(type) {
				case *ast.SelectorExpr:
					if fun.Sel.Name == "setControllerState" && len(call.Args) == 1 {
						sets = append(sets, struct {
							p   pair
							pos token.Pos
						}{pair{types.ExprString(fun.X), types.ExprString(call.Args[0])}, call.Pos()})
					}
				case *ast.Ident:
					if fun.Name == "wireControllerStateFromRuntime" && len(call.Args) == 2 {
						wires = append(wires, struct {
							p   pair
							pos token.Pos
						}{pair{types.ExprString(call.Args[1]), types.ExprString(call.Args[0])}, call.Pos()})
					}
				}
				return true
			})
			for _, set := range sets {
				sites++
				found := false
				for _, w := range wires {
					if w.p == set.p && w.pos < set.pos {
						found = true
					}
				}
				if !found {
					t.Errorf("%s: %s: %s.setControllerState(%s) has no earlier wireControllerStateFromRuntime(%s, %s)",
						fset.Position(set.pos), fn.Name.Name, set.p.rt, set.p.cs, set.p.cs, set.p.rt)
				}
			}
		}
	}
	if sites < 2 {
		t.Fatalf("found %d setControllerState call sites, want at least 2; the scan is not seeing production code", sites)
	}
}

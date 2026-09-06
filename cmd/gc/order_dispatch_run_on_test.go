package main

import (
	"bytes"
	"context"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/events"
	"github.com/gastownhall/gascity/internal/orders"
)

// runOnDispatcher builds a dispatcher over aa with a pinned fleet role and a
// single in-memory store, mirroring buildOrderDispatcherFromListExec but
// exposing the concrete type so the test can pin fleetRole and read stderr.
func runOnDispatcher(aa []orders.Order, role string, store beads.Store, execRun ExecRunner, stderr *bytes.Buffer) *memoryOrderDispatcher {
	dispatchCtx, dispatchCancel := context.WithCancel(context.Background())
	return &memoryOrderDispatcher{
		aa: aa,
		storeFn: func(execStoreTarget) (beads.Store, error) {
			return store, nil
		},
		execRun:              execRun,
		rec:                  events.Discard,
		stderr:               lockedStderr(stderr),
		maxDispatchesPerTick: defaultMaxOrderDispatchesPerTick,
		cfg:                  &config.City{},
		fleetRole:            role,
		dispatchCtx:          dispatchCtx,
		dispatchCancel:       dispatchCancel,
	}
}

func runOnOrder(name, runOn string) orders.Order {
	return orders.Order{
		Name:       name,
		Trigger:    "cooldown",
		Interval:   "1m",
		Exec:       "true",
		RunOn:      runOn,
		NoWorkGate: true,
	}
}

// TestOrderDispatchRunOnRoleMatrix is the core contract: every combination of
// the city's fleet role and an order's run_on, checked against whether the
// order's exec actually ran.
func TestOrderDispatchRunOnRoleMatrix(t *testing.T) {
	for _, tc := range []struct {
		name    string
		role    string
		runOn   string
		wantRun bool
	}{
		{"seat runs undeclared", orders.RoleSeat, "", true},
		{"fleet host runs undeclared", orders.RoleFleetHost, "", true},
		{"seat runs any", orders.RoleSeat, orders.RunOnAny, true},
		{"fleet host runs any", orders.RoleFleetHost, orders.RunOnAny, true},
		{"seat skips fleet-host", orders.RoleSeat, orders.RunOnFleetHost, false},
		{"fleet host runs fleet-host", orders.RoleFleetHost, orders.RunOnFleetHost, true},
		{"seat runs seat", orders.RoleSeat, orders.RunOnSeat, true},
		{"fleet host skips seat", orders.RoleFleetHost, orders.RunOnSeat, false},
		// An undeclared role is a seat, which is what makes this change
		// backward compatible for every city that never sets [city] role.
		{"undeclared role behaves as seat for fleet-host order", "", orders.RunOnFleetHost, false},
		{"undeclared role behaves as seat for seat order", "", orders.RunOnSeat, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var ran atomic.Int32
			exec := func(context.Context, string, string, []string) ([]byte, error) {
				ran.Add(1)
				return nil, nil
			}
			store := beads.NewMemStore()
			var stderr bytes.Buffer
			ad := runOnDispatcher([]orders.Order{runOnOrder("sweep", tc.runOn)}, tc.role, store, exec, &stderr)

			ad.dispatch(context.Background(), t.TempDir(), time.Now())
			ad.drain(context.Background())

			if got := ran.Load() > 0; got != tc.wantRun {
				t.Fatalf("order ran = %v, want %v (stderr: %s)", got, tc.wantRun, stderr.String())
			}
		})
	}
}

// A skipped order records the skip in its own tracking so a reader of order
// history sees a deliberate skip rather than an order that simply never ran.
func TestOrderDispatchRunOnSkipRecordsTracking(t *testing.T) {
	store := beads.NewMemStore()
	var stderr bytes.Buffer
	ad := runOnDispatcher([]orders.Order{runOnOrder("merge-sweep", orders.RunOnFleetHost)}, orders.RoleSeat, store, successfulExec, &stderr)

	ad.dispatch(context.Background(), t.TempDir(), time.Now())
	ad.drain(context.Background())

	tracking := trackingBeads(t, store, orders.RunLabel("merge-sweep"))
	if len(tracking) != 1 {
		t.Fatalf("tracking beads = %d, want 1: %+v", len(tracking), tracking)
	}
	if got := tracking[0].Metadata["close_reason"]; got != "skipped:run_on" {
		t.Fatalf("close_reason = %q, want %q", got, "skipped:run_on")
	}
	if tracking[0].Status != "closed" {
		t.Fatalf("tracking bead status = %q, want closed — an open bead reads as a dispatch in flight", tracking[0].Status)
	}
	if !strings.Contains(stderr.String(), "skipped:run_on") {
		t.Fatalf("stderr = %q, want a skipped:run_on line", stderr.String())
	}
}

// The skip is reported once per dispatcher generation, not once per tick: the
// condition is static for the life of the dispatcher, so repeating it would add
// a line to every tick of stderr and a bead write to every tick of the store.
func TestOrderDispatchRunOnSkipReportedOncePerGeneration(t *testing.T) {
	store := beads.NewMemStore()
	var stderr bytes.Buffer
	aa := []orders.Order{runOnOrder("merge-sweep", orders.RunOnFleetHost)}
	ad := runOnDispatcher(aa, orders.RoleSeat, store, successfulExec, &stderr)

	cityPath := t.TempDir()
	for i := 0; i < 4; i++ {
		ad.dispatch(context.Background(), cityPath, time.Now())
		ad.drain(context.Background())
	}

	if got := len(trackingBeads(t, store, orders.RunLabel("merge-sweep"))); got != 1 {
		t.Fatalf("tracking beads after 4 ticks = %d, want 1", got)
	}
	if got := strings.Count(stderr.String(), "skipped:run_on"); got != 1 {
		t.Fatalf("skip log lines after 4 ticks = %d, want 1:\n%s", got, stderr.String())
	}

	// A reload builds a fresh dispatcher, which reports the skip again — that
	// is what makes the line reappear when the role or order set changes.
	var stderr2 bytes.Buffer
	ad2 := runOnDispatcher(aa, orders.RoleSeat, store, successfulExec, &stderr2)
	ad2.dispatch(context.Background(), cityPath, time.Now())
	ad2.drain(context.Background())
	if got := strings.Count(stderr2.String(), "skipped:run_on"); got != 1 {
		t.Fatalf("skip log lines after reload = %d, want 1:\n%s", got, stderr2.String())
	}
}

// A skipped order must not consume the tick's dispatch budget: a city whose
// pack ships many fleet-host orders would otherwise starve its own seat orders.
func TestOrderDispatchRunOnSkipDoesNotSpendBudget(t *testing.T) {
	var ran atomic.Int32
	exec := func(context.Context, string, string, []string) ([]byte, error) {
		ran.Add(1)
		return nil, nil
	}
	store := beads.NewMemStore()
	var stderr bytes.Buffer
	aa := []orders.Order{
		runOnOrder("host-a", orders.RunOnFleetHost),
		runOnOrder("host-b", orders.RunOnFleetHost),
		runOnOrder("host-c", orders.RunOnFleetHost),
		runOnOrder("seat-only", orders.RunOnSeat),
	}
	ad := runOnDispatcher(aa, orders.RoleSeat, store, exec, &stderr)
	ad.maxDispatchesPerTick = 1

	ad.dispatch(context.Background(), t.TempDir(), time.Now())
	ad.drain(context.Background())

	if ran.Load() != 1 {
		t.Fatalf("exec runs = %d, want 1 — the seat order should dispatch despite three skipped orders ahead of it", ran.Load())
	}
	if got := len(trackingBeads(t, store, orders.RunLabel("seat-only"))); got == 0 {
		t.Fatal("seat-only order produced no tracking bead")
	}
}

// buildOrderDispatcherFromOrderSet must stamp the role from config so the
// production path (not just the test constructor) honors [city] role.
func TestBuildOrderDispatcherStampsFleetRole(t *testing.T) {
	t.Setenv(config.FleetRoleEnvVar, "")
	cfg := &config.City{CityRole: config.CityRoleConfig{Role: orders.RoleFleetHost}}
	var stderr bytes.Buffer
	ad := buildOrderDispatcherFromOrderSet(nil, t.TempDir(), cfg, []orders.Order{runOnOrder("sweep", orders.RunOnFleetHost)}, events.Discard, &stderr)
	if ad == nil {
		t.Fatal("buildOrderDispatcherFromOrderSet returned nil")
	}
	if got := ad.(*memoryOrderDispatcher).fleetRole; got != orders.RoleFleetHost {
		t.Fatalf("fleetRole = %q, want %q", got, orders.RoleFleetHost)
	}
}

// With no [city] role declared, the host-local VOXIST_FLEET_ROLE declaration
// the fleet already exports must supply the role with no city.toml change.
func TestBuildOrderDispatcherFleetRoleFromEnv(t *testing.T) {
	t.Setenv(config.FleetRoleEnvVar, orders.RoleFleetHost)
	var stderr bytes.Buffer
	ad := buildOrderDispatcherFromOrderSet(nil, t.TempDir(), &config.City{}, []orders.Order{runOnOrder("sweep", orders.RunOnFleetHost)}, events.Discard, &stderr)
	if ad == nil {
		t.Fatal("buildOrderDispatcherFromOrderSet returned nil")
	}
	if got := ad.(*memoryOrderDispatcher).fleetRole; got != orders.RoleFleetHost {
		t.Fatalf("fleetRole = %q, want %q", got, orders.RoleFleetHost)
	}
}

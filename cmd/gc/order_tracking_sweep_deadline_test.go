package main

// vp-3dyjq: the stale order-tracking sweep (`gc order sweep-tracking`, run by
// the core order-tracking-sweep order every minute with --stale-after 10m)
// must age each open marker against its own order's effective timeout plus
// grace — the ADR-0130 D2 cutoff the controller watchdog already uses — with
// --stale-after as the floor. A flat 10m clock closed the single-flight marker
// of any order still running past 10m, and because cooldown is measured from
// the run's START the order was due again the moment its marker closed: every
// such order double-fired.

import (
	"bytes"
	"context"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/orders"
)

func longRunningSweepOrder() orders.Order {
	return orders.Order{
		Name:     "long-runner",
		Trigger:  "cooldown",
		Interval: "10m",
		Exec:     "scripts/long-runner.sh",
		Timeout:  "30m",
	}
}

func createOpenSweepTrackingBead(t *testing.T, store beads.Store, scoped string) beads.Bead {
	t.Helper()
	b, err := store.Create(beads.Bead{
		Title:     "order:" + scoped,
		Labels:    []string{"order-run:" + scoped, labelOrderTracking},
		Ephemeral: true,
	})
	if err != nil {
		t.Fatalf("Create(%s): %v", scoped, err)
	}
	return b
}

func residualForOrderList(aa []orders.Order) func(string) time.Duration {
	return func(scoped string) time.Duration {
		return orderTrackingResidualCutoff(aa, 0, scoped)
	}
}

func runDeadlineAwareStaleSweep(t *testing.T, store beads.Store, now time.Time, residualFor func(string) time.Duration, dryRun bool) orderTrackingSweepResult {
	t.Helper()
	result, err := sweepStaleOrderTrackingAcrossStoresLimitMode([]beads.Store{store}, nil, now, defaultOrderTrackingSweepStaleAfter, nil, orderTrackingSweepMetadataInitiator, false, 0, dryRun, residualFor)
	if err != nil {
		t.Fatalf("sweepStaleOrderTrackingAcrossStoresLimitMode: %v", err)
	}
	return result
}

func beadStatus(t *testing.T, store beads.Store, id string) string {
	t.Helper()
	got, err := store.Get(id)
	if err != nil {
		t.Fatalf("Get(%s): %v", id, err)
	}
	return got.Status
}

// TestStaleOrderTrackingSweepKeepsMarkerInsideItsOrderTimeout is the bug: a
// 30m-timeout order 12m into its run is past the flat 10m --stale-after but
// nowhere near its own deadline, so its single-flight marker must survive.
func TestStaleOrderTrackingSweepKeepsMarkerInsideItsOrderTimeout(t *testing.T) {
	store := beads.NewMemStore()
	aa := []orders.Order{longRunningSweepOrder()}
	marker := createOpenSweepTrackingBead(t, store, "long-runner")

	for _, age := range []time.Duration{12 * time.Minute, 30 * time.Minute, 30*time.Minute + orderTrackingDeadlineGrace} {
		result := runDeadlineAwareStaleSweep(t, store, marker.CreatedAt.Add(age), residualForOrderList(aa), false)
		if result.trackingClosed != 0 {
			t.Fatalf("age %v: trackingClosed = %d, want 0 (marker is inside its order's timeout+grace)", age, result.trackingClosed)
		}
		if got := beadStatus(t, store, marker.ID); got != "open" {
			t.Fatalf("age %v: marker status = %q, want open", age, got)
		}
	}
}

// TestStaleOrderTrackingSweepDryRunKeepsMarkerInsideItsOrderTimeout pins the
// --dry-run report to the same predicate, so an operator previewing the sweep
// is not told a live run's marker would close.
func TestStaleOrderTrackingSweepDryRunKeepsMarkerInsideItsOrderTimeout(t *testing.T) {
	store := beads.NewMemStore()
	aa := []orders.Order{longRunningSweepOrder()}
	marker := createOpenSweepTrackingBead(t, store, "long-runner")

	result := runDeadlineAwareStaleSweep(t, store, marker.CreatedAt.Add(12*time.Minute), residualForOrderList(aa), true)
	if result.trackingClosed != 0 {
		t.Fatalf("dry-run trackingClosed = %d, want 0", result.trackingClosed)
	}
}

// TestStaleOrderTrackingSweepClosesMarkerPastItsOrderTimeoutPlusGrace: once the
// dispatch's own deadline plus grace has visibly elapsed, the marker is stale
// and the sweep still recovers it.
func TestStaleOrderTrackingSweepClosesMarkerPastItsOrderTimeoutPlusGrace(t *testing.T) {
	store := beads.NewMemStore()
	aa := []orders.Order{longRunningSweepOrder()}
	marker := createOpenSweepTrackingBead(t, store, "long-runner")

	now := marker.CreatedAt.Add(30*time.Minute + orderTrackingDeadlineGrace + time.Second)
	result := runDeadlineAwareStaleSweep(t, store, now, residualForOrderList(aa), false)
	if result.trackingClosed != 1 {
		t.Fatalf("trackingClosed = %d, want 1", result.trackingClosed)
	}
	if got := beadStatus(t, store, marker.ID); got != "closed" {
		t.Fatalf("marker status = %q, want closed", got)
	}
}

// TestStaleOrderTrackingSweepKeepsStaleAfterFloor: an order whose own deadline
// is shorter than --stale-after, an order the sweep cannot resolve (deleted
// from config, or a scan that failed outright — nil residualFor), all keep the
// pre-existing flat --stale-after behavior. The per-order deadline only ever
// lengthens the wait; it never makes the sweep more aggressive.
func TestStaleOrderTrackingSweepKeepsStaleAfterFloor(t *testing.T) {
	defaultTimeout := orders.Order{Name: "short-runner", Trigger: "cooldown", Interval: "1m", Exec: "scripts/short.sh"}
	if residual := orderTrackingResidualCutoff([]orders.Order{defaultTimeout}, 0, "short-runner"); residual <= 0 || residual >= defaultOrderTrackingSweepStaleAfter {
		t.Fatalf("precondition: default-timeout residual = %v, want in (0, %v)", residual, defaultOrderTrackingSweepStaleAfter)
	}
	aa := []orders.Order{longRunningSweepOrder(), defaultTimeout}

	cases := []struct {
		name        string
		scoped      string
		residualFor func(string) time.Duration
	}{
		{name: "order with default timeout", scoped: "short-runner", residualFor: residualForOrderList(aa)},
		{name: "order not in config", scoped: "deleted-order", residualFor: residualForOrderList(aa)},
		{name: "order set unresolvable", scoped: "long-runner", residualFor: nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			store := beads.NewMemStore()
			marker := createOpenSweepTrackingBead(t, store, tc.scoped)

			early := runDeadlineAwareStaleSweep(t, store, marker.CreatedAt.Add(defaultOrderTrackingSweepStaleAfter-time.Minute), tc.residualFor, false)
			if early.trackingClosed != 0 {
				t.Fatalf("before stale-after: trackingClosed = %d, want 0", early.trackingClosed)
			}
			late := runDeadlineAwareStaleSweep(t, store, marker.CreatedAt.Add(defaultOrderTrackingSweepStaleAfter+time.Minute), tc.residualFor, false)
			if late.trackingClosed != 1 {
				t.Fatalf("after stale-after: trackingClosed = %d, want 1", late.trackingClosed)
			}
			if got := beadStatus(t, store, marker.ID); got != "closed" {
				t.Fatalf("marker status = %q, want closed", got)
			}
		})
	}
}

const sweepTestLongRunnerOrder = `[order]
trigger = "cooldown"
interval = "10m"
exec = "scripts/long-runner.sh"
timeout = "30m"
`

// writeSweepTestCity writes a city whose orders/ holds the 30m long-runner
// order plus any extra order files, and returns its path and loaded config.
func writeSweepTestCity(t *testing.T, cityTOMLExtra string, extraOrders map[string]string) (string, *config.City) {
	t.Helper()
	cityPath := t.TempDir()
	writeFile(t, filepath.Join(cityPath, "city.toml"), "[workspace]\nname = \"test-city\"\n"+cityTOMLExtra)
	if err := os.MkdirAll(filepath.Join(cityPath, "orders"), 0o755); err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(cityPath, "orders", "long-runner.toml"), sweepTestLongRunnerOrder)
	for name, body := range extraOrders {
		writeFile(t, filepath.Join(cityPath, "orders", name), body)
	}
	cfg, err := loadCityConfig(cityPath, io.Discard)
	if err != nil {
		t.Fatalf("loadCityConfig: %v", err)
	}
	return cityPath, cfg
}

// addSweepTestRigOrders gives cfg a rig named rigName whose rig-exclusive
// formula layer carries the given order files, the way pack composition
// stacks a rig's layers on top of the city's.
func addSweepTestRigOrders(t *testing.T, cityPath string, cfg *config.City, rigName string, rigOrders map[string]string) {
	t.Helper()
	rigDir := filepath.Join(cityPath, "rigs", rigName)
	if err := os.MkdirAll(filepath.Join(rigDir, "orders"), 0o755); err != nil {
		t.Fatal(err)
	}
	for name, body := range rigOrders {
		writeFile(t, filepath.Join(rigDir, "orders", name), body)
	}
	cityLayers := cfg.FormulaLayers.City
	if len(cityLayers) == 0 {
		cityLayers = []string{filepath.Join(cityPath, "formulas")}
	}
	if cfg.FormulaLayers.Rigs == nil {
		cfg.FormulaLayers.Rigs = map[string][]string{}
	}
	cfg.FormulaLayers.Rigs[rigName] = append(append([]string(nil), cityLayers...), filepath.Join(rigDir, "formulas"))
}

// TestOrderTrackingSweepResidualForResolvesTheCityOrderSet pins the CLI wiring:
// `gc order sweep-tracking` runs in its own process, so it resolves deadlines
// from the city's discovered order set — with [orders] max_timeout applied the
// same way the dispatcher applies it — not from any in-process state. A rig
// order is matched by its scoped name, so a rig copy of a city order resolves
// its own timeout rather than the city one's.
func TestOrderTrackingSweepResidualForResolvesTheCityOrderSet(t *testing.T) {
	cases := []struct {
		name    string
		orders  string
		want    time.Duration
		wantRig time.Duration
	}{
		{name: "order timeout", orders: "", want: 30*time.Minute + orderTrackingDeadlineGrace, wantRig: 45*time.Minute + orderTrackingDeadlineGrace},
		{name: "max_timeout caps it", orders: "\n[orders]\nmax_timeout = \"5m\"\n", want: 5*time.Minute + orderTrackingDeadlineGrace, wantRig: 5*time.Minute + orderTrackingDeadlineGrace},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cityPath, cfg := writeSweepTestCity(t, tc.orders, nil)
			addSweepTestRigOrders(t, cityPath, cfg, "demo", map[string]string{"long-runner.toml": `[order]
trigger = "cooldown"
interval = "10m"
exec = "scripts/long-runner.sh"
timeout = "45m"
`})

			residualFor, err := orderTrackingSweepResidualFor(cityPath, cfg, io.Discard)
			if err != nil {
				t.Fatalf("orderTrackingSweepResidualFor: %v", err)
			}
			if residualFor == nil {
				t.Fatal("orderTrackingSweepResidualFor = nil, want a resolver over the city's orders")
			}
			if got := residualFor("long-runner"); got != tc.want {
				t.Fatalf("residualFor(long-runner) = %v, want %v", got, tc.want)
			}
			if got := residualFor("long-runner:rig:demo"); got != tc.wantRig {
				t.Fatalf("residualFor(long-runner:rig:demo) = %v, want %v", got, tc.wantRig)
			}
			if got := residualFor("deleted-order"); got != 0 {
				t.Fatalf("residualFor(deleted-order) = %v, want 0 (falls back to --stale-after)", got)
			}
		})
	}
}

// TestOrderTrackingSweepResidualForReportsARigScanFailure: one rig whose orders
// cannot be scanned must not hide the rest of the city's deadlines, but it must
// not pass silently either — that rig's markers fall back to --stale-after.
func TestOrderTrackingSweepResidualForReportsARigScanFailure(t *testing.T) {
	cityPath, cfg := writeSweepTestCity(t, "", nil)
	addSweepTestRigOrders(t, cityPath, cfg, "broken", map[string]string{"bad.toml": "[order\n"})

	residualFor, err := orderTrackingSweepResidualFor(cityPath, cfg, io.Discard)
	if err == nil || !strings.Contains(err.Error(), "broken") {
		t.Fatalf("orderTrackingSweepResidualFor err = %v, want the broken rig's scan error", err)
	}
	if residualFor == nil {
		t.Fatal("residualFor = nil, want the city's orders still resolved")
	}
	if got := residualFor("long-runner"); got != 30*time.Minute+orderTrackingDeadlineGrace {
		t.Fatalf("residualFor(long-runner) = %v, want the city order's deadline", got)
	}
}

// swapOrderTrackingSweepStores points the sweep command at store instead of
// the city's on-disk stores.
func swapOrderTrackingSweepStores(t *testing.T, store beads.Store) {
	t.Helper()
	prev := openOrderTrackingSweepStores
	openOrderTrackingSweepStores = func(string, *config.City, map[string][]string) ([]beads.Store, beads.Store, error) {
		return []beads.Store{store}, nil, nil
	}
	t.Cleanup(func() { openOrderTrackingSweepStores = prev })
}

// TestCmdOrderSweepTrackingKeepsARunningOrdersMarker drives the command itself:
// a 12m-old marker of a 30m order, swept with the packaged --stale-after 10m,
// stays open.
func TestCmdOrderSweepTrackingKeepsARunningOrdersMarker(t *testing.T) {
	cityPath, cfg := writeSweepTestCity(t, "", nil)
	store := beads.NewMemStore()
	swapOrderTrackingSweepStores(t, store)
	marker := createOpenSweepTrackingBead(t, store, "long-runner")

	var stdout, stderr bytes.Buffer
	code := cmdOrderSweepTrackingForCity(cityPath, cfg, marker.CreatedAt.Add(12*time.Minute), defaultOrderTrackingSweepStaleAfter, false, false, false, false, nil, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("cmdOrderSweepTrackingForCity = %d, want 0; stderr: %s", code, stderr.String())
	}
	if got := beadStatus(t, store, marker.ID); got != "open" {
		t.Fatalf("marker status = %q, want open: the sweep closed a marker whose order can still be running", got)
	}
	if !strings.Contains(stdout.String(), "closed 0 stale order-tracking bead") {
		t.Fatalf("stdout = %q, want closed 0", stdout.String())
	}
}

// TestCmdOrderSweepTrackingFailsLoudWhenOrderTimeoutsCannotBeResolved: when the
// order scan fails the sweep still recovers stale markers against
// --stale-after, but exits non-zero with the reason. The controller discards a
// successful exec order's output, so only a failed run (recorded as
// order.failed with its output) makes the degraded sweep visible.
func TestCmdOrderSweepTrackingFailsLoudWhenOrderTimeoutsCannotBeResolved(t *testing.T) {
	cityPath, cfg := writeSweepTestCity(t, "", map[string]string{"broken.toml": "[order\n"})
	store := beads.NewMemStore()
	swapOrderTrackingSweepStores(t, store)
	marker := createOpenSweepTrackingBead(t, store, "long-runner")

	var stdout, stderr bytes.Buffer
	code := cmdOrderSweepTrackingForCity(cityPath, cfg, marker.CreatedAt.Add(12*time.Minute), defaultOrderTrackingSweepStaleAfter, false, false, true, false, nil, &stdout, &stderr)
	if code == 0 {
		t.Fatalf("cmdOrderSweepTrackingForCity = 0, want non-zero when order timeouts could not be resolved; stderr: %s", stderr.String())
	}
	if !strings.Contains(stderr.String(), "--stale-after") || !strings.Contains(stderr.String(), "broken") {
		t.Fatalf("stderr = %q, want the fallback and the scan error named", stderr.String())
	}
	if got := beadStatus(t, store, marker.ID); got != "closed" {
		t.Fatalf("marker status = %q, want closed: the degraded sweep must still recover against --stale-after", got)
	}
}

// TestLongRunningOrderIsNotRefiredWhileTheStaleSweepRuns reproduces the
// incident end to end: a cooldown order whose run outlasts --stale-after keeps
// ticking alongside the per-minute sweep. The flat sweep closed its marker at
// minute 10-11, the order (cooldown measured from run start) was immediately
// due, and the dispatcher fired a second copy while the first was still going.
func TestLongRunningOrderIsNotRefiredWhileTheStaleSweepRuns(t *testing.T) {
	store := beads.NewMemStore()
	release := make(chan struct{})
	execStarted := make(chan struct{}, 1)
	fakeExec := func(ctx context.Context, _, _ string, _ []string) ([]byte, error) {
		select {
		case execStarted <- struct{}{}:
		default:
		}
		select {
		case <-release:
		case <-ctx.Done():
		}
		return nil, nil
	}
	aa := []orders.Order{longRunningSweepOrder()}
	ad := buildOrderDispatcherFromListExec(aa, store, nil, fakeExec, nil)
	if ad == nil {
		t.Fatal("expected non-nil dispatcher")
	}
	defer func() {
		close(release)
		ad.drain(context.Background())
	}()

	cityPath := t.TempDir()
	t0 := time.Now()
	ad.dispatch(context.Background(), cityPath, t0)
	<-execStarted

	residualFor := residualForOrderList(aa)
	for minute := 1; minute <= 25; minute++ {
		now := t0.Add(time.Duration(minute) * time.Minute)
		runDeadlineAwareStaleSweep(t, store, now, residualFor, false)
		ad.dispatch(context.Background(), cityPath, now)
	}

	if got := len(trackingBeads(t, store, "order-run:long-runner")); got != 1 {
		t.Fatalf("tracking beads = %d, want 1: the order re-fired while its first run was still in flight", got)
	}
}

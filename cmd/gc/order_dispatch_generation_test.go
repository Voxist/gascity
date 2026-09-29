package main

// Tests for the ADR-0130 order-tracking reap predicate: markers are stamped
// with the creating controller's generation at creation (D1), and the watchdog
// reaps a foreign generation immediately while markers of the LIVE generation
// survive until their own dispatch's effective timeout plus grace (D2) —
// never a global age clock. vp-qauad.

import (
	"context"
	"io"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/events"
	"github.com/gastownhall/gascity/internal/orders"
)

func genStamp(gen string) map[string]string {
	return map[string]string{orders.GenerationMetadataKey: gen}
}

func trackingBead(scoped, gen string) beads.Bead {
	b := beads.Bead{
		Title:  "order:" + scoped,
		Labels: []string{"order-run:" + scoped, labelOrderTracking},
	}
	if gen != "" {
		b.Metadata = genStamp(gen)
	}
	return b
}

// TestClassifyOrderTrackingRunForReap is the ADR-0130 truth table. The
// fail-safe direction is load-bearing: every missing piece of information can
// only make the verdict LESS aggressive, never more.
func TestClassifyOrderTrackingRunForReap(t *testing.T) {
	live := "ctrl-live"
	now := time.Now()
	neverFor := func(string) time.Duration { return 100 * 365 * 24 * time.Hour }
	tinyFor := func(string) time.Duration { return time.Nanosecond }
	zeroFor := func(string) time.Duration { return 0 }

	run := func(gen string, age time.Duration) orders.OrderRun {
		return orders.OrderRun{
			ID:         "gcg-test",
			Scoped:     "tiny-exec",
			Open:       true,
			CreatedAt:  now.Add(-age),
			Generation: gen,
		}
	}

	cases := []struct {
		name        string
		run         orders.OrderRun
		liveGen     string
		residualFor func(scoped string) time.Duration
		want        orderTrackingReapVerdict
	}{
		// D1: a foreign generation is a PROVEN dead controller — reaped with
		// no clock, at any age. This immediacy is the #2168 jam recovery.
		{"foreign aged 1s", run("ctrl-dead", time.Second), live, neverFor, orderTrackingReapForeign},
		{"foreign aged 1000h", run("ctrl-dead", 1000*time.Hour), live, neverFor, orderTrackingReapForeign},
		// AC1 literal: a marker created by the LIVE controller is never closed
		// by age, no matter how long its dispatch has been running.
		{"same gen aged 1000h past any deadline", run(live, 1000*time.Hour), live, neverFor, orderTrackingKeep},
		{"same gen inert residual tier", run(live, 1000*time.Hour), live, zeroFor, orderTrackingKeep},
		// D2: a live-generation marker may be reaped only once it has outlived
		// its own dispatch's deadline plus grace (crashed child).
		{"same gen past deadline", run(live, time.Hour), live, tinyFor, orderTrackingReapDeadline},
		{"same gen before deadline", run(live, time.Second), live, neverFor, orderTrackingKeep},
		// Unstamped (pre-upgrade or CLI-created) markers ride the deadline tier
		// only — never the foreign tier, which would reap a live manual exec's
		// marker out from under it.
		{"unstamped past deadline", run("", time.Hour), live, tinyFor, orderTrackingReapDeadline},
		{"unstamped before deadline", run("", time.Second), live, neverFor, orderTrackingKeep},
		{"unstamped inert residual", run("", time.Hour), live, zeroFor, orderTrackingKeep},
		// An empty LIVE generation (runtime predating stamping) disables the
		// foreign tier: an unknown live generation must not reclassify every
		// stamped marker as a dead-controller orphan.
		{"empty live gen, stamped marker kept", run("ctrl-dead", time.Second), "", neverFor, orderTrackingKeep},
		{"empty live gen, stamped marker deadline tier", run("ctrl-dead", time.Hour), "", tinyFor, orderTrackingReapDeadline},
		// nil residualFor disables the deadline tier entirely.
		{"nil residual disables deadline tier", run(live, 1000*time.Hour), live, nil, orderTrackingKeep},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := classifyOrderTrackingRunForReap(tc.run, now, tc.liveGen, tc.residualFor); got != tc.want {
				t.Fatalf("verdict = %d, want %d", got, tc.want)
			}
		})
	}
}

// TestOrderTrackingSweepWatchdogNeverClosesLiveGenerationMarker is AC1 at the
// watchdog layer: a tracking bead created by the LIVE controller is never
// closed by the watchdog, at any age. The old global 2-minute clock closed
// exactly this marker, voiding single-flight for every order running longer
// than two minutes.
func TestOrderTrackingSweepWatchdogNeverClosesLiveGenerationMarker(t *testing.T) {
	store := beads.NewMemStore()
	marker, err := store.Create(trackingBead("tiny-exec", "ctrl-live"))
	if err != nil {
		t.Fatalf("Create(live marker): %v", err)
	}
	foreign, err := store.Create(trackingBead("tiny-exec", "ctrl-dead"))
	if err != nil {
		t.Fatalf("Create(foreign marker): %v", err)
	}

	cr := &CityRuntime{
		cityName:             "test-city",
		cfg:                  &config.City{Workspace: config.Workspace{Name: "test-city"}},
		controllerGeneration: "ctrl-live",
		standaloneCityStore:  store,
		stdout:               io.Discard,
		stderr:               io.Discard,
		logPrefix:            "gc test",
	}
	// No dispatcher wired: the deadline tier is inert, so the live marker
	// must survive an arbitrarily old timestamp — while the same pass reaps
	// the foreign-generation marker immediately.
	cr.runOrderTrackingSweepWatchdog(foreign.CreatedAt.Add(1000 * time.Hour))

	got, err := store.Get(marker.ID)
	if err != nil {
		t.Fatalf("Get(live marker): %v", err)
	}
	if got.Status != "open" {
		t.Fatalf("live-generation marker status = %s, want open at any age (the global clock used to kill it)", got.Status)
	}

	gotForeign, err := store.Get(foreign.ID)
	if err != nil {
		t.Fatalf("Get(foreign marker): %v", err)
	}
	if gotForeign.Status != "closed" {
		t.Fatalf("foreign-generation marker status = %s, want closed", gotForeign.Status)
	}
}

// TestOrderTrackingSweepWatchdogLiveGenerationSurvivesRepeatedTicks is the AC2
// shape: dispatch a 10-minute run under a 30-second watchdog with exactly one
// live controller throughout — the marker survives all twenty sweeps. Under
// the old clock the marker died on the second tick and the order re-dispatched.
func TestOrderTrackingSweepWatchdogLiveGenerationSurvivesRepeatedTicks(t *testing.T) {
	store := beads.NewMemStore()
	live, err := store.Create(trackingBead("long-exec", "ctrl-live"))
	if err != nil {
		t.Fatalf("Create(live marker): %v", err)
	}
	foreign, err := store.Create(trackingBead("long-exec", "ctrl-dead"))
	if err != nil {
		t.Fatalf("Create(foreign marker): %v", err)
	}

	cr := &CityRuntime{
		cityName:             "test-city",
		cfg:                  &config.City{Workspace: config.Workspace{Name: "test-city"}},
		controllerGeneration: "ctrl-live",
		standaloneCityStore:  store,
		stdout:               io.Discard,
		stderr:               io.Discard,
		logPrefix:            "gc test",
	}
	ticks := int(10 * time.Minute / (30 * time.Second))
	for i := range ticks {
		now := live.CreatedAt.Add(30 * time.Second * time.Duration(i+1))
		cr.runOrderTrackingSweepWatchdog(now)

		got, err := store.Get(live.ID)
		if err != nil {
			t.Fatalf("tick %d: Get(live): %v", i, err)
		}
		if got.Status != "open" {
			t.Fatalf("tick %d (t+%s): live-generation marker = %s, want open for the whole 10-minute run", i, 30*time.Second*time.Duration(i+1), got.Status)
		}
	}

	gotForeign, err := store.Get(foreign.ID)
	if err != nil {
		t.Fatalf("Get(foreign): %v", err)
	}
	if gotForeign.Status != "closed" {
		t.Fatalf("foreign-generation marker = %s, want closed on the first tick", gotForeign.Status)
	}
}

// TestOrderTrackingSweepByGenerationCloseReasonsAndBudget pins the sweep's
// per-verdict canonical close reasons and the cross-tier close budget.
func TestOrderTrackingSweepByGenerationCloseReasonsAndBudget(t *testing.T) {
	store := beads.NewMemStore()
	foreignA, err := store.Create(trackingBead("order-a", "ctrl-dead"))
	if err != nil {
		t.Fatalf("Create(foreign a): %v", err)
	}
	foreignB, err := store.Create(trackingBead("order-b", "ctrl-dead"))
	if err != nil {
		t.Fatalf("Create(foreign b): %v", err)
	}
	deadlineC, err := store.Create(trackingBead("order-c", "ctrl-live"))
	if err != nil {
		t.Fatalf("Create(deadline c): %v", err)
	}

	residualFor := func(string) time.Duration { return time.Nanosecond }
	res, err := sweepOrderTrackingByGenerationAcrossStoresLimit(
		[]beads.Store{store},
		deadlineC.CreatedAt.Add(time.Hour),
		"ctrl-live",
		residualFor,
		orderTrackingSweepCloseBudget,
	)
	if err != nil {
		t.Fatalf("sweep: %v", err)
	}
	if res.trackingClosed != 3 {
		t.Fatalf("trackingClosed = %d, want 3", res.trackingClosed)
	}

	for id, wantReason := range map[string]string{
		foreignA.ID:  orphanedOrderTrackingCloseReason,
		foreignB.ID:  orphanedOrderTrackingCloseReason,
		deadlineC.ID: orderTrackingDeadlineExceededCloseReason,
	} {
		got, err := store.Get(id)
		if err != nil {
			t.Fatalf("Get(%s): %v", id, err)
		}
		if got.Status != "closed" {
			t.Fatalf("%s status = %s, want closed", id, got.Status)
		}
		if got.Metadata["close_reason"] != wantReason {
			t.Fatalf("%s close_reason = %q, want %q", id, got.Metadata["close_reason"], wantReason)
		}
		if got.Metadata["order_tracking_sweep_by"] != orderTrackingWatchdogMetadataInitiator {
			t.Fatalf("%s order_tracking_sweep_by = %q, want %q", id, got.Metadata["order_tracking_sweep_by"], orderTrackingWatchdogMetadataInitiator)
		}
	}

	// Budget: only `limit` markers close per pass, foreign tier first.
	budgetStore := beads.NewMemStore()
	ids := make([]string, 0, 3)
	for _, scoped := range []string{"order-a", "order-b", "order-c"} {
		b, err := budgetStore.Create(trackingBead(scoped, "ctrl-dead"))
		if err != nil {
			t.Fatalf("Create(%s): %v", scoped, err)
		}
		ids = append(ids, b.ID)
	}
	res, err = sweepOrderTrackingByGenerationAcrossStoresLimit(
		[]beads.Store{budgetStore},
		time.Now().Add(time.Hour),
		"ctrl-live",
		nil,
		2,
	)
	if err != nil {
		t.Fatalf("budget sweep: %v", err)
	}
	if res.trackingClosed != 2 {
		t.Fatalf("budget trackingClosed = %d, want 2", res.trackingClosed)
	}
	stillOpen := 0
	for _, id := range ids {
		got, err := budgetStore.Get(id)
		if err != nil {
			t.Fatalf("Get(%s): %v", id, err)
		}
		if got.Status == "open" {
			stillOpen++
		}
	}
	if stillOpen != 1 {
		t.Fatalf("open after budget sweep = %d, want 1", stillOpen)
	}
}

// TestOrderTrackingSweepWatchdogDeadlineTierUsesEffectiveTimeout wires a real
// dispatcher and proves the D2 residual cutoff derives from the order's OWN
// effective timeout plus grace, never a global constant.
func TestOrderTrackingSweepWatchdogDeadlineTierUsesEffectiveTimeout(t *testing.T) {
	store := beads.NewMemStore()
	od := &memoryOrderDispatcher{
		aa: []orders.Order{{
			Name:     "tiny-exec",
			Trigger:  "cooldown",
			Interval: "1m",
			Exec:     "true",
			Timeout:  "10ms",
		}},
	}
	scoped := od.aa[0].ScopedName()

	oldGrace := orderTrackingDeadlineGrace
	orderTrackingDeadlineGrace = time.Nanosecond
	t.Cleanup(func() { orderTrackingDeadlineGrace = oldGrace })

	cr := &CityRuntime{
		cityName:             "test-city",
		cfg:                  &config.City{Workspace: config.Workspace{Name: "test-city"}},
		controllerGeneration: "ctrl-live",
		od:                   od,
		standaloneCityStore:  store,
		stdout:               io.Discard,
		stderr:               io.Discard,
		logPrefix:            "gc test",
	}

	stale, err := store.Create(trackingBead(scoped, "ctrl-live"))
	if err != nil {
		t.Fatalf("Create(stale same-gen): %v", err)
	}
	cr.runOrderTrackingSweepWatchdog(stale.CreatedAt.Add(time.Second))
	got, err := store.Get(stale.ID)
	if err != nil {
		t.Fatalf("Get(stale): %v", err)
	}
	if got.Status != "closed" {
		t.Fatalf("same-gen marker past its own deadline = %s, want closed by the deadline tier", got.Status)
	}
	if got.Metadata["close_reason"] != orderTrackingDeadlineExceededCloseReason {
		t.Fatalf("close_reason = %q, want %q", got.Metadata["close_reason"], orderTrackingDeadlineExceededCloseReason)
	}

	// A fresh marker of the same generation, still inside its dispatch's
	// deadline-plus-grace, must NOT close: the deadline derives from the
	// dispatch itself.
	fresh, err := store.Create(trackingBead(scoped, "ctrl-live"))
	if err != nil {
		t.Fatalf("Create(fresh same-gen): %v", err)
	}
	orderTrackingDeadlineGrace = time.Hour
	cr.runOrderTrackingSweepWatchdog(fresh.CreatedAt.Add(time.Second))
	gotFresh, err := store.Get(fresh.ID)
	if err != nil {
		t.Fatalf("Get(fresh): %v", err)
	}
	if gotFresh.Status != "open" {
		t.Fatalf("same-gen marker inside its deadline = %s, want open (deadline tier fired before the dispatch's own deadline elapsed)", gotFresh.Status)
	}
}

// TestMemoryOrderDispatcherDispatchStampsGeneration is a regression guard for
// ADR-0130 D1's actual write path, not just the predicate that reads it:
// every tracking marker a real dispatch creates must carry the dispatcher's
// own controller_generation. Deleting `Generation: m.generation` from either
// CreateRun call site in order_dispatch.go, or minting a fresh generation per
// dispatcher instead of threading the boot id through, both pass the rest of
// the suite silently; this test is what catches either.
func TestMemoryOrderDispatcherDispatchStampsGeneration(t *testing.T) {
	store := beads.NewMemStore()
	aa := []orders.Order{{
		Name:     "test-order",
		Trigger:  "cooldown",
		Interval: "1h",
		Exec:     "true",
	}}
	ad := buildOrderDispatcherFromListExec(aa, store, nil, successfulExec, nil)
	if ad == nil {
		t.Fatal("expected non-nil dispatcher")
	}
	md, ok := ad.(*memoryOrderDispatcher)
	if !ok {
		t.Fatalf("dispatcher = %T, want *memoryOrderDispatcher", ad)
	}
	md.generation = "ctrl-x"

	md.dispatch(context.Background(), t.TempDir(), time.Now())
	md.drain(context.Background())

	all := trackingBeads(t, store, "order-run:test-order")
	if len(all) != 1 {
		t.Fatalf("order-run beads = %d, want 1", len(all))
	}
	if got := all[0].Metadata[orders.GenerationMetadataKey]; got != "ctrl-x" {
		t.Fatalf("tracking bead controller_generation = %q, want %q", got, "ctrl-x")
	}
}

// TestOrderDispatcherGenerationSurvivesRescanAndReload is the D1 "reused
// across dispatcher rebuilds" guarantee at the CityRuntime layer: the
// dispatcher's generation must stay pinned to the runtime's boot id across
// both an order-set rescan and a config reload, neither of which may re-mint
// it. Re-minting per rebuild is exactly the bug D1 exists to prevent — it
// would orphan every marker created before the rebuild and hand the watchdog
// a false dead-controller verdict for the runtime's own live runs.
func TestOrderDispatcherGenerationSurvivesRescanAndReload(t *testing.T) {
	cr, tomlPath := bootSplitCityForReloadWithOrder(t)

	assertGeneration := func(stage string) {
		t.Helper()
		if cr.od == nil {
			t.Fatalf("%s: the split city has no order dispatcher, so this test asserts nothing", stage)
		}
		m, ok := cr.od.(*memoryOrderDispatcher)
		if !ok {
			t.Fatalf("%s: cr.od is %T, want *memoryOrderDispatcher", stage, cr.od)
		}
		if cr.controllerGeneration == "" {
			t.Fatalf("%s: runtime's own controllerGeneration is empty; this test asserts nothing", stage)
		}
		if m.generation != cr.controllerGeneration {
			t.Fatalf("%s: dispatcher generation = %q, want the runtime's boot id %q", stage, m.generation, cr.controllerGeneration)
		}
	}

	assertGeneration("boot")

	// The rescan rebuilds the dispatcher only when the order SET changed, so
	// add an order rather than rescanning an unchanged one.
	writeCityOrder(t, cr.cityPath, "sweeper")
	changed, _, err := cr.rescanOrderDispatcher(context.Background(), cr.cityPath, cr.cfg, "test: order scan", time.Now())
	if err != nil {
		t.Fatalf("rescanOrderDispatcher: %v", err)
	}
	if !changed {
		t.Fatal("the rescan reported no change, so it never rebuilt the dispatcher and this stage asserts nothing")
	}
	assertGeneration("after rescanOrderDispatcher")

	// A reload that leaves [storage] alone but changes something else: the
	// dispatcher is rebuilt, and its generation must be rebuilt over the same
	// boot id.
	writeSplitCityConfig(t, tomlPath, cr.cfg.Storage.Bindings["infra"].Path, "\n[daemon]\nshutdown_timeout = \"7s\"\n")
	lastProviderName := "fake"
	if reply := cr.reloadConfigTraced(context.Background(), &lastProviderName, cr.cityPath, nil, reloadSourceManual); reply.Outcome == reloadOutcomeFailed {
		t.Fatalf("reload failed, so the rebuilt dispatcher proves nothing: %s", reply.Error)
	}
	assertGeneration("after reloadConfigTraced")
}

// TestTrackingResidualCutoffNeverGuessesForAnUnknownOrder pins the "never
// guess deadlines" doctrine (stated in runOrderTrackingSweepWatchdog's own
// comment for a dispatcher-level nil residualFor) at the per-order level too:
// an order this dispatcher's own set does not recognize — a manual or
// webhook-fired marker, or one a reload removed — must return 0, not a
// synthesized deadline. classifyOrderTrackingRunForReap treats a 0 residual
// identically to a nil residualFor (never satisfies residual > 0), so this
// keeps the D2 tier disabled for that marker rather than inventing a cutoff
// none of the order's own config ever declared.
func TestTrackingResidualCutoffNeverGuessesForAnUnknownOrder(t *testing.T) {
	aa := []orders.Order{{
		Name:     "known-order",
		Trigger:  "cooldown",
		Interval: "1h",
		Exec:     "true",
		Timeout:  "10m",
	}}
	m := newMemoryOrderDispatcher(nil, aa, t.TempDir(), &config.City{}, events.Discard, io.Discard, "ctrl-x")

	if got := m.trackingResidualCutoff("unknown-order"); got != 0 {
		t.Fatalf("trackingResidualCutoff(unknown-order) = %v, want 0 (never guess a deadline the order's own config never declared)", got)
	}
	if got := m.trackingResidualCutoff("known-order"); got <= 0 {
		t.Fatalf("trackingResidualCutoff(known-order) = %v, want > 0 (a recognized order still gets its own effectiveTimeout+grace)", got)
	}
}

package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/citylayout"
)

// cmd/gc-side coverage for the vc-ny00 store-warming plan: L2's durable
// record (AC3), L1's tick-path isolation (AC1/AC2), and L3's warming pass
// (AC5) with its kill switch (AC7).

// warmingTestWall is the stand-in for the settled 30000ms listener wall.
// Unit tests may not sleep for the real one; the RELATIONS under test are
// exact at any wall, and the real number's own invariant is pinned
// separately by the AC6 suite.
const warmingTestWall = 40 * time.Millisecond

// prefixedStore is a beads.Store that reports an id prefix — the property the
// warming machinery keys every verdict on.
//
// It does not sleep to simulate a slow store. Probe latency is stated through
// storeWarmingPassProbeFn (see statePassLatency), which keeps these tests
// deterministic and keeps the suite from adding fixed-sleep call sites the
// resource census forbids growing.
type prefixedStore struct {
	beads.Store
	prefix string
	err    error
	lists  int
}

func newPrefixedStore(prefix string) *prefixedStore {
	return &prefixedStore{Store: beads.NewMemStore(), prefix: prefix}
}

func (s *prefixedStore) IDPrefix() string { return s.prefix }

func (s *prefixedStore) List(q beads.ListQuery) ([]beads.Bead, error) {
	s.lists++
	if s.err != nil {
		return nil, s.err
	}
	return s.Store.List(q)
}

// statePassLatency makes every warming-pass probe report d, and surfaces the
// store's own error when it has one, so a test can hold a store on either
// side of the wall without waiting for a real clock.
func statePassLatency(t *testing.T, d time.Duration) {
	t.Helper()
	prev := storeWarmingPassProbeFn
	storeWarmingPassProbeFn = func(store beads.Store) (time.Duration, error) {
		if ps, ok := store.(*prefixedStore); ok {
			ps.lists++
			if ps.err != nil {
				return d, ps.err
			}
		}
		return d, nil
	}
	t.Cleanup(func() { storeWarmingPassProbeFn = prev })
}

func useWarmingTestWall(t *testing.T) {
	t.Helper()
	t.Setenv("GC_STORE_WARMING_WALL_MS", fmt.Sprintf("%d", warmingTestWall.Milliseconds()))
}

// degradeStore drives a store's tracker into the open-breaker state through
// the real state machine, so these tests exercise the production predicate
// rather than a hand-set flag.
func degradeStore(t *testing.T, prefix string) {
	t.Helper()
	for i := 0; i < 5; i++ {
		beads.RecordStoreProbeForTest(prefix, warmingTestWall*2, true)
	}
	if !beads.StoreIsDegraded(prefix) {
		t.Fatalf("precondition: store %q did not degrade", prefix)
	}
}

// TestDegradedStoresAreDroppedFromTheTickSweep is AC1's tick half and AC2's
// isolation half.
//
// dispatchOrders and both order-tracking sweep watchdogs run in the SAME
// serial tick body. On 2026-09-06 that body serialized behind stores that
// could not answer, and the observable result was 20 cooldown orders going
// stale simultaneously while the supervisor logged nothing for 15 minutes.
// The store the reconciler has already proven cannot answer must not be
// walked again by the sweep on the tick path.
func TestDegradedStoresAreDroppedFromTheTickSweep(t *testing.T) {
	useWarmingTestWall(t)
	beads.ResetStoreWarmingRegistryForTest()
	t.Cleanup(beads.ResetStoreWarmingRegistryForTest)

	sick := newPrefixedStore("vcnysick")
	well := newPrefixedStore("vcnywell")
	degradeStore(t, "vcnysick")

	var stderr bytes.Buffer
	kept := filterDegradedSweepStores([]beads.Store{sick, well}, &stderr, "test")

	if len(kept) != 1 {
		t.Fatalf("kept %d store(s), want 1 — the degraded store must be dropped", len(kept))
	}
	if got, _ := sweepStorePrefix(kept[0]); got != "vcnywell" {
		t.Fatalf("kept store = %q, want the healthy vcnywell", got)
	}
	if !strings.Contains(stderr.String(), "vcnysick") {
		t.Fatalf("the skip was not announced; a silent skip leaves the operator "+
			"with no reason for the sweep's reduced scope.\nstderr: %s", stderr.String())
	}
}

// TestHealthyStoresSurviveTheSweepFilterUntouched is the other half of the
// isolation property: a degraded store must not take the fleet with it.
func TestHealthyStoresSurviveTheSweepFilterUntouched(t *testing.T) {
	useWarmingTestWall(t)
	beads.ResetStoreWarmingRegistryForTest()
	t.Cleanup(beads.ResetStoreWarmingRegistryForTest)

	stores := []beads.Store{newPrefixedStore("vcnya"), newPrefixedStore("vcnyb")}
	var stderr bytes.Buffer
	kept := filterDegradedSweepStores(stores, &stderr, "test")

	if len(kept) != 2 {
		t.Fatalf("kept %d of 2 healthy stores", len(kept))
	}
	if stderr.Len() != 0 {
		t.Fatalf("a sweep with no degraded store announced something: %s", stderr.String())
	}
}

// TestSweepFilterKeepsStoresItCannotIdentify pins the fail-safe direction.
// "Unknown scope" must never mean "skip": a store-type change that stopped
// exposing IDPrefix would otherwise silently disable both watchdogs, which
// is the stale-tracking jam (#2168) reintroduced by accident.
func TestSweepFilterKeepsStoresItCannotIdentify(t *testing.T) {
	useWarmingTestWall(t)
	beads.ResetStoreWarmingRegistryForTest()
	t.Cleanup(beads.ResetStoreWarmingRegistryForTest)

	anonymous := beads.NewMemStore()
	kept := filterDegradedSweepStores([]beads.Store{anonymous}, nil, "test")
	if len(kept) != 1 {
		t.Fatal("a store with no identifiable prefix was skipped; unknown must mean keep")
	}
}

// TestWorstCaseTickCostIsBoundedByTheTickRead is AC1's arithmetic half:
// whole-tick serialization at the pre-plan bound is impossible by
// construction, not by hope.
//
// Before: each store's reads were bounded at bdReadCommandTimeout (120s), 4x
// the listener's own wall, inside a serial tick — so N stores could cost
// N x 120s per phase. After: a tick-context read is bounded below the wall,
// and a store that exceeds it twice stops costing anything at all for the
// breaker cooldown.
func TestWorstCaseTickCostIsBoundedByTheTickRead(t *testing.T) {
	t.Parallel()

	tickBound := beads.TickReadBoundForTest()
	wall := beads.StoreWarmingWall()
	preplan := beads.ReadCommandTimeoutForTest()

	if tickBound >= wall {
		t.Fatalf("tick read bound %s >= listener wall %s: the client would wait for "+
			"the server's own kill and the tick would pay the wall per store", tickBound, wall)
	}
	if tickBound >= preplan {
		t.Fatalf("tick read bound %s is not below the pre-plan bound %s; the tick's "+
			"worst case did not improve", tickBound, preplan)
	}

	// The property that matters at fleet scale: 20 stores at the pre-plan
	// bound is a 40-minute tick; at the tick bound it is bounded by
	// stores x tickBound, and by zero once the breakers open.
	const stores = 20
	if got := time.Duration(stores) * tickBound; got >= time.Duration(stores)*preplan {
		t.Fatalf("worst-case %d-store tick did not improve: %s", stores, got)
	}
	if time.Duration(stores)*tickBound > 5*time.Minute {
		t.Fatalf("worst-case %d-store tick is %s — still long enough to starve the "+
			"patrol cadence the order-liveness probe measures", stores, time.Duration(stores)*tickBound)
	}
}

// TestStoreWarmingStateFileCarriesPerStoreEntries is AC3's durable half.
func TestStoreWarmingStateFileCarriesPerStoreEntries(t *testing.T) {
	useWarmingTestWall(t)
	beads.ResetStoreWarmingRegistryForTest()
	t.Cleanup(func() {
		beads.ResetStoreWarmingRegistryForTest()
		beads.SetStoreWarmingStateSink(nil)
	})

	cityPath := t.TempDir()
	registerStoreWarmingStateSink(cityPath, nil)

	// A real transition on one store must publish a record covering BOTH.
	beads.MarkStoreWarmingByPrefix("vcnyone")
	beads.MarkStoreWarmingByPrefix("vcnytwo")

	path := storeWarmingStatePath(citylayout.PackStateDir(cityPath, "dolt"))
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v — L2's durable record is what the pack-side consumer "+
			"reads instead of parsing supervisor.log", path, err)
	}
	var record storeWarmingStateRecord
	if err := json.Unmarshal(raw, &record); err != nil {
		t.Fatalf("unmarshal record: %v\nraw: %s", err, raw)
	}
	if record.At.IsZero() {
		t.Fatal("record has no At timestamp; At is what distinguishes a stale record from a fresh one")
	}
	if len(record.Stores) != 2 {
		t.Fatalf("record carries %d store entries, want 2 — one store's transition "+
			"must not drop another's record: %+v", len(record.Stores), record.Stores)
	}
	byStore := map[string]beads.StoreWarmingState{}
	for _, s := range record.Stores {
		byStore[s.Store] = s
	}
	for _, want := range []string{"vcnyone", "vcnytwo"} {
		st, ok := byStore[want]
		if !ok {
			t.Fatalf("record is missing store %q: %+v", want, record.Stores)
		}
		if st.State != "warming" {
			t.Fatalf("store %q state = %q, want warming", want, st.State)
		}
		if st.SinceAt.IsZero() {
			t.Fatalf("store %q has no SinceAt; dwell is what the vc-5gui RCA lacked", want)
		}
	}
}

// TestStoreWarmingStateFileSchemaIsStable pins the wire names.
//
// THIS SCHEMA IS AN ACCEPTED INTERFACE: the pack-side consumer bead builds
// against it before this code ships, in a different repo, and cannot be
// fixed up by a rename here. Fields may be ADDED; renaming or repurposing
// one silently breaks a consumer that this repo's tests cannot see.
func TestStoreWarmingStateFileSchemaIsStable(t *testing.T) {
	t.Parallel()

	raw, err := json.Marshal(storeWarmingStateRecord{
		At: time.Unix(0, 0).UTC(),
		Stores: []beads.StoreWarmingState{{
			Store: "vc", State: "warming", Degraded: true, ProbeMs: 31000,
			BoundExceeded: 2, WallMs: 30000,
			SinceAt:     time.Unix(0, 0).UTC(),
			LastProbeAt: time.Unix(0, 0).UTC(),
		}},
	})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	for _, key := range []string{
		`"at"`, `"stores"`, `"store"`, `"state"`, `"degraded"`, `"probe_ms"`,
		`"bound_exceeded"`, `"wall_ms"`, `"since_at"`, `"last_probe_at"`,
	} {
		if !strings.Contains(string(raw), key) {
			t.Errorf("published schema is missing %s — the consumer parses these "+
				"names; add fields, never rename them.\ngot: %s", key, raw)
		}
	}
}

// TestWarmingPassStopsAsSoonAsAStoreBeatsTheWall pins L3's success path: the
// pass is a warm loop, not a fixed-duration sleep, so a store that is
// already fast costs one attempt.
func TestWarmingPassStopsAsSoonAsAStoreBeatsTheWall(t *testing.T) {
	useWarmingTestWall(t)
	beads.ResetStoreWarmingRegistryForTest()
	t.Cleanup(beads.ResetStoreWarmingRegistryForTest)

	fast := newPrefixedStore("vcnyfast")
	statePassLatency(t, time.Millisecond)
	out := runStoreWarmingPass([]beads.Store{fast}, time.Minute, warmingTestWall, func(time.Duration) {})

	if out.State != storeWarmingPassStateRan {
		t.Fatalf("state = %q, want %q", out.State, storeWarmingPassStateRan)
	}
	if len(out.Stores) != 1 || !out.Stores[0].Warmed {
		t.Fatalf("a fast store was not reported warmed: %+v", out.Stores)
	}
	if out.Stores[0].Attempts != 1 {
		t.Fatalf("attempts = %d, want 1 — a store already inside the wall needs no warming",
			out.Stores[0].Attempts)
	}
}

// TestWarmingPassHonorsItsBudgetAgainstAStoreThatNeverWarms is the
// constraint-3 shape: the pass gives up rather than running forever, and
// reports the honest outcome (not warmed, no error).
func TestWarmingPassHonorsItsBudgetAgainstAStoreThatNeverWarms(t *testing.T) {
	useWarmingTestWall(t)
	beads.ResetStoreWarmingRegistryForTest()
	t.Cleanup(beads.ResetStoreWarmingRegistryForTest)

	slow := newPrefixedStore("vcnyslow")
	statePassLatency(t, warmingTestWall*2)

	started := time.Now()
	out := runStoreWarmingPass([]beads.Store{slow}, 250*time.Millisecond, warmingTestWall, func(time.Duration) {})
	elapsed := time.Since(started)

	if elapsed > 5*time.Second {
		t.Fatalf("the pass ran %s against a 250ms budget; an unbounded warm loop is "+
			"a background watcher by another name", elapsed)
	}
	if len(out.Stores) != 1 {
		t.Fatalf("want 1 store outcome, got %+v", out.Stores)
	}
	if out.Stores[0].Warmed {
		t.Fatal("a store that never beat the wall was reported warmed")
	}
	if out.Stores[0].Attempts == 0 {
		t.Fatal("the pass recorded no attempts")
	}
}

// TestWarmingPassReportsFailedWhenEveryStoreErrors pins AC5's three-way
// distinction: ran / skipped / failed must stay machine-distinguishable.
// A pass whose every store was unreachable is FAILED, not a successful pass
// that happened to warm nothing.
func TestWarmingPassReportsFailedWhenEveryStoreErrors(t *testing.T) {
	useWarmingTestWall(t)
	beads.ResetStoreWarmingRegistryForTest()
	t.Cleanup(beads.ResetStoreWarmingRegistryForTest)

	broken := newPrefixedStore("vcnybroken")
	broken.err = fmt.Errorf("dial tcp: connection refused")
	statePassLatency(t, time.Millisecond)

	out := runStoreWarmingPass([]beads.Store{broken}, 200*time.Millisecond, warmingTestWall, func(time.Duration) {})
	if out.State != storeWarmingPassStateFailed {
		t.Fatalf("state = %q, want %q for a pass whose every store errored", out.State, storeWarmingPassStateFailed)
	}
	if out.Err == "" {
		t.Fatal("a failed pass carries no error text")
	}
}

// TestWarmingPassBudgetIsSharedNotPerStore pins the resource model: the
// contended resource is the ONE listener, so N stores must not each be
// granted the full budget.
func TestWarmingPassBudgetIsSharedNotPerStore(t *testing.T) {
	useWarmingTestWall(t)
	beads.ResetStoreWarmingRegistryForTest()
	t.Cleanup(beads.ResetStoreWarmingRegistryForTest)

	var stores []beads.Store
	for i := 0; i < 4; i++ {
		stores = append(stores, newPrefixedStore(fmt.Sprintf("vcnyb%d", i)))
	}
	statePassLatency(t, warmingTestWall*2)

	started := time.Now()
	runStoreWarmingPass(stores, 300*time.Millisecond, warmingTestWall, func(time.Duration) {})
	elapsed := time.Since(started)

	// Generous ceiling: the point is that it is bounded by ONE budget (plus
	// one in-flight probe per store), not by 4 x budget.
	if elapsed > 2*time.Second {
		t.Fatalf("4 never-warming stores took %s against a shared 300ms budget; "+
			"the budget is being applied per store", elapsed)
	}
}

// TestWarmingPassKillSwitchSkipsLoudly is AC7 for L3 and constraint 4's
// second half: absence is byte-identical to today, and a skip is never
// quiet. A silent skip is the vp-cblo shape — the mechanism did not run and
// nothing says so.
func TestWarmingPassKillSwitchSkipsLoudly(t *testing.T) {
	useWarmingTestWall(t)
	t.Setenv(storeWarmingBudgetEnv, "0")
	beads.ResetStoreWarmingRegistryForTest()
	t.Cleanup(beads.ResetStoreWarmingRegistryForTest)

	if _, enabled := storeWarmingPassBudget(); enabled {
		t.Fatal("GC_STORE_WARMING_BUDGET_S=0 did not disable the pass")
	}

	packStateDir := t.TempDir()
	var stderr bytes.Buffer
	startStoreWarmingPass(t.TempDir(), packStateDir, &stderr)

	if !strings.Contains(stderr.String(), "SKIPPED") {
		t.Fatalf("the disabled pass was not loud on stderr: %q", stderr.String())
	}
	raw, err := os.ReadFile(storeWarmingPassPath(packStateDir))
	if err != nil {
		t.Fatalf("the disabled pass wrote no durable record: %v", err)
	}
	var out storeWarmingPassOutcome
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatalf("unmarshal outcome: %v", err)
	}
	if out.State != storeWarmingPassStateSkipped {
		t.Fatalf("state = %q, want %q", out.State, storeWarmingPassStateSkipped)
	}
	if out.Skipped == "" {
		t.Fatal("the skip record carries no reason")
	}
}

// TestWarmingPassBudgetDefaultsRatherThanSilentlyDisabling pins the one
// direction a misconfiguration must not take. A garbage value is not consent
// to switch the mechanism off — that would be a quiet skip caused by a typo.
func TestWarmingPassBudgetDefaultsRatherThanSilentlyDisabling(t *testing.T) {
	for _, raw := range []string{"not-a-number", "1800s", ""} {
		t.Run("value="+raw, func(t *testing.T) {
			t.Setenv(storeWarmingBudgetEnv, raw)
			budget, enabled := storeWarmingPassBudget()
			if !enabled {
				t.Fatalf("%q disabled the pass; only an explicit 0 may", raw)
			}
			if budget != defaultStoreWarmingBudget {
				t.Fatalf("budget = %s, want the default %s", budget, defaultStoreWarmingBudget)
			}
		})
	}
}

// TestWarmingPassStartRecordsBeforeItRuns pins the property that keeps a
// short-lived process honest: the pass runs on a goroutine so it never
// blocks serving (constraint 3), so a process that exits first must still
// leave evidence the pass began rather than no record at all.
func TestWarmingPassStartRecordsBeforeItRuns(t *testing.T) {
	useWarmingTestWall(t)
	beads.ResetStoreWarmingRegistryForTest()
	t.Cleanup(beads.ResetStoreWarmingRegistryForTest)

	packStateDir := t.TempDir()
	var stderr bytes.Buffer

	prev := storeWarmingPassStoresFn
	// A resolver that never returns: it stands in for a pass still running
	// when the process would exit.
	storeWarmingPassStoresFn = func(string) ([]beads.Store, func(), error) {
		select {}
	}
	t.Cleanup(func() { storeWarmingPassStoresFn = prev })

	startStoreWarmingPass(t.TempDir(), packStateDir, &stderr)

	raw, err := os.ReadFile(storeWarmingPassPath(packStateDir))
	if err != nil {
		t.Fatalf("no record was written before the pass began: %v", err)
	}
	var out storeWarmingPassOutcome
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatalf("unmarshal outcome: %v", err)
	}
	if out.State != storeWarmingPassStateStarted {
		t.Fatalf("state = %q, want %q — a pass that is still running must not read "+
			"as one that never happened", out.State, storeWarmingPassStateStarted)
	}
}

// TestStartWarmingPassReturnsImmediately is constraint 3 stated directly:
// a warming pass never blocks serving. The managed start calls this AFTER
// the server is ready and published, and must not wait on it.
func TestStartWarmingPassReturnsImmediately(t *testing.T) {
	useWarmingTestWall(t)
	beads.ResetStoreWarmingRegistryForTest()
	t.Cleanup(beads.ResetStoreWarmingRegistryForTest)

	prev := storeWarmingPassStoresFn
	// Blocks forever rather than sleeping: the assertion is that the CALLER
	// does not wait, so the stub only has to never return.
	storeWarmingPassStoresFn = func(string) ([]beads.Store, func(), error) {
		select {}
	}
	t.Cleanup(func() { storeWarmingPassStoresFn = prev })

	started := time.Now()
	startStoreWarmingPass(t.TempDir(), t.TempDir(), nil)
	if elapsed := time.Since(started); elapsed > 2*time.Second {
		t.Fatalf("startStoreWarmingPass blocked for %s; a warming pass may never "+
			"block serving (plan constraint 3)", elapsed)
	}
}

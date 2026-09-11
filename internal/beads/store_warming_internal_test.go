package beads

import (
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"
)

// The vc-ny00 store-warming suite: L1's tick-context read bound and
// per-store breaker, and L2's warming state machine.
//
// The incident these pin (vc-5gui, 2026-09-06) is a supervisor that STOPPED
// instead of degrading: reads bounded at 120s — 4x the listener's settled
// read_timeout_millis=30000 — inside a serial tick, so one slow store cost
// every phase of every tick, producing 15 minutes of log silence and 20
// simultaneously-stale cooldown orders. Every test below asserts one of the
// three properties that turn that stall into a degradation: BOUNDED,
// ANNOUNCED, ISOLATED.
//
// Wall-clock note: the real wall is 30s, which no unit test may sleep for.
// These tests move the threshold with GC_STORE_WARMING_WALL_MS and sleep
// past the smaller value, so the RELATIONS under test are exact while the
// suite stays fast. The one place the real number matters — that the tick
// bound sits below the real wall — is asserted against the constants
// directly, with no clock at all.

// warmingTestWall is the stand-in wall these tests measure probes against.
const warmingTestWall = 40 * time.Millisecond

// useTestWall points the state machine at a millisecond-scale wall and
// guarantees the shared registry starts empty, so one test's degraded store
// is never another's starting condition.
func useTestWall(t *testing.T) {
	t.Helper()
	t.Setenv(storeWarmingWallEnv, fmt.Sprintf("%d", warmingTestWall.Milliseconds()))
	resetStoreWarmingRegistryForTest()
	t.Cleanup(resetStoreWarmingRegistryForTest)
}

// TestTickReadBoundStaysBelowTheListenerWall pins the ONE relation in L1
// that is load-bearing rather than merely tuned.
//
// The client bound must expire BEFORE the server's own read_timeout_millis
// reaps the query. If it did not, the tick would pay the full wall on every
// degraded store — which is the 00:33 cliff itself, just with a smaller
// constant. This is asserted against the real defaults, not a test wall:
// it is the invariant a future tuning change must not quietly break.
func TestTickReadBoundStaysBelowTheListenerWall(t *testing.T) {
	t.Parallel()

	wall := time.Duration(defaultStoreWarmingWallMillis) * time.Millisecond
	if defaultBdTickReadTimeout >= wall {
		t.Fatalf("tick read bound %s must be strictly below the listener wall %s, "+
			"or the client waits for the server's own kill and the serial tick "+
			"pays the wall per store (vc-ny00 L1)", defaultBdTickReadTimeout, wall)
	}
	if bdReadCommandTimeout <= wall {
		t.Fatalf("precondition changed: the non-tick read bound %s is no longer "+
			"above the wall %s, so this test no longer describes the defect",
			bdReadCommandTimeout, wall)
	}
}

// TestTickReadBoundAppliesOnlyInsideATickFrame pins the ISOLATION half of
// L1: the shorter bound is a property of the serial tick, not of the
// process. A CLI command, a hook or a sling keeps the 120s bound, because
// their cost model is one human waiting for one command — not a serial loop
// over every store.
func TestTickReadBoundAppliesOnlyInsideATickFrame(t *testing.T) {
	if got := bdTickReadBound(); got != bdReadCommandTimeout {
		t.Fatalf("outside a tick frame: bound = %s, want the unmodified %s", got, bdReadCommandTimeout)
	}

	prev := SetReconcilerTickTrigger("patrol")
	inTick := bdTickReadBound()
	RestoreReconcilerTickTrigger(prev)

	if inTick != defaultBdTickReadTimeout {
		t.Fatalf("inside a tick frame: bound = %s, want %s", inTick, defaultBdTickReadTimeout)
	}
	if got := bdTickReadBound(); got != bdReadCommandTimeout {
		t.Fatalf("after the tick frame closed: bound = %s, want %s restored", got, bdReadCommandTimeout)
	}
}

// TestTickReadBoundKillSwitchRestoresThePrePlanBound covers AC7 for L1's
// knob: with the mechanism disabled, a read-class call inside a tick is
// bounded exactly as it was before this plan existed.
func TestTickReadBoundKillSwitchRestoresThePrePlanBound(t *testing.T) {
	for _, off := range []string{"0", "-1", "not-a-number"} {
		t.Run(off, func(t *testing.T) {
			t.Setenv(bdTickReadTimeoutEnv, off)
			prev := SetReconcilerTickTrigger("patrol")
			defer RestoreReconcilerTickTrigger(prev)
			if got := bdTickReadBound(); got != bdReadCommandTimeout {
				t.Fatalf("GC_BD_TICK_READ_TIMEOUT_S=%s: bound = %s, want the pre-plan %s",
					off, got, bdReadCommandTimeout)
			}
		})
	}
}

// TestTickReadBoundHonorsAnOperatorSetValue pins the tuning knob's other
// direction — an explicit number is used verbatim, so an operator can widen
// or narrow the bound on a running supervisor without a rebuild.
func TestTickReadBoundHonorsAnOperatorSetValue(t *testing.T) {
	t.Setenv(bdTickReadTimeoutEnv, "7")
	prev := SetReconcilerTickTrigger("patrol")
	defer RestoreReconcilerTickTrigger(prev)
	if got := bdTickReadBound(); got != 7*time.Second {
		t.Fatalf("bound = %s, want 7s", got)
	}
}

// TestBreakerTripsWithinNBoundExceededReads is AC1's state-machine half:
// a store whose reads exceed the bound is broken within N reads, not after
// an unbounded run of them.
func TestBreakerTripsWithinNBoundExceededReads(t *testing.T) {
	useTestWall(t)
	tr := storeWarmingTrackerFor("vcny-trip")
	now := time.Now()

	st, _ := tr.recordProbe(now, warmingTestWall+time.Millisecond, true, bdReadCommandTimeout)
	if st.Degraded {
		t.Fatalf("after 1 bound-exceeded read the breaker is open; want it to need %d", defaultStoreBreakerTrips)
	}
	if st.State != "warming" {
		t.Fatalf("after 1 bound-exceeded read: state = %q, want warming", st.State)
	}

	st, _ = tr.recordProbe(now, warmingTestWall+time.Millisecond, true, bdReadCommandTimeout)
	if !st.Degraded {
		t.Fatalf("after %d bound-exceeded reads the breaker is still closed; the "+
			"tick would keep paying for this store (vc-ny00 AC1)", defaultStoreBreakerTrips)
	}
	if st.BoundExceeded != defaultStoreBreakerTrips {
		t.Fatalf("BoundExceeded = %d, want %d", st.BoundExceeded, defaultStoreBreakerTrips)
	}
	if !tr.breakerOpen(now) {
		t.Fatal("breakerOpen reports closed immediately after the trip")
	}
	if !tr.breakerOpen(now.Add(storeBreakerCooldown() - time.Second)) {
		t.Fatal("breaker closed before its cooldown expired")
	}
	if tr.breakerOpen(now.Add(storeBreakerCooldown() + time.Second)) {
		t.Fatal("breaker still open after the cooldown expired; re-entry must be " +
			"by probe, and the probe is the first reconcile after the cooldown")
	}
}

// TestSlowButAnsweringStoreIsWarmingNeverDegraded pins the distinction the
// breaker rests on. A store that answers over the wall is doing real work
// and returning real data; skipping it would trade a slow answer for no
// answer. Only a store that FAILED at or beyond the wall is degraded.
func TestSlowButAnsweringStoreIsWarmingNeverDegraded(t *testing.T) {
	useTestWall(t)
	tr := storeWarmingTrackerFor("vcny-slow")
	now := time.Now()

	for i := 0; i < defaultStoreBreakerTrips+3; i++ {
		st, _ := tr.recordProbe(now, warmingTestWall*2, false, bdReadCommandTimeout)
		if st.Degraded {
			t.Fatalf("probe %d: a SUCCEEDING slow read opened the breaker; only a "+
				"read that failed at or beyond the wall may", i+1)
		}
		if st.State != "warming" {
			t.Fatalf("probe %d: state = %q, want warming", i+1, st.State)
		}
	}
}

// TestFailFastDoesNotTripTheWarmingBreaker keeps this mechanism off another
// mechanism's territory. A read that fails immediately is a broken store or
// a bad query — the existing syncFailures circuit breaker's class — not a
// slow one. Tripping here would make the warming record explain a stale
// order set that warming had nothing to do with, which is precisely the
// false signal the pack-side consumer must not be handed.
func TestFailFastDoesNotTripTheWarmingBreaker(t *testing.T) {
	useTestWall(t)
	tr := storeWarmingTrackerFor("vcny-fast-fail")
	now := time.Now()

	for i := 0; i < defaultStoreBreakerTrips+3; i++ {
		st, _ := tr.recordProbe(now, time.Millisecond, true, bdReadCommandTimeout)
		if st.Degraded {
			t.Fatalf("probe %d: a fast failure opened the warming breaker", i+1)
		}
	}
}

// TestWarmingExitsOnlyAfterTwoConsecutiveSubWallProbes pins the anti-flap
// rule. One good probe is a data point; two is a trend. The pack-side
// consumer's "is there a FRESH warming record" test is only meaningful if
// the state does not oscillate at the boundary.
func TestWarmingExitsOnlyAfterTwoConsecutiveSubWallProbes(t *testing.T) {
	useTestWall(t)
	tr := storeWarmingTrackerFor("vcny-exit")
	now := time.Now()

	if st := tr.snapshot(now); st.State != "warming" {
		t.Fatalf("a freshly tracked store starts %q; want warming — a store this "+
			"process has never read has not been SHOWN healthy", st.State)
	}

	st, _ := tr.recordProbe(now, time.Millisecond, false, bdReadCommandTimeout)
	if st.State != "warming" {
		t.Fatalf("after 1 good probe: state = %q, want warming still", st.State)
	}
	st, changed := tr.recordProbe(now, time.Millisecond, false, bdReadCommandTimeout)
	if st.State != "healthy" {
		t.Fatalf("after 2 consecutive good probes: state = %q, want healthy", st.State)
	}
	if !changed {
		t.Fatal("the healthy transition was not announced as a change")
	}

	// A single slow probe re-enters warming and resets the run, so the exit
	// needs two fresh good probes rather than one.
	if st, _ = tr.recordProbe(now, warmingTestWall*2, false, bdReadCommandTimeout); st.State != "warming" {
		t.Fatalf("after a slow probe: state = %q, want warming", st.State)
	}
	if st, _ = tr.recordProbe(now, time.Millisecond, false, bdReadCommandTimeout); st.State != "warming" {
		t.Fatalf("one good probe after re-entry: state = %q, want warming (run was reset)", st.State)
	}
	if st, _ = tr.recordProbe(now, time.Millisecond, false, bdReadCommandTimeout); st.State != "healthy" {
		t.Fatalf("two good probes after re-entry: state = %q, want healthy", st.State)
	}
}

// TestAPassingProbeClosesAnOpenBreakerImmediately pins the recovery edge.
// The cooldown exists to stop hammering a store that cannot answer, not to
// keep punishing one that just did.
func TestAPassingProbeClosesAnOpenBreakerImmediately(t *testing.T) {
	useTestWall(t)
	tr := storeWarmingTrackerFor("vcny-recover")
	now := time.Now()

	for i := 0; i < defaultStoreBreakerTrips; i++ {
		tr.recordProbe(now, warmingTestWall+time.Millisecond, true, bdReadCommandTimeout)
	}
	if !tr.breakerOpen(now) {
		t.Fatal("precondition: breaker did not open")
	}
	st, _ := tr.recordProbe(now, time.Millisecond, false, bdReadCommandTimeout)
	if st.Degraded || tr.breakerOpen(now) {
		t.Fatal("a passing probe left the breaker open")
	}
}

// TestBreakerKillSwitchNeverSkipsAStore covers AC7 for L1's breaker knob:
// with GC_STORE_BREAKER_TRIPS=0 the state machine still measures and still
// announces, but no store is ever skipped — byte-identical to the pre-plan
// reconcile schedule.
func TestBreakerKillSwitchNeverSkipsAStore(t *testing.T) {
	useTestWall(t)
	t.Setenv(storeBreakerTripsEnv, "0")
	tr := storeWarmingTrackerFor("vcny-nobreaker")
	now := time.Now()

	for i := 0; i < 10; i++ {
		st, _ := tr.recordProbe(now, warmingTestWall+time.Millisecond, true, bdReadCommandTimeout)
		if st.Degraded {
			t.Fatalf("probe %d: breaker opened with GC_STORE_BREAKER_TRIPS=0", i+1)
		}
		if st.State != "warming" {
			t.Fatalf("probe %d: state = %q, want warming — the kill switch disables "+
				"SKIPPING, not measurement", i+1, st.State)
		}
	}
	if tr.breakerOpen(now) {
		t.Fatal("breakerOpen true with the breaker disabled")
	}
}

// TestOneStoresBreakerDoesNotDegradeAnother is the ISOLATION property in
// its most direct form: the 2026-09-06 failure was one slow store stalling
// order dispatch fleet-wide.
func TestOneStoresBreakerDoesNotDegradeAnother(t *testing.T) {
	useTestWall(t)
	now := time.Now()
	sick := storeWarmingTrackerFor("vcny-sick")
	well := storeWarmingTrackerFor("vcny-well")

	for i := 0; i < defaultStoreBreakerTrips; i++ {
		sick.recordProbe(now, warmingTestWall+time.Millisecond, true, bdReadCommandTimeout)
		well.recordProbe(now, time.Millisecond, false, bdReadCommandTimeout)
	}
	if !sick.breakerOpen(now) {
		t.Fatal("the sick store's breaker did not open")
	}
	if well.breakerOpen(now) {
		t.Fatal("a healthy store was degraded by another store's breaker")
	}
	if !StoreIsDegraded("vcny-sick") {
		t.Fatal("StoreIsDegraded does not see the sick store's open breaker")
	}
	if StoreIsDegraded("vcny-well") {
		t.Fatal("StoreIsDegraded reports a healthy store as degraded")
	}
}

// TestStoreIsDegradedIsFalseForAnUnprobedStore pins that absence of
// evidence is not evidence of degradation. The registry must not mint a
// warming tracker on a bare lookup, or every store on a fresh process would
// read as degraded and the tick would skip all of them.
func TestStoreIsDegradedIsFalseForAnUnprobedStore(t *testing.T) {
	useTestWall(t)
	if StoreIsDegraded("vcny-never-seen") {
		t.Fatal("an unprobed store reports degraded")
	}
	if got := StoreWarmingStates(); len(got) != 0 {
		t.Fatalf("a bare StoreIsDegraded lookup created %d tracker(s); it must not", len(got))
	}
}

// TestStoreWarmingStatesAreSortedAndPerStore pins the shape the durable
// record and its pack-side consumer depend on: one entry per store, stable
// order across passes.
func TestStoreWarmingStatesAreSortedAndPerStore(t *testing.T) {
	useTestWall(t)
	now := time.Now()
	for _, name := range []string{"vcny-c", "vcny-a", "vcny-b"} {
		storeWarmingTrackerFor(name).recordProbe(now, time.Millisecond, false, bdReadCommandTimeout)
	}
	got := StoreWarmingStates()
	if len(got) != 3 {
		t.Fatalf("got %d states, want 3", len(got))
	}
	for i, want := range []string{"vcny-a", "vcny-b", "vcny-c"} {
		if got[i].Store != want {
			t.Fatalf("state[%d].Store = %q, want %q (states must be sorted)", i, got[i].Store, want)
		}
		if got[i].WallMs != warmingTestWall.Milliseconds() {
			t.Fatalf("state[%d].WallMs = %d, want %d — a consumer cannot interpret "+
				"ProbeMs without the wall it was judged against",
				i, got[i].WallMs, warmingTestWall.Milliseconds())
		}
	}
}

// TestMarkStoreWarmingCoversTheManagedStartEdge pins the one warming
// transition no reconcile probe can observe in advance: the provider
// restarting the server underneath a cache that is already live and healthy.
func TestMarkStoreWarmingCoversTheManagedStartEdge(t *testing.T) {
	useTestWall(t)
	now := time.Now()
	tr := storeWarmingTrackerFor("vcny-restart")
	tr.recordProbe(now, time.Millisecond, false, bdReadCommandTimeout)
	tr.recordProbe(now, time.Millisecond, false, bdReadCommandTimeout)
	if st := tr.snapshot(now); st.State != "healthy" {
		t.Fatalf("precondition: state = %q, want healthy", st.State)
	}

	var published []StoreWarmingState
	SetStoreWarmingStateSink(func(st StoreWarmingState) { published = append(published, st) })
	t.Cleanup(func() { SetStoreWarmingStateSink(nil) })

	MarkStoreWarmingByPrefix("vcny-restart")
	if st := tr.snapshot(now); st.State != "warming" {
		t.Fatalf("after MarkStoreWarmingByPrefix: state = %q, want warming", st.State)
	}
	if len(published) != 1 || published[0].State != "warming" {
		t.Fatalf("the managed-start warming edge was not published: %+v", published)
	}

	// Idempotent: marking an already-warming store announces nothing new.
	published = nil
	MarkStoreWarmingByPrefix("vcny-restart")
	if len(published) != 0 {
		t.Fatalf("re-marking an already-warming store published %d transition(s), want 0", len(published))
	}
}

// TestOnlyTransitionsArePublished keeps the durable record a record of
// CHANGES. Rewriting the file on every probe would make its At timestamp
// meaningless as a dwell signal, which is the thing the vc-5gui RCA lacked.
func TestOnlyTransitionsArePublished(t *testing.T) {
	useTestWall(t)
	var published []StoreWarmingState
	SetStoreWarmingStateSink(func(st StoreWarmingState) { published = append(published, st) })
	t.Cleanup(func() { SetStoreWarmingStateSink(nil) })

	now := time.Now()
	tr := storeWarmingTrackerFor("vcny-transitions")
	tr.recordProbe(now, time.Millisecond, false, bdReadCommandTimeout)
	st, changed := tr.recordProbe(now, time.Millisecond, false, bdReadCommandTimeout)
	if !changed || st.State != "healthy" {
		t.Fatalf("want an announced healthy transition, got changed=%v state=%q", changed, st.State)
	}
	// Three more good probes change nothing.
	for i := 0; i < 3; i++ {
		if _, changed := tr.recordProbe(now, time.Millisecond, false, bdReadCommandTimeout); changed {
			t.Fatalf("steady-state probe %d was announced as a transition", i+1)
		}
	}
}

// slowFailRunner is a bd runner whose list calls fail the way the listener
// reaping a query at read_timeout_millis fails, which is what the 2026-09-06
// incident's reads did.
//
// It does NOT sleep to produce a slow read. The duration the state machine
// reacts to is supplied through storeWarmingProbeElapsedFn (see stateElapsed
// below), so these tests are deterministic rather than racing a real clock on
// a loaded host.
type slowFailRunner struct {
	mu    sync.Mutex
	fail  bool
	lists int
}

func (r *slowFailRunner) run(_, name string, args ...string) ([]byte, error) {
	r.mu.Lock()
	fail := r.fail
	if len(args) > 0 && args[0] == "list" {
		r.lists++
	}
	r.mu.Unlock()

	if name != "bd" || len(args) == 0 {
		return nil, fmt.Errorf("unexpected command %q %v", name, args)
	}
	switch args[0] {
	case "list":
		if fail {
			return nil, fmt.Errorf("timed out after %s", warmingTestWall)
		}
		return []byte(`[{"id":"vcny-1","title":"one","status":"open"}]`), nil
	case "version":
		return []byte("bd version 1.0.4\n"), nil
	}
	return []byte(`[]`), nil
}

func (r *slowFailRunner) set(fail bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.fail = fail
}

// stateElapsed makes every probe report the given duration, so a test can put
// the state machine on either side of the wall without sleeping.
func stateElapsed(t *testing.T, d time.Duration) {
	t.Helper()
	storeWarmingProbeElapsedFn = func(time.Duration) time.Duration { return d }
	t.Cleanup(func() {
		storeWarmingProbeElapsedFn = func(actual time.Duration) time.Duration { return actual }
	})
}

func (r *slowFailRunner) listCount() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.lists
}

// TestDegradedStoreIsSkippedAndAnnouncedByTheReconciler is AC1's reconcile
// half and AC2's announcement half, on the real code path.
//
// It reproduces the 00:33 cliff in miniature: a store whose reads exceed the
// bound. What must follow is the OPPOSITE of the incident — the reconciler
// stops paying for the store (the backing is not called again), and the skip
// is announced rather than silent.
func TestDegradedStoreIsSkippedAndAnnouncedByTheReconciler(t *testing.T) {
	useTestWall(t)
	logs := captureLog(t)

	runner := &slowFailRunner{}
	runner.set(true)
	stateElapsed(t, warmingTestWall+time.Millisecond)
	cache := newCachingStore(NewBdStore("/city", runner.run), "vcnyskip", nil)

	for i := 0; i < defaultStoreBreakerTrips; i++ {
		cache.runReconciliation()
	}
	if !cache.StoreDegraded() {
		t.Fatalf("after %d bound-exceeded reconciles the store is not degraded; "+
			"the tick would keep paying for it", defaultStoreBreakerTrips)
	}
	callsAtTrip := runner.listCount()

	// The whole point: a degraded store costs the next cycles NOTHING.
	for i := 0; i < 5; i++ {
		cache.runReconciliation()
	}
	if got := runner.listCount(); got != callsAtTrip {
		t.Fatalf("the backing was called %d more time(s) while the breaker was open; "+
			"a degraded store must cost the reconciler nothing until the cooldown expires",
			got-callsAtTrip)
	}

	out := logs.String()
	if !strings.Contains(out, "store=degraded") {
		t.Fatalf("the skip was not announced — a silent skip is the vp-cblo shape "+
			"this layer exists to prevent.\nlog:\n%s", out)
	}
	if !strings.Contains(out, "vcnyskip") {
		t.Fatalf("the announcement does not name the store.\nlog:\n%s", out)
	}
}

// TestDegradedSkipIsAnnouncedOncePerEpisode keeps the announcement useful.
// One line per episode is a signal; one per skipped cycle is a flood that
// buries the rest of the supervisor log — the same reason the availability
// gate's own skip dedupes.
func TestDegradedSkipIsAnnouncedOncePerEpisode(t *testing.T) {
	useTestWall(t)
	logs := captureLog(t)

	runner := &slowFailRunner{}
	runner.set(true)
	stateElapsed(t, warmingTestWall+time.Millisecond)
	cache := newCachingStore(NewBdStore("/city", runner.run), "vcnyonce", nil)
	for i := 0; i < defaultStoreBreakerTrips; i++ {
		cache.runReconciliation()
	}
	for i := 0; i < 5; i++ {
		cache.runReconciliation()
	}

	if got := strings.Count(logs.String(), "store=degraded"); got != 1 {
		t.Fatalf("the skip was announced %d times, want exactly 1 per episode", got)
	}
}

// TestReconcileHeartbeatCarriesTheWarmingGauge is AC3's log half: the gauge
// rides the once-a-minute line operators already grep.
func TestReconcileHeartbeatCarriesTheWarmingGauge(t *testing.T) {
	useTestWall(t)
	logs := captureLog(t)

	runner := &slowFailRunner{}
	stateElapsed(t, time.Millisecond)
	cache := newCachingStore(NewBdStore("/city", runner.run), "vcnygauge", nil)
	cache.runReconciliation()

	out := logs.String()
	if !strings.Contains(out, "beads cache: reconciled rig=vcnygauge") {
		t.Fatalf("no reconcile heartbeat emitted.\nlog:\n%s", out)
	}
	if !strings.Contains(out, "store=warming") || !strings.Contains(out, "probe_ms=") {
		t.Fatalf("the heartbeat carries no warming gauge; a dwell needs a series "+
			"and this is the line that provides it.\nlog:\n%s", out)
	}
}

// TestDegradedStoreStillHeartbeatsWithinOneWindow is AC3's hard half, and
// the reason the gauge could not simply ride the success line.
//
// During the incident the store answered NOTHING for 15 minutes. A gauge
// emitted only by successful reconciles would go silent for exactly the
// episode it exists to describe, and the pack-side consumer would find no
// fresh warming record to explain the stale order set — reproducing the
// false page this plan removes.
func TestDegradedStoreStillHeartbeatsWithinOneWindow(t *testing.T) {
	useTestWall(t)
	logs := captureLog(t)

	runner := &slowFailRunner{}
	runner.set(true)
	stateElapsed(t, warmingTestWall+time.Millisecond)
	cache := newCachingStore(NewBdStore("/city", runner.run), "vcnyquiet", nil)
	cache.runReconciliation()

	out := logs.String()
	if !strings.Contains(out, "beads cache: reconcile failed rig=vcnyquiet") {
		t.Fatalf("a failing reconcile emitted no heartbeat at all.\nlog:\n%s", out)
	}
	if !strings.Contains(out, "store=warming") {
		t.Fatalf("the failing reconcile's heartbeat carries no warming gauge.\nlog:\n%s", out)
	}
}

// TestWarmingStateKillSwitchLeavesTheHeartbeatPrePlanIdentical is AC7 for
// L2: with the layer off, the line an operator greps is exactly the line
// they grepped before this plan (constraint 4).
func TestWarmingStateKillSwitchLeavesTheHeartbeatPrePlanIdentical(t *testing.T) {
	useTestWall(t)
	t.Setenv(storeWarmingStateEnv, "0")
	logs := captureLog(t)

	runner := &slowFailRunner{}
	stateElapsed(t, time.Millisecond)
	cache := newCachingStore(NewBdStore("/city", runner.run), "vcnyoff", nil)
	cache.runReconciliation()

	out := strings.TrimSpace(logs.String())
	if !strings.Contains(out, "beads cache: reconciled rig=vcnyoff") {
		t.Fatalf("no reconcile heartbeat emitted.\nlog:\n%s", out)
	}
	if strings.Contains(out, "store=") || strings.Contains(out, "probe_ms=") {
		t.Fatalf("GC_STORE_WARMING_STATE=0 still emitted the gauge; the kill switch "+
			"must leave the line byte-identical to pre-plan.\nlog:\n%s", out)
	}
	if !strings.HasSuffix(out, "deps_wipes=0") {
		t.Fatalf("the disabled line does not end where the pre-plan line ended "+
			"(deps_wipes=N).\nlog:\n%s", out)
	}
}

// TestWarmingStateKillSwitchSuppressesTheDurableRecord completes AC7 for
// L2: nothing is published when the layer is off.
func TestWarmingStateKillSwitchSuppressesTheDurableRecord(t *testing.T) {
	useTestWall(t)
	t.Setenv(storeWarmingStateEnv, "0")

	var published []StoreWarmingState
	SetStoreWarmingStateSink(func(st StoreWarmingState) { published = append(published, st) })
	t.Cleanup(func() { SetStoreWarmingStateSink(nil) })

	runner := &slowFailRunner{}
	stateElapsed(t, time.Millisecond)
	cache := newCachingStore(NewBdStore("/city", runner.run), "vcnyoff2", nil)
	cache.runReconciliation()
	cache.runReconciliation()
	MarkStoreWarmingByPrefix("vcnyoff2")

	if len(published) != 0 {
		t.Fatalf("GC_STORE_WARMING_STATE=0 published %d record(s), want 0: %+v", len(published), published)
	}
}

// TestReconcileRecoversAfterTheCooldownExpires pins the re-entry rule: the
// first reconcile after the cooldown IS the probe, so a store that healed
// resumes without operator action.
func TestReconcileRecoversAfterTheCooldownExpires(t *testing.T) {
	useTestWall(t)

	runner := &slowFailRunner{}
	runner.set(true)
	stateElapsed(t, warmingTestWall+time.Millisecond)
	cache := newCachingStore(NewBdStore("/city", runner.run), "vcnyheal", nil)
	for i := 0; i < defaultStoreBreakerTrips; i++ {
		cache.runReconciliation()
	}
	if !cache.StoreDegraded() {
		t.Fatal("precondition: store did not degrade")
	}

	// The store heals and the cooldown expires. The clock is MOVED, not
	// waited on: the breaker's contract is time-based, and sleeping through
	// a real cooldown would make this test both slow and flaky on a loaded
	// host without testing anything the seam does not.
	runner.set(false)
	stateElapsed(t, time.Millisecond)
	base := time.Now()
	storeWarmingNowFn = func() time.Time { return base.Add(2 * storeBreakerCooldown()) }
	t.Cleanup(func() { storeWarmingNowFn = time.Now })
	callsBefore := runner.listCount()
	cache.runReconciliation()
	if runner.listCount() == callsBefore {
		t.Fatal("the reconciler never re-probed after the cooldown expired; " +
			"re-entry is by probe and this store would stay degraded forever")
	}
	if cache.StoreDegraded() {
		t.Fatal("a passing probe after the cooldown left the store degraded")
	}
}

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

// open is the breaker verdict "unavailable, probe admission at until" — what
// the cache reads through its AvailabilityGate while the scope's transport
// breaker is open.
func open(until time.Time) storeAvailability {
	return storeAvailability{degraded: true, until: until}
}

// closed is the breaker verdict "available".
var closed = storeAvailability{}

// TestBoundExceededReadsAreWarmingNotADegradeDecision pins the division of
// labor. A run of bound-exceeded reads is warming EVIDENCE, counted and
// published; whether the store is skipped is the transport breaker's call,
// which this state machine never makes on its own.
func TestBoundExceededReadsAreWarmingNotADegradeDecision(t *testing.T) {
	useTestWall(t)
	tr := storeWarmingTrackerFor("vcny-bound")
	now := time.Now()
	tr.recordProbe(now, time.Millisecond, false, bdReadCommandTimeout, closed)
	if st, _ := tr.recordProbe(now, time.Millisecond, false, bdReadCommandTimeout, closed); st.State != "healthy" {
		t.Fatalf("precondition: state = %q, want healthy", st.State)
	}

	for i := 1; i <= 5; i++ {
		st, _ := tr.recordProbe(now, warmingTestWall+time.Millisecond, true, bdReadCommandTimeout, closed)
		if st.State != "warming" {
			t.Fatalf("bound-exceeded read %d: state = %q, want warming", i, st.State)
		}
		if st.BoundExceeded != i {
			t.Fatalf("bound-exceeded read %d: BoundExceeded = %d, want %d", i, st.BoundExceeded, i)
		}
		if st.Degraded {
			t.Fatalf("bound-exceeded read %d: the state machine declared the store degraded "+
				"while the breaker was closed; degraded is the transport breaker's verdict only", i)
		}
	}
}

// TestATickBoundTimeoutIsBoundExceeded pins why the caller passes the bound
// that applied to the read. A tick-context read is reaped at L1's bound, well
// below the wall; judged against the wall alone it would look like a fast
// failure and the store would never read as warming during the very episode
// L1 exists to bound.
func TestATickBoundTimeoutIsBoundExceeded(t *testing.T) {
	useTestWall(t)
	tr := storeWarmingTrackerFor("vcny-tickbound")
	tickBound := warmingTestWall / 4

	st, _ := tr.recordProbe(time.Now(), tickBound, true, tickBound, closed)
	if st.BoundExceeded != 1 {
		t.Fatalf("a read failed at the tick bound (%s, below the %s wall): BoundExceeded = %d, want 1",
			tickBound, warmingTestWall, st.BoundExceeded)
	}
}

// TestDegradedFollowsTheBreakerVerdict is the replacement for a second
// breaker: the published degraded flag and deadline are exactly the transport
// breaker's, a degraded store is always warming, and it cannot leave warming
// while the breaker still holds it — however fast its last reads were.
func TestDegradedFollowsTheBreakerVerdict(t *testing.T) {
	useTestWall(t)
	tr := storeWarmingTrackerFor("vcny-verdict")
	now := time.Now()
	tr.recordProbe(now, time.Millisecond, false, bdReadCommandTimeout, closed)
	tr.recordProbe(now, time.Millisecond, false, bdReadCommandTimeout, closed)

	until := now.Add(time.Minute)
	st, changed := tr.observeAvailability(now, open(until))
	if !changed || !st.Degraded || st.State != "warming" {
		t.Fatalf("breaker opened: changed=%v degraded=%v state=%q, want an announced degraded warming store",
			changed, st.Degraded, st.State)
	}
	if !st.DegradedUntil.Equal(until) {
		t.Fatalf("DegradedUntil = %v, want the breaker's own deadline %v", st.DegradedUntil, until)
	}

	for i := 0; i < 3; i++ {
		if st, _ = tr.recordProbe(now, time.Millisecond, false, bdReadCommandTimeout, open(until)); st.State != "warming" {
			t.Fatalf("fast probe %d while the breaker is open: state = %q, want warming", i+1, st.State)
		}
	}

	st, changed = tr.observeAvailability(now, closed)
	if !changed || st.Degraded || !st.DegradedUntil.IsZero() {
		t.Fatalf("breaker closed: changed=%v degraded=%v until=%v, want an announced recovery with no deadline",
			changed, st.Degraded, st.DegradedUntil)
	}
	if st.State != "warming" {
		t.Fatalf("breaker closed: state = %q, want warming until two good probes", st.State)
	}
	tr.recordProbe(now, time.Millisecond, false, bdReadCommandTimeout, closed)
	if st, _ = tr.recordProbe(now, time.Millisecond, false, bdReadCommandTimeout, closed); st.State != "healthy" {
		t.Fatalf("two good probes after recovery: state = %q, want healthy", st.State)
	}
}

// TestSlowButAnsweringStoreIsWarmingNeverDegraded pins that slow is not
// degraded. A store that answers over the wall is doing real work and
// returning real data; nothing in this state machine may mark it degraded.
func TestSlowButAnsweringStoreIsWarmingNeverDegraded(t *testing.T) {
	useTestWall(t)
	tr := storeWarmingTrackerFor("vcny-slow")
	now := time.Now()

	for i := 0; i < 5; i++ {
		st, _ := tr.recordProbe(now, warmingTestWall*2, false, bdReadCommandTimeout, closed)
		if st.Degraded {
			t.Fatalf("probe %d: a SUCCEEDING slow read marked the store degraded", i+1)
		}
		if st.State != "warming" {
			t.Fatalf("probe %d: state = %q, want warming", i+1, st.State)
		}
	}
}

// TestFailFastIsNotWarmingEvidence keeps this mechanism off another
// mechanism's territory. A read that fails immediately is a broken store or a
// bad query, not a slow one; reporting it as warming would make the record
// explain a stale order set that warming had nothing to do with, which is the
// false signal the pack-side consumer must not be handed.
func TestFailFastIsNotWarmingEvidence(t *testing.T) {
	useTestWall(t)
	tr := storeWarmingTrackerFor("vcny-fast-fail")
	now := time.Now()
	tr.recordProbe(now, time.Millisecond, false, bdReadCommandTimeout, closed)
	tr.recordProbe(now, time.Millisecond, false, bdReadCommandTimeout, closed)

	for i := 0; i < 5; i++ {
		st, _ := tr.recordProbe(now, time.Millisecond, true, bdReadCommandTimeout, closed)
		if st.State != "healthy" || st.BoundExceeded != 0 {
			t.Fatalf("fast failure %d: state=%q bound_exceeded=%d, want healthy and 0",
				i+1, st.State, st.BoundExceeded)
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

	if st := tr.snapshot(); st.State != "warming" {
		t.Fatalf("a freshly tracked store starts %q; want warming — a store this "+
			"process has never read has not been SHOWN healthy", st.State)
	}

	st, _ := tr.recordProbe(now, time.Millisecond, false, bdReadCommandTimeout, closed)
	if st.State != "warming" {
		t.Fatalf("after 1 good probe: state = %q, want warming still", st.State)
	}
	st, changed := tr.recordProbe(now, time.Millisecond, false, bdReadCommandTimeout, closed)
	if st.State != "healthy" {
		t.Fatalf("after 2 consecutive good probes: state = %q, want healthy", st.State)
	}
	if !changed {
		t.Fatal("the healthy transition was not announced as a change")
	}

	// A single slow probe re-enters warming and resets the run, so the exit
	// needs two fresh good probes rather than one.
	if st, _ = tr.recordProbe(now, warmingTestWall*2, false, bdReadCommandTimeout, closed); st.State != "warming" {
		t.Fatalf("after a slow probe: state = %q, want warming", st.State)
	}
	if st, _ = tr.recordProbe(now, time.Millisecond, false, bdReadCommandTimeout, closed); st.State != "warming" {
		t.Fatalf("one good probe after re-entry: state = %q, want warming (run was reset)", st.State)
	}
	if st, _ = tr.recordProbe(now, time.Millisecond, false, bdReadCommandTimeout, closed); st.State != "healthy" {
		t.Fatalf("two good probes after re-entry: state = %q, want healthy", st.State)
	}
}

// TestOneStoresBreakerDoesNotDegradeAnother is the ISOLATION property in its
// most direct form: the 2026-09-06 failure was one slow store stalling order
// dispatch fleet-wide. Each store's record carries its own scope's verdict.
func TestOneStoresBreakerDoesNotDegradeAnother(t *testing.T) {
	useTestWall(t)
	now := time.Now()
	sick := storeWarmingTrackerFor("vcny-sick")
	well := storeWarmingTrackerFor("vcny-well")

	sick.observeAvailability(now, open(now.Add(time.Minute)))
	well.recordProbe(now, time.Millisecond, false, bdReadCommandTimeout, closed)

	byStore := map[string]StoreWarmingState{}
	for _, st := range StoreWarmingStates() {
		byStore[st.Store] = st
	}
	if !byStore["vcny-sick"].Degraded {
		t.Fatal("the sick store's record does not carry its breaker's verdict")
	}
	if byStore["vcny-well"].Degraded {
		t.Fatal("a healthy store's record was degraded by another store's breaker")
	}
}

// TestStoreWarmingStatesAreSortedAndPerStore pins the shape the durable
// record and its pack-side consumer depend on: one entry per store, stable
// order across passes.
func TestStoreWarmingStatesAreSortedAndPerStore(t *testing.T) {
	useTestWall(t)
	now := time.Now()
	for _, name := range []string{"vcny-c", "vcny-a", "vcny-b"} {
		storeWarmingTrackerFor(name).recordProbe(now, time.Millisecond, false, bdReadCommandTimeout, closed)
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
	tr.recordProbe(now, time.Millisecond, false, bdReadCommandTimeout, closed)
	tr.recordProbe(now, time.Millisecond, false, bdReadCommandTimeout, closed)
	if st := tr.snapshot(); st.State != "healthy" {
		t.Fatalf("precondition: state = %q, want healthy", st.State)
	}

	var published []StoreWarmingState
	SetStoreWarmingStateSink(func(st StoreWarmingState) { published = append(published, st) })
	t.Cleanup(func() { SetStoreWarmingStateSink(nil) })

	MarkStoreWarmingByPrefix("vcny-restart")
	if st := tr.snapshot(); st.State != "warming" {
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
	now := time.Now()
	tr := storeWarmingTrackerFor("vcny-transitions")
	tr.recordProbe(now, time.Millisecond, false, bdReadCommandTimeout, closed)
	st, changed := tr.recordProbe(now, time.Millisecond, false, bdReadCommandTimeout, closed)
	if !changed || st.State != "healthy" {
		t.Fatalf("want an announced healthy transition, got changed=%v state=%q", changed, st.State)
	}
	// Three more good probes, and a steady-closed breaker, change nothing.
	for i := 0; i < 3; i++ {
		if _, changed := tr.recordProbe(now, time.Millisecond, false, bdReadCommandTimeout, closed); changed {
			t.Fatalf("steady-state probe %d was announced as a transition", i+1)
		}
		if _, changed := tr.observeAvailability(now, closed); changed {
			t.Fatalf("steady closed breaker observation %d was announced as a transition", i+1)
		}
	}
}

// slowFailRunner is a bd runner whose list calls fail the way a read reaped
// at its deadline fails, which is what the 2026-09-06 incident's reads did.
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

func (r *slowFailRunner) listCount() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.lists
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

// breakerGate is an AvailabilityGate whose verdict the test sets, standing in
// for the scope's *resilience.Breaker that cmd/gc wires into every cache —
// including its OpenUntil deadline. That the real breaker trips on the reads
// L1 bounds is pinned end to end in cmd/gc
// (TestTickBoundReadTimeoutCountsAgainstTheScopeBreaker).
type breakerGate struct {
	mu        sync.Mutex
	available bool
	probeDue  bool
	until     time.Time
}

func newBreakerGate() *breakerGate { return &breakerGate{available: true} }

func (g *breakerGate) Available() bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.available
}

func (g *breakerGate) ProbeDue() bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.probeDue
}

func (g *breakerGate) OpenUntil() time.Time {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.available {
		return time.Time{}
	}
	return g.until
}

func (g *breakerGate) trip(until time.Time) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.available, g.probeDue, g.until = false, false, until
}

func (g *breakerGate) probeWindow() {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.probeDue = true
}

func (g *breakerGate) close() {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.available, g.probeDue, g.until = true, false, time.Time{}
}

// TestDegradedStoreIsSkippedAndAnnouncedByTheReconciler is AC1's reconcile
// half and AC2's announcement half, on the real code path: once the scope's
// transport breaker is open, the reconciler stops paying for the store (the
// backing is not called again), the skip is announced once, and the durable
// record says degraded with the breaker's own deadline.
func TestDegradedStoreIsSkippedAndAnnouncedByTheReconciler(t *testing.T) {
	useTestWall(t)
	logs := captureLog(t)
	var published []StoreWarmingState
	SetStoreWarmingStateSink(func(st StoreWarmingState) { published = append(published, st) })
	t.Cleanup(func() { SetStoreWarmingStateSink(nil) })

	runner := &slowFailRunner{}
	runner.set(true)
	stateElapsed(t, warmingTestWall+time.Millisecond)
	gate := newBreakerGate()
	cache := newCachingStore(NewBdStore("/city", runner.run), "vcnyskip", nil)
	cache.SetAvailabilityGate(gate)

	cache.runReconciliation()
	until := time.Now().Add(time.Hour)
	gate.trip(until)
	callsAtTrip := runner.listCount()

	for i := 0; i < 5; i++ {
		cache.runReconciliation()
	}
	if got := runner.listCount(); got != callsAtTrip {
		t.Fatalf("the backing was called %d more time(s) while the breaker was open; "+
			"a degraded store must cost the reconciler nothing until a probe is due",
			got-callsAtTrip)
	}
	if got := strings.Count(logs.String(), "circuit breaker open"); got != 1 {
		t.Fatalf("the skip was announced %d time(s), want exactly 1 per episode — "+
			"silence is the vp-cblo shape, one line per cycle is a flood.\nlog:\n%s", got, logs.String())
	}
	st := cache.WarmingState()
	if !st.Degraded || st.State != "warming" || !st.DegradedUntil.Equal(until) {
		t.Fatalf("record while skipped = %+v, want degraded warming until the breaker's %v", st, until)
	}
	if len(published) == 0 || !published[len(published)-1].Degraded {
		t.Fatalf("the degraded transition was not published to the durable record: %+v", published)
	}
}

// TestReconcileRecoversWhenTheBreakerAdmitsAProbe pins the re-entry rule:
// when the breaker's probe is due the reconcile runs and IS the probe, and
// once the breaker closes the record says so without operator action.
func TestReconcileRecoversWhenTheBreakerAdmitsAProbe(t *testing.T) {
	useTestWall(t)

	runner := &slowFailRunner{}
	runner.set(true)
	stateElapsed(t, warmingTestWall+time.Millisecond)
	gate := newBreakerGate()
	cache := newCachingStore(NewBdStore("/city", runner.run), "vcnyheal", nil)
	cache.SetAvailabilityGate(gate)
	cache.runReconciliation()
	gate.trip(time.Now().Add(time.Hour))
	cache.runReconciliation()
	if !cache.WarmingState().Degraded {
		t.Fatal("precondition: store did not read as degraded")
	}

	runner.set(false)
	stateElapsed(t, time.Millisecond)
	gate.probeWindow()
	callsBefore := runner.listCount()
	cache.runReconciliation()
	if runner.listCount() == callsBefore {
		t.Fatal("the reconciler did not run the probe the breaker admitted; " +
			"this store would stay degraded forever")
	}

	gate.close()
	cache.runReconciliation()
	if st := cache.WarmingState(); st.Degraded {
		t.Fatalf("the breaker closed but the record still says degraded: %+v", st)
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
// L2: nothing is published when the layer is off, the breaker's verdict
// included.
func TestWarmingStateKillSwitchSuppressesTheDurableRecord(t *testing.T) {
	useTestWall(t)
	t.Setenv(storeWarmingStateEnv, "0")

	var published []StoreWarmingState
	SetStoreWarmingStateSink(func(st StoreWarmingState) { published = append(published, st) })
	t.Cleanup(func() { SetStoreWarmingStateSink(nil) })

	runner := &slowFailRunner{}
	stateElapsed(t, time.Millisecond)
	gate := newBreakerGate()
	cache := newCachingStore(NewBdStore("/city", runner.run), "vcnyoff2", nil)
	cache.SetAvailabilityGate(gate)
	cache.runReconciliation()
	cache.runReconciliation()
	gate.trip(time.Now().Add(time.Hour))
	cache.runReconciliation()
	MarkStoreWarmingByPrefix("vcnyoff2")

	if len(published) != 0 {
		t.Fatalf("GC_STORE_WARMING_STATE=0 published %d record(s), want 0: %+v", len(published), published)
	}
}

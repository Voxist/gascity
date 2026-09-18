package beads

import (
	"fmt"
	"os"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// L2 of the vc-ny00 store-warming plan: the warming state machine.
//
// THE PRINCIPLE. read_timeout_millis=30000 is steady-state truth and this
// file does not touch it. A store that cannot answer inside the wall is
// DEGRADED — announced, bounded, and isolated to that store — and is never
// a reason for the supervisor to stop. The 2026-09-06 incident (vc-5gui)
// was the opposite of all three: unannounced (15 minutes of log silence),
// unbounded (reads bounded at 4x the wall inside a serial tick), and
// un-isolated (one slow store stalled order dispatch fleet-wide).
//
// THE PROBE IS THE RECONCILE. CachingStore already times its full-scan
// List on every reconcile cycle (the `took=` field on the once-a-minute
// heartbeat). No new query path is needed to MEASURE store health: a
// reconcile that beats the wall is a passing probe, one that exceeds it is
// a failing probe. L3's warming pass supplies the heavier representative
// read, but the state machine here runs continuously off the reconcile and
// therefore also catches the class the incident actually showed — a cliff
// that began +29 minutes after start, long after any boot-time gate had
// finished.
//
// STATES. healthy | warming, per store.
//
//	enter warming  <- construction (a fresh cache has never probed the
//	                  store, which is exactly the managed-start case), a
//	                  bound-exceeded read, any probe >= the wall, or the
//	                  scope's transport breaker reporting the store
//	                  unavailable.
//	exit  warming  <- two CONSECUTIVE sub-wall probes while the breaker is
//	                  closed. Two, not one, so a store oscillating at the
//	                  boundary does not flap the announced state and the
//	                  pack-side consumer's "fresh warming record" test
//	                  stays meaningful.
//
// DEGRADED IS NOT DECIDED HERE. Whether a store is skipped is the scope's
// transport circuit breaker (internal/resilience, configured by
// [beads.resilience]), reached through the cache's AvailabilityGate. That
// breaker already counts a bd call killed at its per-command deadline as a
// transport failure — including a tick-context read killed at L1's bound —
// and already makes the reconcile skip a scope it holds open while the cache
// serves stale-marked data. This file only OBSERVES the gate, so the
// published record says what the one breaker decided rather than what a
// second opinion would have.
//
// WHY A PROCESS-GLOBAL REGISTRY. The durable record (cmd/gc) is ONE object
// covering every store, rewritten on any store's transition, so a
// transition on store A must be able to render store B's current state.
// Keying trackers by normalized id prefix is what makes every store's
// record reachable from one place.

const (
	// defaultStoreWarmingWallMillis mirrors the operator-settled
	// read_timeout_millis. It is the threshold a probe is measured
	// against, NOT a value this code ever writes into a server config:
	// nothing here renders dolt configuration. Kept as its own knob so a
	// city that settles on a different wall can align the state machine
	// without a rebuild.
	defaultStoreWarmingWallMillis = 30000
	// storeWarmingHealthyRunToExit is how many consecutive sub-wall
	// probes return a store to healthy. See the STATES note above.
	storeWarmingHealthyRunToExit = 2

	storeWarmingWallEnv = "GC_STORE_WARMING_WALL_MS"
	// storeWarmingStateEnv is L2's kill switch. Off => the heartbeat line
	// reverts to its pre-plan shape and no state is published. Whether a
	// store is SKIPPED is the transport breaker's call and is governed by
	// [beads.resilience], not by this switch.
	storeWarmingStateEnv = "GC_STORE_WARMING_STATE"

	// storeWarmingNoPrefix is the display/key form for a store with no id
	// prefix, matching the heartbeat line's own rendering of rig=.
	storeWarmingNoPrefix = "(no-prefix)"
)

// StoreWarmingState is one store's published warming/degraded record.
//
// THIS IS AN ACCEPTED INTERFACE. The pack-side consumer (order-liveness
// probe and sentinel) reads the aggregate of these records out of
// PackStateDir to decide whether a stale order set is explained by a
// warming store — degraded-info — or is a genuine P1 page. Field names are
// therefore part of the contract: add fields, never rename or repurpose
// them. Explicit json tags rather than Go's defaults, because a consumer
// outside this repo parses them: the wire name must not move when a field
// is renamed in Go.
type StoreWarmingState struct {
	// Store is the cache's id prefix ("vc", "vp", ...); "(no-prefix)" for
	// a store without one, matching the heartbeat line's own rendering.
	Store string `json:"store"`
	// State is "warming" or "healthy".
	State string `json:"state"`
	// Degraded reports whether the scope's transport circuit breaker
	// ([beads.resilience]) is not closed, as last observed by this store's
	// cache — the store is being skipped and its consumers are reading a
	// stale-marked cache. A store can be warming without being degraded
	// (slow but answering); it cannot be degraded without being warming.
	Degraded bool `json:"degraded"`
	// ProbeMs is the most recent probe's duration in milliseconds.
	ProbeMs int64 `json:"probe_ms"`
	// BoundExceeded is the current run of consecutive bound-exceeded
	// reconcile reads; it resets to zero on any read that was not one. It
	// is a measurement, not a trip count: the breaker keeps its own.
	BoundExceeded int `json:"bound_exceeded"`
	// WallMs is the threshold this store's probes were judged against, so
	// a consumer can interpret ProbeMs without knowing the city's config.
	WallMs int64 `json:"wall_ms"`
	// SinceAt dates the CURRENT state — when the store last entered
	// warming (or last returned to healthy). This is what makes dwell
	// measurable, which is what the vc-5gui RCA lacked.
	SinceAt time.Time `json:"since_at"`
	// LastProbeAt is when ProbeMs was measured. A consumer deciding
	// whether a warming record is FRESH enough to explain a stale order
	// set must judge on this, not on the file's mtime.
	LastProbeAt time.Time `json:"last_probe_at"`
	// DegradedUntil is the open breaker's probe-admission deadline; zero
	// when the breaker is closed or half-open.
	DegradedUntil time.Time `json:"degraded_until,omitempty"`
}

// storeWarmingNowFn is the seam over the clock that dates state
// transitions (since_at, last_probe_at), matching the deliveryWindowNowFn
// convention used by the delivery window. It never governs the probe
// measurement: how long a store's read actually took is real elapsed time,
// measured by the reconcile.
var storeWarmingNowFn = time.Now

func storeWarmingNow() time.Time { return storeWarmingNowFn() }

// storeWarmingProbeElapsedFn is the seam over the latency a probe reports.
// Production is the identity function, so this is behaviorally invisible
// outside tests.
//
// It exists because the alternative is worse: a test that needs a read to
// exceed the wall would otherwise have to SLEEP past it, which is slow, flaky
// on a loaded host, and — per the resource census's own standing invariant —
// a fixed-sleep call site that may not grow. Stating the observed latency is
// both more honest about what is under test (the state machine's reaction to
// a duration) and deterministic.
var storeWarmingProbeElapsedFn = func(actual time.Duration) time.Duration { return actual }

// storeWarmingSink receives every state TRANSITION (not every probe) so
// the durable record is rewritten when something changed and left alone
// otherwise. cmd/gc registers the PackStateDir writer at boot; in a process
// that never registers one — every CLI invocation — the state machine still
// runs and still announces on the log line, it simply has nowhere durable
// to publish, which is the pre-plan behavior for those processes anyway.
var storeWarmingSink atomic.Pointer[func(StoreWarmingState)]

// SetStoreWarmingStateSink registers the durable publisher for warming
// state transitions. Passing nil clears it.
func SetStoreWarmingStateSink(fn func(StoreWarmingState)) {
	if fn == nil {
		storeWarmingSink.Store(nil)
		return
	}
	storeWarmingSink.Store(&fn)
}

func publishStoreWarmingState(st StoreWarmingState) {
	if p := storeWarmingSink.Load(); p != nil {
		(*p)(st)
	}
}

// storeWarmingEnabled is L2's kill switch (AC7). Default on: an
// unannounced degradation is the vp-cblo shape this layer exists to
// prevent, so silence must be something an operator chose explicitly.
func storeWarmingEnabled() bool {
	switch strings.TrimSpace(os.Getenv(storeWarmingStateEnv)) {
	case "0", "false", "off":
		return false
	}
	return true
}

func envPositiveInt(key string, def int) int {
	raw := strings.TrimSpace(os.Getenv(key))
	if raw == "" {
		return def
	}
	v, err := strconv.Atoi(raw)
	if err != nil || v <= 0 {
		return def
	}
	return v
}

func storeWarmingWall() time.Duration {
	return time.Duration(envPositiveInt(storeWarmingWallEnv, defaultStoreWarmingWallMillis)) * time.Millisecond
}

// StoreWarmingWall is the threshold a store's probe is judged against — the
// operator-settled listener read_timeout_millis, mirrored here. Exported for
// L3's warming pass (cmd/gc), which must decide "is this store warm yet"
// against the SAME number the reconcile state machine uses, rather than
// keeping a second copy of the knob that could drift from this one.
//
// This is a threshold to COMPARE against, never a value written into a
// server config: nothing in this package renders dolt configuration.
func StoreWarmingWall() time.Duration {
	return storeWarmingWall()
}

// storeWarmingTracker is one store's state machine. It carries its own
// mutex rather than riding CachingStore.mu: the durable record is rendered
// from every tracker whenever any one of them transitions, and that render
// must not queue behind another store's cache work.
type storeWarmingTracker struct {
	mu            sync.Mutex
	store         string
	warming       bool
	since         time.Time
	healthyRun    int
	boundExceeded int
	// degraded and degradedUntil are the transport breaker's verdict as
	// last observed through the cache's AvailabilityGate — never decided
	// here.
	degraded      bool
	degradedUntil time.Time
	probeMs       int64
	lastProbeAt   time.Time
	wallMs        int64
	// announced records whether this store's record has ever been
	// published. A never-published tracker always needs its first write:
	// a fresh tracker is created ALREADY warming, so "did the state
	// change" is false on the managed-start edge and the durable record
	// would stay empty during exactly the window it exists to explain.
	announced bool
}

func newStoreWarmingTracker(store string, now time.Time) *storeWarmingTracker {
	if strings.TrimSpace(store) == "" {
		store = storeWarmingNoPrefix
	}
	// Construction enters warming: a cache that has not yet completed a
	// sub-wall probe has not been SHOWN to be healthy, and on a managed
	// start it demonstrably is not. Announcing warming and then exiting
	// after two good probes is the honest ordering; assuming healthy and
	// waiting for a failure would publish "healthy" for a store the
	// process has never successfully read.
	return &storeWarmingTracker{store: store, warming: true, since: now}
}

// storeWarmingRegistry keys one tracker per logical store so the durable
// record can render every store on any one store's transition. See the WHY
// A PROCESS-GLOBAL REGISTRY note at the top of this file.
var storeWarmingRegistry = struct {
	mu       sync.Mutex
	trackers map[string]*storeWarmingTracker
}{trackers: map[string]*storeWarmingTracker{}}

// storeWarmingKey normalizes a store id prefix into a registry key.
func storeWarmingKey(prefix string) string {
	prefix = normalizeIDPrefix(prefix)
	if prefix == "" {
		return storeWarmingNoPrefix
	}
	return prefix
}

// storeWarmingTrackerFor returns the shared tracker for a store prefix,
// creating it (in the warming state) on first sight.
func storeWarmingTrackerFor(prefix string) *storeWarmingTracker {
	key := storeWarmingKey(prefix)
	storeWarmingRegistry.mu.Lock()
	defer storeWarmingRegistry.mu.Unlock()
	if t, ok := storeWarmingRegistry.trackers[key]; ok {
		return t
	}
	t := newStoreWarmingTracker(key, storeWarmingNow())
	storeWarmingRegistry.trackers[key] = t
	return t
}

// StoreWarmingStates returns a snapshot of every tracked store's record,
// sorted by store id so the durable file and any log rendering are stable
// across passes.
func StoreWarmingStates() []StoreWarmingState {
	storeWarmingRegistry.mu.Lock()
	trackers := make([]*storeWarmingTracker, 0, len(storeWarmingRegistry.trackers))
	for _, t := range storeWarmingRegistry.trackers {
		trackers = append(trackers, t)
	}
	storeWarmingRegistry.mu.Unlock()

	out := make([]StoreWarmingState, 0, len(trackers))
	for _, t := range trackers {
		out = append(out, t.snapshot())
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Store < out[j].Store })
	return out
}

// MarkStoreWarmingByPrefix forces a store into the warming state and
// publishes the transition. This is the "enter on managed start" edge: the
// provider restarts the server underneath caches that are already live, and
// no reconcile probe can observe that in advance.
func MarkStoreWarmingByPrefix(prefix string) {
	if st, changed := storeWarmingTrackerFor(prefix).markWarming(storeWarmingNow()); changed && storeWarmingEnabled() {
		publishStoreWarmingState(st)
	}
}

// resetStoreWarmingRegistryForTest drops every tracked store. Tests call it
// so one test's degraded store is not another's starting condition.
func resetStoreWarmingRegistryForTest() {
	storeWarmingRegistry.mu.Lock()
	defer storeWarmingRegistry.mu.Unlock()
	storeWarmingRegistry.trackers = map[string]*storeWarmingTracker{}
}

// storeAvailability is the transport breaker's verdict for one store, as
// read through its cache's AvailabilityGate at one instant.
type storeAvailability struct {
	degraded bool
	until    time.Time
}

// recordProbe folds one completed reconcile read into the state machine.
// elapsed is the read's wall time; failed says whether it errored.
//
// A read is BOUND-EXCEEDED when it failed at or beyond the deadline that
// applied to it — the shape of a query reaped by the client bound or the
// listener's read_timeout_millis. That enters warming. A read that failed
// FAST is a different defect (a broken store, a bad query, a refused
// connection): it is not warming evidence, and not credited as a healthy
// probe either.
//
// A read that SUCCEEDED but took at least the wall is slow-but-answering:
// warming. Whether ANY of these reads makes the store degraded is the
// transport breaker's decision, passed in as avail.
//
// Returns the resulting snapshot and whether the announced state changed.
func (t *storeWarmingTracker) recordProbe(now time.Time, elapsed time.Duration, failed bool, bound time.Duration, avail storeAvailability) (StoreWarmingState, bool) {
	wall := storeWarmingWall()
	t.mu.Lock()
	defer t.mu.Unlock()

	t.probeMs = elapsed.Milliseconds()
	t.lastProbeAt = now
	t.wallMs = wall.Milliseconds()

	// A read is bound-exceeded when it failed at or beyond the deadline
	// that actually applied to it — whichever of the client bound and the
	// server wall would reap it first.
	//
	// Taking the MINIMUM is what makes this correct inside a tick. The
	// tick-context client bound is deliberately BELOW the wall (L1), so a
	// tick-context read that times out fails at ~10s, well short of the
	// 30s wall; judging it against the wall alone would classify a genuine
	// timeout as a "fast failure" and the store would never read as
	// warming for the very reads L1 exists to bound. The trigger is process-global and
	// best-effort, so which bound applied is only knowable at READ time —
	// hence the caller passes it rather than this function re-deriving it
	// and racing the tick frame.
	threshold := wall
	if bound > 0 && bound < threshold {
		threshold = bound
	}
	atOrOverWall := elapsed >= wall
	boundExceeded := failed && elapsed >= threshold

	wasWarming, wasDegraded := t.warming, t.degraded

	switch {
	case boundExceeded:
		t.boundExceeded++
		t.enterWarmingLocked(now)
	case atOrOverWall:
		// Answered, but not inside the wall. Warming, not degraded.
		t.boundExceeded = 0
		t.healthyRun = 0
	case failed:
		// Failed fast — not warming evidence. Do not credit it as a
		// healthy probe either; leave the run where it is.
		t.boundExceeded = 0
	default:
		t.boundExceeded = 0
		t.healthyRun++
	}

	if atOrOverWall {
		t.enterWarmingLocked(now)
	}
	t.applyAvailabilityLocked(now, avail)
	if !t.degraded && t.warming && t.healthyRun >= storeWarmingHealthyRunToExit {
		t.warming = false
		t.since = now
	}
	return t.snapshotLocked(), t.noteChangeLocked(wasWarming, wasDegraded)
}

// observeAvailability folds the transport breaker's current verdict into the
// state machine without a probe. The reconcile calls it on every cycle —
// including the cycles the breaker makes it skip, which are the only cycles
// of an outage that run no probe at all — so the record flips to degraded
// when the breaker opens and back when it closes.
func (t *storeWarmingTracker) observeAvailability(now time.Time, avail storeAvailability) (StoreWarmingState, bool) {
	if t == nil {
		return StoreWarmingState{}, false
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	wasWarming, wasDegraded := t.warming, t.degraded
	t.applyAvailabilityLocked(now, avail)
	return t.snapshotLocked(), t.noteChangeLocked(wasWarming, wasDegraded)
}

// applyAvailabilityLocked records the breaker's verdict. A degraded store is
// warming by definition — a store the breaker will not let anyone read has
// not been shown healthy — so degradation also resets the exit run.
func (t *storeWarmingTracker) applyAvailabilityLocked(now time.Time, avail storeAvailability) {
	t.degraded = avail.degraded
	t.degradedUntil = time.Time{}
	if avail.degraded {
		t.degradedUntil = avail.until
		t.enterWarmingLocked(now)
	}
}

// noteChangeLocked reports whether the announced state moved, counting a
// never-published tracker as changed (see announced).
func (t *storeWarmingTracker) noteChangeLocked(wasWarming, wasDegraded bool) bool {
	changed := wasWarming != t.warming || wasDegraded != t.degraded || !t.announced
	if changed {
		t.announced = true
	}
	return changed
}

// markWarming forces the store into the warming state — the "enter on
// managed start" edge, for a cache that already exists when the provider
// restarts underneath it.
func (t *storeWarmingTracker) markWarming(now time.Time) (StoreWarmingState, bool) {
	if t == nil {
		return StoreWarmingState{}, false
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	wasWarming, wasDegraded := t.warming, t.degraded
	t.enterWarmingLocked(now)
	return t.snapshotLocked(), t.noteChangeLocked(wasWarming, wasDegraded)
}

func (t *storeWarmingTracker) enterWarmingLocked(now time.Time) {
	t.healthyRun = 0
	if !t.warming {
		t.warming = true
		t.since = now
	}
}

func (t *storeWarmingTracker) snapshot() StoreWarmingState {
	if t == nil {
		return StoreWarmingState{}
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.snapshotLocked()
}

func (t *storeWarmingTracker) snapshotLocked() StoreWarmingState {
	state := "healthy"
	if t.warming {
		state = "warming"
	}
	wall := t.wallMs
	if wall == 0 {
		wall = storeWarmingWall().Milliseconds()
	}
	return StoreWarmingState{
		Store:         t.store,
		State:         state,
		Degraded:      t.degraded,
		ProbeMs:       t.probeMs,
		BoundExceeded: t.boundExceeded,
		WallMs:        wall,
		SinceAt:       t.since,
		LastProbeAt:   t.lastProbeAt,
		DegradedUntil: t.degradedUntil,
	}
}

// heartbeatFieldLocked renders the L2 gauge that rides the once-a-minute
// reconcile heartbeat: `store=warming probe_ms=123` (plus `degraded=true`
// while the transport breaker holds the store unavailable). It rides that
// line rather than a new endpoint for the PR #166 deps= reason — the trigger
// condition is a DWELL, a dwell needs a series, and this is the line
// operators already grep.
func (t *storeWarmingTracker) heartbeatField() string {
	if t == nil {
		return ""
	}
	st := t.snapshot()
	field := fmt.Sprintf("store=%s probe_ms=%d", st.State, st.ProbeMs)
	if st.Degraded {
		field += " degraded=true"
	}
	return field
}

// WarmingState returns this cache's current published warming record.
func (c *CachingStore) WarmingState() StoreWarmingState {
	if c == nil {
		return StoreWarmingState{}
	}
	return c.warm.snapshot()
}

// MarkStoreWarming forces this cache into the warming state. The provider
// calls it when the managed server restarts underneath a live cache, which
// is the one warming edge the reconcile probe cannot observe in advance.
func (c *CachingStore) MarkStoreWarming() {
	if c == nil {
		return
	}
	if st, changed := c.warm.markWarming(storeWarmingNow()); changed && storeWarmingEnabled() {
		publishStoreWarmingState(st)
	}
}

// recordStoreProbe folds one reconcile read into this cache's state machine
// and publishes the transition when the announced state moved. It is the
// single call site the reconcile paths (success and failure) share. The
// breaker verdict is read AFTER the probe, because the probe's own bd call is
// what the breaker just counted.
func (c *CachingStore) recordStoreProbe(now time.Time, elapsed time.Duration, failed bool, bound time.Duration) {
	if c == nil || c.warm == nil {
		return
	}
	st, changed := c.warm.recordProbe(now, storeWarmingProbeElapsedFn(elapsed), failed, bound, c.storeAvailability())
	if changed && storeWarmingEnabled() {
		publishStoreWarmingState(st)
	}
}

// observeStoreAvailability folds the transport breaker's current verdict into
// this cache's state machine and publishes the transition, if any.
func (c *CachingStore) observeStoreAvailability() {
	if c == nil || c.warm == nil {
		return
	}
	st, changed := c.warm.observeAvailability(storeWarmingNow(), c.storeAvailability())
	if changed && storeWarmingEnabled() {
		publishStoreWarmingState(st)
	}
}

// storeAvailability reads the transport breaker's verdict through the
// cache's AvailabilityGate. No gate means no breaker, which is never
// degraded. The open deadline is published when the gate exposes one (the
// production *resilience.Breaker does).
func (c *CachingStore) storeAvailability() storeAvailability {
	g := c.availabilityGateRef()
	if g == nil || g.Available() {
		return storeAvailability{}
	}
	avail := storeAvailability{degraded: true}
	if d, ok := g.(interface{ OpenUntil() time.Time }); ok {
		avail.until = d.OpenUntil()
	}
	return avail
}

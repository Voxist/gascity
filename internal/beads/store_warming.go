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

// L1 (per-store breaker) and L2 (warming state machine) of the vc-ny00
// store-warming plan.
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
//	                  store, which is exactly the managed-start case), the
//	                  first bound-exceeded read, or any probe >= the wall.
//	exit  warming  <- two CONSECUTIVE sub-wall probes. Two, not one, so a
//	                  store oscillating at the boundary does not flap the
//	                  announced state and the pack-side consumer's "fresh
//	                  warming record" test stays meaningful.
//
// THE BREAKER. N consecutive bound-exceeded reads (default 2) open a
// per-store breaker for a cooldown (default 60s). While it is open the
// store's reconcile is skipped outright: the cached snapshot serves,
// stale-marked, and the skip is announced. Re-entry is by probe — the
// first reconcile after the cooldown expires IS the probe — never by
// retry-till-it-hurts.
//
// WHY A PROCESS-GLOBAL REGISTRY. The breaker has two readers that hold
// DIFFERENT Go objects for the same logical store: the reconciler's
// long-lived CachingStore, and the order-tracking sweep, which opens a
// fresh beads.Store per scope on every watchdog pass
// (city_runtime.orderTrackingSweepStores). A tracker owned by CachingStore
// alone would leave the sweep — the tick-path loop the plan names
// explicitly — serializing behind exactly the store the reconciler has
// already proven cannot answer. Keying trackers by normalized id prefix is
// what makes one store's verdict visible to every holder of that store.

const (
	// defaultStoreWarmingWallMillis mirrors the operator-settled
	// read_timeout_millis. It is the threshold a probe is measured
	// against, NOT a value this code ever writes into a server config:
	// nothing here renders dolt configuration. Kept as its own knob so a
	// city that settles on a different wall can align the state machine
	// without a rebuild.
	defaultStoreWarmingWallMillis = 30000
	// defaultStoreBreakerTrips is N — consecutive bound-exceeded reads
	// before the breaker opens. 2 rather than 1 because a single killed
	// query is also what a one-off write contention spike looks like;
	// two in a row is a store, not a query.
	defaultStoreBreakerTrips = 2
	// defaultStoreBreakerCooldown is how long the store is skipped once
	// the breaker opens. Long enough that a degraded store stops costing
	// the reconciler anything at all, short enough that recovery is
	// noticed within one heartbeat window.
	defaultStoreBreakerCooldown = 60 * time.Second
	// storeWarmingHealthyRunToExit is how many consecutive sub-wall
	// probes return a store to healthy. See the STATES note above.
	storeWarmingHealthyRunToExit = 2

	storeWarmingWallEnv     = "GC_STORE_WARMING_WALL_MS"
	storeBreakerTripsEnv    = "GC_STORE_BREAKER_TRIPS"
	storeBreakerCooldownEnv = "GC_STORE_BREAKER_COOLDOWN_S"
	// storeWarmingStateEnv is L2's kill switch. Off => the heartbeat line
	// reverts to its pre-plan shape and no state is published; the
	// breaker (L1) is governed separately by storeBreakerTripsEnv.
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
	// Degraded reports whether the breaker is currently open — the store
	// is being skipped and its consumers are reading a stale-marked
	// cache. A store can be warming without being degraded (slow but
	// answering); it cannot be degraded without being warming.
	Degraded bool `json:"degraded"`
	// ProbeMs is the most recent probe's duration in milliseconds.
	ProbeMs int64 `json:"probe_ms"`
	// BoundExceeded is the current run of consecutive bound-exceeded
	// reads; it resets to zero on any passing probe.
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
	// DegradedUntil is when the open breaker's cooldown expires; zero
	// when the breaker is closed.
	DegradedUntil time.Time `json:"degraded_until,omitempty"`
}

// storeWarmingNowFn is the seam over the breaker's clock, matching the
// deliveryWindowNowFn convention used by the delivery window. The breaker's
// whole contract is time-based (a 60s cooldown, re-entry by the first probe
// after it expires), and a test that had to SLEEP through that cooldown
// would be both slow and flaky on a loaded host. Production reads the real
// clock; only tests move it.
//
// Note this governs the breaker's DECISIONS, never the probe measurement:
// how long a store's read actually took is real elapsed time, measured by
// the reconcile, and is not something a seam may fake.
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

// storeBreakerTrips returns N, or 0 when the breaker is disabled.
// GC_STORE_BREAKER_TRIPS=0 is L1's breaker kill switch: probes are still
// measured and the state machine still announces, but no store is ever
// skipped, which is byte-identical to pre-plan reconcile scheduling.
func storeBreakerTrips() int {
	raw := strings.TrimSpace(os.Getenv(storeBreakerTripsEnv))
	if raw == "" {
		return defaultStoreBreakerTrips
	}
	v, err := strconv.Atoi(raw)
	if err != nil || v < 0 {
		return defaultStoreBreakerTrips
	}
	return v
}

func storeBreakerCooldown() time.Duration {
	raw := strings.TrimSpace(os.Getenv(storeBreakerCooldownEnv))
	if raw == "" {
		return defaultStoreBreakerCooldown
	}
	secs, err := strconv.Atoi(raw)
	if err != nil || secs <= 0 {
		return defaultStoreBreakerCooldown
	}
	return time.Duration(secs) * time.Second
}

// storeWarmingTracker is one store's state machine. It carries its own
// mutex rather than riding CachingStore.mu: it is written from the
// reconcile goroutine and read from the tick goroutine (the order-tracking
// sweep's skip check), and entangling those reads with the cache's main
// lock would put a tick-path reader behind whatever holds it.
type storeWarmingTracker struct {
	mu            sync.Mutex
	store         string
	warming       bool
	since         time.Time
	healthyRun    int
	boundExceeded int
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

// storeWarmingRegistry keys one tracker per logical store so every holder
// of that store — the reconciler's CachingStore and the order-tracking
// sweep's per-pass store objects — reads and writes the SAME verdict. See
// the WHY A PROCESS-GLOBAL REGISTRY note at the top of this file.
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

// lookupStoreWarmingTracker returns the tracker for a prefix WITHOUT
// creating one. A store nothing has probed yet has no verdict, and
// inventing a warming tracker on a bare lookup would make every
// never-probed store read as warming.
func lookupStoreWarmingTracker(prefix string) *storeWarmingTracker {
	key := storeWarmingKey(prefix)
	storeWarmingRegistry.mu.Lock()
	defer storeWarmingRegistry.mu.Unlock()
	return storeWarmingRegistry.trackers[key]
}

// StoreIsDegraded reports whether the store owning this id prefix has an
// OPEN breaker — it is being skipped and its consumers are reading a
// stale-marked snapshot. Tick-path loops that hold a store object other
// than the reconciler's cache (the order-tracking sweep) call this to skip
// a store rather than serialize behind it.
//
// A store no one has probed is not degraded: absence of evidence is not
// evidence of degradation, and the alternative would skip every store on a
// fresh process.
func StoreIsDegraded(prefix string) bool {
	return lookupStoreWarmingTracker(prefix).breakerOpen(storeWarmingNow())
}

// StoreWarmingStates returns a snapshot of every tracked store's record,
// sorted by store id so the durable file and any log rendering are stable
// across passes.
func StoreWarmingStates() []StoreWarmingState {
	now := storeWarmingNow()
	storeWarmingRegistry.mu.Lock()
	trackers := make([]*storeWarmingTracker, 0, len(storeWarmingRegistry.trackers))
	for _, t := range storeWarmingRegistry.trackers {
		trackers = append(trackers, t)
	}
	storeWarmingRegistry.mu.Unlock()

	out := make([]StoreWarmingState, 0, len(trackers))
	for _, t := range trackers {
		out = append(out, t.snapshot(now))
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

// breakerOpen reports whether this store is currently being skipped.
func (t *storeWarmingTracker) breakerOpen(now time.Time) bool {
	if t == nil {
		return false
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	return !t.degradedUntil.IsZero() && now.Before(t.degradedUntil)
}

// recordProbe folds one completed reconcile read into the state machine.
// elapsed is the read's wall time; failed says whether it errored.
//
// A read is BOUND-EXCEEDED when it failed at or beyond the wall — that is
// the shape of the listener reaping the query at read_timeout_millis. A
// read that failed FAST is a different defect (a broken store, a bad
// query, a refused connection) and must not open a breaker whose whole
// premise is "this store is slow"; it is left to the existing syncFailures
// circuit breaker, which already handles that class.
//
// A read that SUCCEEDED but took at least the wall is slow-but-answering:
// warming, never degraded. The cache it produced is real data, and
// skipping a store that is doing useful work would trade a slow answer for
// no answer.
//
// Returns the resulting snapshot and whether the announced state changed.
func (t *storeWarmingTracker) recordProbe(now time.Time, elapsed time.Duration, failed bool, bound time.Duration) (StoreWarmingState, bool) {
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
	// timeout as a "fast failure" and the breaker would never trip for the
	// very reads L1 exists to bound. The trigger is process-global and
	// best-effort, so which bound applied is only knowable at READ time —
	// hence the caller passes it rather than this function re-deriving it
	// and racing the tick frame.
	threshold := wall
	if bound > 0 && bound < threshold {
		threshold = bound
	}
	atOrOverWall := elapsed >= wall
	boundExceeded := failed && elapsed >= threshold

	wasWarming := t.warming
	wasDegraded := !t.degradedUntil.IsZero() && now.Before(t.degradedUntil)

	switch {
	case boundExceeded:
		t.boundExceeded++
		t.healthyRun = 0
		if trips := storeBreakerTrips(); trips > 0 && t.boundExceeded >= trips {
			t.degradedUntil = now.Add(storeBreakerCooldown())
		}
	case atOrOverWall:
		// Answered, but not inside the wall. Warming, not degraded.
		t.boundExceeded = 0
		t.healthyRun = 0
	case failed:
		// Failed fast — not this mechanism's signal. Do not credit it as
		// a healthy probe either; leave the run where it is.
		t.boundExceeded = 0
	default:
		t.boundExceeded = 0
		t.healthyRun++
		// A passing probe closes an open breaker immediately: the
		// cooldown exists to stop hammering a store that cannot answer,
		// not to keep punishing one that just did.
		t.degradedUntil = time.Time{}
	}

	if atOrOverWall {
		t.enterWarmingLocked(now)
	} else if t.warming && t.healthyRun >= storeWarmingHealthyRunToExit {
		t.warming = false
		t.since = now
	}

	isDegraded := !t.degradedUntil.IsZero() && now.Before(t.degradedUntil)
	changed := wasWarming != t.warming || wasDegraded != isDegraded || !t.announced
	if changed {
		t.announced = true
	}
	return t.snapshotLocked(now), changed
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
	was := t.warming
	t.enterWarmingLocked(now)
	changed := !was || !t.announced
	if changed {
		t.announced = true
	}
	return t.snapshotLocked(now), changed
}

func (t *storeWarmingTracker) enterWarmingLocked(now time.Time) {
	t.healthyRun = 0
	if !t.warming {
		t.warming = true
		t.since = now
	}
}

func (t *storeWarmingTracker) snapshot(now time.Time) StoreWarmingState {
	if t == nil {
		return StoreWarmingState{}
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.snapshotLocked(now)
}

func (t *storeWarmingTracker) snapshotLocked(now time.Time) StoreWarmingState {
	state := "healthy"
	if t.warming {
		state = "warming"
	}
	degraded := !t.degradedUntil.IsZero() && now.Before(t.degradedUntil)
	until := t.degradedUntil
	if !degraded {
		until = time.Time{}
	}
	wall := t.wallMs
	if wall == 0 {
		wall = storeWarmingWall().Milliseconds()
	}
	return StoreWarmingState{
		Store:         t.store,
		State:         state,
		Degraded:      degraded,
		ProbeMs:       t.probeMs,
		BoundExceeded: t.boundExceeded,
		WallMs:        wall,
		SinceAt:       t.since,
		LastProbeAt:   t.lastProbeAt,
		DegradedUntil: until,
	}
}

// heartbeatFieldLocked renders the L2 gauge that rides the once-a-minute
// reconcile heartbeat: `store=warming probe_ms=123` (plus `degraded=true`
// while the breaker is open). It rides that line rather than a new endpoint
// for the PR #166 deps= reason — the trigger condition is a DWELL, a dwell
// needs a series, and this is the line operators already grep.
func (t *storeWarmingTracker) heartbeatField(now time.Time) string {
	if t == nil {
		return ""
	}
	st := t.snapshot(now)
	field := fmt.Sprintf("store=%s probe_ms=%d", st.State, st.ProbeMs)
	if st.Degraded {
		field += " degraded=true"
	}
	return field
}

// StoreDegraded reports whether this cache's breaker is open — the store is
// being skipped and reads are served from a stale-marked snapshot.
func (c *CachingStore) StoreDegraded() bool {
	if c == nil {
		return false
	}
	return c.warm.breakerOpen(storeWarmingNow())
}

// WarmingState returns this cache's current published warming record.
func (c *CachingStore) WarmingState() StoreWarmingState {
	if c == nil {
		return StoreWarmingState{}
	}
	return c.warm.snapshot(storeWarmingNow())
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
// single call site the reconcile paths (success and failure) share.
func (c *CachingStore) recordStoreProbe(now time.Time, elapsed time.Duration, failed bool, bound time.Duration) {
	if c == nil || c.warm == nil {
		return
	}
	st, changed := c.warm.recordProbe(now, storeWarmingProbeElapsedFn(elapsed), failed, bound)
	if changed && storeWarmingEnabled() {
		publishStoreWarmingState(st)
	}
}

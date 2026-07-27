package api

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
)

// TestStatusWarmEntryRoundTrip pins the warm-cache primitives: full and lite
// variants are stored and read back independently, and an empty server reports
// no warm body (vp-e0hv).
func TestStatusWarmEntryRoundTrip(t *testing.T) {
	s := &Server{}
	if _, ok := s.warmStatusBody(false); ok {
		t.Fatal("warmStatusBody(full) ok=true on empty server, want false")
	}
	if _, ok := s.warmStatusBody(true); ok {
		t.Fatal("warmStatusBody(lite) ok=true on empty server, want false")
	}

	now := time.Now()
	s.setWarmStatusBody(false, StatusBody{Name: "full-city"}, now)
	s.setWarmStatusBody(true, StatusBody{Name: "lite-city"}, now)

	gotFull, ok := s.warmStatusBody(false)
	if !ok || gotFull.body.Name != "full-city" {
		t.Fatalf("warm full = %+v ok=%v, want full-city", gotFull, ok)
	}
	gotLite, ok := s.warmStatusBody(true)
	if !ok || gotLite.body.Name != "lite-city" {
		t.Fatalf("warm lite = %+v ok=%v, want lite-city", gotLite, ok)
	}
	if !gotFull.builtAt.Equal(now) {
		t.Fatalf("warm full builtAt = %v, want %v", gotFull.builtAt, now)
	}
}

// TestHandleStatusServesWarmWithoutRebuild is the core vp-e0hv guarantee: after
// the first (cold) synchronous build, repeated non-blocking /status requests
// are served from the warm entry and do NOT re-run the O(store) build, even
// with the time-bucket cache forced to miss on every request and background
// refresh disabled.
func TestHandleStatusServesWarmWithoutRebuild(t *testing.T) {
	oldTTL := timeBucketResponseCacheTTL
	timeBucketResponseCacheTTL = time.Nanosecond // bucket cache misses every request
	oldRefresh := statusWarmRefreshAfter
	statusWarmRefreshAfter = time.Hour // never background-refresh during the test
	t.Cleanup(func() {
		timeBucketResponseCacheTTL = oldTTL
		statusWarmRefreshAfter = oldRefresh
	})

	state := newFakeState(t)
	store := &countingStore{Store: beads.NewMemStore()}
	state.stores["myrig"] = store
	h := newTestCityHandler(t, state)

	req := httptest.NewRequest(http.MethodGet, cityURL(state, "/status"), nil)
	for i := 0; i < 5; i++ {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("status #%d = %d, want 200", i, rec.Code)
		}
	}
	if store.listCalls != 1 {
		t.Fatalf("List calls = %d, want 1 (one cold build, then warm serve)", store.listCalls)
	}
}

// TestHandleStatusRefreshesAgedWarmBody verifies the stale-while-revalidate
// side: when the served warm body is older than statusWarmRefreshAfter, the
// request serves it immediately AND triggers exactly one background rebuild
// (run inline here via statusBuildAsyncHook) — the request thread never blocks
// on the rebuild.
func TestHandleStatusRefreshesAgedWarmBody(t *testing.T) {
	oldTTL := timeBucketResponseCacheTTL
	timeBucketResponseCacheTTL = time.Nanosecond
	oldRefresh := statusWarmRefreshAfter
	statusWarmRefreshAfter = 0 // any warm serve is "aged" → triggers refresh
	oldHook := statusBuildAsyncHook
	statusBuildAsyncHook = func(build func()) { build() } // run refresh inline
	t.Cleanup(func() {
		timeBucketResponseCacheTTL = oldTTL
		statusWarmRefreshAfter = oldRefresh
		statusBuildAsyncHook = oldHook
	})

	state := newFakeState(t)
	store := &countingStore{Store: beads.NewMemStore()}
	state.stores["myrig"] = store
	h := newTestCityHandler(t, state)

	req := httptest.NewRequest(http.MethodGet, cityURL(state, "/status"), nil)
	// Request 0: cold build (1). Requests 1-3: warm serve + one inline refresh
	// each (refreshAfter=0). So builds grow by one per request after the first.
	for i := 0; i < 4; i++ {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("status #%d = %d, want 200", i, rec.Code)
		}
	}
	if store.listCalls < 2 {
		t.Fatalf("List calls = %d, want >= 2 (aged warm body must trigger background refresh)", store.listCalls)
	}
}

// TestBuildAndStoreStatusRecoversFromBuildPanic pins the d3a36d2355 hardening
// (vp-e0hv rework gate, fd:a789c717): a panic anywhere in buildStatusBody's
// synchronous call chain must not escape the singleflight-wrapped build and
// crash the supervisor. storeHealthComputer is the injection seam — it runs
// synchronously inside buildStatusBody on the same goroutine singleflight
// spawns for the build closure, exactly where a real panic in the agent/rig
// fan-out would occur — unlike Store.List/Count, which run in their own
// timeout-guarded sub-goroutines and are out of scope for this recover().
// The pre-existing warm entry must survive untouched and the caller gets a
// zero-value body for the panicking build; the next refresh retries normally.
func TestBuildAndStoreStatusRecoversFromBuildPanic(t *testing.T) {
	state := newFakeState(t)
	s := &Server{state: state}
	s.storeHealthComputer = func(context.Context) (*StatusStoreHealth, error) {
		panic("simulated build panic")
	}

	seeded := StatusBody{Name: "pre-panic-warm-body"}
	seededAt := time.Now()
	s.setWarmStatusBody(false, seeded, seededAt)

	got := s.buildAndStoreStatus(false) // must not panic

	if got.Name != "" {
		t.Fatalf("buildAndStoreStatus after a build panic = %+v, want zero-value StatusBody", got)
	}
	entry, ok := s.warmStatusBody(false)
	if !ok {
		t.Fatal("warm entry missing after build panic, want the pre-panic entry to survive untouched")
	}
	if entry.body.Name != seeded.Name || !entry.builtAt.Equal(seededAt) {
		t.Fatalf("warm entry after build panic = %+v at %v, want unchanged seeded entry %+v at %v",
			entry.body, entry.builtAt, seeded, seededAt)
	}
}

// TestBuildAndStoreStatusEscapesWedgedBuild pins the d3a36d2355 hardening
// (vp-e0hv rework gate, fd:a789c717): a build wedged on an uncancellable read
// must not poison the singleflight key forever. storeHealthComputer blocking
// simulates the real unbounded reads (storehealth.LastMaintenance / WalkSize
// take no context) that motivated this guard. buildAndStoreStatus must escape
// at statusWarmBuildTimeout, serve the last warm body, and Forget(key) must
// restore per-request retry so the NEXT call starts a fresh build instead of
// joining the dead (leaked, still-running) leader.
func TestBuildAndStoreStatusEscapesWedgedBuild(t *testing.T) {
	// statusWarmBuildTimeout is a package-level var read (unsynchronized, by
	// design — it is effectively static config) by the build goroutine this
	// test leaves running past the timeout. Set it exactly once, before any
	// build goroutine exists, and never touch it again: reassigning it mid-test
	// would race that goroutine's own read of it. One fixed value has to serve
	// both builds, so it must be generous enough for a real (non-wedged) build
	// — including the synchronous version-probe subprocess call buildStatusBody
	// makes — to finish comfortably under -race/CI load, while still keeping
	// the test itself fast.
	oldTimeout := statusWarmBuildTimeout
	statusWarmBuildTimeout = 500 * time.Millisecond
	t.Cleanup(func() { statusWarmBuildTimeout = oldTimeout })

	state := newFakeState(t)
	s := &Server{state: state}

	// unblock is closed in t.Cleanup, not inline: the first build stays
	// wedged for the lifetime of the test, exactly like the leaked
	// goroutine the real code documents (fix 2, vp-e0hv plan, is the
	// separate root fix that makes reads ctx-cancellable).
	unblock := make(chan struct{})
	t.Cleanup(func() { close(unblock) })
	s.storeHealthMu.Lock()
	s.storeHealthComputer = func(context.Context) (*StatusStoreHealth, error) {
		<-unblock
		return &StatusStoreHealth{SizeBytes: 1}, nil
	}
	s.storeHealthMu.Unlock()

	seeded := StatusBody{Name: "pre-wedge-warm-body"}
	s.setWarmStatusBody(false, seeded, time.Now())

	got := s.buildAndStoreStatus(false)
	if got.Name != seeded.Name {
		t.Fatalf("buildAndStoreStatus during a wedged build = %+v, want the pre-wedge warm body %+v", got, seeded)
	}

	// Swap in a non-blocking computer under the same lock the wedged
	// goroutine already read past, so this reassignment cannot race it.
	s.storeHealthMu.Lock()
	s.storeHealthComputer = func(context.Context) (*StatusStoreHealth, error) {
		return &StatusStoreHealth{SizeBytes: 2}, nil
	}
	s.storeHealthMu.Unlock()

	got2 := s.buildAndStoreStatus(false)
	if got2.StoreHealth == nil || got2.StoreHealth.SizeBytes != 2 {
		t.Fatalf("buildAndStoreStatus after timeout = %+v, want a fresh build (StoreHealth.SizeBytes=2), not a join to the wedged leader", got2)
	}
}

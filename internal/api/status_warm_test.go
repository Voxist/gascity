package api

import (
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

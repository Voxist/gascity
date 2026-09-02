package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
)

// TestHandleStatusReseedsWarmEntryAfterLongIdle pins the warm path's
// long-idle contract: once the warm StatusView entry ages past
// statusWarmServeMaxAge (a >5m idle), the next poll serves it immediately
// (stale, flagged by CacheAgeS) without blocking, and its background refresh
// must re-seed the warm
// entry and every subsequent poll must be served from the warm path again.
//
// Before the fix the expired warm entry was skipped, the TTL floor missed, and
// upstream's stale-while-revalidate branch answered instead. Its refresher
// (refreshStatusResponseInBackground) stores only the response-cache entry and
// never calls setWarmStatusBody, so the warm entry stayed expired for the rest
// of the process lifetime and every later poll ran on the SWR pipeline: no
// singleflight, a 30s runBackground budget the ~28s build overruns on a loaded
// city, and no bound on staleness. The test models that with both caches seeded
// ten minutes old, then polls the way the dashboard does.
func TestHandleStatusReseedsWarmEntryAfterLongIdle(t *testing.T) {
	oldTTL := timeBucketResponseCacheTTL
	timeBucketResponseCacheTTL = time.Nanosecond // bucket cache misses every request
	oldRefresh := statusWarmRefreshAfter
	statusWarmRefreshAfter = time.Hour // a re-seeded warm body must serve without refreshing
	// The background refresh runs synchronously so the assertions are on
	// state, never on goroutine timing (the package's other warm tests pin
	// the hook the same way).
	oldHook := statusBuildAsyncHook
	statusBuildAsyncHook = func(build func()) { build() }
	t.Cleanup(func() {
		timeBucketResponseCacheTTL = oldTTL
		statusWarmRefreshAfter = oldRefresh
		statusBuildAsyncHook = oldHook
	})

	state := newFakeState(t)
	store := &countingStore{Store: beads.NewMemStore()}
	state.stores["myrig"] = store
	srv := New(state)
	h := newTestCityHandlerWith(t, state, srv)

	// A long idle: both entries buildAndStoreStatus seeds together are now well
	// past statusWarmServeMaxAge. The bodies carry distinct names so the served
	// body identifies which path answered.
	idle := 2 * statusWarmServeMaxAge
	seededAt := time.Now().Add(-idle)
	srv.setWarmStatusBody(false, StatusBody{Name: "expired-warm-body"}, seededAt)
	srv.responseCacheMu.Lock()
	srv.responseCacheEntries = map[string]responseCacheEntry{
		"status": {index: responseCacheTimeBucket(seededAt), storedAt: seededAt, value: StatusBody{Name: "expired-response-body"}},
	}
	srv.responseCacheMu.Unlock()

	poll := func(i int) StatusBody {
		t.Helper()
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, cityURL(state, "/status"), nil))
		if rec.Code != http.StatusOK {
			t.Fatalf("poll #%d status = %d, want 200", i, rec.Code)
		}
		var body StatusBody
		if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
			t.Fatalf("poll #%d decode: %v", i, err)
		}
		// Let any background refresh the poll kicked land before the next
		// poll, so the assertions are on state, never on timing.
		srv.waitForBackground()
		return body
	}

	// The first poll after the idle serves the stale warm body IMMEDIATELY —
	// it never blocks on the rebuild (vp-e0hv) — flagged with its real age,
	// and kicks exactly one background refresh through buildAndStoreStatus.
	// It must not answer off the stale-while-revalidate response entry either:
	// that entry has no age bound and its path is a detour, not a regime.
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, cityURL(state, "/status"), nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("first poll status = %d, want 200", rec.Code)
	}
	firstBody := StatusBody{}
	if err := json.Unmarshal(rec.Body.Bytes(), &firstBody); err != nil {
		t.Fatalf("first poll decode: %v", err)
	}
	if firstBody.Name != "expired-warm-body" {
		t.Fatalf("first poll after a %s idle served %q, want the warm body served immediately (stale, flagged) rather than a blocking rebuild or the SWR entry", idle, firstBody.Name)
	}
	// The stale body must be FLAGGED: the age header carries the warm age,
	// not ~0 off the fresh store, so the CLI's staleness banner fires.
	age, err := strconv.ParseFloat(rec.Header().Get("X-GC-Cache-Age-S"), 64)
	if err != nil {
		t.Fatalf("X-GC-Cache-Age-S = %q, want a float: %v", rec.Header().Get("X-GC-Cache-Age-S"), err)
	}
	if age < idle.Seconds()-1 {
		t.Fatalf("X-GC-Cache-Age-S = %v for a %s-old warm body served stale, want >= %v", age, idle, idle.Seconds()-1)
	}
	// (With the refresh hook pinned synchronous, the one build the poll kicked
	// has already run by the time the response is returned; the served BODY
	// above proves the request did not wait for it.)
	if store.listCalls != 1 {
		t.Fatalf("List calls after the first poll = %d, want exactly 1 (one singleflighted background rebuild)", store.listCalls)
	}
	entry, ok := srv.warmStatusBody(false)
	if !ok {
		t.Fatal("warm entry missing after the first poll following a long idle")
	}
	if !entry.builtAt.After(seededAt) {
		t.Fatalf("warm entry builtAt = %v after the first poll's refresh, want re-seeded later than %v; an expired warm entry that is never re-seeded leaves every later poll stale", entry.builtAt, seededAt)
	}
	// The NEXT poll is fresh, served from the re-seeded warm entry.
	if second := poll(1); second.Name == "expired-warm-body" || second.Name == "expired-response-body" {
		t.Fatalf("second poll served the stale %q body; the refresh did not re-seed the warm path", second.Name)
	}

	// Subsequent polls are served from the re-seeded warm entry: no rebuild,
	// warm body untouched.
	for i := 2; i < 5; i++ {
		poll(i)
	}
	if store.listCalls != 1 {
		t.Fatalf("List calls after warm polls = %d, want 1 (polls after re-seeding must serve the warm entry, not rebuild)", store.listCalls)
	}
	after, _ := srv.warmStatusBody(false)
	if !after.builtAt.Equal(entry.builtAt) {
		t.Fatalf("warm entry builtAt changed from %v to %v across warm-served polls", entry.builtAt, after.builtAt)
	}
}

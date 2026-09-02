package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
)

// TestHandleStatusReseedsWarmEntryAfterLongIdle pins the regime the
// 2026-08-31 resync broke: once the warm StatusView entry ages past
// statusWarmServeMaxAge (a >5m idle), the next poll must re-seed the warm
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
	t.Cleanup(func() {
		timeBucketResponseCacheTTL = oldTTL
		statusWarmRefreshAfter = oldRefresh
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

	// The first poll after the idle must pay the one synchronous rebuild and
	// re-seed the warm entry — not answer with a stale body off the SWR path.
	first := poll(0)
	if first.Name == "expired-response-body" || first.Name == "expired-warm-body" {
		t.Fatalf("first poll after a %s idle served the stale %q body via the response cache, want a fresh synchronous build", idle, first.Name)
	}
	entry, ok := srv.warmStatusBody(false)
	if !ok {
		t.Fatal("warm entry missing after the first poll following a long idle")
	}
	if !entry.builtAt.After(seededAt) {
		t.Fatalf("warm entry builtAt = %v after the first poll, want re-seeded later than %v; an expired warm entry is never re-seeded, so every later poll runs on the SWR pipeline", entry.builtAt, seededAt)
	}
	if store.listCalls != 1 {
		t.Fatalf("List calls after the first poll = %d, want exactly 1 (one singleflighted rebuild)", store.listCalls)
	}

	// Subsequent polls are served from the re-seeded warm entry: no rebuild,
	// warm body untouched.
	for i := 1; i < 4; i++ {
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

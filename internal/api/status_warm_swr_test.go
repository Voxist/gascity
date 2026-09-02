package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
)

// TestRefreshStatusResponseInBackgroundReseedsWarmEntry pins the contract
// that makes the stale-while-revalidate refresher inert as a trap: one
// refresh seeds BOTH the warm StatusView entry and the response cache. Before
// this, the refresher stored only the response-cache entry, so any path that
// reached it with a cleared warm entry could never get back onto the warm
// path (the regime lock TestHandleStatusReseedsWarmEntryAfterLongIdle pins
// from the handler side).
func TestRefreshStatusResponseInBackgroundReseedsWarmEntry(t *testing.T) {
	state := newFakeState(t)
	state.stores["myrig"] = beads.NewMemStore()
	srv := New(state)

	seededAt := time.Now().Add(-time.Hour)
	srv.setWarmStatusBody(false, StatusBody{Name: "old-warm-body"}, seededAt)
	srv.responseCacheMu.Lock()
	srv.responseCacheEntries = map[string]responseCacheEntry{
		"status": {index: responseCacheTimeBucket(seededAt), storedAt: seededAt, value: StatusBody{Name: "old-response-body"}},
	}
	srv.responseCacheMu.Unlock()

	srv.refreshStatusResponseInBackground("status", false)
	srv.waitForBackground()

	entry, ok := srv.warmStatusBody(false)
	if !ok {
		t.Fatal("warm entry missing after the SWR refresh")
	}
	if !entry.builtAt.After(seededAt) {
		t.Fatalf("warm entry builtAt = %v after the SWR refresh, want later than %v: the refresher must re-seed the warm entry, not only the response cache", entry.builtAt, seededAt)
	}
	if entry.body.Name == "old-warm-body" {
		t.Fatal("warm entry body still the seeded one after the SWR refresh")
	}

	srv.responseCacheMu.Lock()
	resp := srv.responseCacheEntries["status"]
	refreshing := srv.responseRefreshing["status"]
	srv.responseCacheMu.Unlock()
	if !resp.storedAt.After(seededAt) {
		t.Fatalf("response cache storedAt = %v after the SWR refresh, want later than %v", resp.storedAt, seededAt)
	}
	if refreshing {
		t.Fatal("responseRefreshing[\"status\"] still true after the refresh completed (leaked guard)")
	}
}

// TestHandleStatusSWRRefreshReseedsWarmEntry is the handler-side view of the
// same contract: a poll answered by the stale-while-revalidate branch (a
// response-cache entry but no warm entry) kicks a refresh that leaves the
// handler back on the warm path, so the NEXT poll is served from the warm
// entry without another rebuild. With a refresher that seeds only the
// response cache, every later poll re-enters the SWR branch and rebuilds.
func TestHandleStatusSWRRefreshReseedsWarmEntry(t *testing.T) {
	oldTTL := timeBucketResponseCacheTTL
	timeBucketResponseCacheTTL = time.Nanosecond // bucket cache misses every request
	oldFloor := statusResponseTTLFloor
	statusResponseTTLFloor = 0 // floor off: force past the TTL-floor cache
	oldRefresh := statusWarmRefreshAfter
	statusWarmRefreshAfter = time.Hour // a re-seeded warm body must serve without refreshing
	t.Cleanup(func() {
		timeBucketResponseCacheTTL = oldTTL
		statusResponseTTLFloor = oldFloor
		statusWarmRefreshAfter = oldRefresh
	})

	state := newFakeState(t)
	store := &countingStore{Store: beads.NewMemStore()}
	state.stores["myrig"] = store
	srv := New(state)
	h := newTestCityHandlerWith(t, state, srv)

	// The SWR precondition: a response-cache entry older than every fast
	// path, and no warm entry at all.
	seededAt := time.Now().Add(-time.Hour)
	srv.responseCacheMu.Lock()
	srv.responseCacheEntries = map[string]responseCacheEntry{
		"status": {index: responseCacheTimeBucket(seededAt), storedAt: seededAt, value: StatusBody{Name: "stale-response-body"}},
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
		srv.waitForBackground()
		return body
	}

	if first := poll(0); first.Name != "stale-response-body" {
		t.Fatalf("first poll served %q, want the stale response-cache body via the SWR branch", first.Name)
	}
	if store.listCalls != 1 {
		t.Fatalf("List calls after the SWR poll = %d, want 1 (one background refresh)", store.listCalls)
	}
	entry, ok := srv.warmStatusBody(false)
	if !ok {
		t.Fatal("warm entry missing after the SWR refresh; the refresher must seed it so the handler returns to the warm path")
	}

	second := poll(1)
	if second.Name == "stale-response-body" {
		t.Fatal("second poll still served the stale response-cache body, want the refreshed body")
	}
	if store.listCalls != 1 {
		t.Fatalf("List calls after the second poll = %d, want 1 (served from the warm entry the refresh seeded, not rebuilt)", store.listCalls)
	}
	after, _ := srv.warmStatusBody(false)
	if !after.builtAt.Equal(entry.builtAt) {
		t.Fatalf("warm entry builtAt changed from %v to %v across a warm-served poll", entry.builtAt, after.builtAt)
	}
}

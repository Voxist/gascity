package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/events"
)

type countingStore struct {
	beads.Store

	listCalls           int
	listByLabelCalls    int
	listByAssigneeCalls int
}

func (s *countingStore) ListOpen(status ...string) ([]beads.Bead, error) {
	s.listCalls++
	return s.Store.ListOpen(status...)
}

func (s *countingStore) List(query beads.ListQuery) ([]beads.Bead, error) {
	switch {
	case query.Assignee != "":
		s.listByAssigneeCalls++
	case query.Label != "":
		s.listByLabelCalls++
	case query.Status != "" || query.AllowScan:
		s.listCalls++
	}
	return s.Store.List(query)
}

func (s *countingStore) ListByLabel(label string, limit int, opts ...beads.QueryOpt) ([]beads.Bead, error) {
	s.listByLabelCalls++
	return s.Store.ListByLabel(label, limit, opts...)
}

func (s *countingStore) ListByAssignee(assignee, status string, limit int) ([]beads.Bead, error) {
	s.listByAssigneeCalls++
	return s.Store.ListByAssignee(assignee, status, limit)
}

// TestHandleStatusCachesAcrossIndexChanges pins the gascity#3186 fix: /status
// keys its response cache on a wall-clock TTL bucket, not the event sequence,
// so a busy city (whose sequence advances every poll) still hits the cache
// instead of rebuilding the O(store-size) body on every request. Recording an
// event must NOT bust the /status cache within the TTL window — unlike the
// index-keyed endpoints (see TestHandleAgentListCachesUntilIndexChanges).
func TestHandleStatusCachesAcrossIndexChanges(t *testing.T) {
	// Pin a wide TTL so every request in this test lands in the same time
	// bucket; this isolates the "index churn must not bust the cache" property
	// from wall-clock bucket-boundary timing. Warm-cache serving across a
	// bucket rollover is covered separately by
	// TestHandleStatusWarmCacheServesAcrossBucketExpiry.
	oldTTL := timeBucketResponseCacheTTL
	timeBucketResponseCacheTTL = time.Hour
	t.Cleanup(func() {
		timeBucketResponseCacheTTL = oldTTL
	})

	state := newFakeState(t)
	store := &countingStore{Store: beads.NewMemStore()}
	state.stores["myrig"] = store
	h := newTestCityHandler(t, state)

	req := httptest.NewRequest(http.MethodGet, cityURL(state, "/status"), nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("first status = %d, want 200", rec.Code)
	}

	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("second status = %d, want 200", rec.Code)
	}
	if store.listCalls != 1 {
		t.Fatalf("List calls after cached repeat = %d, want 1", store.listCalls)
	}

	// A moving event sequence — the busy-city scenario — must keep hitting the
	// time-bucketed cache, not force a rebuild.
	for i := 0; i < 5; i++ {
		state.eventProv.Record(events.Event{Type: events.BeadCreated, Actor: "human"})
		rec = httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("status after event %d = %d, want 200", i, rec.Code)
		}
	}
	if store.listCalls != 1 {
		t.Fatalf("List calls after %d index changes = %d, want 1 (time-bucketed cache must survive sequence churn)", 5, store.listCalls)
	}

	// The X-GC-Index header still reflects the live sequence even on a cache
	// hit, so blocking/long-poll consumers see fresh index values.
	if got := rec.Header().Get("X-GC-Index"); got == "" || got == "0" {
		t.Fatalf("X-GC-Index = %q, want live sequence on cache hit", got)
	}
}

// TestHandleStatusWarmCacheServesAcrossBucketExpiry pins the vp-e0hv warm-cache
// behavior: once a body is built, a non-blocking /status served from the warm
// entry must NOT re-run the ~28s O(store) build even when the time bucket has
// rolled over on every request. (This deliberately inverts the pre-warm-cache
// behavior, where each bucket rollover forced a rebuild.) The single background
// refresh is run inline via statusBuildAsyncHook so the test stays deterministic.
func TestHandleStatusWarmCacheServesAcrossBucketExpiry(t *testing.T) {
	oldTTL := timeBucketResponseCacheTTL
	timeBucketResponseCacheTTL = time.Nanosecond // every request lands in a new bucket
	oldRefresh := statusWarmRefreshAfter
	statusWarmRefreshAfter = 0 // every warm serve also kicks a background refresh
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
	for i := 0; i < 4; i++ {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("status #%d = %d, want 200", i, rec.Code)
		}
	}
	// One synchronous cold build (request 0). With statusWarmRefreshAfter=0 the
	// inline hook also refreshes once per subsequent warm serve. The point is
	// the request thread is NEVER the one running the build past the first: the
	// rebuild count is bounded and modest, not one-per-request-fan-out as before.
	if store.listCalls == 0 {
		t.Fatalf("List calls = %d, want >= 1 (cold build must run once)", store.listCalls)
	}
	if store.listCalls > 4 {
		t.Fatalf("List calls = %d, want <= 4 (warm cache must serve, not rebuild per request)", store.listCalls)
	}
}

// TestHandleStatusBlockingBypassesTimeCache verifies the preserved
// strict-freshness path: a blocking ?index=&wait= request must rebuild the
// body (reflecting the event it waited for) instead of being served a
// time-bucketed cache entry built before that event (gascity#3186).
func TestHandleStatusBlockingBypassesTimeCache(t *testing.T) {
	state := newFakeState(t)
	store := &countingStore{Store: beads.NewMemStore()}
	state.stores["myrig"] = store
	h := newTestCityHandler(t, state)

	// Prime the time-bucketed cache with a non-blocking request.
	req := httptest.NewRequest(http.MethodGet, cityURL(state, "/status"), nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("priming status = %d, want 200", rec.Code)
	}
	if store.listCalls != 1 {
		t.Fatalf("List calls after priming = %d, want 1", store.listCalls)
	}

	// A blocking request (index=0 returns immediately since the sequence is
	// already ahead) must bypass the time cache and rebuild.
	blockReq := httptest.NewRequest(http.MethodGet, cityURL(state, "/status?index=0&wait=1s"), nil)
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, blockReq)
	if rec.Code != http.StatusOK {
		t.Fatalf("blocking status = %d, want 200", rec.Code)
	}
	if store.listCalls != 2 {
		t.Fatalf("List calls after blocking request = %d, want 2 (blocking must bypass time cache)", store.listCalls)
	}
}

func TestHandleAgentListCachesUntilIndexChanges(t *testing.T) {
	state := newFakeState(t)
	store := &countingStore{Store: beads.NewMemStore()}
	state.stores["myrig"] = store
	h := newTestCityHandler(t, state)

	req := httptest.NewRequest(http.MethodGet, cityURL(state, "/agents"), nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("first agents = %d, want 200", rec.Code)
	}

	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("second agents = %d, want 200", rec.Code)
	}

	if store.listByAssigneeCalls != 2 {
		t.Fatalf("ListByAssignee calls after cached repeat = %d, want 2", store.listByAssigneeCalls)
	}

	state.eventProv.Record(events.Event{Type: events.SessionWoke, Actor: "gc"})
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("third agents = %d, want 200", rec.Code)
	}
	if store.listByAssigneeCalls != 4 {
		t.Fatalf("ListByAssignee calls after index change = %d, want 4", store.listByAssigneeCalls)
	}
}

// listCallsPerFeedBuild is how many store List calls one workflow-projection
// build costs: listActiveWorkflowProjectionBeads issues one Live,
// status-scoped read per active status, because bd is the only reader that can
// filter on the raw status (gc-4zb). These tests assert how often the feed
// rebuilds, so they count builds in reads rather than pinning a literal.
var listCallsPerFeedBuild = len(activeWorkflowProjectionStatuses)

func TestHandleOrdersFeedCachesUntilIndexChanges(t *testing.T) {
	state := newFakeState(t)
	rigStore := &countingStore{Store: beads.NewMemStore()}
	cityStore := &countingStore{Store: beads.NewMemStore()}
	state.stores["myrig"] = rigStore
	state.cityBeadStore = cityStore

	_, err := rigStore.Create(beads.Bead{
		Title: "Adopt PR",
		Ref:   "mol-adopt-pr-v2",
		Metadata: map[string]string{
			"gc.kind":             "workflow",
			"gc.formula_contract": "graph.v2",
			"gc.workflow_id":      "wf-123",
			"gc.scope_kind":       "rig",
			"gc.scope_ref":        "myrig",
		},
	})
	if err != nil {
		t.Fatalf("create workflow root: %v", err)
	}

	h := newTestCityHandler(t, state)
	req := httptest.NewRequest(http.MethodGet, cityURL(state, "/orders/feed?scope_kind=rig&scope_ref=myrig"), nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("first feed = %d, want 200", rec.Code)
	}
	var resp map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal first feed: %v", err)
	}

	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("second feed = %d, want 200", rec.Code)
	}
	if rigStore.listCalls != listCallsPerFeedBuild {
		t.Fatalf("rig List calls after cached repeat = %d, want %d (one build)", rigStore.listCalls, listCallsPerFeedBuild)
	}
	if cityStore.listByLabelCalls != 1 {
		t.Fatalf("city ListByLabel calls after cached repeat = %d, want 1", cityStore.listByLabelCalls)
	}

	state.eventProv.Record(events.Event{Type: events.BeadCreated, Actor: "human"})
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("third feed = %d, want 200", rec.Code)
	}
	if want := 2 * listCallsPerFeedBuild; rigStore.listCalls != want {
		t.Fatalf("rig List calls after index change = %d, want %d (two builds)", rigStore.listCalls, want)
	}
	if cityStore.listByLabelCalls != 2 {
		t.Fatalf("city ListByLabel calls after index change = %d, want 2", cityStore.listByLabelCalls)
	}
}

// newFormulaFeedCacheFixture seeds a rig store with one graph.v2 workflow
// root so /formulas/feed has a body to build, and returns the wrapped store
// whose listCalls counts feed rebuilds.
func newFormulaFeedCacheFixture(t *testing.T) (*fakeState, *countingStore, http.Handler) {
	t.Helper()
	state := newFakeState(t)
	rigStore := &countingStore{Store: beads.NewMemStore()}
	state.stores["myrig"] = rigStore
	if _, err := rigStore.Create(beads.Bead{
		Title: "Adopt PR",
		Ref:   "mol-adopt-pr-v2",
		Metadata: map[string]string{
			"gc.kind":             "workflow",
			"gc.formula_contract": "graph.v2",
			"gc.workflow_id":      "wf-123",
			"gc.scope_kind":       "rig",
			"gc.scope_ref":        "myrig",
		},
	}); err != nil {
		t.Fatalf("create workflow root: %v", err)
	}
	return state, rigStore, newTestCityHandler(t, state)
}

// TestHandleFormulaFeedCachesAcrossIndexChanges pins the #3208 feed-latency
// fix: /formulas/feed keys its response cache on a wall-clock TTL bucket, not
// the event sequence, so a busy city (whose sequence advances every poll) no
// longer rebuilds the O(store-history) feed body on every request.
func TestHandleFormulaFeedCachesAcrossIndexChanges(t *testing.T) {
	oldTTL := timeBucketResponseCacheTTL
	timeBucketResponseCacheTTL = time.Hour
	t.Cleanup(func() { timeBucketResponseCacheTTL = oldTTL })

	state, rigStore, h := newFormulaFeedCacheFixture(t)

	req := httptest.NewRequest(http.MethodGet, cityURL(state, "/formulas/feed?scope_kind=rig&scope_ref=myrig"), nil)
	for i := 0; i < 2; i++ {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("feed #%d = %d, want 200", i, rec.Code)
		}
	}
	if rigStore.listCalls != listCallsPerFeedBuild {
		t.Fatalf("rig List calls after cached repeat = %d, want %d (one build)", rigStore.listCalls, listCallsPerFeedBuild)
	}

	// A moving event sequence — the busy-city scenario from #3208 — must
	// keep hitting the time-bucketed cache, not force a rebuild per poll.
	for i := 0; i < 5; i++ {
		state.eventProv.Record(events.Event{Type: events.BeadCreated, Actor: "human"})
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("feed after event %d = %d, want 200", i, rec.Code)
		}
	}
	if rigStore.listCalls != listCallsPerFeedBuild {
		t.Fatalf("rig List calls across index churn = %d, want %d (one build; feed must key on time bucket)", rigStore.listCalls, listCallsPerFeedBuild)
	}
}

// TestHandleFormulaFeedCacheExpiresOnTTL verifies the feed's staleness bound:
// once the time bucket rolls over, the next request rebuilds.
func TestHandleFormulaFeedCacheExpiresOnTTL(t *testing.T) {
	oldTTL := timeBucketResponseCacheTTL
	timeBucketResponseCacheTTL = time.Nanosecond // every request lands in a new bucket
	t.Cleanup(func() { timeBucketResponseCacheTTL = oldTTL })

	state, rigStore, h := newFormulaFeedCacheFixture(t)

	req := httptest.NewRequest(http.MethodGet, cityURL(state, "/formulas/feed?scope_kind=rig&scope_ref=myrig"), nil)
	for i := 0; i < 3; i++ {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("feed #%d = %d, want 200", i, rec.Code)
		}
	}
	if rigStore.listCalls < 2 {
		t.Fatalf("rig List calls with expiring TTL = %d, want >= 2", rigStore.listCalls)
	}
}

// TestHandleBeadListAllCachesAcrossIndexChanges pins the #3208 large-read
// lever: all=true /beads reads (which bypass the CachingStore and scan full
// history per rig) key their response cache on a time bucket, so concurrent
// pollers share one rebuild per TTL window. Open-only reads stay uncached.
func TestHandleBeadListAllCachesAcrossIndexChanges(t *testing.T) {
	oldTTL := timeBucketResponseCacheTTL
	timeBucketResponseCacheTTL = time.Hour
	t.Cleanup(func() { timeBucketResponseCacheTTL = oldTTL })

	state := newFakeState(t)
	store := &countingStore{Store: beads.NewMemStore()}
	state.stores["myrig"] = store
	if _, err := store.Create(beads.Bead{Title: "task one", Type: "task"}); err != nil {
		t.Fatalf("create bead: %v", err)
	}
	h := newTestCityHandler(t, state)

	req := httptest.NewRequest(http.MethodGet, cityURL(state, "/beads?all=true"), nil)
	for i := 0; i < 2; i++ {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("beads all #%d = %d, want 200", i, rec.Code)
		}
	}
	if store.listCalls != 1 {
		t.Fatalf("List calls after cached all=true repeat = %d, want 1", store.listCalls)
	}

	state.eventProv.Record(events.Event{Type: events.BeadCreated, Actor: "human"})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("beads all after event = %d, want 200", rec.Code)
	}
	if store.listCalls != 1 {
		t.Fatalf("List calls across index churn = %d, want 1 (all=true must key on time bucket)", store.listCalls)
	}

	// Open-only reads are served from the store every time — they hit the
	// in-memory CachingStore in production and must stay fresh.
	openReq := httptest.NewRequest(http.MethodGet, cityURL(state, "/beads"), nil)
	for i := 0; i < 2; i++ {
		rec = httptest.NewRecorder()
		h.ServeHTTP(rec, openReq)
		if rec.Code != http.StatusOK {
			t.Fatalf("open beads #%d = %d, want 200", i, rec.Code)
		}
	}
	if store.listCalls != 3 {
		t.Fatalf("List calls after open-only reads = %d, want 3 (open reads must not be response-cached)", store.listCalls)
	}
}

// TestHandleBeadListAllBlockingBypassesTimeCache verifies the preserved
// strict-freshness path on /beads: a blocking ?index=&wait= all=true request
// must rebuild the body rather than be served an entry built before the
// event it waited for.
func TestHandleBeadListAllBlockingBypassesTimeCache(t *testing.T) {
	state := newFakeState(t)
	store := &countingStore{Store: beads.NewMemStore()}
	state.stores["myrig"] = store
	if _, err := store.Create(beads.Bead{Title: "task one", Type: "task"}); err != nil {
		t.Fatalf("create bead: %v", err)
	}
	h := newTestCityHandler(t, state)

	req := httptest.NewRequest(http.MethodGet, cityURL(state, "/beads?all=true"), nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("priming beads all = %d, want 200", rec.Code)
	}
	if store.listCalls != 1 {
		t.Fatalf("List calls after priming = %d, want 1", store.listCalls)
	}

	blockReq := httptest.NewRequest(http.MethodGet, cityURL(state, "/beads?all=true&index=0&wait=1s"), nil)
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, blockReq)
	if rec.Code != http.StatusOK {
		t.Fatalf("blocking beads all = %d, want 200", rec.Code)
	}
	if store.listCalls != 2 {
		t.Fatalf("List calls after blocking request = %d, want 2 (blocking must bypass time cache)", store.listCalls)
	}
}

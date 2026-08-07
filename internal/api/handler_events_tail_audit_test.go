package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/gastownhall/gascity/internal/events"
)

// countingRecorder wraps a real FileRecorder to count how often the event-list
// handler reaches the full-scan read path (List / ListInFlight) versus the
// bounded ListTail fast path. vp-x7x8w: a first-page GET /events after a
// rotation must be served by the bounded ListTail (which post-fix spans the
// archives newest-first) and must NOT fall through to the unbounded full scan
// that decodes the entire archive set oldest-first.
type countingRecorder struct {
	*events.FileRecorder
	listCalls     atomic.Int64
	inFlightCalls atomic.Int64
}

func (c *countingRecorder) List(filter events.Filter) ([]events.Event, error) {
	c.listCalls.Add(1)
	return c.FileRecorder.List(filter)
}

func (c *countingRecorder) ListInFlight(filter events.Filter) ([]events.Event, error) {
	c.inFlightCalls.Add(1)
	return c.FileRecorder.ListInFlight(filter)
}

// TestFetchEventPageFirstPageBoundedAfterRotation is the T-004 contract: after a
// rotation leaves fewer than `limit` events in the active file (the rest live in
// a gzipped archive), a first-page GET /events returns the newest `limit` events
// AND does not invoke the full-scan List/ListInFlight path. Before the fix, the
// handler's ListTail fast path read the active file only, returned a short page,
// and fell through to List — which decoded the whole archive set oldest-first.
func TestFetchEventPageFirstPageBoundedAfterRotation(t *testing.T) {
	state := newFakeState(t)
	var stderr strings.Builder
	dir := t.TempDir()
	rec, err := events.NewFileRecorder(filepath.Join(dir, "events.jsonl"), &stderr)
	if err != nil {
		t.Fatalf("NewFileRecorder: %v", err)
	}
	t.Cleanup(func() { _ = rec.Close() })
	// Seed 6 events (seqs 1..6) into the active log.
	for i := 0; i < 6; i++ {
		rec.Record(events.Event{Type: events.BeadCreated, Actor: "a"})
	}

	h := newTestCityHandler(t, state)
	state.eventProv = rec

	// Rotate: seqs 1..6 move to a .rotating-* then .gz archive; the new active
	// log holds only the rotation anchor event.
	rotateReq := newPostRequest(cityURL(state, "/events/rotate"), nil)
	rotateRec := httptest.NewRecorder()
	h.ServeHTTP(rotateRec, rotateReq)
	if rotateRec.Code != http.StatusOK {
		t.Fatalf("rotate: status %d body %s", rotateRec.Code, rotateRec.Body.String())
	}
	// Record a few more events into the fresh active log so the newest page
	// spans active (anchor + 8..10) + archive (1..6). Wait for async gzip.
	for i := 0; i < 3; i++ {
		rec.Record(events.Event{Type: events.BeadCreated, Actor: "a"})
	}
	rec.WaitForRotations()

	// Swap in the counting wrapper so we can observe which read path the
	// handler takes. It embeds the same FileRecorder the recorder state held.
	counted := &countingRecorder{FileRecorder: rec}
	state.eventProv = counted

	// First page, no cursor, limit 5: the newest 5 events span the active file
	// and the archive. The bounded ListTail path must serve this without the
	// full-scan List/ListInFlight fall-through.
	req := httptest.NewRequest("GET", cityURL(state, "/events?limit=5"), nil)
	got := httptest.NewRecorder()
	h.ServeHTTP(got, req)
	if got.Code != http.StatusOK {
		t.Fatalf("GET /events: status %d body %s", got.Code, got.Body.String())
	}
	var resp struct {
		Items []struct {
			Seq uint64 `json:"seq"`
		} `json:"items"`
	}
	if err := json.NewDecoder(got.Body).Decode(&resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(resp.Items) != 5 {
		t.Fatalf("page len = %d, want 5 (must span active + archive)", len(resp.Items))
	}

	// The contract: the bounded ListTail path served this page. The full-scan
	// List/ListInFlight must NOT have run on a first-page read — that is the
	// unbounded path that decoded the entire archive set before the fix.
	if n := counted.listCalls.Load() + counted.inFlightCalls.Load(); n != 0 {
		t.Fatalf("first-page read invoked the full-scan path %d time(s); the bounded "+
			"ListTail fast path must serve first-page reads without falling through "+
			"to List/ListInFlight", n)
	}
}

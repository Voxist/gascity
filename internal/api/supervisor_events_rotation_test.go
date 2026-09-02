package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/events"
)

// TestSupervisorEventListSpansArchivesAfterRotation pins the supervisor
// /v0/events list's opt-in to Filter.SpanArchives: right after a rotation the
// active log holds only the anchor plus a few new rows while the newest
// archive holds the page, and the handler reports Total=len(rows) — so an
// active-only tail would present as "the server only had N events". The
// bounded tail readers keep the active-only default; this history page, like
// the events-list API's first page (vp-x7x8w), must reach into the archive.
func TestSupervisorEventListSpansArchivesAfterRotation(t *testing.T) {
	state := newFakeState(t)
	state.cityName = "alpha"
	var stderr strings.Builder
	dir := t.TempDir()
	rec, err := events.NewFileRecorder(filepath.Join(dir, "events.jsonl"), &stderr)
	if err != nil {
		t.Fatalf("NewFileRecorder: %v", err)
	}
	t.Cleanup(func() { _ = rec.Close() })
	for i := 0; i < 6; i++ {
		rec.Record(events.Event{Type: events.BeadCreated, Actor: "a"})
	}
	if _, err := rec.ForceRotate(); err != nil {
		t.Fatalf("ForceRotate: %v", err)
	}
	for i := 0; i < 2; i++ {
		rec.Record(events.Event{Type: events.BeadCreated, Actor: "a"})
	}
	rec.WaitForRotations()
	state.eventProv = rec

	sm := newTestSupervisorMux(t, map[string]*fakeState{"alpha": state})
	req := httptest.NewRequest("GET", "/v0/events?limit=5", nil)
	got := httptest.NewRecorder()
	sm.ServeHTTP(got, req)
	if got.Code != http.StatusOK {
		t.Fatalf("GET /v0/events: status %d body %s", got.Code, got.Body.String())
	}
	var resp struct {
		Items []json.RawMessage `json:"items"`
		Total int               `json:"total"`
	}
	if err := json.NewDecoder(got.Body).Decode(&resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	// Active log after rotation: the anchor plus 2 new rows = 3; the page of 5
	// must draw the remaining rows from the archive.
	if len(resp.Items) != 5 {
		t.Fatalf("items = %d, want 5 (a short page means the tail stopped at the active log)", len(resp.Items))
	}
}

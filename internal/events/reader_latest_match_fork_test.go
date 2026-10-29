package events

import (
	"path/filepath"
	"testing"
	"time"
)

// writeRotatingEvents writes evts as an in-flight rotating-* file in dir: the
// plain-JSONL segment a rotation renames the active log to before gzipping it.
func writeRotatingEvents(t *testing.T, dir string, rotatedAt time.Time, firstSeq, lastSeq uint64, evts ...Event) {
	t.Helper()
	writeEventsJSONL(t, filepath.Join(dir, formatRotatingBasename(rotatedAt, firstSeq, lastSeq)), evts...)
}

// TestReadLatestMatchFindsMatchOnlyInRotatingFile: a match that lives only in
// a segment mid-rotation is found, and it wins over an older archive's match
// (ga-s69mk.12).
func TestReadLatestMatchFindsMatchOnlyInRotatingFile(t *testing.T) {
	dir := t.TempDir()
	live := filepath.Join(dir, "events.jsonl")
	base := time.Date(2026, 7, 1, 0, 0, 0, 0, time.UTC)
	writeEventsArchive(t, dir, base.Add(time.Hour), 1, 2,
		Event{Seq: 1, Type: ControllerStarted, Ts: base},
		Event{Seq: 2, Type: OrderFired, Subject: "x", Ts: base.Add(time.Minute)},
	)
	writeRotatingEvents(t, dir, base.Add(2*time.Hour), 3, 5,
		Event{Seq: 3, Type: ControllerStarted, Ts: base.Add(70 * time.Minute)},
		Event{Seq: 4, Type: ControllerStarted, Ts: base.Add(80 * time.Minute)},
		Event{Seq: 5, Type: OrderFired, Subject: "y", Ts: base.Add(90 * time.Minute)},
	)
	writeEventsJSONL(t, live, Event{Seq: 6, Type: OrderFired, Subject: "z", Ts: base.Add(3 * time.Hour)})

	ev, ok, warnings, err := ReadLatestMatch(live, Filter{Type: ControllerStarted})
	if err != nil || !ok {
		t.Fatalf("ReadLatestMatch: ok=%v err=%v, want the rotating file's match", ok, err)
	}
	if ev.Seq != 4 {
		t.Fatalf("ev.Seq = %d, want 4 (last match in the in-flight rotating file, not the archive's 1)", ev.Seq)
	}
	if len(warnings) != 0 {
		t.Fatalf("warnings = %v, want none", warnings)
	}
}

// TestReadLatestMatchNewestRotatingFileWins: with two in-flight segments the
// newer one is consulted first.
func TestReadLatestMatchNewestRotatingFileWins(t *testing.T) {
	dir := t.TempDir()
	live := filepath.Join(dir, "events.jsonl")
	base := time.Date(2026, 7, 1, 0, 0, 0, 0, time.UTC)
	writeRotatingEvents(t, dir, base.Add(time.Hour), 1, 2,
		Event{Seq: 1, Type: ControllerStarted, Ts: base},
		Event{Seq: 2, Type: OrderFired, Ts: base.Add(time.Minute)},
	)
	writeRotatingEvents(t, dir, base.Add(2*time.Hour), 3, 4,
		Event{Seq: 3, Type: ControllerStarted, Ts: base.Add(70 * time.Minute)},
		Event{Seq: 4, Type: OrderFired, Ts: base.Add(80 * time.Minute)},
	)

	ev, ok, _, err := ReadLatestMatch(live, Filter{Type: ControllerStarted})
	if err != nil || !ok {
		t.Fatalf("ReadLatestMatch: ok=%v err=%v, want match", ok, err)
	}
	if ev.Seq != 3 {
		t.Fatalf("ev.Seq = %d, want 3 (newest rotating file)", ev.Seq)
	}
}

// TestReadLatestMatchFallsThroughRotatingFileToArchive: an in-flight segment
// with no match does not hide an older archive's match.
func TestReadLatestMatchFallsThroughRotatingFileToArchive(t *testing.T) {
	dir := t.TempDir()
	live := filepath.Join(dir, "events.jsonl")
	base := time.Date(2026, 7, 1, 0, 0, 0, 0, time.UTC)
	writeEventsArchive(t, dir, base.Add(time.Hour), 1, 1,
		Event{Seq: 1, Type: ControllerStarted, Ts: base},
	)
	writeRotatingEvents(t, dir, base.Add(2*time.Hour), 2, 3,
		Event{Seq: 2, Type: OrderFired, Ts: base.Add(70 * time.Minute)},
		Event{Seq: 3, Type: OrderFired, Ts: base.Add(80 * time.Minute)},
	)
	writeEventsJSONL(t, live, Event{Seq: 4, Type: OrderFired, Ts: base.Add(3 * time.Hour)})

	ev, ok, warnings, err := ReadLatestMatch(live, Filter{Type: ControllerStarted})
	if err != nil || !ok {
		t.Fatalf("ReadLatestMatch: ok=%v err=%v, want the archive's match", ok, err)
	}
	if ev.Seq != 1 {
		t.Fatalf("ev.Seq = %d, want 1", ev.Seq)
	}
	if len(warnings) != 0 {
		t.Fatalf("warnings = %v, want none", warnings)
	}
}

// TestReadLatestMatchNewerArchiveBeatsStuckRotatingFile: a rotating file whose
// gzip failed can trail a later archive. The walk is by seq window, so the
// newer archive's match wins over the stuck file's older one.
func TestReadLatestMatchNewerArchiveBeatsStuckRotatingFile(t *testing.T) {
	dir := t.TempDir()
	live := filepath.Join(dir, "events.jsonl")
	base := time.Date(2026, 7, 1, 0, 0, 0, 0, time.UTC)
	writeRotatingEvents(t, dir, base.Add(time.Hour), 1, 2,
		Event{Seq: 1, Type: ControllerStarted, Ts: base},
		Event{Seq: 2, Type: OrderFired, Ts: base.Add(time.Minute)},
	)
	writeEventsArchive(t, dir, base.Add(2*time.Hour), 3, 4,
		Event{Seq: 3, Type: ControllerStarted, Ts: base.Add(70 * time.Minute)},
		Event{Seq: 4, Type: OrderFired, Ts: base.Add(80 * time.Minute)},
	)
	writeEventsJSONL(t, live, Event{Seq: 5, Type: OrderFired, Ts: base.Add(3 * time.Hour)})

	ev, ok, warnings, err := ReadLatestMatch(live, Filter{Type: ControllerStarted})
	if err != nil || !ok {
		t.Fatalf("ReadLatestMatch: ok=%v err=%v, want match", ok, err)
	}
	if ev.Seq != 3 {
		t.Fatalf("ev.Seq = %d, want 3 (the newer archive, not the stuck rotating file's 1)", ev.Seq)
	}
	if len(warnings) != 0 {
		t.Fatalf("warnings = %v, want none", warnings)
	}
}

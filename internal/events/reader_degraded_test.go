package events

import (
	"bytes"
	"compress/gzip"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// writeEventsJSONL writes evts to path as plain JSONL, one per line — the
// on-disk shape of an active log. Unlike writeJSONLEvents it takes full
// Event values so tests can exercise Type/Ts predicates.
func writeEventsJSONL(t *testing.T, path string, evts ...Event) {
	t.Helper()
	var b bytes.Buffer
	for _, e := range evts {
		line, err := json.Marshal(e)
		if err != nil {
			t.Fatalf("marshal event: %v", err)
		}
		b.Write(line)
		b.WriteByte('\n')
	}
	if err := os.WriteFile(path, b.Bytes(), 0o644); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

func testArchiveBasename(rotatedAt time.Time, firstSeq, lastSeq uint64) string {
	return fmt.Sprintf("events.jsonl.archive-%s-seq-%d-%d.gz",
		rotatedAt.UTC().Format("20060102T150405Z"), firstSeq, lastSeq)
}

// writeEventsArchive writes evts as a canonically named rotation archive in
// dir.
func writeEventsArchive(t *testing.T, dir string, rotatedAt time.Time, firstSeq, lastSeq uint64, evts ...Event) {
	t.Helper()
	name := testArchiveBasename(rotatedAt, firstSeq, lastSeq)
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	for _, e := range evts {
		line, err := json.Marshal(e)
		if err != nil {
			t.Fatalf("marshal event: %v", err)
		}
		if _, err := gz.Write(append(line, '\n')); err != nil {
			t.Fatalf("gzip write: %v", err)
		}
	}
	if err := gz.Close(); err != nil {
		t.Fatalf("gzip close: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, name), buf.Bytes(), 0o644); err != nil {
		t.Fatalf("write archive: %v", err)
	}
}

// writeCorruptEventsArchive writes a canonically named archive whose body is
// not valid gzip — the truncated-archive class from vc-89s. Any attempt to
// open it errors, which is what lets tests prove an archive was NOT opened.
func writeCorruptEventsArchive(t *testing.T, dir string, rotatedAt time.Time, firstSeq, lastSeq uint64) string {
	t.Helper()
	name := testArchiveBasename(rotatedAt, firstSeq, lastSeq)
	body := []byte{0x1f, 0x8b, 0x00, 0xde, 0xad, 0xbe, 0xef}
	if err := os.WriteFile(filepath.Join(dir, name), body, 0o644); err != nil {
		t.Fatalf("write corrupt archive: %v", err)
	}
	return name
}

func TestReadFilteredWithWarningsSkipsCorruptArchive(t *testing.T) {
	dir := t.TempDir()
	live := filepath.Join(dir, "events.jsonl")
	base := time.Date(2026, 7, 1, 0, 0, 0, 0, time.UTC)
	writeEventsArchive(t, dir, base.Add(time.Hour), 1, 2,
		Event{Seq: 1, Type: OrderFired, Subject: "a", Ts: base},
		Event{Seq: 2, Type: OrderFired, Subject: "b", Ts: base.Add(30 * time.Minute)},
	)
	corrupt := writeCorruptEventsArchive(t, dir, base.Add(2*time.Hour), 3, 4)
	writeEventsJSONL(t, live, Event{Seq: 5, Type: OrderFired, Subject: "c", Ts: base.Add(3 * time.Hour)})

	// The fail-fast contract of ReadFiltered is preserved for existing callers.
	if _, err := ReadFiltered(live, Filter{}); err == nil {
		t.Fatalf("ReadFiltered: want fail-fast error on corrupt archive, got nil")
	}

	evts, warnings, err := ReadFilteredWithWarnings(live, Filter{})
	if err != nil {
		t.Fatalf("ReadFilteredWithWarnings: %v", err)
	}
	if got, want := fmt.Sprint(seqsOf(evts)), fmt.Sprint([]uint64{1, 2, 5}); got != want {
		t.Fatalf("seqs = %v, want %v (valid archive + live must survive the corrupt sibling)", got, want)
	}
	if len(warnings) != 1 || !strings.Contains(warnings[0], corrupt) {
		t.Fatalf("warnings = %v, want exactly one naming %s", warnings, corrupt)
	}
}

func TestReadFilteredWithWarningsPrunesAncientCorruptArchive(t *testing.T) {
	// vc-89s: an archive whose rotation timestamp predates Since is pruned
	// WITHOUT being opened — proven here because opening this one would
	// produce a warning (it is corrupt).
	dir := t.TempDir()
	live := filepath.Join(dir, "events.jsonl")
	base := time.Date(2026, 7, 1, 0, 0, 0, 0, time.UTC)
	writeCorruptEventsArchive(t, dir, base, 1, 2)
	writeEventsJSONL(t, live, Event{Seq: 3, Type: OrderFired, Subject: "c", Ts: base.Add(3 * time.Hour)})

	evts, warnings, err := ReadFilteredWithWarnings(live, Filter{Since: base.Add(time.Hour)})
	if err != nil {
		t.Fatalf("ReadFilteredWithWarnings: %v", err)
	}
	if len(warnings) != 0 {
		t.Fatalf("warnings = %v, want none: the pre-Since archive must be pruned unopened", warnings)
	}
	if got, want := fmt.Sprint(seqsOf(evts)), fmt.Sprint([]uint64{3}); got != want {
		t.Fatalf("seqs = %v, want %v", got, want)
	}
}

func TestReadLatestMatchPrefersLiveTail(t *testing.T) {
	// A live-file hit returns without opening archives: the corrupt archive
	// would otherwise surface as a warning.
	dir := t.TempDir()
	live := filepath.Join(dir, "events.jsonl")
	base := time.Date(2026, 7, 1, 0, 0, 0, 0, time.UTC)
	writeCorruptEventsArchive(t, dir, base.Add(time.Hour), 1, 2)
	writeEventsJSONL(t, live,
		Event{Seq: 3, Type: ControllerStarted, Ts: base.Add(2 * time.Hour)},
		Event{Seq: 4, Type: OrderFired, Subject: "x", Ts: base.Add(3 * time.Hour)},
	)

	ev, ok, warnings, err := ReadLatestMatch(live, Filter{Type: ControllerStarted})
	if err != nil || !ok {
		t.Fatalf("ReadLatestMatch: ok=%v err=%v, want match", ok, err)
	}
	if ev.Seq != 3 {
		t.Fatalf("ev.Seq = %d, want 3", ev.Seq)
	}
	if len(warnings) != 0 {
		t.Fatalf("warnings = %v, want none: archives must not be opened on a live-tail hit", warnings)
	}
}

func TestReadLatestMatchNewestArchiveWins(t *testing.T) {
	// No live-file match; both archives contain matches. The newest-first
	// walk must return the newest archive's LAST match — never an older
	// archive's, and never the first-in-file one.
	dir := t.TempDir()
	live := filepath.Join(dir, "events.jsonl")
	base := time.Date(2026, 7, 1, 0, 0, 0, 0, time.UTC)
	writeEventsArchive(t, dir, base.Add(time.Hour), 1, 2,
		Event{Seq: 1, Type: ControllerStarted, Ts: base},
		Event{Seq: 2, Type: OrderFired, Subject: "x", Ts: base.Add(time.Minute)},
	)
	writeEventsArchive(t, dir, base.Add(2*time.Hour), 3, 5,
		Event{Seq: 3, Type: ControllerStarted, Ts: base.Add(70 * time.Minute)},
		Event{Seq: 4, Type: ControllerStarted, Ts: base.Add(80 * time.Minute)},
		Event{Seq: 5, Type: OrderFired, Subject: "y", Ts: base.Add(90 * time.Minute)},
	)
	writeEventsJSONL(t, live, Event{Seq: 6, Type: OrderFired, Subject: "z", Ts: base.Add(3 * time.Hour)})

	ev, ok, warnings, err := ReadLatestMatch(live, Filter{Type: ControllerStarted})
	if err != nil || !ok {
		t.Fatalf("ReadLatestMatch: ok=%v err=%v, want match", ok, err)
	}
	if ev.Seq != 4 {
		t.Fatalf("ev.Seq = %d, want 4 (last match in the newest archive)", ev.Seq)
	}
	if len(warnings) != 0 {
		t.Fatalf("warnings = %v, want none", warnings)
	}
}

func TestReadLatestMatchFindsMatchOnlyInOldestArchive(t *testing.T) {
	// vc-89s regression shape: the rare event survives only in the oldest
	// archive (e.g. a controller that started before every rotation since).
	// A live-file-only or windowed read comes back empty here.
	dir := t.TempDir()
	live := filepath.Join(dir, "events.jsonl")
	base := time.Date(2026, 7, 1, 0, 0, 0, 0, time.UTC)
	writeEventsArchive(t, dir, base.Add(time.Hour), 1, 2,
		Event{Seq: 1, Type: ControllerStarted, Ts: base},
		Event{Seq: 2, Type: OrderFired, Subject: "x", Ts: base.Add(time.Minute)},
	)
	writeEventsArchive(t, dir, base.Add(2*time.Hour), 3, 4,
		Event{Seq: 3, Type: OrderFired, Subject: "y", Ts: base.Add(70 * time.Minute)},
		Event{Seq: 4, Type: OrderFired, Subject: "y", Ts: base.Add(80 * time.Minute)},
	)
	writeEventsJSONL(t, live, Event{Seq: 5, Type: OrderFired, Subject: "z", Ts: base.Add(3 * time.Hour)})

	ev, ok, warnings, err := ReadLatestMatch(live, Filter{Type: ControllerStarted})
	if err != nil || !ok {
		t.Fatalf("ReadLatestMatch: ok=%v err=%v, want match from oldest archive", ok, err)
	}
	if ev.Seq != 1 {
		t.Fatalf("ev.Seq = %d, want 1", ev.Seq)
	}
	if len(warnings) != 0 {
		t.Fatalf("warnings = %v, want none", warnings)
	}
}

func TestReadLatestMatchSkipsCorruptNewestArchiveAndContinues(t *testing.T) {
	// A corrupt newest archive is skipped with a warning and the walk
	// continues into older archives — the answer can only get OLDER, never
	// silently absent while an older record exists.
	dir := t.TempDir()
	live := filepath.Join(dir, "events.jsonl")
	base := time.Date(2026, 7, 1, 0, 0, 0, 0, time.UTC)
	writeEventsArchive(t, dir, base.Add(time.Hour), 1, 2,
		Event{Seq: 1, Type: OrderFired, Subject: "x", Ts: base},
		Event{Seq: 2, Type: ControllerStarted, Ts: base.Add(time.Minute)},
	)
	corrupt := writeCorruptEventsArchive(t, dir, base.Add(2*time.Hour), 3, 4)
	writeEventsJSONL(t, live, Event{Seq: 5, Type: OrderFired, Subject: "z", Ts: base.Add(3 * time.Hour)})

	ev, ok, warnings, err := ReadLatestMatch(live, Filter{Type: ControllerStarted})
	if err != nil || !ok {
		t.Fatalf("ReadLatestMatch: ok=%v err=%v, want match past the corrupt archive", ok, err)
	}
	if ev.Seq != 2 {
		t.Fatalf("ev.Seq = %d, want 2", ev.Seq)
	}
	if len(warnings) != 1 || !strings.Contains(warnings[0], corrupt) {
		t.Fatalf("warnings = %v, want exactly one naming %s", warnings, corrupt)
	}
}

func TestReadLatestMatchNoMatchAnywhere(t *testing.T) {
	dir := t.TempDir()
	live := filepath.Join(dir, "events.jsonl")
	base := time.Date(2026, 7, 1, 0, 0, 0, 0, time.UTC)
	writeEventsArchive(t, dir, base.Add(time.Hour), 1, 1,
		Event{Seq: 1, Type: OrderFired, Subject: "x", Ts: base},
	)
	writeEventsJSONL(t, live, Event{Seq: 2, Type: OrderFired, Subject: "z", Ts: base.Add(3 * time.Hour)})

	ev, ok, warnings, err := ReadLatestMatch(live, Filter{Type: ControllerStarted})
	if err != nil {
		t.Fatalf("ReadLatestMatch: %v", err)
	}
	if ok {
		t.Fatalf("ok = true (ev=%+v), want no match", ev)
	}
	if len(warnings) != 0 {
		t.Fatalf("warnings = %v, want none", warnings)
	}
}

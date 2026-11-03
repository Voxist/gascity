package events

import (
	"bytes"
	"compress/gzip"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// writeTruncatedEventsArchive writes a canonically named archive holding a
// VALID gzip stream cut off part-way through, the shape a rotation killed
// mid-compression leaves behind: the header and the first lines decode, then
// the reader hits an unexpected EOF. writeCorruptEventsArchive's bad header
// fails at gzip.NewReader instead, so the two cover both error sites.
func writeTruncatedEventsArchive(t *testing.T, dir string, rotatedAt time.Time, firstSeq, lastSeq uint64) string {
	t.Helper()
	name := testArchiveBasename(rotatedAt, firstSeq, lastSeq)
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	for seq := firstSeq; seq <= lastSeq; seq++ {
		line, err := json.Marshal(Event{Seq: seq, Type: OrderFired, Subject: strings.Repeat("x", 64), Ts: rotatedAt})
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
	body := buf.Bytes()
	if err := os.WriteFile(filepath.Join(dir, name), body[:len(body)/2], 0o644); err != nil {
		t.Fatalf("write truncated archive: %v", err)
	}
	return name
}

// TestReadFilteredWithInFlightWarningsSkipsUnreadableArchives pins the lenient
// in-flight read the completions warm path uses. The fail-fast read fails the
// whole call on one unreadable archive, so a caller that retries it re-reads
// the full history every time and never succeeds while the archive stays
// broken. The lenient read skips each unreadable archive, names it, and still
// returns everything readable: the good archive and the active log.
func TestReadFilteredWithInFlightWarningsSkipsUnreadableArchives(t *testing.T) {
	dir := t.TempDir()
	live := filepath.Join(dir, "events.jsonl")
	base := time.Date(2026, 10, 9, 13, 0, 0, 0, time.UTC)
	writeEventsArchive(t, dir, base, 1, 2,
		Event{Seq: 1, Type: ExecutionStepCompleted, Subject: "a", Ts: base},
		Event{Seq: 2, Type: OrderFired, Subject: "b", Ts: base},
	)
	truncated := writeTruncatedEventsArchive(t, dir, base.Add(time.Hour), 3, 400)
	corrupt := writeCorruptEventsArchive(t, dir, base.Add(2*time.Hour), 401, 402)
	writeEventsJSONL(t, live, Event{Seq: 403, Type: ExecutionStepCompleted, Subject: "c", Ts: base.Add(3 * time.Hour)})

	filter := Filter{Type: ExecutionStepCompleted}

	// The fail-fast contract of ReadFilteredWithInFlight is unchanged.
	if _, err := ReadFilteredWithInFlight(live, filter); err == nil {
		t.Fatal("ReadFilteredWithInFlight: want fail-fast error on an unreadable archive, got nil")
	}

	got, warnings, err := ReadFilteredWithInFlightWarnings(live, filter)
	if err != nil {
		t.Fatalf("ReadFilteredWithInFlightWarnings: %v", err)
	}
	var seqs []uint64
	for _, e := range got {
		seqs = append(seqs, e.Seq)
	}
	if len(seqs) != 2 || seqs[0] != 1 || seqs[1] != 403 {
		t.Fatalf("seqs = %v, want [1 403]: the readable archive and the active log", seqs)
	}
	if len(warnings) != 2 {
		t.Fatalf("warnings = %q, want one per unreadable archive", warnings)
	}
	if !strings.Contains(warnings[0], truncated) || !strings.Contains(warnings[1], corrupt) {
		t.Fatalf("warnings = %q, want them to name %s and %s", warnings, truncated, corrupt)
	}

	// The FileRecorder exposes the same read through InFlightWarningsProvider.
	rec, err := NewFileRecorder(live, &bytes.Buffer{})
	if err != nil {
		t.Fatalf("NewFileRecorder: %v", err)
	}
	t.Cleanup(func() { _ = rec.Close() })
	var provider Provider = rec
	lenient, ok := provider.(InFlightWarningsProvider)
	if !ok {
		t.Fatal("*FileRecorder does not implement InFlightWarningsProvider")
	}
	recGot, recWarnings, err := lenient.ListInFlightWithWarnings(filter)
	if err != nil {
		t.Fatalf("ListInFlightWithWarnings: %v", err)
	}
	if len(recGot) != 2 || len(recWarnings) != 2 {
		t.Fatalf("ListInFlightWithWarnings = %d event(s), %d warning(s); want 2 and 2", len(recGot), len(recWarnings))
	}
}

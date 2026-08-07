package events

import (
	"bytes"
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"
)

// TestReadFilteredTailSpansArchivesNewestFirst is the T-001 regression for
// vp-x7x8w: a bounded descending tail read must walk archives newest-first
// and stop as soon as `limit` events are collected, never opening older
// archives once the cap is satisfied. Today ReadFilteredTail reads only the
// active file, so a limit larger than the active file's own event count
// silently returns fewer than limit instead of reaching into archives.
func TestReadFilteredTailSpansArchivesNewestFirst(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "events.jsonl")

	// Oldest archive: seqs 1-3. Written as INVALID gzip content — if the
	// bounded tail read ever opens this file, gzip decoding fails loudly,
	// which is how this test proves "never opened" instead of merely hoping.
	oldest := filepath.Join(dir, formatArchiveBasename(
		time.Date(2026, 5, 7, 12, 0, 0, 0, time.UTC), 1, 3))
	if err := os.WriteFile(oldest, []byte("not a gzip stream"), 0o644); err != nil {
		t.Fatalf("write poisoned oldest archive: %v", err)
	}

	// Newer archive: seqs 4-6, real gzip content.
	newerSrc := filepath.Join(dir, "newer-source.jsonl")
	writeJSONLEvents(t, newerSrc, 4, 5, 6)
	newer := filepath.Join(dir, formatArchiveBasename(
		time.Date(2026, 5, 7, 12, 5, 0, 0, time.UTC), 4, 6))
	var stderr bytes.Buffer
	if err := gzipAndArchive(newerSrc, newer, &stderr); err != nil {
		t.Fatalf("gzipAndArchive: %v (stderr %q)", err, stderr.String())
	}

	// Active file: seqs 7-9 (newest).
	writeJSONLEvents(t, path, 7, 8, 9)

	got, err := ReadFilteredTail(path, Filter{}, 5)
	if err != nil {
		t.Fatalf("ReadFilteredTail: %v (a non-nil error means the poisoned "+
			"oldest archive was opened)", err)
	}
	if want := []uint64{5, 6, 7, 8, 9}; !reflect.DeepEqual(seqsOf(got), want) {
		t.Fatalf("ReadFilteredTail(limit=5) seqs = %v, want %v (newest 5, "+
			"spanning the active file + one archive)", seqsOf(got), want)
	}
}

// TestReadFilteredTailBoundedArchiveOpens is the T-006 latency guard: a first-page
// (no cursor) descending read must open only O(1) archives — the ones whose seq
// window actually overlaps the needed tail — never O(archives). This keeps the
// vp-x7x8w regression from silently returning: if a future change reverts the
// bounded newest-first walk, the poisoned older archives are opened and gzip
// decoding fails loudly here instead of quietly decoding a multi-GB set in prod.
//
// Setup: the active file alone holds `limit` matches (the newest page), and EVERY
// archive on disk is poisoned (invalid gzip). A correct bounded read satisfies the
// cap from the active file and never opens an archive, so the poisoned content is
// never decoded. The pre-fix oldest-first full scan would open them all and fail.
func TestReadFilteredTailBoundedArchiveOpens(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "events.jsonl")

	// Many poisoned archives with ascending seq windows. If the bounded read ever
	// opens one, gzip decoding of "not a gzip stream" fails loudly.
	for i, win := range [][2]uint64{{1, 3}, {4, 6}, {7, 9}, {10, 12}, {13, 15}} {
		_ = i
		poisoned := filepath.Join(dir, formatArchiveBasename(
			time.Date(2026, 5, 7, 12, i, 0, 0, time.UTC), win[0], win[1]))
		if err := os.WriteFile(poisoned, []byte("not a gzip stream"), 0o644); err != nil {
			t.Fatalf("write poisoned archive %s: %v", poisoned, err)
		}
	}

	// Active file holds the newest 5 matches (seqs 16..20) — enough to satisfy
	// limit=5 without reaching into any archive.
	writeJSONLEvents(t, path, 16, 17, 18, 19, 20)

	got, err := ReadFilteredTail(path, Filter{}, 5)
	if err != nil {
		t.Fatalf("ReadFilteredTail: %v (a non-nil error means a poisoned archive "+
			"was opened — the bounded read should have stopped at the active file)", err)
	}
	if want := []uint64{16, 17, 18, 19, 20}; !reflect.DeepEqual(seqsOf(got), want) {
		t.Fatalf("ReadFilteredTail(limit=5) seqs = %v, want %v", seqsOf(got), want)
	}
}

package events

import (
	"bytes"
	"io"
	"path/filepath"
	"reflect"
	"testing"
	"time"
)

// zeroCursorTailFixture lays out the shape every per-tick tail reader sees on
// a rotated log: one .gz archive holding older matches of the filtered type
// (seqs 1-3), and an active file holding fewer matches (seqs 4-5) than the
// limit those readers pass. The filter is a zero-cursor Type filter — AfterSeq
// 0 overlaps every archive, so archiveOverlapsFilter cannot skip the archive
// and only the SpanArchives polarity decides whether it is opened.
func zeroCursorTailFixture(t *testing.T) (path string, filter Filter) {
	t.Helper()
	dir := t.TempDir()
	path = filepath.Join(dir, "events.jsonl")

	archiveSrc := filepath.Join(dir, "archive-src.jsonl")
	writeJSONLEvents(t, archiveSrc, 1, 2, 3)
	archive := filepath.Join(dir, formatArchiveBasename(
		time.Date(2026, 5, 7, 12, 0, 0, 0, time.UTC), 1, 3))
	var stderr bytes.Buffer
	if err := gzipAndArchive(archiveSrc, archive, &stderr); err != nil {
		t.Fatalf("gzipAndArchive: %v (stderr %q)", err, stderr.String())
	}

	writeJSONLEvents(t, path, 4, 5)
	return path, Filter{Type: string(BeadCreated), AfterSeq: 0}
}

// TestReadFilteredTailActiveOnlyByDefault pins upstream's ListTail contract as
// the default: a bounded tail read with fewer than `limit` matches in the
// active file returns just those matches and does NOT reach into the .gz
// archives. This is the read every dispatch tick performs — an event order
// with a zero cursor (vp-8jig), `gc order check`'s order.fired tail, the
// doctor order-firing check, storehealth's LastMaintenance — and each of them
// documents "active file only, no archives". Spanning by default here made a
// never-fired rare event type gunzip the whole retained archive set on every
// tick and fired orders on weeks-old archived events. The archive-spanning
// behavior is opt-in via Filter.SpanArchives
// (TestReadFilteredTailSpanArchivesOptIn).
func TestReadFilteredTailActiveOnlyByDefault(t *testing.T) {
	path, filter := zeroCursorTailFixture(t)

	got, err := ReadFilteredTail(path, filter, 5)
	if err != nil {
		t.Fatalf("ReadFilteredTail: %v", err)
	}
	if want := []uint64{4, 5}; !reflect.DeepEqual(seqsOf(got), want) {
		t.Fatalf("ReadFilteredTail(limit=5) seqs = %v, want %v (active file only; "+
			"the archive's seqs 1-3 must not be returned without SpanArchives)",
			seqsOf(got), want)
	}
}

// TestReadFilteredTailSpanArchivesOptIn is the control for
// TestReadFilteredTailActiveOnlyByDefault on the same fixture: with
// Filter.SpanArchives the short active tail IS filled from the archive, so
// the default test cannot pass vacuously on a fixture whose archive is
// unreadable or non-overlapping.
func TestReadFilteredTailSpanArchivesOptIn(t *testing.T) {
	path, filter := zeroCursorTailFixture(t)
	filter.SpanArchives = true

	got, err := ReadFilteredTail(path, filter, 5)
	if err != nil {
		t.Fatalf("ReadFilteredTail: %v", err)
	}
	if want := []uint64{1, 2, 3, 4, 5}; !reflect.DeepEqual(seqsOf(got), want) {
		t.Fatalf("ReadFilteredTail(SpanArchives, limit=5) seqs = %v, want %v "+
			"(active 4-5 filled from the archive's 1-3)", seqsOf(got), want)
	}
}

// TestFileRecorderListTailActiveOnlyByDefault pins the same default at the
// TailProvider boundary the per-tick callers actually use: FileRecorder.ListTail
// forwards the filter unchanged, so a zero-value Filter stays active-only.
func TestFileRecorderListTailActiveOnlyByDefault(t *testing.T) {
	path, filter := zeroCursorTailFixture(t)
	recorder, err := NewFileRecorder(path, io.Discard)
	if err != nil {
		t.Fatalf("NewFileRecorder: %v", err)
	}
	t.Cleanup(func() { _ = recorder.Close() })

	got, err := recorder.ListTail(filter, 5)
	if err != nil {
		t.Fatalf("ListTail: %v", err)
	}
	if want := []uint64{4, 5}; !reflect.DeepEqual(seqsOf(got), want) {
		t.Fatalf("ListTail(limit=5) seqs = %v, want %v (active file only by default)",
			seqsOf(got), want)
	}
}

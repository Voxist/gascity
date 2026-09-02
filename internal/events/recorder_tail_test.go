package events

import (
	"bytes"
	"path/filepath"
	"reflect"
	"testing"
	"time"
)

// TestFileRecorderListTailSpansArchivesAfterRotation is the T-003 contract:
// FileRecorder.ListTail with Filter.SpanArchives must return the trailing
// matches across the active log AND sibling archives, so the event-list
// handler's fast path stays authoritative after a rotation leaves fewer than
// `limit` matches in the active file. Before vp-x7x8w there was no way to ask
// for this: ListTail read the active file only and silently returned a short
// page, forcing the handler's full-scan fall-through. The active-only read is
// still the default (TestFileRecorderListTailActiveOnlyByDefault).
func TestFileRecorderListTailSpansArchivesAfterRotation(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "events.jsonl")

	// Archive holds the older half of the seq stream (1-3), gzipped.
	archiveSrc := filepath.Join(dir, "archive-src.jsonl")
	writeJSONLEvents(t, archiveSrc, 1, 2, 3)
	archive := filepath.Join(dir, formatArchiveBasename(
		time.Date(2026, 5, 7, 12, 0, 0, 0, time.UTC), 1, 3))
	var stderr bytes.Buffer
	if err := gzipAndArchive(archiveSrc, archive, &stderr); err != nil {
		t.Fatalf("gzipAndArchive: %v (stderr %q)", err, stderr.String())
	}

	// Active file holds the newer half (4-5): fewer than the requested limit,
	// so a tail that reads only the active file returns 2, not 5.
	writeJSONLEvents(t, path, 4, 5)

	recorder, err := NewFileRecorder(path, &stderr)
	if err != nil {
		t.Fatalf("NewFileRecorder: %v", err)
	}
	t.Cleanup(func() { _ = recorder.Close() })

	got, err := recorder.ListTail(Filter{SpanArchives: true}, 5)
	if err != nil {
		t.Fatalf("ListTail: %v", err)
	}
	if want := []uint64{1, 2, 3, 4, 5}; !reflect.DeepEqual(seqsOf(got), want) {
		t.Fatalf("ListTail(limit=5) seqs = %v, want %v (active file 4-5 + archive 1-3)",
			seqsOf(got), want)
	}
}

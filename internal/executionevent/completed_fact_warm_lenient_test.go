package executionevent

import (
	"bytes"
	"compress/gzip"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/events"
)

// writeTruncatedArchive writes a canonically named rotation archive in dir
// holding events firstSeq..lastSeq as a gzip stream cut off part-way through:
// the first lines decode, then the reader hits an unexpected EOF. This is the
// shape a rotation killed mid-compression leaves, and the one that wedged the
// completions sweep in production (two archives, retried every ~6.5 min).
func writeTruncatedArchive(t *testing.T, dir string, firstSeq, lastSeq uint64) string {
	t.Helper()
	name := fmt.Sprintf("events.jsonl.archive-20261009T132524Z-seq-%d-%d.gz", firstSeq, lastSeq)
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	for seq := firstSeq; seq <= lastSeq; seq++ {
		fmt.Fprintf(gz, `{"seq":%d,"type":"order.fired","ts":"2026-10-09T13:00:00Z","actor":"test","subject":"%s"}`+"\n", seq, strings.Repeat("x", 64))
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

// TestCompletionBackstopWarmSkipsTruncatedArchive: one truncated archive must
// degrade the completion record, not fail it. A fail-fast warm over a
// permanently truncated archive never succeeds, so the sweep backs off and
// retries forever, re-reading the whole archive history on every retry and
// converging nothing. The lenient warm skips the archive, names it in the
// result, and the chunk runs.
//
// It also pins the interplay with the high-water: the skipped archive sits
// below the read's reach, so the next sweep's tail read does not reopen it and
// does not warn again.
func TestCompletionBackstopWarmSkipsTruncatedArchive(t *testing.T) {
	dir := t.TempDir()
	truncated := writeTruncatedArchive(t, dir, 1, 400)
	journal, err := events.NewFileRecorder(filepath.Join(dir, "events.jsonl"), &bytes.Buffer{})
	if err != nil {
		t.Fatalf("NewFileRecorder: %v", err)
	}
	t.Cleanup(func() { _ = journal.Close() })

	store, _, _ := completionCorpus(t, 1)
	stores := []beads.GraphStore{{Store: store}}
	backstop := &CompletionBackstop{}

	result := backstop.Pass(journal, stores, "execution-reconcile")
	if result.WarmFailed {
		t.Fatalf("Pass over a journal with one truncated archive reported WarmFailed (err=%v); the archive must be skipped, not fatal", result.WarmErr)
	}
	if len(result.WarmWarnings) != 1 || !strings.Contains(result.WarmWarnings[0], truncated) {
		t.Fatalf("WarmWarnings = %q, want exactly one naming %s", result.WarmWarnings, truncated)
	}
	if result.Emitted != 1 || !result.SweepComplete {
		t.Fatalf("Pass = %+v, want the one closed root converged in a complete sweep", result)
	}

	// The next sweep re-derives from the high-water, which is past the archive.
	again := backstop.Pass(journal, stores, "execution-reconcile")
	if again.WarmFailed {
		t.Fatalf("second Pass reported WarmFailed (err=%v)", again.WarmErr)
	}
	if len(again.WarmWarnings) != 0 {
		t.Fatalf("second Pass warned %q: its tail read reopened an archive below the high-water", again.WarmWarnings)
	}
	if again.Emitted != 0 {
		t.Fatalf("second Pass emitted %d duplicate fact(s)", again.Emitted)
	}
}

// TestCompletionBackstopWarmFailureCarriesTheError: a warm that fails must say
// why. The caller backs off and retries, and a back-off with no reason is a
// lane that fails silently for as long as the cause lasts.
func TestCompletionBackstopWarmFailureCarriesTheError(t *testing.T) {
	store, _, _ := completionCorpus(t, 1)
	result := (&CompletionBackstop{}).Pass(events.NewFailFake(), []beads.GraphStore{{Store: store}}, "execution-reconcile")
	if !result.WarmFailed {
		t.Fatal("Pass over an unreadable journal did not report WarmFailed")
	}
	if result.WarmErr == nil {
		t.Fatal("WarmFailed is set but WarmErr is nil: the caller cannot log why")
	}
}

// TestCompletedFactIndexHighWaterIsTheReadsReach pins where a re-derivation
// resumes. Completion facts are a small share of the journal, so the newest
// fact can sit far below the journal's head. Resuming at that fact makes every
// tail read reopen every archive above it, which on a busy city is most of the
// history. A read that started when the journal's LatestSeq was N has seen
// every event up to N, so the next read resumes at N.
func TestCompletedFactIndexHighWaterIsTheReadsReach(t *testing.T) {
	store, rootIDs, _ := completionCorpus(t, 1)
	journal := &filterRecordingJournal{Provider: events.NewFake()}
	stores := []beads.GraphStore{{Store: store}}
	var index CompletedFactIndex

	// The cold pass reads an empty journal and emits the one fact (seq 1).
	if emitted := index.ReconcileRoots(journal, stores, rootIDs, "execution-reconcile"); emitted != 1 {
		t.Fatalf("cold pass emitted %d fact(s), want 1", emitted)
	}
	// Then the journal moves on with events that are not completion facts.
	const noise = 50
	for range noise {
		journal.Record(events.Event{Type: events.OrderFired, Actor: "test"})
	}
	head, err := journal.LatestSeq()
	if err != nil || head != 1+noise {
		t.Fatalf("journal head = %d (err %v), want %d", head, err, 1+noise)
	}

	// This re-derivation reads up to the head; its newest FACT is still seq 1.
	index.Invalidate()
	if emitted := index.ReconcileRoots(journal, stores, rootIDs, "execution-reconcile"); emitted != 0 {
		t.Fatalf("re-derivation emitted %d duplicate fact(s)", emitted)
	}

	// So the next one must resume at the head, not at the last fact.
	index.Invalidate()
	before := len(journal.filters)
	if emitted := index.ReconcileRoots(journal, stores, rootIDs, "execution-reconcile"); emitted != 0 {
		t.Fatalf("tail re-derivation emitted %d duplicate fact(s)", emitted)
	}
	if len(journal.filters) <= before {
		t.Fatal("the invalidated index performed no re-read; the assertion below measures nothing")
	}
	if got := journal.filters[len(journal.filters)-1].AfterSeq; got != head {
		t.Fatalf("tail re-read resumed at AfterSeq=%d, want the previous read's reach %d (not the newest fact's seq 1)", got, head)
	}
}

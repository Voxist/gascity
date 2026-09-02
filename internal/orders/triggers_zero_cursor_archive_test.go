package orders

import (
	"bytes"
	"compress/gzip"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/events"
)

// writeArchivedEvents writes evts as a gzipped canonical events archive beside
// path, using the rotation naming convention
// (events.jsonl.archive-<ts>-seq-<first>-<last>.gz) that the archive-aware
// read paths discover by directory listing.
func writeArchivedEvents(t *testing.T, path string, evts []events.Event) {
	t.Helper()
	var raw bytes.Buffer
	for _, e := range evts {
		line, err := json.Marshal(e)
		if err != nil {
			t.Fatal(err)
		}
		raw.Write(line)
		raw.WriteString("\n")
	}
	var gz bytes.Buffer
	zw := gzip.NewWriter(&gz)
	if _, err := zw.Write(raw.Bytes()); err != nil {
		t.Fatal(err)
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	name := fmt.Sprintf("events.jsonl.archive-%s-seq-%d-%d.gz",
		evts[0].Ts.UTC().Format("20060102T150405Z"), evts[0].Seq, evts[len(evts)-1].Seq)
	if err := os.WriteFile(filepath.Join(filepath.Dir(path), name), gz.Bytes(), 0o644); err != nil {
		t.Fatal(err)
	}
}

// TestCheckTriggerEventZeroCursorIgnoresArchivedEvents pins the other half of
// the vp-8jig contract on a real FileRecorder: a zero-cursor event order reads
// the active file only, so matching events that have rotated into a .gz
// archive neither fire the order nor get decoded every tick. An event trigger
// reacts to NEW events; resurrecting a weeks-old archived match would fire the
// order on stale history and, for a never-fired rare type, gunzip the whole
// retained archive set on every dispatch tick.
func TestCheckTriggerEventZeroCursorIgnoresArchivedEvents(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "events.jsonl")
	ts := time.Date(2026, 5, 1, 0, 0, 0, 0, time.UTC)

	// The only bead.closed events live in the archive; the active file holds
	// unrelated traffic, as it would after a rotation.
	writeArchivedEvents(t, path, []events.Event{
		{Seq: 1, Type: "bead.closed", Ts: ts},
		{Seq: 2, Type: "bead.closed", Ts: ts.Add(time.Minute)},
	})
	rec, err := events.NewFileRecorder(path, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = rec.Close() })
	rec.Record(events.Event{Type: "bead.created", Ts: ts.Add(time.Hour)})

	// Control: the archive-aware List path DOES see the archived matches, so a
	// not-due verdict below is archive-blindness and not a broken fixture.
	viaList, err := rec.List(events.Filter{Type: "bead.closed"})
	if err != nil {
		t.Fatal(err)
	}
	if len(viaList) != 2 {
		t.Fatalf("List got %d events, want 2 (fixture must be readable via the archive-aware path)", len(viaList))
	}

	a := Order{Name: "convoy-check", Trigger: "event", On: "bead.closed"}
	res := checkEvent(a, rec, nil) // nil cursorFn ⇒ cursor 0
	if res.Due {
		t.Fatalf("checkEvent Due = true (%s), want false — the only matches are archived and a zero-cursor read is active-file only", res.Reason)
	}

	// A fresh matching event in the active file still fires, so the not-due
	// verdict above is about archives, not a dead trigger.
	rec.Record(events.Event{Type: "bead.closed", Ts: ts.Add(2 * time.Hour)})
	res = checkEvent(a, rec, nil)
	if !res.Due {
		t.Fatalf("checkEvent Due = false (%s), want true — a matching event now sits in the active file", res.Reason)
	}
}

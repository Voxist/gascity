package events

import (
	"encoding/json"
	"io"
	"path/filepath"
	"testing"
	"time"
)

// TestActiveOnlyHonoredWithUnboundedLimit pins Filter.ActiveOnly on the
// limit <= 0 branch of ReadFilteredTail, which short-circuits to ReadFiltered
// before the archive guard. Without the check there the field is silently
// ignored and archived records come back with no error — the same inert-knob
// failure the field was added to prevent.
func TestActiveOnlyHonoredWithUnboundedLimit(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	path := filepath.Join(dir, "events.jsonl")
	rec, err := NewFileRecorder(path, io.Discard)
	if err != nil {
		t.Fatalf("recorder: %v", err)
	}
	rec.Record(Event{Type: "probe", Message: "active"})
	rec.Record(Event{Type: "probe", Message: "active-second"})

	// A canonical seq-stamped archive sibling holding an older record of the
	// same type (naming per archiveBasenameRE).
	archived := Event{
		Type:    "probe",
		Message: "archived",
		Seq:     1,
		Ts:      time.Date(2020, 1, 1, 0, 0, 0, 0, time.UTC),
	}
	line, err := json.Marshal(archived)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	writeGzipFile(t, filepath.Join(dir, "events.jsonl.archive-20200101T000000Z-seq-1-1.gz"), string(line)+"\n")

	// Control: without ActiveOnly the archived record IS reachable, so the
	// assertion below cannot pass vacuously.
	all, err := ReadFilteredTail(path, Filter{Type: "probe"}, 0)
	if err != nil {
		t.Fatalf("control read: %v", err)
	}
	if !containsMessage(all, "archived") {
		t.Skip("archive fixture not reachable on the unbounded path; guard would be vacuous")
	}

	got, err := ReadFilteredTail(path, Filter{Type: "probe", ActiveOnly: true}, 0)
	if err != nil {
		t.Fatalf("ReadFilteredTail: %v", err)
	}
	// BOTH halves matter, and the second is why an earlier version of this
	// guard was worthless. Asserting only "no archived record" is satisfied by
	// returning NOTHING, which is exactly what the first attempt at this fix
	// did (it routed ActiveOnly to a zero-limit tail walk that stops before
	// reading a byte). A silent-empty is a worse bug than the silent-ignore it
	// replaced, and the test could not tell them apart.
	if !containsMessage(got, "active") {
		t.Fatalf("ActiveOnly returned %d events and none from the ACTIVE file; "+
			"the bound must exclude archives, not suppress the whole read", len(got))
	}
	if containsMessage(got, "archived") {
		t.Fatal("ActiveOnly was ignored on the unbounded (limit <= 0) branch; an archived record was returned")
	}
}

func containsMessage(events []Event, msg string) bool {
	for _, e := range events {
		if e.Message == msg {
			return true
		}
	}
	return false
}

package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/readywork"
	"github.com/spf13/cobra"
)

// This file pins `gc ready --fields`/`--no-description` (vp-7llgp): the
// field-projection flags added because 78% of a measured 918KB federated read
// was description bodies a caller that only needs id/priority/status/
// metadata still had to pay for. Default behavior (neither flag set) is
// covered by the existing default-shape tests in cmd_ready_test.go and is not
// re-asserted here beyond TestReadyFlagsDefaultProjectionIsNil.

func TestReadyFlagsDefaultProjectionIsNil(t *testing.T) {
	if got := (readyFlags{}).projectionFields(); got != nil {
		t.Fatalf("projectionFields() with neither --fields nor --no-description = %v, want nil (no projection, the unchanged default row)", got)
	}
}

func TestReadyFlagsProjectionFieldsCanonicalOrderAndDedup(t *testing.T) {
	flags := readyFlags{fields: []string{"status", "id", "id", "metadata"}}
	got := flags.projectionFields()
	want := []string{"id", "status", "metadata"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("projectionFields() = %v, want %v — canonical wire order regardless of the order given, de-duplicated", got, want)
	}
}

func TestReadyFlagsNoDescriptionProjectsEveryFieldButDescription(t *testing.T) {
	flags := readyFlags{noDescription: true}
	got := flags.projectionFields()
	if len(got) != len(readyWireFieldOrder)-1 {
		t.Fatalf("projectionFields() with --no-description = %v (%d fields), want every wire field but description (%d)", got, len(got), len(readyWireFieldOrder)-1)
	}
	for _, name := range got {
		if name == "description" {
			t.Fatalf("projectionFields() with --no-description still carries description: %v", got)
		}
	}
}

func TestReadyFlagsValidateRejectsUnknownFieldName(t *testing.T) {
	flags := readyFlags{fields: []string{"id", "not_a_real_field"}}
	err := flags.validate()
	if err == nil || !strings.Contains(err.Error(), `"not_a_real_field"`) {
		t.Fatalf("validate() = %v, want a rejection naming the unknown field", err)
	}
}

func TestReadyFlagsValidateRejectsFieldsCombinedWithNoDescription(t *testing.T) {
	flags := readyFlags{fields: []string{"id"}, noDescription: true}
	err := flags.validate()
	if err == nil || !strings.Contains(err.Error(), "--fields") || !strings.Contains(err.Error(), "--no-description") {
		t.Fatalf("validate() = %v, want a rejection naming both --fields and --no-description", err)
	}
}

func TestReadyFlagsValidateRunsBeforeAnyStoreIsOpened(t *testing.T) {
	// A malformed --fields must not cost a city-wide federated read to be told
	// it was malformed: GC_CITY is left unset/pointed nowhere, so any attempt
	// to resolve a city would fail on that, not on the flag.
	t.Setenv("GC_CITY", filepath.Join(t.TempDir(), "does-not-exist"))
	var stdout, stderr bytes.Buffer
	code := cmdReady(readyFlags{fields: []string{"not_a_real_field"}}, &stdout, &stderr)
	if code == 0 {
		t.Fatalf("cmdReady accepted an unknown --fields entry; stdout=%s", stdout.String())
	}
	if !strings.Contains(stderr.String(), "not_a_real_field") {
		t.Fatalf("cmdReady stderr = %q, want it to name the unknown field (proving validation ran before any city/store resolution)", stderr.String())
	}
	if stdout.Len() != 0 {
		t.Fatalf("cmdReady wrote %q to stdout alongside the validation failure", stdout.String())
	}
}

// TestReadyFieldsFlagAcceptsRepeatedAndCommaSeparatedForms pins --fields onto
// pflag's StringSlice parsing: both repeatable and comma-separated forms must
// accumulate onto the same list, which bd's own --metadata-field/--exclude-*
// flags (StringArrayVar, no comma-splitting) do not do.
func TestReadyFieldsFlagAcceptsRepeatedAndCommaSeparatedForms(t *testing.T) {
	var flags readyFlags
	var includeEphemeral, jsonOut bool
	cmd := &cobra.Command{Use: "ready"}
	registerReadyFlags(cmd, &flags, &includeEphemeral, &jsonOut)
	if err := cmd.Flags().Parse([]string{"--fields", "id,status", "--fields", "metadata"}); err != nil {
		t.Fatalf("parse: %v", err)
	}
	want := []string{"id", "status", "metadata"}
	if !reflect.DeepEqual(flags.fields, want) {
		t.Fatalf("flags.fields = %v, want %v", flags.fields, want)
	}
}

// readyProjectedRowKeys decodes one projected JSON object into its bare key
// set, independent of Go map iteration order, so a test can assert "exactly
// these keys" without caring what order writeReadyRows wrote them in.
func readyProjectedRowKeys(t *testing.T, raw json.RawMessage) map[string]bool {
	t.Helper()
	var m map[string]json.RawMessage
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatalf("decode projected row %s: %v", raw, err)
	}
	keys := make(map[string]bool, len(m))
	for k := range m {
		keys[k] = true
	}
	return keys
}

// TestProjectReadyRowsEmitsExactlyTheRequestedFieldsInOrder pins the pure
// projection function directly: the emitted object carries exactly fields,
// each present field in that order, byte for byte.
func TestProjectReadyRowsEmitsExactlyTheRequestedFieldsInOrder(t *testing.T) {
	row := readywork.ToReadyBead(beads.Bead{
		ID:       "gc-1",
		Title:    "work",
		Status:   "open",
		Type:     "task",
		Priority: readyPriority(1),
		Metadata: map[string]string{"gc.routed_to": "rig-A/executor"},
	})
	rows, err := projectReadyRows([]readywork.ReadyBead{row}, []string{"id", "status", "priority", "metadata"})
	if err != nil {
		t.Fatalf("projectReadyRows: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("got %d projected rows, want 1", len(rows))
	}
	data, err := json.Marshal(rows[0])
	if err != nil {
		t.Fatalf("marshal projected row: %v", err)
	}
	want := `{"id":"gc-1","status":"open","priority":1,"metadata":{"gc.routed_to":"rig-A/executor"}}`
	if string(data) != want {
		t.Fatalf("projected row = %s, want %s", data, want)
	}
}

// TestProjectReadyRowsOmitsFieldsTheRowDoesNotCarry pins that a requested
// field absent from the row's own (omitempty) marshaling stays absent under
// projection, rather than coming back as null or a zero value — a consumer
// that only checks key presence must see the same "not set" signal either
// way.
func TestProjectReadyRowsOmitsFieldsTheRowDoesNotCarry(t *testing.T) {
	row := readywork.ToReadyBead(beads.Bead{ID: "gc-1", Title: "work", Status: "open", Type: "task"})
	rows, err := projectReadyRows([]readywork.ReadyBead{row}, []string{"id", "assignee", "description"})
	if err != nil {
		t.Fatalf("projectReadyRows: %v", err)
	}
	data, err := json.Marshal(rows[0])
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	want := `{"id":"gc-1"}`
	if string(data) != want {
		t.Fatalf("projected row = %s, want %s — assignee/description are both empty and omitempty on the row, and must not be synthesized", data, want)
	}
}

// TestWriteReadyRowsFallsBackToTheFullRowWhenNoFieldsAreRequested pins
// writeReadyRows' no-projection branch: the exact bytes writeJSON(rows) would
// have produced, unchanged.
func TestWriteReadyRowsFallsBackToTheFullRowWhenNoFieldsAreRequested(t *testing.T) {
	row := readywork.ToReadyBead(beads.Bead{ID: "gc-1", Title: "work", Status: "open", Type: "task"})
	rows := []readywork.ReadyBead{row}

	var direct, viaWriter bytes.Buffer
	if err := writeJSON(&direct, rows); err != nil {
		t.Fatalf("writeJSON: %v", err)
	}
	if err := writeReadyRows(&viaWriter, rows, nil); err != nil {
		t.Fatalf("writeReadyRows: %v", err)
	}
	if direct.String() != viaWriter.String() {
		t.Fatalf("writeReadyRows(nil fields) = %q, want the unprojected writeJSON output %q", viaWriter.String(), direct.String())
	}
}

// TestWriteReadyRowsEmitsAnEmptyArrayForNoRows keeps the empty-result
// contract (TestReadyEmptyResultIsAnEmptyArray's rule) true under projection
// too: `null` is not a JSON array any consumer here parses.
func TestWriteReadyRowsEmitsAnEmptyArrayForNoRows(t *testing.T) {
	var buf bytes.Buffer
	if err := writeReadyRows(&buf, []readywork.ReadyBead{}, []string{"id", "status"}); err != nil {
		t.Fatalf("writeReadyRows: %v", err)
	}
	if got := strings.TrimSpace(buf.String()); got != "[]" {
		t.Fatalf("writeReadyRows of zero rows under projection = %q, want []", got)
	}
}

// newReadyFieldsTestCity writes a minimal real on-disk file-provider city
// with no rigs, left ambient (GC_CITY), the same pattern newReadyCityWithRigs
// and newReadyCityWithBrokenRig use — just without any rig at all, since the
// end-to-end test below only needs the city leg.
func newReadyFieldsTestCity(t *testing.T) string {
	t.Helper()
	cityDir := t.TempDir()
	cityToml := "[workspace]\nname = \"readyfields\"\n\n[beads]\nprovider = \"file\"\n\n[session]\nprovider = \"fake\"\n"
	if err := os.WriteFile(filepath.Join(cityDir, "city.toml"), []byte(cityToml), 0o644); err != nil {
		t.Fatalf("write city.toml: %v", err)
	}
	if err := ensureScopedFileStoreLayout(cityDir); err != nil {
		t.Fatalf("ensuring scoped file store layout: %v", err)
	}
	if err := ensurePersistedScopeLocalFileStore(cityDir); err != nil {
		t.Fatalf("ensuring file store: %v", err)
	}
	prevCityFlag, prevRigFlag := cityFlag, rigFlag
	cityFlag, rigFlag = "", ""
	t.Cleanup(func() { cityFlag, rigFlag = prevCityFlag, prevRigFlag })
	t.Setenv("GC_CITY", cityDir)
	return cityDir
}

// TestWriteReadyRowsMeetsTheBeadsOwnSizeTarget reproduces vp-7llgp's own
// acceptance criterion in isolation: "a projected call over the same 290-row
// query returns well under 250KB". The bead's live measurement was 290 rows,
// 917,903 bytes total, 716,833 of it (78%) description — i.e. ~2,472 bytes of
// description per row on average. This seeds that same shape synthetically
// (no live city needed) and asserts --no-description's output clears the
// bead's own bar by a wide margin.
func TestWriteReadyRowsMeetsTheBeadsOwnSizeTarget(t *testing.T) {
	const rowCount = 290
	const avgDescriptionBytes = 2472 // 716,833 / 290, rounded, from the bead's own measurement
	description := strings.Repeat("x", avgDescriptionBytes)

	rows := make([]readywork.ReadyBead, 0, rowCount)
	for i := 0; i < rowCount; i++ {
		rows = append(rows, readywork.ToReadyBead(beads.Bead{
			ID:          fmt.Sprintf("vp-synthetic-%03d", i),
			Title:       "synthetic routed work",
			Status:      "open",
			Type:        "task",
			Description: description,
			Metadata:    map[string]string{"gc.routed_to": "voxist.platform-architect"},
		}))
	}

	var full bytes.Buffer
	if err := writeReadyRows(&full, rows, nil); err != nil {
		t.Fatalf("writeReadyRows (full): %v", err)
	}
	var projected bytes.Buffer
	if err := writeReadyRows(&projected, rows, (readyFlags{noDescription: true}).projectionFields()); err != nil {
		t.Fatalf("writeReadyRows (--no-description): %v", err)
	}

	const targetBytes = 250 * 1024
	if projected.Len() >= targetBytes {
		t.Fatalf("--no-description over %d synthetic rows = %d bytes, want well under the bead's own %d-byte target", rowCount, projected.Len(), targetBytes)
	}
	if projected.Len() >= full.Len()/2 {
		t.Fatalf("--no-description (%d bytes) did not substantially shrink the full row (%d bytes); description is supposed to dominate the payload", projected.Len(), full.Len())
	}
	t.Logf("full=%d bytes, --no-description=%d bytes (%.1f%% of full) over %d rows", full.Len(), projected.Len(), 100*float64(projected.Len())/float64(full.Len()), rowCount)
}

// TestCmdReadyFieldsEndToEndProjectsThroughTheRealCommand exercises --fields
// through the actual command wiring (flag parse -> validate -> city resolve
// -> store open -> read -> project -> emit), not just the pure functions
// above, the way TestCmdReadyOnALegacyCityFederatesCityAndRigStores does for
// the unprojected path.
func TestCmdReadyFieldsEndToEndProjectsThroughTheRealCommand(t *testing.T) {
	cityDir := newReadyFieldsTestCity(t)
	cityStore, err := openStoreAtForCity(cityDir, cityDir)
	if err != nil {
		t.Fatalf("open city store: %v", err)
	}
	created := mustCreateReadyBead(t, cityStore, beads.Bead{
		Title:    "work",
		Type:     "task",
		Priority: readyPriority(1),
		Metadata: map[string]string{"gc.routed_to": "rig-A/executor"},
	})

	var stdout, stderr bytes.Buffer
	flags := readyFlags{fields: []string{"status", "id", "priority"}}
	if err := flags.validate(); err != nil {
		t.Fatalf("validate: %v", err)
	}
	if code := cmdReady(flags, &stdout, &stderr); code != 0 {
		t.Fatalf("cmdReady = %d, stderr: %s", code, stderr.String())
	}

	var rows []json.RawMessage
	if err := json.Unmarshal(stdout.Bytes(), &rows); err != nil {
		t.Fatalf("decode %q: %v", stdout.String(), err)
	}
	if len(rows) != 1 {
		t.Fatalf("got %d rows, want 1", len(rows))
	}
	keys := readyProjectedRowKeys(t, rows[0])
	want := map[string]bool{"id": true, "status": true, "priority": true}
	if !reflect.DeepEqual(keys, want) {
		t.Fatalf("projected row keys = %v, want exactly %v (requested id/status/priority out of order, title/metadata/description excluded)", keys, want)
	}
	var decoded struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(rows[0], &decoded); err != nil {
		t.Fatalf("decode id: %v", err)
	}
	if decoded.ID != created.ID {
		t.Fatalf("projected row id = %q, want %q", decoded.ID, created.ID)
	}
}

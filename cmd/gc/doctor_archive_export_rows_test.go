package main

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/doctor"
)

// The jsonl-export script writes one {"rows":[...]} payload per file, on a
// single line. Counting lines would report 1 row for any non-empty archive.
func TestCountArchiveExportRowsCountsDoltJSONPayloadRows(t *testing.T) {
	path := filepath.Join(t.TempDir(), "issues.jsonl")
	if err := os.WriteFile(path, []byte(`{"rows":[{"id":"a"},{"id":"b"},{"id":"c"}]}`+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	n, err := countArchiveExportRows(path)
	if err != nil || n != 3 {
		t.Fatalf("countArchiveExportRows = %d, %v; want 3, nil", n, err)
	}
}

func TestCountArchiveExportRowsEmptyPayloadIsZero(t *testing.T) {
	path := filepath.Join(t.TempDir(), "issues.jsonl")
	if err := os.WriteFile(path, []byte(`{"rows":[]}`+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	n, err := countArchiveExportRows(path)
	if err != nil || n != 0 {
		t.Fatalf("countArchiveExportRows = %d, %v; want 0, nil", n, err)
	}
}

// dolt prints a bare {} for an empty result set, which is what the export
// script writes for a rig with no beads yet. The script scores it as 0; a line
// count would score it as 1 and turn every fresh rig into "data loss".
func TestCountArchiveExportRowsEmptyResultSetIsZero(t *testing.T) {
	path := filepath.Join(t.TempDir(), "issues.jsonl")
	if err := os.WriteFile(path, []byte("{}\n\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	n, err := countArchiveExportRows(path)
	if err != nil || n != 0 {
		t.Fatalf("countArchiveExportRows = %d, %v; want 0, nil", n, err)
	}
}

func TestCountArchiveExportRowsFallsBackToLineCount(t *testing.T) {
	path := filepath.Join(t.TempDir(), "issues.jsonl")
	if err := os.WriteFile(path, []byte("{\"id\":\"a\"}\n{\"id\":\"b\"}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	n, err := countArchiveExportRows(path)
	if err != nil || n != 2 {
		t.Fatalf("countArchiveExportRows = %d, %v; want 2, nil", n, err)
	}
}

// archiveExportRowsFor resolves the rig's Dolt database from its metadata and
// reads <archive>/<db>/issues.jsonl, honoring GC_JSONL_ARCHIVE_REPO.
func TestArchiveExportRowsForReadsPerDatabasePayload(t *testing.T) {
	cityDir := t.TempDir()
	rigDir := filepath.Join(cityDir, "rig")
	archive := filepath.Join(cityDir, "archive")
	for _, dir := range []string{filepath.Join(rigDir, ".beads"), filepath.Join(archive, "vp")} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(rigDir, ".beads", "metadata.json"),
		[]byte(`{"backend":"dolt","dolt_mode":"server","dolt_database":"vp"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(archive, "vp", "issues.jsonl"),
		[]byte(`{"rows":[{"id":"a"},{"id":"b"}]}`+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("GC_JSONL_ARCHIVE_REPO", archive)

	n, ok := archiveExportRowsFor(cityDir)(rigDir)
	if !ok || n != 2 {
		t.Fatalf("archiveExportRowsFor = %d, %v; want 2, true", n, ok)
	}
}

// The legacy-managed acceptance shape (TestBeadsProxiedDefault/
// legacy-managed-city-unchanged): a rig freshly added to a grandfathered
// managed city has no beads, has had no local issues.jsonl since gc reaps it,
// and the core pack's export has archived its empty table as {}. That is a
// fresh rig, not data loss, and must not block doctor.
func TestRigDataPresenceFreshRigWithEmptyArchivedExportIsOK(t *testing.T) {
	cityDir := t.TempDir()
	rigDir := filepath.Join(cityDir, "legacy-rig")
	archive := filepath.Join(cityDir, "archive")
	for _, dir := range []string{filepath.Join(rigDir, ".beads"), filepath.Join(archive, "lr")} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	for path, body := range map[string]string{
		filepath.Join(rigDir, ".beads", "metadata.json"): `{"database":"dolt","backend":"dolt","dolt_mode":"server","dolt_database":"lr"}`,
		filepath.Join(rigDir, ".beads", "identity.toml"): "[project]\nid = \"p-legacy\"\n",
		filepath.Join(rigDir, ".beads", "config.yaml"):   "gc.endpoint_origin: inherited_city\nexport.auto: false\n",
		filepath.Join(archive, "lr", "issues.jsonl"):     "{}\n\n",
		filepath.Join(archive, "lr.jsonl"):               "{}\n\n",
	} {
		if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("GC_JSONL_ARCHIVE_REPO", archive)

	rig := config.Rig{Name: "legacy-rig", Path: rigDir}
	check := doctor.NewRigDataPresenceCheck(cityDir, rig, func(string) (beads.Store, error) {
		return beads.NewMemStore(), nil
	}).WithArchiveExportRows(archiveExportRowsFor(cityDir))
	res := check.Run(&doctor.CheckContext{CityPath: cityDir})
	if res.Status != doctor.StatusOK {
		t.Fatalf("data-presence on a fresh legacy-managed rig = %v (%s), want OK", res.Status, res.Message)
	}
}

package main

import (
	"os"
	"path/filepath"
	"testing"
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

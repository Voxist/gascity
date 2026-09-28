package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"

	"github.com/gastownhall/gascity/internal/beads/contract"
	"github.com/gastownhall/gascity/internal/doctor"
	"github.com/gastownhall/gascity/internal/fsys"
)

// archiveExportRowsFor returns a lookup that reports how many rows a rig's
// durable archived export holds. It is injected into RigDataPresenceCheck so
// that check does not have to learn where a pack keeps its state.
//
// Why a second oracle exists at all: on a gc-managed scope the local
// .beads/issues.jsonl is deliberately removed (reapStaleBdExportJSONL, and
// again after EnsureCanonicalConfig writes export.auto=false), because the
// re-import cycle stalls bd writes on large datasets. That is the exact
// configuration the data-presence check targets, so without a durable source
// its blocking branch can never fire.
//
// The archive repo is maintained by the core pack's jsonl-export script as a
// git working tree, one directory per database, and is not reaped. Reading the
// checked-out file is enough — no git invocation, so a doctor run stays cheap
// and cannot hang on a repo lock.
func archiveExportRowsFor(cityPath string) func(rigPath string) (int, bool) {
	return func(rigPath string) (int, bool) {
		// ReadDoltDatabase takes the metadata.json path, not the scope root.
		db, ok, err := contract.ReadDoltDatabase(fsys.OSFS{}, filepath.Join(rigPath, ".beads", "metadata.json"))
		if err != nil || !ok || strings.TrimSpace(db) == "" {
			return 0, false
		}
		for _, repo := range jsonlArchiveRepoCandidates(os.Getenv, cityPath) {
			// Per-database directory first, then the flattened mirror the export
			// script also writes; either is a valid record that the rig held rows.
			for _, candidate := range []string{
				filepath.Join(repo, db, "issues.jsonl"),
				filepath.Join(repo, db+".jsonl"),
			} {
				if n, err := countArchiveExportRows(candidate); err == nil && n > 0 {
					return n, true
				}
			}
		}
		return 0, false
	}
}

// countArchiveExportRows counts the rows in one archived export file. The
// jsonl-export script writes each file as a single `dolt sql -r json` payload,
// {"rows":[...]}, on one line — despite the .jsonl name — so a line count
// would report 1 for any non-empty table and the deficit comparison against
// the live store could never fire. The rows array is the record count the
// script itself uses (count_jsonl_rows). A file that is not that shape is
// counted as newline-delimited JSON, the way the local issues.jsonl is.
func countArchiveExportRows(path string) (int, error) {
	f, err := os.Open(path)
	if err != nil {
		return 0, err
	}
	var payload struct {
		Rows []json.RawMessage `json:"rows"`
	}
	decodeErr := json.NewDecoder(f).Decode(&payload)
	f.Close() //nolint:errcheck // read-only
	if decodeErr == nil && payload.Rows != nil {
		return len(payload.Rows), nil
	}
	return doctor.CountJSONLLines(path)
}

// jsonlArchiveRepoCandidates mirrors the archive-repo precedence in the core
// pack's jsonl-export script, most current first. An explicit
// GC_JSONL_ARCHIVE_REPO override replaces the search entirely. Kept in this
// package because it encodes pack layout, which internal/doctor must not
// depend on, and shared with jsonlArchiveDoctorCheck.resolveArchiveRepo so the
// two readers of the archive cannot drift from the script or each other.
//
// getenv is passed in rather than read directly so callers that already own an
// env seam (the doctor check) keep using it.
func jsonlArchiveRepoCandidates(getenv func(string) string, cityPath string) []string {
	if override := strings.TrimSpace(getenv("GC_JSONL_ARCHIVE_REPO")); override != "" {
		return []string{override}
	}
	runtime := strings.TrimSpace(getenv("GC_CITY_RUNTIME_DIR"))
	if runtime == "" {
		runtime = filepath.Join(cityPath, ".gc", "runtime")
	}
	base := strings.TrimSpace(getenv("GC_PACK_STATE_DIR"))
	if base == "" {
		base = filepath.Join(runtime, "packs", "core")
	}
	return []string{
		filepath.Join(base, "jsonl-archive"),
		// Legacy locations, still present on cities that predate the core pack
		// split. Checked after the current path so a live archive always wins.
		filepath.Join(runtime, "packs", "maintenance", "jsonl-archive"),
		filepath.Join(cityPath, ".gc", "jsonl-archive"),
	}
}

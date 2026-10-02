package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// ga-2hgoz. This is not the race the bead's title implies. bd's own
// --force/--reinit-local init arm bounds its internal schema migration
// attempt (measured ~5s; TESTING.md documents ~30s as what a full migration
// needs under load), so a forced reinit can return success with its schema
// behind. wait_for_bd_runtime_schema only confirms the `config` table is
// queryable (bd_runtime_schema_state's `SELECT 1 FROM config LIMIT 1`); it
// has no notion of migration version, so it reports "ready" over exactly
// that half-migrated state. A later bd client on the same shared server then
// opens the database, sees a schema behind the binary it's running, and
// refuses with beads' #5920 shared-server guard ("refusing to auto-apply N
// pending schema migrations to a shared server database"). The two existing
// finish_bd_schema_migrations call sites (the metadata-present adopt branch
// and the GC_SCOPE_METADATA_PRESEEDED branch) already close this gap for
// their paths; the forced-reinit fall-through at the bottom of op_init did
// not call it at all. These tests drive that exact fall-through with a fake
// bd whose `init --force` succeeds but leaves the schema behind (simulating
// the internal bound), the way TestOpHealthPropagatesQueryProbeExitCode
// drives op_health against a stub.
//
// writeFakeBdForFinishMigrationTest and writeFakeDoltForFinishMigrationTest
// are shared by both tests below.

// writeFakeDoltForFinishMigrationTest installs a dolt stub whose `config`
// table answers "table not found" until initMarker exists, modeling bd
// init's own schema becoming queryable only once the (simulated) forced
// reinit has run. information_schema.tables always reports zero bd tables,
// which is what authorizes the forced-reinit branch in the first place
// (bd_runtime_store_holds_bd_tables must answer "absent", not "unknown", or
// probe_schema_state_or_die refuses instead of reinitializing).
func writeFakeDoltForFinishMigrationTest(t *testing.T, binDir, initMarker string) {
	t.Helper()
	body := fmt.Sprintf(`#!/bin/sh
set -eu
query=""
prev=""
for arg in "$@"; do
  if [ "$prev" = "-q" ]; then
    query="$arg"
    break
  fi
  prev="$arg"
done
case "$query" in
  *information_schema.tables*)
    printf 'cnt\n0\n'
    exit 0
    ;;
  *"FROM config"*)
    if [ -f %q ]; then
      printf '1\n1\n'
      exit 0
    fi
    echo "table not found: config" >&2
    exit 1
    ;;
  *)
    exit 0
    ;;
esac
`, initMarker)
	if err := os.WriteFile(filepath.Join(binDir, "dolt"), []byte(body), 0o755); err != nil {
		t.Fatalf("write fake dolt: %v", err)
	}
}

// writeFakeBdForFinishMigrationTest installs a bd stub that answers `init`
// by writing initMarker (flipping the fake dolt's config-table answer, the
// way a real forced reinit creates the table) and answers `migrate schema`
// by recording the call in migrateMarker and exiting with migrateExit,
// printing migrateStderr first when migrateExit is non-zero.
func writeFakeBdForFinishMigrationTest(t *testing.T, binDir, initMarker, migrateMarker string, migrateExit int, migrateStderr string) {
	t.Helper()
	body := fmt.Sprintf(`#!/bin/sh
set -eu
case "${1:-}" in
  init)
    printf '%%s\n' "$@" > %q
    exit 0
    ;;
  migrate)
    if [ "${2:-}" = "schema" ]; then
      printf '%%s\n' "$@" > %q
      if [ %d -ne 0 ]; then
        printf '%%s\n' %q >&2
        exit %d
      fi
      exit 0
    fi
    exit 0
    ;;
esac
exit 0
`, initMarker, migrateMarker, migrateExit, migrateStderr, migrateExit)
	if err := os.WriteFile(filepath.Join(binDir, "bd"), []byte(body), 0o755); err != nil {
		t.Fatalf("write fake bd: %v", err)
	}
}

// setUpForcedReinitCity writes a scope that already looks initialized
// (metadata.json present, dolt_database "hq") so op_init takes the
// metadata-present branch, finds the schema missing (via the fake dolt
// above) and falls through to the forced run_bd_init_pinned call --
// precisely the path that used to skip finish_bd_schema_migrations.
func setUpForcedReinitCity(t *testing.T) string {
	t.Helper()
	cityPath := t.TempDir()
	if err := os.MkdirAll(filepath.Join(cityPath, ".gc"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(cityPath, ".beads"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cityPath, ".beads", "metadata.json"),
		[]byte(`{"database":"dolt","backend":"dolt","dolt_mode":"server","dolt_database":"hq"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	materializeBuiltinPacksForTest(t, cityPath)
	return cityPath
}

func runForcedReinitInit(t *testing.T, cityPath, binDir string) (string, string, error) {
	t.Helper()
	script := gcBeadsBdScriptPath(cityPath)
	return runGCBeadsBdCommand(t, sanitizedBaseEnv(append(gcBeadsBdTestHomeEnv(t),
		"GC_CITY_PATH="+cityPath,
		"PATH="+strings.Join([]string{binDir, os.Getenv("PATH")}, string(os.PathListSeparator)),
	)...), script, "init", cityPath, "gc", "hq")
}

// TestGcBeadsBdInitFinishesSchemaMigrationsAfterForcedReinit is the ga-2hgoz
// regression guard. Before the fix, op_init's forced-reinit fall-through
// never called finish_bd_schema_migrations, so a fake bd whose `init --force`
// leaves the schema behind (the bound this bead is about) was reported
// ready without ever having its migrations finished -- this test's
// migrateMarker assertion is RED against the pre-fix script and GREEN
// after examples/bd/assets/scripts/gc-beads-bd.sh gained the call.
func TestGcBeadsBdInitFinishesSchemaMigrationsAfterForcedReinit(t *testing.T) {
	cityPath := setUpForcedReinitCity(t)

	binDir := filepath.Join(t.TempDir(), "bin")
	if err := os.MkdirAll(binDir, 0o755); err != nil {
		t.Fatal(err)
	}
	initMarker := filepath.Join(t.TempDir(), "bd-init-ran")
	migrateMarker := filepath.Join(t.TempDir(), "bd-migrate-ran")

	writeFakeDoltForFinishMigrationTest(t, binDir, initMarker)
	writeFakeBdForFinishMigrationTest(t, binDir, initMarker, migrateMarker, 0, "")
	if err := os.WriteFile(filepath.Join(binDir, "sleep"), []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatal(err)
	}

	stdout, stderr, err := runForcedReinitInit(t, cityPath, binDir)
	out := stdout + stderr
	if err != nil {
		t.Fatalf("init with a forced reinit whose schema migration succeeds = %v, want success:\n%s", err, out)
	}
	if _, statErr := os.Stat(initMarker); statErr != nil {
		t.Fatalf("fake bd init never ran; this test did not exercise the forced-reinit path it means to: %v\n%s", statErr, out)
	}
	if _, statErr := os.Stat(migrateMarker); statErr != nil {
		t.Fatalf("bd migrate schema was never run after the forced reinit (ga-2hgoz gap): %v\n%s", statErr, out)
	}
}

// TestGcBeadsBdInitFailsWhenPostForcedReinitMigrationFails is ga-2hgoz's
// constraint that an unbounded `bd migrate schema` is fine, but a FAILED one
// must surface as a clear, non-zero init failure -- not be swallowed by
// whatever runs after it (custom-types/prefix/identity configuration,
// normalize_scope_after_init). The failure here is a generic error, not one
// of the #4259 remote-migrate-gate shapes finish_bd_schema_migrations
// already treats as a graceful, non-fatal degradation.
func TestGcBeadsBdInitFailsWhenPostForcedReinitMigrationFails(t *testing.T) {
	cityPath := setUpForcedReinitCity(t)

	binDir := filepath.Join(t.TempDir(), "bin")
	if err := os.MkdirAll(binDir, 0o755); err != nil {
		t.Fatal(err)
	}
	initMarker := filepath.Join(t.TempDir(), "bd-init-ran")
	migrateMarker := filepath.Join(t.TempDir(), "bd-migrate-ran")

	writeFakeDoltForFinishMigrationTest(t, binDir, initMarker)
	writeFakeBdForFinishMigrationTest(t, binDir, initMarker, migrateMarker, 1, "simulated migration failure: disk full")
	if err := os.WriteFile(filepath.Join(binDir, "sleep"), []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatal(err)
	}

	stdout, stderr, err := runForcedReinitInit(t, cityPath, binDir)
	out := stdout + stderr
	if err == nil {
		t.Fatalf("init with a forced reinit whose schema migration FAILS = nil error, want a failure:\n%s", out)
	}
	if _, statErr := os.Stat(migrateMarker); statErr != nil {
		t.Fatalf("bd migrate schema was never attempted, so this did not exercise the failure path: %v\n%s", statErr, out)
	}
	if !strings.Contains(out, "failed to complete bd schema migrations for database 'hq'") {
		t.Fatalf("migration failure was not surfaced with finish_bd_schema_migrations' own message:\n%s", out)
	}
}

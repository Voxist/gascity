package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	gcapi "github.com/gastownhall/gascity/internal/api"
	"github.com/gastownhall/gascity/internal/beads/contract"
	"github.com/gastownhall/gascity/internal/events"
	"github.com/gastownhall/gascity/internal/fsys"
)

// projectIdentityRecordingRecorder captures emitted events for assertions.
//
// MERGE INTENT (v1.4.0 resync): fork-only helper (absent upstream). The merge
// kept its four call sites but dropped the definition, so the package no longer
// compiled. Restored verbatim from fork/main.
type projectIdentityRecordingRecorder struct {
	records []events.Event
}

func (r *projectIdentityRecordingRecorder) Record(e events.Event) {
	r.records = append(r.records, e)
}

func TestEnsureProjectIDCmdRequiresCityFlag(t *testing.T) {
	var stdout, stderr bytes.Buffer
	cmd := newEnsureProjectIDCmd(&stdout, &stderr)
	metadataPath := filepath.Join(t.TempDir(), ".beads", "metadata.json")
	cmd.SetArgs([]string{
		"--metadata", metadataPath,
		"--port", "3306",
		"--database", "hq",
	})

	err := cmd.Execute()
	if err == nil {
		t.Fatal("ensure-project-id without --city succeeded")
	}
	if !strings.Contains(err.Error(), `required flag(s) "city" not set`) {
		t.Fatalf("ensure-project-id error = %v, want required --city", err)
	}
}

func writeProjectIDMetadataFile(t *testing.T, scopeRoot string, projectID string) string {
	t.Helper()
	beadsDir := filepath.Join(scopeRoot, ".beads")
	if err := os.MkdirAll(beadsDir, 0o700); err != nil {
		t.Fatal(err)
	}
	meta := map[string]any{
		"backend":       "dolt",
		"database":      "dolt",
		"dolt_database": "hq",
		"dolt_mode":     "server",
	}
	if projectID != "" {
		meta["project_id"] = projectID
	}
	encoded, err := json.MarshalIndent(meta, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	encoded = append(encoded, '\n')
	metadataPath := filepath.Join(beadsDir, "metadata.json")
	if err := os.WriteFile(metadataPath, encoded, 0o644); err != nil {
		t.Fatal(err)
	}
	return metadataPath
}

func startProjectIDTestServer(t *testing.T, setupQueries ...string) (string, func()) {
	t.Helper()
	repoDir := filepath.Join(t.TempDir(), "hq")
	_, port, _, cleanup := startPasswordedDoltServer(t, repoDir, setupQueries...)
	return fmt.Sprintf("%d", port), cleanup
}

func seedDatabaseProjectIDQueries(projectID string) []string {
	return []string{
		"CREATE TABLE IF NOT EXISTS metadata (`key` VARCHAR(255) PRIMARY KEY, value LONGTEXT)",
		fmt.Sprintf("INSERT INTO metadata (`key`, value) VALUES ('_project_id', '%s') ON DUPLICATE KEY UPDATE value = VALUES(value)", projectID),
	}
}

// nativeStorageFixtureBootTimeout bounds fixture cold-boot of a passworded
// Dolt server before beads.OpenNativeStorage. Isolated cost is ~4s; 60s
// leaves headroom for shard-parallel host contention (ga-uswva7).
const nativeStorageFixtureBootTimeout = 60 * time.Second

// TestNativeStorageFixtureBootTimeoutSurvivesShardContention guards against
// ga-uswva7: a 15s budget left ~11s margin over the ~4s isolated cold-boot,
// which shard-parallel contention intermittently exceeded.
func TestNativeStorageFixtureBootTimeoutSurvivesShardContention(t *testing.T) {
	const minSafeBootTimeout = 60 * time.Second
	if nativeStorageFixtureBootTimeout < minSafeBootTimeout {
		t.Fatalf("nativeStorageFixtureBootTimeout = %s, want >= %s (see ga-uswva7: isolated cold-boot ~4s, shard-parallel contention intermittently exceeded 15s)",
			nativeStorageFixtureBootTimeout, minSafeBootTimeout)
	}
}

func startPasswordedDoltServer(t *testing.T, repoDir string, setupQueries ...string) (string, int, int, func()) {
	t.Helper()
	skipSlowCmdGCTest(t, "requires a real Dolt server; run make test-cmd-gc-process for full coverage")
	configureTestDoltIdentityEnv(t)

	doltPath := os.Getenv("GC_DOLT_REAL_BINARY")
	var err error
	if doltPath == "" {
		doltPath, err = exec.LookPath("dolt")
		if err != nil {
			t.Skip("dolt not installed")
		}
	}
	if repoDir == "" {
		repoDir = t.TempDir()
	}
	if err := os.MkdirAll(repoDir, 0o755); err != nil {
		t.Fatalf("MkdirAll(%s): %v", repoDir, err)
	}

	run := func(args ...string) {
		t.Helper()
		cmd := exec.Command(doltPath, args...)
		cmd.Dir = repoDir
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("dolt %s: %v\n%s", strings.Join(args, " "), err, out)
		}
	}

	run("init")
	for _, query := range setupQueries {
		run("sql", "-q", query)
	}
	run("sql", "-q", "CREATE USER 'root'@'%' IDENTIFIED BY 'secret'; GRANT ALL ON *.* TO 'root'@'%';")

	port := reserveRandomTCPPort(t)
	cmd := exec.Command(doltPath, "sql-server", "--host", "127.0.0.1", "--port", fmt.Sprintf("%d", port), "--allow-cleartext-passwords", "--loglevel=warning")
	cmd.Dir = repoDir
	cmd.Stdout = os.Stderr
	cmd.Stderr = os.Stderr
	if err := cmd.Start(); err != nil {
		t.Fatalf("start passworded dolt sql-server: %v", err)
	}

	t.Setenv("GC_DOLT_PASSWORD", "secret")
	deadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) {
		if err := managedDoltQueryProbeDirect("127.0.0.1", fmt.Sprintf("%d", port), "root"); err == nil {
			cleanup := func() {
				if cmd.Process != nil {
					_ = cmd.Process.Kill()
				}
				_, _ = cmd.Process.Wait()
			}
			return repoDir, port, cmd.Process.Pid, cleanup
		}
		time.Sleep(250 * time.Millisecond)
	}

	_ = cmd.Process.Kill()
	_, _ = cmd.Process.Wait()
	t.Fatalf("passworded dolt sql-server on %d did not become query-ready", port)
	return "", 0, 0, func() {}
}

func TestManagedDoltHealthCheckWithPasswordUsesDirectHelpersAgainstRealServer(t *testing.T) {
	binDir := t.TempDir()
	realDolt, err := exec.LookPath("dolt")
	if err != nil {
		t.Skip("dolt not installed")
	}
	t.Setenv("GC_DOLT_REAL_BINARY", realDolt)
	fakeDolt := filepath.Join(binDir, "dolt")
	if err := os.WriteFile(fakeDolt, []byte("#!/bin/sh\nexit 99\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))

	_, port, _, cleanup := startPasswordedDoltServer(t, "")
	defer cleanup()

	report, err := managedDoltHealthCheck("0.0.0.0", fmt.Sprintf("%d", port), "root", true)
	if err != nil {
		t.Fatalf("managedDoltHealthCheck() error = %v", err)
	}
	if !report.QueryReady || report.ReadOnly != "false" {
		t.Fatalf("managedDoltHealthCheck() = %+v, want query-ready writable server", report)
	}
	if report.ConnectionCount == "" {
		t.Fatalf("managedDoltHealthCheck() = %+v, want connection count", report)
	}
}

func TestManagedDoltWaitReadyWithPasswordUsesDirectQueryProbe(t *testing.T) {
	binDir := t.TempDir()
	realDolt, err := exec.LookPath("dolt")
	if err != nil {
		t.Skip("dolt not installed")
	}
	t.Setenv("GC_DOLT_REAL_BINARY", realDolt)
	fakeDolt := filepath.Join(binDir, "dolt")
	if err := os.WriteFile(fakeDolt, []byte("#!/bin/sh\nexit 99\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))

	repoDir, port, pid, cleanup := startPasswordedDoltServer(t, "")
	defer cleanup()

	report, err := waitForManagedDoltReady(repoDir, "0.0.0.0", fmt.Sprintf("%d", port), "root", pid, 5*time.Second, false)
	if err != nil {
		t.Fatalf("waitForManagedDoltReady() error = %v", err)
	}
	if !report.Ready || !report.PIDAlive {
		t.Fatalf("waitForManagedDoltReady() = %+v, want ready pid_alive", report)
	}
}

func TestRecoverManagedDoltProcessWithPasswordReusesHealthyRealServer(t *testing.T) {
	skipSlowCmdGCTest(t, "requires a managed dolt server; run make test-cmd-gc-process for full coverage")
	cityPath := t.TempDir()
	layout, err := resolveManagedDoltRuntimeLayout(cityPath)
	if err != nil {
		t.Fatalf("resolveManagedDoltRuntimeLayout: %v", err)
	}
	if err := os.MkdirAll(layout.DataDir, 0o755); err != nil {
		t.Fatalf("MkdirAll(data dir): %v", err)
	}

	_, port, pid, cleanup := startPasswordedDoltServer(t, layout.DataDir, "CREATE DATABASE IF NOT EXISTS `hq`")
	defer cleanup()
	t.Cleanup(func() {
		if state, err := readDoltRuntimeStateFile(layout.StateFile); err == nil && state.PID > 0 {
			_ = terminateManagedDoltPID("", state.PID)
		}
	})

	if err := os.MkdirAll(filepath.Dir(layout.PIDFile), 0o755); err != nil {
		t.Fatalf("MkdirAll(runtime dir): %v", err)
	}
	if err := os.WriteFile(layout.PIDFile, []byte(fmt.Sprintf("%d\n", pid)), 0o644); err != nil {
		t.Fatalf("WriteFile(pid): %v", err)
	}
	if err := writeDoltRuntimeStateFile(layout.StateFile, doltRuntimeState{
		Running:   true,
		PID:       pid,
		Port:      port,
		DataDir:   layout.DataDir,
		StartedAt: time.Now().UTC().Format(time.RFC3339),
	}); err != nil {
		t.Fatalf("writeDoltRuntimeStateFile: %v", err)
	}

	report, err := recoverManagedDoltProcess(cityPath, "127.0.0.1", fmt.Sprintf("%d", port), "root", "warning", 10*time.Second)
	if err != nil {
		t.Fatalf("recoverManagedDoltProcess() error = %v", err)
	}
	if !report.Ready || !report.Healthy {
		t.Fatalf("recoverManagedDoltProcess() = %+v, want ready healthy", report)
	}
	if !report.HadPID {
		t.Fatalf("recoverManagedDoltProcess() HadPID = false, want true")
	}
	if report.PID != pid {
		t.Fatalf("recoverManagedDoltProcess() pid = %d, want reused pid %d", report.PID, pid)
	}
	if report.Port != port {
		t.Fatalf("recoverManagedDoltProcess() port = %d, want %d", report.Port, port)
	}
	if report.Restarted {
		t.Fatalf("recoverManagedDoltProcess() Restarted = true, want false")
	}
}

func TestProjectIdentityL3AdapterContractAndManagedComposition(t *testing.T) {
	skipSlowCmdGCTest(t, "requires a managed dolt server; run make test-cmd-gc-process for full coverage")
	cityDir := t.TempDir()
	repoDir := filepath.Join(cityDir, "hq")
	_, port, _, cleanup := startPasswordedDoltServer(t, repoDir)
	defer cleanup()
	portString := fmt.Sprintf("%d", port)

	db, err := managedDoltOpenDatabase("127.0.0.1", portString, "root", "hq")
	if err != nil {
		t.Fatalf("managedDoltOpenDatabase: %v", err)
	}
	defer func() { _ = db.Close() }()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := db.PingContext(ctx); err != nil {
		t.Fatalf("PingContext with password: %v", err)
	}

	runProjectIdentityL3SeedContract(
		t,
		func(ctx context.Context) (string, bool, error) {
			return readDatabaseProjectID(ctx, db)
		},
		func(ctx context.Context, projectID string) (bool, error) {
			return seedDatabaseProjectID(ctx, db, projectID)
		},
	)

	if _, err := db.ExecContext(ctx, "DELETE FROM metadata WHERE `key` = '_project_id'"); err != nil {
		t.Fatalf("delete database _project_id: %v", err)
	}
	if projectID, ok, err := readDatabaseProjectID(ctx, db); err != nil || ok || projectID != "" {
		t.Fatalf("L3 after contract reset = (%q, %v, %v), want absent", projectID, ok, err)
	}

	scopeRoot := filepath.Join(cityDir, "rigs", "demo")
	metadataPath := writeProjectIDMetadataFile(t, scopeRoot, "composition-id")
	if err := contract.WriteProjectIdentity(fsys.OSFS{}, scopeRoot, "composition-id"); err != nil {
		t.Fatalf("WriteProjectIdentity: %v", err)
	}
	recorder := &projectIdentityApplyRecordingRecorder{}
	report, err := ensureManagedDoltProjectIDWithRecorder(metadataPath, "127.0.0.1", portString, "root", "hq", cityDir, recorder)
	if err != nil {
		t.Fatalf("ensureManagedDoltProjectIDWithRecorder: %v", err)
	}
	wantReport := managedDoltProjectIDReport{
		ProjectID:       "composition-id",
		DatabaseUpdated: true,
		Source:          "l3-seed",
		Layer:           "l1",
	}
	if report != wantReport {
		t.Fatalf("report = %+v, want %+v", report, wantReport)
	}
	assertProjectIdentityApplyStampedEvents(t, recorder.records, []projectIdentityApplyStampedEvent{
		{source: "cache_repair", layer: "L3", newID: "composition-id"},
	})
	l1, l1OK, err := contract.ReadProjectIdentity(fsys.OSFS{}, scopeRoot)
	if err != nil {
		t.Fatalf("ReadProjectIdentity: %v", err)
	}
	l2, err := readManagedMetadataProjectID(metadataPath)
	if err != nil {
		t.Fatalf("readManagedMetadataProjectID: %v", err)
	}
	l3, l3OK, err := readDatabaseProjectID(ctx, db)
	if err != nil {
		t.Fatalf("readDatabaseProjectID: %v", err)
	}
	if !l1OK || !l3OK || l1 != "composition-id" || l2 != "composition-id" || l3 != "composition-id" {
		t.Fatalf("composition state = (L1:%q/%v L2:%q L3:%q/%v), want composition-id in all layers", l1, l1OK, l2, l3, l3OK)
	}
}

// TestEnsureProjectIDRestoresFromCityIdentityMap covers the L0 pre-heal path
// added by vp-cz7o.21. Requires a managed Dolt server.
func TestEnsureProjectIDRestoresFromCityIdentityMap(t *testing.T) {
	skipSlowCmdGCTest(t, "requires a managed dolt server; run make test-cmd-gc-process for full coverage")

	const canonicalID = "c3c9af4a-0000-0000-0000-000000000000"
	const differentID = "different-uuid-9999-0000-000000000000"

	writeCityTOMLForRig := func(t *testing.T, cityDir, rigName, rigPath, projectID string) {
		t.Helper()
		content := "[[rigs]]\nname = \"" + rigName + "\"\npath = \"" + rigPath + "\"\n\n" +
			"[identity_map]\n" + rigName + " = { project_id = \"" + projectID + "\" }\n"
		if err := os.WriteFile(filepath.Join(cityDir, "city.toml"), []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	t.Run("case1_L1_absent_L3_matches_L0_repairs_L1_L2", func(t *testing.T) {
		// L1 absent, L3==L0 → pre-heal writes L1 and L2; reconcile returns NoOp,
		// but the report must still say both files were rewritten from L0.
		scopeRoot := t.TempDir()
		cityDir := t.TempDir()
		writeCityTOMLForRig(t, cityDir, "my-rig", scopeRoot, canonicalID)
		metadataPath := writeProjectIDMetadataFile(t, scopeRoot, "")
		port, cleanup := startProjectIDTestServer(t, seedDatabaseProjectIDQueries(canonicalID)...)
		defer cleanup()

		rec := &projectIdentityRecordingRecorder{}
		report, err := ensureManagedDoltProjectIDWithRecorder(metadataPath, "127.0.0.1", port, "root", "hq", cityDir, rec)
		if err != nil {
			t.Fatalf("ensureManagedDoltProjectIDWithRecorder: %v", err)
		}
		wantReport := managedDoltProjectIDReport{
			ProjectID:           canonicalID,
			IdentityFileUpdated: true,
			MetadataUpdated:     true,
			Source:              "restored_from_canonical",
			Layer:               "l0",
		}
		if report != wantReport {
			t.Fatalf("report = %+v, want %+v", report, wantReport)
		}
		assertProjectIdentityFile(t, scopeRoot, canonicalID)
		assertMetadataProjectID(t, metadataPath, canonicalID)
		assertDatabaseProjectID(t, port, canonicalID)

		payloads := decodeProjectIdentityStampedPayloads(t, rec.records)
		if len(payloads) != 1 {
			t.Fatalf("emitted %d event(s), want exactly one restored_from_canonical: %+v", len(payloads), payloads)
		}
		if p := payloads[0]; p.Source != "restored_from_canonical" || p.Layer != "L0" || p.OldID != "" || p.NewID != canonicalID {
			t.Fatalf("event = %+v, want restored_from_canonical L0 -> %s", p, canonicalID)
		}
	})

	t.Run("case1b_L1_regenerated_wrong_L3_matches_L0_repairs_L1_L2", func(t *testing.T) {
		// L1 and L2 were regenerated to a wrong value, L3==L0 → pre-heal
		// overwrites both from L0 and the report reflects the rewrite.
		scopeRoot := t.TempDir()
		cityDir := t.TempDir()
		writeCityTOMLForRig(t, cityDir, "my-rig", scopeRoot, canonicalID)
		metadataPath := writeProjectIDMetadataFile(t, scopeRoot, differentID)
		if err := contract.WriteProjectIdentity(fsys.OSFS{}, scopeRoot, differentID); err != nil {
			t.Fatalf("WriteProjectIdentity: %v", err)
		}
		port, cleanup := startProjectIDTestServer(t, seedDatabaseProjectIDQueries(canonicalID)...)
		defer cleanup()

		rec := &projectIdentityRecordingRecorder{}
		report, err := ensureManagedDoltProjectIDWithRecorder(metadataPath, "127.0.0.1", port, "root", "hq", cityDir, rec)
		if err != nil {
			t.Fatalf("ensureManagedDoltProjectIDWithRecorder: %v", err)
		}
		wantReport := managedDoltProjectIDReport{
			ProjectID:           canonicalID,
			IdentityFileUpdated: true,
			MetadataUpdated:     true,
			Source:              "restored_from_canonical",
			Layer:               "l0",
		}
		if report != wantReport {
			t.Fatalf("report = %+v, want %+v", report, wantReport)
		}
		assertProjectIdentityFile(t, scopeRoot, canonicalID)
		assertMetadataProjectID(t, metadataPath, canonicalID)

		payloads := decodeProjectIdentityStampedPayloads(t, rec.records)
		if len(payloads) != 1 {
			t.Fatalf("emitted %d event(s), want exactly one restored_from_canonical: %+v", len(payloads), payloads)
		}
		if p := payloads[0]; p.Source != "restored_from_canonical" || p.Layer != "L0" || p.OldID != differentID || p.NewID != canonicalID {
			t.Fatalf("event = %+v, want restored_from_canonical L0 %s -> %s", p, differentID, canonicalID)
		}
	})

	t.Run("case2_L3_disagrees_with_L0_refuses_and_writes_nothing", func(t *testing.T) {
		// L3≠L0 with L1/L2 absent is the 2026-06-20 wipe shape. The pre-heal
		// must emit canonical_l3_mismatch and refuse, so reconcile never gets
		// the chance to adopt the non-canonical L3 id into L1/L2.
		scopeRoot := t.TempDir()
		cityDir := t.TempDir()
		writeCityTOMLForRig(t, cityDir, "my-rig", scopeRoot, canonicalID)
		metadataPath := writeProjectIDMetadataFile(t, scopeRoot, "")
		port, cleanup := startProjectIDTestServer(t, seedDatabaseProjectIDQueries(differentID)...)
		defer cleanup()

		rec := &projectIdentityRecordingRecorder{}
		report, err := ensureManagedDoltProjectIDWithRecorder(metadataPath, "127.0.0.1", port, "root", "hq", cityDir, rec)
		if err == nil {
			t.Fatalf("ensureManagedDoltProjectIDWithRecorder succeeded with report %+v, want canonical mismatch refusal", report)
		}
		for _, want := range []string{"canonical identity mismatch", scopeRoot, canonicalID, differentID, "human triage"} {
			if !strings.Contains(err.Error(), want) {
				t.Fatalf("error = %q, want it to mention %q", err, want)
			}
		}
		if report != (managedDoltProjectIDReport{}) {
			t.Fatalf("report = %+v, want zero report on refusal", report)
		}
		assertProjectIdentityFileAbsent(t, scopeRoot)
		assertMetadataProjectID(t, metadataPath, "")
		assertDatabaseProjectID(t, port, differentID)

		payloads := decodeProjectIdentityStampedPayloads(t, rec.records)
		if len(payloads) != 1 {
			t.Fatalf("emitted %d event(s), want exactly one canonical_l3_mismatch: %+v", len(payloads), payloads)
		}
		if p := payloads[0]; p.Source != "canonical_l3_mismatch" || p.Layer != "L0" || p.OldID != canonicalID || p.NewID != differentID {
			t.Fatalf("event = %+v, want canonical_l3_mismatch L0 %s vs %s", p, canonicalID, differentID)
		}
	})

	t.Run("case3_L1_already_matches_L0_noop", func(t *testing.T) {
		// L1==L0==L3 → pre-heal condition false; reconcile returns NoOp.
		scopeRoot := t.TempDir()
		cityDir := t.TempDir()
		writeCityTOMLForRig(t, cityDir, "my-rig", scopeRoot, canonicalID)
		metadataPath := writeProjectIDMetadataFile(t, scopeRoot, canonicalID)
		if err := contract.WriteProjectIdentity(fsys.OSFS{}, scopeRoot, canonicalID); err != nil {
			t.Fatalf("WriteProjectIdentity: %v", err)
		}
		port, cleanup := startProjectIDTestServer(t, seedDatabaseProjectIDQueries(canonicalID)...)
		defer cleanup()

		rec := &projectIdentityRecordingRecorder{}
		report, err := ensureManagedDoltProjectIDWithRecorder(metadataPath, "127.0.0.1", port, "root", "hq", cityDir, rec)
		if err != nil {
			t.Fatalf("ensureManagedDoltProjectIDWithRecorder: %v", err)
		}
		if report.ProjectID != canonicalID || report.Source != "match" {
			t.Fatalf("report = %+v, want match/noop", report)
		}
		if report.IdentityFileUpdated || report.MetadataUpdated || report.DatabaseUpdated {
			t.Fatalf("unexpected update on noop path: %+v", report)
		}
		if len(rec.records) != 0 {
			t.Fatalf("emitted %d event(s) on noop path, want none: %+v", len(rec.records), rec.records)
		}
	})

	t.Run("case4_no_identity_map_entry_preheal_skipped", func(t *testing.T) {
		// city.toml has no [identity_map] → pre-heal skips entirely.
		// Existing reconcile adopts from L3 (L1 absent, L2 absent, L3 present).
		scopeRoot := t.TempDir()
		cityDir := t.TempDir()
		if err := os.WriteFile(filepath.Join(cityDir, "city.toml"),
			[]byte("[workspace]\nname = \"demo\"\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		metadataPath := writeProjectIDMetadataFile(t, scopeRoot, "")
		port, cleanup := startProjectIDTestServer(t, seedDatabaseProjectIDQueries(canonicalID)...)
		defer cleanup()

		rec := &projectIdentityRecordingRecorder{}
		report, err := ensureManagedDoltProjectIDWithRecorder(metadataPath, "127.0.0.1", port, "root", "hq", cityDir, rec)
		if err != nil {
			t.Fatalf("ensureManagedDoltProjectIDWithRecorder: %v", err)
		}
		if report.ProjectID != canonicalID {
			t.Fatalf("report.ProjectID = %q, want %q (L3)", report.ProjectID, canonicalID)
		}
		for _, p := range decodeProjectIdentityStampedPayloads(t, rec.records) {
			if p.Source == "restored_from_canonical" {
				t.Fatal("restored_from_canonical emitted when no identity_map entry")
			}
		}
	})

	t.Run("case5_L3_absent_L1_absent_restores_from_L0_and_seeds_L3", func(t *testing.T) {
		// Re-init-to-empty shape: identity_map has the canonical id, the DB has
		// no project_id row, L1 is missing, L2 has none. The canonical id must
		// be restored into L1/L2 and seeded into L3 — never a freshly minted one.
		scopeRoot := t.TempDir()
		cityDir := t.TempDir()
		writeCityTOMLForRig(t, cityDir, "my-rig", scopeRoot, canonicalID)
		metadataPath := writeProjectIDMetadataFile(t, scopeRoot, "")
		port, cleanup := startProjectIDTestServer(t)
		defer cleanup()

		rec := &projectIdentityRecordingRecorder{}
		report, err := ensureManagedDoltProjectIDWithRecorder(metadataPath, "127.0.0.1", port, "root", "hq", cityDir, rec)
		if err != nil {
			t.Fatalf("ensureManagedDoltProjectIDWithRecorder: %v", err)
		}
		wantReport := managedDoltProjectIDReport{
			ProjectID:           canonicalID,
			IdentityFileUpdated: true,
			MetadataUpdated:     true,
			DatabaseUpdated:     true,
			Source:              "restored_from_canonical",
			Layer:               "l0",
		}
		if report != wantReport {
			t.Fatalf("report = %+v, want %+v", report, wantReport)
		}
		assertProjectIdentityFile(t, scopeRoot, canonicalID)
		assertMetadataProjectID(t, metadataPath, canonicalID)
		assertDatabaseProjectID(t, port, canonicalID)

		payloads := decodeProjectIdentityStampedPayloads(t, rec.records)
		if len(payloads) != 2 {
			t.Fatalf("emitted %d event(s), want restored_from_canonical then cache_repair L3: %+v", len(payloads), payloads)
		}
		if p := payloads[0]; p.Source != "restored_from_canonical" || p.Layer != "L0" || p.OldID != "" || p.NewID != canonicalID {
			t.Fatalf("event[0] = %+v, want restored_from_canonical L0 -> %s", p, canonicalID)
		}
		if p := payloads[1]; p.Source != "cache_repair" || p.Layer != "L3" || p.NewID != canonicalID {
			t.Fatalf("event[1] = %+v, want cache_repair L3 -> %s", p, canonicalID)
		}
	})

	t.Run("case5b_L3_absent_L1_absent_L2_matches_L0_restores_L1_only", func(t *testing.T) {
		// Same shape but metadata.json already carries the canonical id: L1 is
		// restored, L2 is left alone, and L3 is seeded.
		scopeRoot := t.TempDir()
		cityDir := t.TempDir()
		writeCityTOMLForRig(t, cityDir, "my-rig", scopeRoot, canonicalID)
		metadataPath := writeProjectIDMetadataFile(t, scopeRoot, canonicalID)
		port, cleanup := startProjectIDTestServer(t)
		defer cleanup()

		rec := &projectIdentityRecordingRecorder{}
		report, err := ensureManagedDoltProjectIDWithRecorder(metadataPath, "127.0.0.1", port, "root", "hq", cityDir, rec)
		if err != nil {
			t.Fatalf("ensureManagedDoltProjectIDWithRecorder: %v", err)
		}
		wantReport := managedDoltProjectIDReport{
			ProjectID:           canonicalID,
			IdentityFileUpdated: true,
			DatabaseUpdated:     true,
			Source:              "restored_from_canonical",
			Layer:               "l0",
		}
		if report != wantReport {
			t.Fatalf("report = %+v, want %+v", report, wantReport)
		}
		assertProjectIdentityFile(t, scopeRoot, canonicalID)
		assertDatabaseProjectID(t, port, canonicalID)
	})

	t.Run("case5c_L3_absent_L1_absent_L2_disagrees_with_L0_refuses", func(t *testing.T) {
		// With no L3 to break the tie, an L2 that disagrees with L0 is not
		// guessed at: refuse and write nothing.
		scopeRoot := t.TempDir()
		cityDir := t.TempDir()
		writeCityTOMLForRig(t, cityDir, "my-rig", scopeRoot, canonicalID)
		metadataPath := writeProjectIDMetadataFile(t, scopeRoot, differentID)
		port, cleanup := startProjectIDTestServer(t)
		defer cleanup()

		rec := &projectIdentityRecordingRecorder{}
		report, err := ensureManagedDoltProjectIDWithRecorder(metadataPath, "127.0.0.1", port, "root", "hq", cityDir, rec)
		if err == nil {
			t.Fatalf("ensureManagedDoltProjectIDWithRecorder succeeded with report %+v, want canonical mismatch refusal", report)
		}
		for _, want := range []string{"canonical identity mismatch", scopeRoot, canonicalID, differentID, "human triage"} {
			if !strings.Contains(err.Error(), want) {
				t.Fatalf("error = %q, want it to mention %q", err, want)
			}
		}
		assertProjectIdentityFileAbsent(t, scopeRoot)
		assertMetadataProjectID(t, metadataPath, differentID)
		assertDatabaseProjectIDAbsent(t, port)

		payloads := decodeProjectIdentityStampedPayloads(t, rec.records)
		if len(payloads) != 1 {
			t.Fatalf("emitted %d event(s), want exactly one canonical_l2_mismatch: %+v", len(payloads), payloads)
		}
		if p := payloads[0]; p.Source != "canonical_l2_mismatch" || p.Layer != "L0" || p.OldID != canonicalID || p.NewID != differentID {
			t.Fatalf("event = %+v, want canonical_l2_mismatch L0 %s vs %s", p, canonicalID, differentID)
		}
	})

	t.Run("case5d_L3_absent_L1_disagrees_with_L0_refuses", func(t *testing.T) {
		// L1 was regenerated to a wrong value and the DB is empty: with no L3
		// to break the tie, refuse rather than seed L3 from the wrong L1.
		scopeRoot := t.TempDir()
		cityDir := t.TempDir()
		writeCityTOMLForRig(t, cityDir, "my-rig", scopeRoot, canonicalID)
		metadataPath := writeProjectIDMetadataFile(t, scopeRoot, "")
		if err := contract.WriteProjectIdentity(fsys.OSFS{}, scopeRoot, differentID); err != nil {
			t.Fatalf("WriteProjectIdentity: %v", err)
		}
		port, cleanup := startProjectIDTestServer(t)
		defer cleanup()

		rec := &projectIdentityRecordingRecorder{}
		report, err := ensureManagedDoltProjectIDWithRecorder(metadataPath, "127.0.0.1", port, "root", "hq", cityDir, rec)
		if err == nil {
			t.Fatalf("ensureManagedDoltProjectIDWithRecorder succeeded with report %+v, want canonical mismatch refusal", report)
		}
		for _, want := range []string{"canonical identity mismatch", scopeRoot, canonicalID, "identity file says " + differentID, "human triage"} {
			if !strings.Contains(err.Error(), want) {
				t.Fatalf("error = %q, want it to mention %q", err, want)
			}
		}
		if report != (managedDoltProjectIDReport{}) {
			t.Fatalf("report = %+v, want zero report on refusal", report)
		}
		assertProjectIdentityFile(t, scopeRoot, differentID)
		assertMetadataProjectID(t, metadataPath, "")
		assertDatabaseProjectIDAbsent(t, port)

		payloads := decodeProjectIdentityStampedPayloads(t, rec.records)
		if len(payloads) != 1 {
			t.Fatalf("emitted %d event(s), want exactly one canonical_l1_mismatch: %+v", len(payloads), payloads)
		}
		if p := payloads[0]; p.Source != "canonical_l1_mismatch" || p.Layer != "L0" || p.OldID != canonicalID || p.NewID != differentID {
			t.Fatalf("event = %+v, want canonical_l1_mismatch L0 %s vs %s", p, canonicalID, differentID)
		}
	})

	t.Run("case7_stale_identity_map_with_consistent_layers_opens_the_store", func(t *testing.T) {
		// The map disagrees with the database, but L1==L2==L3 already agree:
		// this is a STALE [identity_map] entry (a re-mint, or a rig re-added
		// under a new id), and reconcile has nothing to write. Failing here
		// would strand a healthy rig with no operator override, so the drift is
		// reported as an event and the store opens.
		scopeRoot := t.TempDir()
		cityDir := t.TempDir()
		writeCityTOMLForRig(t, cityDir, "my-rig", scopeRoot, canonicalID)
		if err := contract.WriteProjectIdentity(fsys.OSFS{}, scopeRoot, differentID); err != nil {
			t.Fatal(err)
		}
		metadataPath := writeProjectIDMetadataFile(t, scopeRoot, differentID)
		port, cleanup := startProjectIDTestServer(t, seedDatabaseProjectIDQueries(differentID)...)
		defer cleanup()
		rec := &projectIdentityRecordingRecorder{}
		report, err := ensureManagedDoltProjectIDWithRecorder(metadataPath, "127.0.0.1", port, "root", "hq", cityDir, rec)
		if err != nil {
			t.Fatalf("ensureManagedDoltProjectIDWithRecorder refused a consistent scope over a stale identity_map entry: %v", err)
		}
		if report.ProjectID != differentID {
			t.Fatalf("report = %+v, want the scope's own consistent id %q", report, differentID)
		}
		if report.IdentityFileUpdated || report.MetadataUpdated {
			t.Fatalf("report = %+v, want no writes when nothing is pending", report)
		}
		assertProjectIdentityFile(t, scopeRoot, differentID)
		assertMetadataProjectID(t, metadataPath, differentID)
		assertDatabaseProjectID(t, port, differentID)
		payloads := decodeProjectIdentityStampedPayloads(t, rec.records)
		if len(payloads) != 1 || payloads[0].Source != "canonical_l3_mismatch" {
			t.Fatalf("events = %+v, want exactly one canonical_l3_mismatch so the drift stays visible", payloads)
		}
	})

	t.Run("case8_unreadable_city_toml_with_consistent_layers_opens_the_store", func(t *testing.T) {
		// city.toml is momentarily unreadable (city-config-reload pulls it every
		// 120s). With L1==L2==L3 agreeing there is nothing for reconcile to get
		// wrong, so a torn read must not fail this scope — otherwise one bad
		// read takes every managed store down at once, including scopes the map
		// never mentioned.
		scopeRoot := t.TempDir()
		cityDir := t.TempDir()
		if err := os.WriteFile(filepath.Join(cityDir, "city.toml"), []byte("[[rigs]\nname = \"broken"), 0o644); err != nil {
			t.Fatal(err)
		}
		if err := contract.WriteProjectIdentity(fsys.OSFS{}, scopeRoot, canonicalID); err != nil {
			t.Fatal(err)
		}
		metadataPath := writeProjectIDMetadataFile(t, scopeRoot, canonicalID)
		port, cleanup := startProjectIDTestServer(t, seedDatabaseProjectIDQueries(canonicalID)...)
		defer cleanup()
		rec := &projectIdentityRecordingRecorder{}
		report, err := ensureManagedDoltProjectIDWithRecorder(metadataPath, "127.0.0.1", port, "root", "hq", cityDir, rec)
		if err != nil {
			t.Fatalf("ensureManagedDoltProjectIDWithRecorder failed a consistent scope over an unreadable city.toml: %v", err)
		}
		if report.ProjectID != canonicalID || report.IdentityFileUpdated || report.MetadataUpdated {
			t.Fatalf("report = %+v, want the existing id and no writes", report)
		}
		payloads := decodeProjectIdentityStampedPayloads(t, rec.records)
		if len(payloads) != 1 || payloads[0].Source != "l0_read_error" {
			t.Fatalf("events = %+v, want exactly one l0_read_error so the unreadable map stays visible", payloads)
		}
	})

	t.Run("case6_city_toml_unreadable_fails_closed", func(t *testing.T) {
		// A malformed city.toml (e.g. mid-edit by city-config-reload) must not
		// let reconcile mint a fresh id for a rig whose canonical id may be
		// declared in the unreadable map.
		scopeRoot := t.TempDir()
		cityDir := t.TempDir()
		if err := os.WriteFile(filepath.Join(cityDir, "city.toml"), []byte("[[rigs]\nname = \"broken"), 0o644); err != nil {
			t.Fatal(err)
		}
		metadataPath := writeProjectIDMetadataFile(t, scopeRoot, "")
		port, cleanup := startProjectIDTestServer(t)
		defer cleanup()

		rec := &projectIdentityRecordingRecorder{}
		report, err := ensureManagedDoltProjectIDWithRecorder(metadataPath, "127.0.0.1", port, "root", "hq", cityDir, rec)
		if err == nil {
			t.Fatalf("ensureManagedDoltProjectIDWithRecorder succeeded with report %+v, want L0 read failure", report)
		}
		if !strings.Contains(err.Error(), "reading city identity map for "+scopeRoot) {
			t.Fatalf("error = %q, want it wrapped as a city identity map read failure", err)
		}
		if report != (managedDoltProjectIDReport{}) {
			t.Fatalf("report = %+v, want zero report on refusal", report)
		}
		assertProjectIdentityFileAbsent(t, scopeRoot)
		assertMetadataProjectID(t, metadataPath, "")
		assertDatabaseProjectIDAbsent(t, port)

		payloads := decodeProjectIdentityStampedPayloads(t, rec.records)
		if len(payloads) != 1 {
			t.Fatalf("emitted %d event(s), want exactly one l0_read_error: %+v", len(payloads), payloads)
		}
		if p := payloads[0]; p.Source != "l0_read_error" || p.Layer != "L0" || p.OldID != "" || p.NewID == "" || !strings.Contains(err.Error(), p.NewID) {
			t.Fatalf("event = %+v, want l0_read_error L0 carrying the read error text in new_id", p)
		}
	})
}

func assertProjectIdentityFileAbsent(t *testing.T, scopeRoot string) {
	t.Helper()
	got, ok, err := contract.ReadProjectIdentity(fsys.OSFS{}, scopeRoot)
	if err != nil {
		t.Fatalf("ReadProjectIdentity: %v", err)
	}
	if ok {
		t.Fatalf("identity project_id = %q, want absent", got)
	}
}

func assertDatabaseProjectIDAbsent(t *testing.T, port string) {
	t.Helper()
	db, err := managedDoltOpenDatabase("127.0.0.1", port, "root", "hq")
	if err != nil {
		t.Fatalf("managedDoltOpenDatabase: %v", err)
	}
	defer func() { _ = db.Close() }()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	got, ok, err := readDatabaseProjectID(ctx, db)
	if err != nil {
		t.Fatalf("readDatabaseProjectID: %v", err)
	}
	if ok {
		t.Fatalf("database _project_id = %q, want absent", got)
	}
}

// MERGE INTENT (v1.4.0 resync): fork-only test helpers (absent upstream) whose
// definitions the merge dropped while keeping their call sites. Restored from
// fork/main so the package compiles.

func assertDatabaseProjectID(t *testing.T, port string, want string) {
	t.Helper()
	db, err := managedDoltOpenDatabase("127.0.0.1", port, "root", "hq")
	if err != nil {
		t.Fatalf("managedDoltOpenDatabase: %v", err)
	}
	defer func() { _ = db.Close() }()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	got, ok, err := readDatabaseProjectID(ctx, db)
	if err != nil {
		t.Fatalf("readDatabaseProjectID: %v", err)
	}
	if !ok {
		t.Fatal("database project id missing")
	}
	if got != want {
		t.Fatalf("database _project_id = %q, want %q", got, want)
	}
}

func assertMetadataProjectID(t *testing.T, metadataPath string, want string) {
	t.Helper()
	got, err := readManagedMetadataProjectID(metadataPath)
	if err != nil {
		t.Fatalf("readManagedMetadataProjectID: %v", err)
	}
	if got != want {
		t.Fatalf("metadata project_id = %q, want %q", got, want)
	}
}

func assertProjectIdentityFile(t *testing.T, scopeRoot string, want string) {
	t.Helper()
	got, ok, err := contract.ReadProjectIdentity(fsys.OSFS{}, scopeRoot)
	if err != nil {
		t.Fatalf("ReadProjectIdentity: %v", err)
	}
	if !ok {
		t.Fatal("identity project id missing")
	}
	if got != want {
		t.Fatalf("identity project_id = %q, want %q", got, want)
	}
}

func decodeProjectIdentityStampedPayloads(t *testing.T, records []events.Event) []gcapi.ProjectIdentityStampedPayload {
	t.Helper()
	payloads := make([]gcapi.ProjectIdentityStampedPayload, 0, len(records))
	for i, record := range records {
		var payload gcapi.ProjectIdentityStampedPayload
		if err := json.Unmarshal(record.Payload, &payload); err != nil {
			t.Fatalf("unmarshal records[%d].Payload: %v", i, err)
		}
		payloads = append(payloads, payload)
	}
	return payloads
}

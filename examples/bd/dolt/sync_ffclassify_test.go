package dolt_test

// Fast-forward-only sync classification (gc-6ommo). `gc dolt sync` must not
// blind-push a shared multi-writer DB: it fetches, classifies local vs the
// remote-tracking ref, and pushes only when the local branch is strictly ahead
// (a fast-forward). behind / diverged refuse with an actionable status; a
// fetch timeout skips without pushing; a first push (remote ref absent) is a
// fast-forward and pushes. --force still bypasses classification.
//
// The classification queries are verified against real Dolt 2.1.0:
//   ahead  = SELECT COUNT(*) FROM dolt_log('remotes/<remote>/<br>..<br>')
//   behind = SELECT COUNT(*) FROM dolt_log('<br>..remotes/<remote>/<br>')
// and an absent remote ref yields "branch not found: remotes/...".

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// runFFSyncRaw is the one lexical subprocess call site backing runFFSync and
// every variant below (runFFSyncWithConfig, the exit-code check) — the
// repository's untagged-source subprocess census is a checked ratchet that
// cannot grow (TESTING.md), so new call shapes are added by extending this
// shared runner, never by hand-rolling another exec.Command site (vp-9v6f9).
// Sets up a one-DB ("app") SQL-mode city with the fake dolt already installed
// in binDir, runs `gc dolt sync <args>` with extraEnv appended after the base
// sync env (so an override such as GC_DOLT_REMOTE_<DB> takes effect), and
// returns combined output plus the command's error, so callers that must
// assert the sync itself did not fail — as opposed to merely producing
// unexpected output — can do so.
//
// Upstream #6010 introduced the same helper under the name runFFSyncEnv; the
// 2026-09-05 resync kept this one name and repointed #6010's callers at it,
// rather than carry two identical runners past the census ratchet.
func runFFSyncRaw(t *testing.T, binDir string, extraEnv []string, args ...string) (string, error) {
	t.Helper()
	root := repoRoot(t)
	script := filepath.Join(root, syncScript)
	port, cleanup := startReachableTCPListener(t)
	defer cleanup()

	cityPath := t.TempDir()
	dataDir := filepath.Join(cityPath, "data")
	if err := os.MkdirAll(filepath.Join(dataDir, "app", ".dolt"), 0o755); err != nil {
		t.Fatalf("mkdir db: %v", err)
	}
	writeSyncFakeBeadsBD(t, cityPath)

	env := append(syncFilteredEnv(),
		"PATH="+binDir+":"+os.Getenv("PATH"),
		"GC_CITY_PATH="+cityPath,
		"GC_PACK_DIR="+root,
		"GC_DOLT_DATA_DIR="+dataDir,
		fmt.Sprintf("GC_DOLT_PORT=%d", port),
		"GC_DOLT_USER=root",
		"GC_DOLT_PASSWORD=",
	)
	env = append(env, extraEnv...)

	cmd := exec.Command("sh", append([]string{script}, args...)...)
	cmd.Env = env
	out, err := cmd.CombinedOutput()
	return string(out), err
}

// runFFSync sets up a one-DB ("app") SQL-mode city with the fake dolt already
// installed in binDir, runs `gc dolt sync <args>`, and returns combined output.
func runFFSync(t *testing.T, binDir string, args ...string) string {
	t.Helper()
	out, _ := runFFSyncRaw(t, binDir, nil, args...)
	return out
}

// fakeDoltHeader is the shared preamble: log argv and answer the remote-lookup
// + active_branch metadata queries the sync path issues before classification.
func fakeDoltHeader(logPath, branch string) string {
	return "#!/bin/sh\n" +
		"printf '%s\\n' \"$*\" >> \"" + logPath + "\"\n" +
		"case \"$*\" in\n" +
		"  *\"SELECT name, url FROM dolt_remotes ORDER BY name\"*)\n" +
		"    printf 'name,url\\norigin,file:///example.invalid/repo\\n' ; exit 0 ;;\n" +
		"  *\"SELECT active_branch()\"*)\n" +
		"    printf 'active_branch()\\n" + branch + "\\n' ; exit 0 ;;\n"
}

func installFFFakeDolt(t *testing.T, dir, body string) string {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, "dolt"), []byte(body), 0o755); err != nil {
		t.Fatalf("write fake dolt: %v", err)
	}
	return filepath.Join(dir, "dolt.log")
}

// writeSyncFakeDoltClassify: fetch succeeds; the ahead/behind range queries
// report the given counts; DOLT_PUSH is logged and succeeds.
func writeSyncFakeDoltClassify(t *testing.T, dir string, ahead, behind int) string {
	t.Helper()
	branch := "main"
	logPath := filepath.Join(dir, "dolt.log")
	aheadPat := "dolt_log('remotes/origin/" + branch + ".." + branch + "')"
	behindPat := "dolt_log('" + branch + "..remotes/origin/" + branch + "')"
	body := fakeDoltHeader(logPath, branch) +
		"  *\"CALL DOLT_FETCH(\"*) exit 0 ;;\n" +
		"  *\"" + aheadPat + "\"*) printf 'n\\n" + fmt.Sprintf("%d", ahead) + "\\n' ; exit 0 ;;\n" +
		"  *\"" + behindPat + "\"*) printf 'n\\n" + fmt.Sprintf("%d", behind) + "\\n' ; exit 0 ;;\n" +
		"esac\nexit 0\n"
	return installFFFakeDolt(t, dir, body)
}

// writeSyncFakeDoltFetchTimeout: the DOLT_FETCH call exits 124 (timeout).
func writeSyncFakeDoltFetchTimeout(t *testing.T, dir, branch string) string {
	t.Helper()
	logPath := filepath.Join(dir, "dolt.log")
	body := fakeDoltHeader(logPath, branch) +
		"  *\"CALL DOLT_FETCH(\"*) printf 'context deadline exceeded\\n' >&2 ; exit 124 ;;\n" +
		"esac\nexit 0\n"
	return installFFFakeDolt(t, dir, body)
}

// writeSyncFakeDoltFirstPush models a brand-new branch absent on the remote:
// DOLT_FETCH errors "invalid ref spec" (exit 1) — the real Dolt 2.1.0 signal
// for a branch that does not exist on a populated remote (an empty remote
// instead errors "no branches found in remote"). Both are first-push signals;
// the push then creates the branch (a fast-forward). No classify query runs
// because fetch never establishes a remote-tracking ref.
func writeSyncFakeDoltFirstPush(t *testing.T, dir, branch string) string {
	t.Helper()
	logPath := filepath.Join(dir, "dolt.log")
	body := fakeDoltHeader(logPath, branch) +
		"  *\"CALL DOLT_FETCH(\"*) printf 'fetch failed: invalid ref spec\\n' >&2 ; exit 1 ;;\n" +
		"esac\nexit 0\n"
	return installFFFakeDolt(t, dir, body)
}

// writeSyncFakeDoltPushTimeout models a fast-forward push (ahead=2, behind=0)
// whose CALL DOLT_PUSH exits 124 (the client bound fired) while the server-side
// push is left running orphaned. The reaper's processlist query returns
// connection 42 ONLY when it carries BOTH the gc-dolt-sync tag AND the
// `'CALL %'` scope guard, so a regression that drops the tag, drops the guard,
// or inverts the scoping yields no id and fails the reap assertion — the test
// exercises the real predicate rather than an unconditional id.
func writeSyncFakeDoltPushTimeout(t *testing.T, dir string) string {
	t.Helper()
	branch := "main"
	logPath := filepath.Join(dir, "dolt.log")
	aheadPat := "dolt_log('remotes/origin/" + branch + ".." + branch + "')"
	behindPat := "dolt_log('" + branch + "..remotes/origin/" + branch + "')"
	body := fakeDoltHeader(logPath, branch) +
		"  *\"CALL DOLT_FETCH(\"*) exit 0 ;;\n" +
		"  *\"" + aheadPat + "\"*) printf 'n\\n2\\n' ; exit 0 ;;\n" +
		"  *\"" + behindPat + "\"*) printf 'n\\n0\\n' ; exit 0 ;;\n" +
		// reaper SELECT: well-formed (tag + guard) -> orphan connection 42.
		"  *\"gc-dolt-sync:\"*\"'CALL %'\"*) printf 'id\\n42\\n' ; exit 0 ;;\n" +
		// the push itself times out (client bound fired).
		"  *\"CALL DOLT_PUSH(\"*) printf 'context deadline exceeded\\n' >&2 ; exit 124 ;;\n" +
		"esac\nexit 0\n"
	return installFFFakeDolt(t, dir, body)
}

// writeSyncFakeDoltStartupSweep models an up-to-date DB (no push) with one push
// present on the server carrying owner pid ownerPID. The start-of-run sweep's
// processlist query returns "id,pid" (42,ownerPID) ONLY when it carries both
// the gc-dolt-sync tag AND the `'CALL %'` scope guard, so a dropped tag/guard
// fails the assertion. Whether connection 42 is reaped depends on whether
// ownerPID is alive (kill -0), which the caller controls.
func writeSyncFakeDoltStartupSweep(t *testing.T, dir string, ownerPID int) string {
	t.Helper()
	branch := "main"
	logPath := filepath.Join(dir, "dolt.log")
	aheadPat := "dolt_log('remotes/origin/" + branch + ".." + branch + "')"
	behindPat := "dolt_log('" + branch + "..remotes/origin/" + branch + "')"
	body := fakeDoltHeader(logPath, branch) +
		"  *\"CALL DOLT_FETCH(\"*) exit 0 ;;\n" +
		"  *\"" + aheadPat + "\"*) printf 'n\\n0\\n' ; exit 0 ;;\n" +
		"  *\"" + behindPat + "\"*) printf 'n\\n0\\n' ; exit 0 ;;\n" +
		"  *\"gc-dolt-sync:\"*\"'CALL %'\"*) printf 'id,pid\\n42," + fmt.Sprintf("%d", ownerPID) + "\\n' ; exit 0 ;;\n" +
		"esac\nexit 0\n"
	return installFFFakeDolt(t, dir, body)
}

func pushed(log string) bool { return strings.Contains(log, "DOLT_PUSH") }

func readLog(t *testing.T, logPath string) string {
	t.Helper()
	data, err := os.ReadFile(logPath)
	if err != nil {
		return ""
	}
	return string(data)
}

func TestSyncAheadOnlyFastForwardPushes(t *testing.T) {
	binDir := t.TempDir()
	logPath := writeSyncFakeDoltClassify(t, binDir, 2, 0)
	out := runFFSync(t, binDir, "--db", "app")
	log := readLog(t, logPath)
	if !strings.Contains(log, "CALL DOLT_PUSH('origin', 'main')") {
		t.Fatalf("ahead-only should fast-forward push.\nout:\n%s\nlog:\n%s", out, log)
	}
	if strings.Contains(log, "--force") {
		t.Fatalf("ahead-only push must not use --force.\nlog:\n%s", log)
	}
}

func TestSyncPushTimeoutReapsOrphanedServerSidePush(t *testing.T) {
	binDir := t.TempDir()
	logPath := writeSyncFakeDoltPushTimeout(t, binDir)
	out := runFFSync(t, binDir, "--db", "app")
	log := readLog(t, logPath)
	if !strings.Contains(log, "CALL DOLT_PUSH('origin', 'main')") {
		t.Fatalf("expected a fast-forward push attempt.\nout:\n%s\nlog:\n%s", out, log)
	}
	if !strings.Contains(log, "gc-dolt-sync:") {
		t.Fatalf("push must be tagged with the reap marker.\nlog:\n%s", log)
	}
	// After the client-side 124 timeout, the orphaned server-side push
	// (connection 42) must be reaped with a KILL so it stops contending on the
	// shared server (vc-ewyro). The fake only yields id 42 for a well-formed
	// reaper query, so a broken tag/guard would surface here as a missing KILL.
	if !strings.Contains(log, "KILL 42") {
		t.Fatalf("orphaned server-side push must be reaped with KILL 42.\nout:\n%s\nlog:\n%s", out, log)
	}
	if !strings.Contains(out, "reaped orphaned server-side push") {
		t.Fatalf("expected an operator-visible reap notice.\nout:\n%s", out)
	}
}

func TestSyncStartupSweepReapsStrandedPush(t *testing.T) {
	binDir := t.TempDir()
	// A push whose owner pid is far above any assignable pid (INT_MAX): kill -0
	// fails, so the owner is dead and the stranded push must be reaped.
	logPath := writeSyncFakeDoltStartupSweep(t, binDir, 2147483647)
	out := runFFSync(t, binDir, "--db", "app")
	log := readLog(t, logPath)
	if pushed(log) {
		t.Fatalf("up-to-date DB must NOT push.\nout:\n%s\nlog:\n%s", out, log)
	}
	// The start-of-run sweep must reap the dead-owner orphan (connection 42) —
	// the signal-handler-free cover for the order-cancel orphan path (vc-ewyro).
	if !strings.Contains(log, "KILL 42") {
		t.Fatalf("startup sweep must reap the dead-owner push with KILL 42.\nout:\n%s\nlog:\n%s", out, log)
	}
}

func TestSyncStartupSweepSparesLiveOwnerPush(t *testing.T) {
	binDir := t.TempDir()
	// The push's owner is this live test process, standing in for a concurrently
	// running sync/compact push. A live owner (kill -0 succeeds) means the push
	// is healthy, not orphaned, so the sweep must NOT reap it — otherwise a sync
	// tick would kill a healthy concurrent compact force-push (review finding).
	logPath := writeSyncFakeDoltStartupSweep(t, binDir, os.Getpid())
	out := runFFSync(t, binDir, "--db", "app")
	log := readLog(t, logPath)
	if strings.Contains(log, "KILL") {
		t.Fatalf("a push with a LIVE owner must NOT be reaped.\nout:\n%s\nlog:\n%s", out, log)
	}
}

func TestSyncBehindRefusesAndDoesNotPush(t *testing.T) {
	binDir := t.TempDir()
	logPath := writeSyncFakeDoltClassify(t, binDir, 0, 3)
	out := runFFSync(t, binDir, "--db", "app")
	if pushed(readLog(t, logPath)) {
		t.Fatalf("behind DB must NOT be pushed.\nout:\n%s", out)
	}
	if !strings.Contains(out, "behind") {
		t.Fatalf("expected a 'behind' status.\nout:\n%s", out)
	}
}

func TestSyncDivergedRefusesAndDoesNotPush(t *testing.T) {
	binDir := t.TempDir()
	logPath := writeSyncFakeDoltClassify(t, binDir, 2, 3)
	out := runFFSync(t, binDir, "--db", "app")
	if pushed(readLog(t, logPath)) {
		t.Fatalf("diverged DB must NOT be pushed.\nout:\n%s", out)
	}
	if !strings.Contains(out, "diverged") {
		t.Fatalf("expected a 'diverged' status.\nout:\n%s", out)
	}
}

func TestSyncUpToDateSkipsPush(t *testing.T) {
	binDir := t.TempDir()
	logPath := writeSyncFakeDoltClassify(t, binDir, 0, 0)
	out := runFFSync(t, binDir, "--db", "app")
	if pushed(readLog(t, logPath)) {
		t.Fatalf("up-to-date DB must NOT be pushed.\nout:\n%s", out)
	}
	if !strings.Contains(out, "up-to-date") {
		t.Fatalf("expected an 'up-to-date' status.\nout:\n%s", out)
	}
}

func TestSyncFetchTimeoutSkipsNeverPushes(t *testing.T) {
	binDir := t.TempDir()
	logPath := writeSyncFakeDoltFetchTimeout(t, binDir, "main")
	out := runFFSync(t, binDir, "--db", "app")
	if pushed(readLog(t, logPath)) {
		t.Fatalf("a fetch timeout must NEVER push.\nout:\n%s", out)
	}
	if !strings.Contains(out, "fetch timed out") {
		t.Fatalf("expected a 'fetch timed out' status.\nout:\n%s", out)
	}
}

// runFFSyncWithConfig is runFFSync plus GC_DOLT_CONFIG_FILE pointed at a
// generated dolt-config.yaml stamped with the given read_timeout_millis, so
// cold-open-wall tests can use a short deadline instead of waiting out the
// real managed default of 15s (vp-9v6f9). readTimeoutMillis == 0 omits the
// env var entirely, exercising the documented-default fallback.
func runFFSyncWithConfig(t *testing.T, binDir string, readTimeoutMillis int, args ...string) string {
	t.Helper()
	var extraEnv []string
	if readTimeoutMillis > 0 {
		cfgPath := filepath.Join(t.TempDir(), "dolt-config.yaml")
		cfg := fmt.Sprintf("listener:\n  read_timeout_millis: %d\n", readTimeoutMillis)
		if err := os.WriteFile(cfgPath, []byte(cfg), 0o644); err != nil {
			t.Fatalf("write fake dolt-config.yaml: %v", err)
		}
		extraEnv = append(extraEnv, "GC_DOLT_CONFIG_FILE="+cfgPath)
	}
	out, _ := runFFSyncRaw(t, binDir, extraEnv, args...)
	return out
}

// writeSyncFakeDoltColdWall models the managed listener's read_timeout_millis
// killing CALL DOLT_FETCH server-side: the fetch sleeps 2s (standing in for
// the spool time; paired with a test-scoped 2000ms listener deadline so the
// test stays fast without waiting out the real 15s default) then exits 1
// with no recognizable first-push signal — vp-9v6f9's measured
// ".dolt/git-remote-cache grows 4 KB/attempt" residue is exactly this: a
// spool killed mid-flight, not a normal fetch error.
func writeSyncFakeDoltColdWall(t *testing.T, dir string) string {
	t.Helper()
	branch := "main"
	logPath := filepath.Join(dir, "dolt.log")
	body := fakeDoltHeader(logPath, branch) +
		"  *\"CALL DOLT_FETCH(\"*) sleep 2" +
		" ; printf 'row read wait bigger than connection timeout\\n' >&2 ; exit 1 ;;\n" +
		"esac\nexit 0\n"
	return installFFFakeDolt(t, dir, body)
}

// writeSyncFakeDoltGenericFetchFailure models a fetch that fails FAST — well
// under the listener deadline — with an error unrelated to the first-push
// signals (vp-catlj's corrupt-remote shape: "Blob not found"). This must
// never be misclassified as COLD-OPEN WALL.
func writeSyncFakeDoltGenericFetchFailure(t *testing.T, dir string) string {
	t.Helper()
	branch := "main"
	logPath := filepath.Join(dir, "dolt.log")
	body := fakeDoltHeader(logPath, branch) +
		"  *\"CALL DOLT_FETCH(\"*) printf 'Blob not found: deadbeef.darc\\n' >&2 ; exit 1 ;;\n" +
		"esac\nexit 0\n"
	return installFFFakeDolt(t, dir, body)
}

// TestSyncColdOpenWallIsClassified is T-001: a fetch killed past 90% of a
// (short, test-scoped) listener deadline must be reported as COLD-OPEN WALL,
// not the generic "fetch failed" — this is the live root cause behind
// vp-9v6f9 (hq's cold CALL DOLT_FETCH is always killed at ~15.1s by the
// managed listener's 15s read_timeout_millis, dead flat across 12 measured
// attempts).
func TestSyncColdOpenWallIsClassified(t *testing.T) {
	binDir := t.TempDir()
	logPath := writeSyncFakeDoltColdWall(t, binDir)
	out := runFFSyncWithConfig(t, binDir, 2000, "--db", "app")
	if !strings.Contains(readLog(t, logPath), "CALL DOLT_FETCH(") {
		t.Fatalf("expected a fetch attempt.\nout:\n%s", out)
	}
	if !strings.Contains(out, "COLD-OPEN WALL") {
		t.Fatalf("expected a COLD-OPEN WALL status.\nout:\n%s", out)
	}
	if !strings.Contains(out, "NO off-box copy this server lifetime") {
		t.Fatalf("expected the no-off-box-copy explanation.\nout:\n%s", out)
	}
}

// TestSyncListenerDeadlineParsedFromGeneratedConfig is T-002:
// listener_read_timeout_secs() must read the live listener's
// read_timeout_millis from the generated dolt-config.yaml (converted to
// seconds), and fall back to the documented managed default (15) when the
// file is absent — surfaced via the (fast, no-sleep) generic fetch-failed
// message so this is testable without waiting out any real deadline.
func TestSyncListenerDeadlineParsedFromGeneratedConfig(t *testing.T) {
	t.Run("custom deadline from generated config", func(t *testing.T) {
		binDir := t.TempDir()
		writeSyncFakeDoltGenericFetchFailure(t, binDir)
		// 10s (not 2s): an instant failure must stay comfortably under 90% of
		// the deadline even with date(1)'s 1s granularity rounding an ~0s
		// fetch up to "1s" across a wall-clock second boundary — a 2s
		// deadline's 1s (integer) threshold has no such margin and flakes.
		out := runFFSyncWithConfig(t, binDir, 10000, "--db", "app")
		if !strings.Contains(out, "listener deadline 10s") {
			t.Fatalf("expected the custom 10s deadline to be reported.\nout:\n%s", out)
		}
	})
	t.Run("falls back to the documented default when config is absent", func(t *testing.T) {
		binDir := t.TempDir()
		writeSyncFakeDoltGenericFetchFailure(t, binDir)
		out := runFFSyncWithConfig(t, binDir, 0, "--db", "app")
		if !strings.Contains(out, "listener deadline 15s") {
			t.Fatalf("expected the documented default (15s) when no config file is set.\nout:\n%s", out)
		}
	})
}

// TestSyncRecordsFetchElapsed is T-003: the elapsed wall-clock time around
// CALL DOLT_FETCH must be captured and reported, independent of whether the
// failure crosses the cold-open-wall threshold.
func TestSyncRecordsFetchElapsed(t *testing.T) {
	binDir := t.TempDir()
	writeSyncFakeDoltColdWall(t, binDir)
	// A generous 30s deadline keeps a 2s fetch well under the 90% cold-wall
	// threshold, so this isolates elapsed-capture from cold-wall classification.
	out := runFFSyncWithConfig(t, binDir, 30000, "--db", "app")
	if strings.Contains(out, "COLD-OPEN WALL") {
		t.Fatalf("2s elapsed against a 30s deadline must not classify as cold-open wall.\nout:\n%s", out)
	}
	if !regexp.MustCompile(`after [2-9][0-9]*s`).MatchString(out) {
		t.Fatalf("expected elapsed seconds (>=2) reported in the fetch-failed line.\nout:\n%s", out)
	}
}

// TestSyncFastFetchFailureIsNotColdWall is T-005: a fetch that fails FAST —
// well under the listener deadline, vp-catlj's corrupt-remote shape — must
// keep the existing generic "fetch failed" status, never COLD-OPEN WALL.
func TestSyncFastFetchFailureIsNotColdWall(t *testing.T) {
	binDir := t.TempDir()
	logPath := writeSyncFakeDoltGenericFetchFailure(t, binDir)
	// 10s (not 2s): see the matching comment in
	// TestSyncListenerDeadlineParsedFromGeneratedConfig — a 2s deadline's 1s
	// threshold leaves no margin against date(1)'s 1s clock granularity.
	out := runFFSyncWithConfig(t, binDir, 10000, "--db", "app")
	if !strings.Contains(readLog(t, logPath), "CALL DOLT_FETCH(") {
		t.Fatalf("expected a fetch attempt.\nout:\n%s", out)
	}
	if strings.Contains(out, "COLD-OPEN WALL") {
		t.Fatalf("a fast fetch failure must not classify as COLD-OPEN WALL.\nout:\n%s", out)
	}
	if !strings.Contains(out, "fetch failed") {
		t.Fatalf("expected the generic 'fetch failed' status.\nout:\n%s", out)
	}
}

// TestSyncSummaryListsStoresWithNoOffBoxCopy and
// TestSyncSummarySilentWhenAllPushed are T-006: an end-of-run backup-coverage
// summary names every store that hit the cold-open wall, and says nothing on
// a clean run.
func TestSyncSummaryListsStoresWithNoOffBoxCopy(t *testing.T) {
	binDir := t.TempDir()
	writeSyncFakeDoltColdWall(t, binDir)
	out := runFFSyncWithConfig(t, binDir, 2000, "--db", "app")
	if strings.Count(out, "NO OFF-BOX COPY:") != 1 {
		t.Fatalf("expected exactly one 'NO OFF-BOX COPY:' header.\nout:\n%s", out)
	}
	if strings.Count(out, "app") < 2 { // once in the COLD-OPEN WALL line, once in the summary
		t.Fatalf("expected the store name to appear in the summary.\nout:\n%s", out)
	}
}

func TestSyncSummarySilentWhenAllPushed(t *testing.T) {
	binDir := t.TempDir()
	writeSyncFakeDoltClassify(t, binDir, 2, 0)
	out := runFFSync(t, binDir, "--db", "app")
	if strings.Contains(out, "NO OFF-BOX COPY:") {
		t.Fatalf("a clean run must not print the no-off-box-copy summary.\nout:\n%s", out)
	}
}

// TestSyncColdWallExitsNonZero is T-007: the cold-open wall is a failure and
// must still set the script's exit status non-zero, exactly like the generic
// fetch-failed arm did before this change.
func TestSyncColdWallExitsNonZero(t *testing.T) {
	binDir := t.TempDir()
	writeSyncFakeDoltColdWall(t, binDir)
	cfgPath := filepath.Join(t.TempDir(), "dolt-config.yaml")
	if err := os.WriteFile(cfgPath, []byte("listener:\n  read_timeout_millis: 2000\n"), 0o644); err != nil {
		t.Fatalf("write fake dolt-config.yaml: %v", err)
	}
	out, err := runFFSyncRaw(t, binDir, []string{"GC_DOLT_CONFIG_FILE=" + cfgPath}, "--db", "app")
	var ee *exec.ExitError
	if !errors.As(err, &ee) || ee.ExitCode() != 1 {
		t.Fatalf("expected exit 1 on a cold-open wall, got err=%v\nout:\n%s", err, out)
	}
}

func TestSyncFirstPushWhenRemoteRefAbsentPushes(t *testing.T) {
	binDir := t.TempDir()
	logPath := writeSyncFakeDoltFirstPush(t, binDir, "main")
	out := runFFSync(t, binDir, "--db", "app")
	if !strings.Contains(readLog(t, logPath), "CALL DOLT_PUSH('origin', 'main')") {
		t.Fatalf("first push (absent remote ref) must push.\nout:\n%s\nlog:\n%s", out, readLog(t, logPath))
	}
}

func TestSyncForceStillPushesWhenDiverged(t *testing.T) {
	binDir := t.TempDir()
	logPath := writeSyncFakeDoltClassify(t, binDir, 2, 3)
	out := runFFSync(t, binDir, "--db", "app", "--force")
	if !strings.Contains(readLog(t, logPath), "CALL DOLT_PUSH('--force', '--set-upstream', 'origin', 'main')") {
		t.Fatalf("--force must bypass classification and force-push.\nout:\n%s\nlog:\n%s", out, readLog(t, logPath))
	}
}

// writeSyncFakeDoltEmptyRemoteFirstPush models a first-ever push to an empty
// remote: DOLT_FETCH errors "no branches found in remote" (the other Dolt 2.1.0
// first-push signal, distinct from "invalid ref spec" for a new branch on a
// populated remote). The push then creates the branch (a fast-forward).
func writeSyncFakeDoltEmptyRemoteFirstPush(t *testing.T, dir, branch string) string {
	t.Helper()
	logPath := filepath.Join(dir, "dolt.log")
	body := fakeDoltHeader(logPath, branch) +
		"  *\"CALL DOLT_FETCH(\"*) printf 'fetch failed: no branches found in remote\\n' >&2 ; exit 1 ;;\n" +
		"esac\nexit 0\n"
	return installFFFakeDolt(t, dir, body)
}

func TestSyncEmptyRemoteFirstPushPushes(t *testing.T) {
	binDir := t.TempDir()
	logPath := writeSyncFakeDoltEmptyRemoteFirstPush(t, binDir, "main")
	out := runFFSync(t, binDir, "--db", "app")
	if !strings.Contains(readLog(t, logPath), "CALL DOLT_PUSH('origin', 'main')") {
		t.Fatalf("first push to an empty remote must push.\nout:\n%s\nlog:\n%s", out, readLog(t, logPath))
	}
}

// --- gc-fqi7kq: deterministic, opt-in-gated remote selection ---
//
// find_remote_sql (and the CLI-mode equivalent) must resolve the SAME remote
// for a given database regardless of what order dolt_remotes rows arrive in;
// must never auto-select a non-file:// remote when a file:// alternative
// exists, or when no local alternative exists at all (skip with a stated
// reason instead); and must honor an explicit GC_DOLT_REMOTE_<DB> override —
// even over a non-local remote. These tests run --force --dry-run so the
// fake dolt only needs to answer the remote-lookup + active_branch queries;
// ff-classification is already covered by the tests above.

// writeSyncFakeDoltMultiRemote installs a fake dolt that answers the
// remote-lookup query with remoteRows ("name,url" pairs) in exactly the
// order given, and active_branch() with "main". Callers only need the
// installed binary (they assert on gc dolt sync's stdout), not the log
// path, so unlike the other fixture builders in this file this one returns
// nothing.
func writeSyncFakeDoltMultiRemote(t *testing.T, dir string, remoteRows []string) {
	t.Helper()
	logPath := filepath.Join(dir, "dolt.log")
	reply := "name,url\\n" + strings.Join(remoteRows, "\\n") + "\\n"
	body := `#!/bin/sh
printf '%s\n' "$*" >> "` + logPath + `"
case "$*" in
  *"SELECT name, url FROM dolt_remotes ORDER BY name"*)
    printf '` + reply + `'
    ;;
  *"SELECT active_branch()"*)
    printf 'active_branch()\nmain\n'
    ;;
esac
exit 0
`
	if err := os.WriteFile(filepath.Join(dir, "dolt"), []byte(body), 0o755); err != nil {
		t.Fatalf("write fake dolt: %v", err)
	}
}

// TestSyncRemoteSelectionIsOrderIndependent is the RED test from gc-fqi7kq: a
// db whose dolt_remotes returns rows in varying order must resolve the same
// remote every time. Mirrors the real incident's remote set (a git+https
// upstream plus two file:// backups) across three distinct row orderings.
func TestSyncRemoteSelectionIsOrderIndependent(t *testing.T) {
	t.Parallel()
	origin := "origin,git+https://github.com/gastownhall/beads"
	usb := "usb,file:///mnt/usb/beads"
	usbSnap := "usb_snap_20260820,file:///mnt/usb/beads_snap"
	orders := [][]string{
		{origin, usb, usbSnap},
		{usbSnap, usb, origin},
		{usb, origin, usbSnap},
	}
	const want = "-> usb:main (file:///mnt/usb/beads)"
	for i, rows := range orders {
		binDir := t.TempDir()
		writeSyncFakeDoltMultiRemote(t, binDir, rows)
		out := runFFSync(t, binDir, "--db", "app", "--force", "--dry-run")
		if !strings.Contains(out, want) {
			t.Fatalf("order %d: expected remote 'usb' selected regardless of row order.\nrows: %v\nout:\n%s", i, rows, out)
		}
	}
}

// TestSyncMultiRemoteAmbiguousNonLocalSkipsAndNeverPushes covers the case
// where every configured remote is non-local: a default (non-opt-in) sync
// must report the database as skipped, with a stated reason, and never
// select or push to either remote.
func TestSyncMultiRemoteAmbiguousNonLocalSkipsAndNeverPushes(t *testing.T) {
	t.Parallel()
	binDir := t.TempDir()
	writeSyncFakeDoltMultiRemote(t, binDir, []string{
		"mirror,git+https://example.invalid/mirror",
		"origin,git+https://github.com/gastownhall/beads",
	})
	out := runFFSync(t, binDir, "--db", "app", "--force", "--dry-run")
	if !strings.Contains(out, "skipped") || !strings.Contains(out, "no local remote") {
		t.Fatalf("expected an ambiguous-remote skip with a stated reason.\nout:\n%s", out)
	}
	if strings.Contains(out, "would") {
		t.Fatalf("ambiguous remotes must never be auto-selected.\nout:\n%s", out)
	}
}

// TestSyncMultiRemotePrefersLocalOverGitHttpsRemote covers the case where a
// git+https remote is configured alongside a file:// alternative: the
// git+https remote must never be the one selected/pushed.
func TestSyncMultiRemotePrefersLocalOverGitHttpsRemote(t *testing.T) {
	t.Parallel()
	binDir := t.TempDir()
	writeSyncFakeDoltMultiRemote(t, binDir, []string{
		"origin,git+https://github.com/gastownhall/beads",
		"usb,file:///mnt/usb/beads",
	})
	out := runFFSync(t, binDir, "--db", "app", "--force", "--dry-run")
	if !strings.Contains(out, "-> usb:main (file:///mnt/usb/beads)") {
		t.Fatalf("expected the local (file://) remote to be preferred over git+https.\nout:\n%s", out)
	}
	if strings.Contains(out, "-> origin:") {
		t.Fatalf("git+https remote must never be auto-selected when a local alternative exists.\nout:\n%s", out)
	}
}

// TestSyncRemoteEnvOverridePinsNonLocalRemote verifies that GC_DOLT_REMOTE_<DB>
// pins remote selection even to a non-local (git+https) remote that the
// default policy would otherwise skip past in favor of a file:// alternative.
func TestSyncRemoteEnvOverridePinsNonLocalRemote(t *testing.T) {
	t.Parallel()
	binDir := t.TempDir()
	writeSyncFakeDoltMultiRemote(t, binDir, []string{
		"origin,git+https://github.com/gastownhall/beads",
		"usb,file:///mnt/usb/beads",
	})

	out, err := runFFSyncRaw(t, binDir, []string{"GC_DOLT_REMOTE_APP=origin"}, "--db", "app", "--force", "--dry-run")
	if err != nil {
		t.Fatalf("gc dolt sync failed: %v\n%s", err, out)
	}
	if !strings.Contains(out, "-> origin:main (git+https://github.com/gastownhall/beads)") {
		t.Fatalf("GC_DOLT_REMOTE_APP=origin should pin selection to origin even though usb (file://) is available.\nout:\n%s", out)
	}
}

// TestSyncSoleNonLocalRemoteSkipsAndNeverPushes covers the case the original
// fix (ga-fqi7kq) missed: select_remote's sr_count -eq 1 branch returned the
// sole candidate unconditionally, with no locality check at all, so a
// database whose only configured remote is non-local (e.g. a public
// git+https upstream) was auto-selected and pushed to. The locality rule
// that already applies when there are multiple remotes must apply
// identically when there is exactly one.
func TestSyncSoleNonLocalRemoteSkipsAndNeverPushes(t *testing.T) {
	t.Parallel()
	binDir := t.TempDir()
	writeSyncFakeDoltMultiRemote(t, binDir, []string{
		"origin,git+https://github.com/gastownhall/beads",
	})

	out := runFFSync(t, binDir, "--db", "app", "--force", "--dry-run")
	if !strings.Contains(out, "skipped") || !strings.Contains(out, "no local remote") {
		t.Fatalf("a sole non-local remote must be skipped with a stated reason, same as the multi-remote ambiguous case.\nout:\n%s", out)
	}
	if strings.Contains(out, "would") {
		t.Fatalf("a sole non-local remote must never be auto-selected or pushed to.\nout:\n%s", out)
	}
}

// TestSyncSQLUnknownRemoteOverrideFailsWithStatedReason covers the one path
// where select_remote returns non-zero: GC_DOLT_REMOTE_<DB> names a remote
// that is not configured. The database must fail (not silently fall back to
// the default policy), the stderr must name the offending override, and the
// trailing failure summary must attribute the failure to the refusal rather
// than to a remote *query* failure that did not happen.
func TestSyncSQLUnknownRemoteOverrideFailsWithStatedReason(t *testing.T) {
	t.Parallel()
	binDir := t.TempDir()
	writeSyncFakeDoltMultiRemote(t, binDir, []string{
		"origin,git+https://github.com/gastownhall/beads",
		"usb,file:///mnt/usb/beads",
	})

	out, err := runFFSyncRaw(t, binDir, []string{"GC_DOLT_REMOTE_APP=nope"}, "--db", "app", "--force", "--dry-run")
	if err == nil {
		t.Fatalf("an override naming an unconfigured remote must fail the database.\nout:\n%s", out)
	}
	if !strings.Contains(out, "GC_DOLT_REMOTE override 'nope' does not match any configured remote") {
		t.Fatalf("expected the specific unknown-override error.\nout:\n%s", out)
	}
	if !strings.Contains(out, "remote selection refused") {
		t.Fatalf("summary must attribute the failure to the refusal, not to a remote-query failure.\nout:\n%s", out)
	}
	if strings.Contains(out, "failed to query remotes") {
		t.Fatalf("the remote query succeeded; it must not be blamed.\nout:\n%s", out)
	}
	if strings.Contains(out, "would") {
		t.Fatalf("a refused override must never fall back to selecting a remote.\nout:\n%s", out)
	}
}

// --- CLI-mode (.dolt/remotes.json) equivalents of the SQL cases above ---
//
// sync_database_cli feeds select_remote from remotes_json_pairs instead of
// dolt_remotes, so the same policy has a second, independently-parsed
// candidate source. These mirror the SQL cases against it: prefer local,
// skip a sole non-local remote, and honor the override. runSync forces CLI
// mode by pointing the script at an unreachable SQL port.

// writeSyncCLIRemotes creates the data/<db>/.dolt/remotes.json fixture that
// sync_database_cli parses, and returns the city path it was created under.
func writeSyncCLIRemotes(t *testing.T, remotesJSON string) string {
	t.Helper()
	cityPath := t.TempDir()
	dbDir := filepath.Join(cityPath, "data", "app")
	if err := os.MkdirAll(filepath.Join(dbDir, ".dolt"), 0o755); err != nil {
		t.Fatalf("mkdir db: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dbDir, ".dolt", "remotes.json"), []byte(remotesJSON), 0o644); err != nil {
		t.Fatalf("write remotes: %v", err)
	}
	return cityPath
}

// TestSyncCLIMultiRemotePrefersLocal is the CLI-mode twin of
// TestSyncMultiRemotePrefersLocalOverGitHttpsRemote: with a git+https remote
// listed first and a file:// alternative available, the local one wins.
func TestSyncCLIMultiRemotePrefersLocal(t *testing.T) {
	t.Parallel()
	cityPath := writeSyncCLIRemotes(t, `{"remotes":[{"name":"origin","url":"git+https://github.com/gastownhall/beads"},{"name":"usb","url":"file:///mnt/usb/beads"}]}`)
	binDir := t.TempDir()
	_ = writeSyncFakeDolt(t, binDir)
	_ = writeSyncFakeBeadsBD(t, cityPath)

	out, err := runSync(t, binDir, cityPath, []string{"GC_DOLT_REMOTE_APP="}, "--db", "app", "--dry-run")
	if err != nil {
		t.Fatalf("gc dolt sync failed: %v\n%s", err, out)
	}
	if !strings.Contains(out, "-> usb:main (file:///mnt/usb/beads)") {
		t.Fatalf("CLI mode should prefer the local (file://) remote over git+https.\nout:\n%s", out)
	}
	if strings.Contains(out, "-> origin:") {
		t.Fatalf("git+https remote must never be auto-selected when a local alternative exists.\nout:\n%s", out)
	}
}

// TestSyncCLISoleNonLocalRemoteSkips is the CLI-mode twin of
// TestSyncSoleNonLocalRemoteSkipsAndNeverPushes: a sole non-local remote is
// not exempt from the locality rule just because there was nothing to
// disambiguate.
func TestSyncCLISoleNonLocalRemoteSkips(t *testing.T) {
	t.Parallel()
	cityPath := writeSyncCLIRemotes(t, `{"remotes":[{"name":"origin","url":"git+https://github.com/gastownhall/beads"}]}`)
	binDir := t.TempDir()
	_ = writeSyncFakeDolt(t, binDir)
	_ = writeSyncFakeBeadsBD(t, cityPath)

	out, err := runSync(t, binDir, cityPath, []string{"GC_DOLT_REMOTE_APP="}, "--db", "app", "--dry-run")
	if err != nil {
		t.Fatalf("a skip is not a failure; sync should exit 0: %v\n%s", err, out)
	}
	if !strings.Contains(out, "skipped") || !strings.Contains(out, "no local remote") {
		t.Fatalf("a sole non-local remote must be skipped with a stated reason.\nout:\n%s", out)
	}
	if strings.Contains(out, "would") {
		t.Fatalf("a sole non-local remote must never be auto-selected or pushed to.\nout:\n%s", out)
	}
}

// TestSyncCLIRemoteOverridePinsNonLocal is the CLI-mode twin of
// TestSyncRemoteEnvOverridePinsNonLocalRemote: an explicit override pins a
// non-local remote the default policy would otherwise pass over.
func TestSyncCLIRemoteOverridePinsNonLocal(t *testing.T) {
	t.Parallel()
	cityPath := writeSyncCLIRemotes(t, `{"remotes":[{"name":"origin","url":"git+https://github.com/gastownhall/beads"},{"name":"usb","url":"file:///mnt/usb/beads"}]}`)
	binDir := t.TempDir()
	_ = writeSyncFakeDolt(t, binDir)
	_ = writeSyncFakeBeadsBD(t, cityPath)

	out, err := runSync(t, binDir, cityPath, []string{"GC_DOLT_REMOTE_APP=origin"}, "--db", "app", "--dry-run")
	if err != nil {
		t.Fatalf("gc dolt sync failed: %v\n%s", err, out)
	}
	if !strings.Contains(out, "-> origin:main (git+https://github.com/gastownhall/beads)") {
		t.Fatalf("GC_DOLT_REMOTE_APP=origin should pin selection to origin even though usb (file://) is available.\nout:\n%s", out)
	}
}

// TestSyncRejectsInvalidFetchTimeout covers the GC_DOLT_SYNC_FETCH_TIMEOUT_SECS
// validator (the twin of the push-timeout validator): the bound is checked at
// startup before any database is touched, and an empty / non-numeric / all-zero
// value aborts with exit 2 rather than running the fetch unbounded.
func TestSyncRejectsInvalidFetchTimeout(t *testing.T) {
	root := repoRoot(t)
	script := filepath.Join(root, syncScript)
	binDir := t.TempDir()
	_ = writeSyncFakeDolt(t, binDir) // never invoked: the validator aborts first
	cityPath := t.TempDir()
	for _, bad := range []string{"abc", "", "0", "00", "-5"} {
		cmd := exec.Command("sh", script, "--db", "app")
		cmd.Env = append(syncFilteredEnv(),
			"PATH="+binDir+":"+os.Getenv("PATH"),
			"GC_CITY_PATH="+cityPath,
			"GC_PACK_DIR="+root,
			"GC_DOLT_DATA_DIR="+filepath.Join(cityPath, "data"),
			"GC_DOLT_PORT=1",
			"GC_DOLT_USER=root",
			"GC_DOLT_PASSWORD=",
			"GC_DOLT_SYNC_FETCH_TIMEOUT_SECS="+bad,
		)
		out, err := cmd.CombinedOutput()
		var ee *exec.ExitError
		if !errors.As(err, &ee) || ee.ExitCode() != 2 {
			t.Errorf("fetch timeout %q: want exit 2, got err=%v\nout: %s", bad, err, out)
		}
		if !strings.Contains(string(out), "invalid GC_DOLT_SYNC_FETCH_TIMEOUT_SECS") {
			t.Errorf("fetch timeout %q: want validation message\nout: %s", bad, out)
		}
	}
}

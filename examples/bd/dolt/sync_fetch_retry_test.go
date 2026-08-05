package dolt_test

// Pre-push fetch retry (vp-q3kp).
//
// The cost that matters is COLD vs WARM, not delta size and not store size. A
// store's first remote operation in a given sql-server lifetime must spool the
// remote's blobs; every later operation on that store in the same lifetime is
// sub-second. Against a short server read timeout the cold operation is what
// dies — and since the FETCH runs first, the push is never even attempted.
//
// MEASURED on the live voxist-city fleet 2026-08-04, read_timeout_millis=15000,
// dolt 2.2.3, git+ssh remotes. Same stores, two server lifetimes:
//
//	WARM (later in a lifetime):  hq 1.83s · vp 1.57s · vr 1.90s · vm 2.11s
//	COLD (after a restart):      hq 15.2s FAIL · vp 15.2s FAIL · vr 15.7s FAIL
//	                             vm 15.2s FAIL
//
// vr and vm went from ~2s to failing with no change in delta — cold/warm is the
// dominant variable, and a restart resets every store at once.
//
// Attempts needed to converge FROM COLD, against git-remote-cache size:
//
//	vg 1 (5.7M) · vl 1 (12M) · vw 1 (57M) · vm 2 (342M) · vr 2 (465M)
//	va 3 (871M) · vp 4 (799M) · hq NEVER (2.1G)
//
// A fetch is read-only and idempotent, so a torn fetch loses nothing, and
// partial spool progress is retained across attempts — which is what makes
// retrying a way to pay the cold cost in installments that each fit inside the
// deadline. That is why retry is correct here and would not be for a push.
//
// It does NOT rescue every store: hq advanced 4 KB per attempt against a
// 2.24 GB cache over 12 attempts, so no budget converges it. The default of 5
// covers everything that converges at all, with one attempt of margin over the
// worst (vp at 4), and deliberately stops short of burning cycle time on hq.
//
// Only two failures skip the retry: a first-push signal (not a failure) and
// exit 124 (the client bound already spent its whole budget). Everything else
// retries — see TestSyncRetriesBlobNotFound for the one that looks like it
// should be an exception and is not.

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// runFFSyncRC is runFFSync plus the exit code, which the retry contract turns
// load-bearing: a converged retry must exit 0, and an exhausted or terminal
// failure must exit non-zero so the order's own failure signal still fires.
func runFFSyncRC(t *testing.T, binDir string) (string, int) {
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

	cmd := exec.Command("sh", script)
	cmd.Env = append(syncFilteredEnv(),
		"PATH="+binDir+":"+os.Getenv("PATH"),
		"GC_CITY_PATH="+cityPath,
		"GC_PACK_DIR="+root,
		"GC_DOLT_DATA_DIR="+dataDir,
		fmt.Sprintf("GC_DOLT_PORT=%d", port),
		"GC_DOLT_USER=root",
		"GC_DOLT_PASSWORD=",
		// Keep the tests fast: the production default backoff is seconds.
		"GC_DOLT_SYNC_FETCH_RETRY_DELAY_SECS=0",
	)
	out, err := cmd.CombinedOutput()
	code := 0
	ee := &exec.ExitError{}
	if errors.As(err, &ee) {
		code = ee.ExitCode()
	} else if err != nil {
		t.Fatalf("run sync: %v", err)
	}
	return string(out), code
}

// writeSyncFakeDoltFetchFlaky makes DOLT_FETCH fail with failMsg for the first
// failCount attempts and succeed afterwards, counting attempts in a file so a
// test can assert the exact number of tries. Classification then reports
// ahead=2/behind=0 so a converged fetch proceeds to a real push.
//
// The counter is incremented with a read-modify-write on a plain file rather
// than `wc -l` on the shared argv log, so the count reflects DOLT_FETCH calls
// only and cannot drift when unrelated queries are added to the sync path.
func writeSyncFakeDoltFetchFlaky(t *testing.T, dir, failMsg string, failCount int) (logPath, countPath string) {
	t.Helper()
	branch := "main"
	logPath = filepath.Join(dir, "dolt.log")
	countPath = filepath.Join(dir, "fetch.count")
	aheadPat := "dolt_log('remotes/origin/" + branch + ".." + branch + "')"
	behindPat := "dolt_log('" + branch + "..remotes/origin/" + branch + "')"
	body := fakeDoltHeader(logPath, branch) +
		"  *\"CALL DOLT_FETCH(\"*)\n" +
		"    n=$(cat \"" + countPath + "\" 2>/dev/null || echo 0)\n" +
		"    n=$((n+1)) ; printf '%s' \"$n\" > \"" + countPath + "\"\n" +
		"    if [ \"$n\" -le " + fmt.Sprintf("%d", failCount) + " ]; then\n" +
		"      printf '" + failMsg + "\\n' >&2 ; exit 1\n" +
		"    fi\n" +
		"    exit 0 ;;\n" +
		"  *\"" + aheadPat + "\"*) printf 'n\\n2\\n' ; exit 0 ;;\n" +
		"  *\"" + behindPat + "\"*) printf 'n\\n0\\n' ; exit 0 ;;\n" +
		"esac\nexit 0\n"
	installFFFakeDolt(t, dir, body)
	return logPath, countPath
}

func fetchAttempts(t *testing.T, countPath string) int {
	t.Helper()
	data, err := os.ReadFile(countPath)
	if err != nil {
		return 0
	}
	var n int
	if _, err := fmt.Sscanf(strings.TrimSpace(string(data)), "%d", &n); err != nil {
		t.Fatalf("parse fetch count %q: %v", string(data), err)
	}
	return n
}

// TestSyncRetriesTransientFetchFailureThenPushes is the hq/vp case measured
// above: the fetch loses to the 15s server deadline once, converges on the
// retry, and the push — the actual work — then happens. Asserting the push
// (positive work), not merely the absence of an error.
func TestSyncRetriesTransientFetchFailureThenPushes(t *testing.T) {
	binDir := t.TempDir()
	logPath, countPath := writeSyncFakeDoltFetchFlaky(t, binDir, "unexpected EOF", 1)

	out, code := runFFSyncRC(t, binDir)

	if got := fetchAttempts(t, countPath); got != 2 {
		t.Fatalf("expected 2 fetch attempts (1 failure + 1 converged), got %d\nout:\n%s", got, out)
	}
	if !pushed(readLog(t, logPath)) {
		t.Fatalf("expected a DOLT_PUSH after the fetch converged.\nout:\n%s\nlog:\n%s", out, readLog(t, logPath))
	}
	if code != 0 {
		t.Fatalf("expected exit 0 after a converged retry, got %d\nout:\n%s", code, out)
	}
}

// TestSyncRetriesFetchTwiceThenPushes: two consecutive deadline losses before
// the spool is far enough along to converge. Measured from cold on the live
// fleet, va needed 3 attempts and vp needed 4, so the default budget must
// comfortably exceed 2 or those stores stay unsynced for the server lifetime.
func TestSyncRetriesFetchTwiceThenPushes(t *testing.T) {
	binDir := t.TempDir()
	logPath, countPath := writeSyncFakeDoltFetchFlaky(t, binDir, "unexpected EOF", 2)

	out, code := runFFSyncRC(t, binDir)

	if got := fetchAttempts(t, countPath); got != 3 {
		t.Fatalf("expected 3 fetch attempts, got %d\nout:\n%s", got, out)
	}
	if !pushed(readLog(t, logPath)) {
		t.Fatalf("expected a DOLT_PUSH after two retries.\nout:\n%s", out)
	}
	if code != 0 {
		t.Fatalf("expected exit 0, got %d\nout:\n%s", code, out)
	}
}

// TestSyncFetchRetryExhaustionDoesNotPush: when the fetch never converges the
// classification is unknown, so the push must NOT happen (RULE 1 — an unknown
// propagates as unknown; a blind push is exactly the fast-forward-only
// guarantee gc-6ommo exists to protect) and the run must exit non-zero.
func TestSyncFetchRetryExhaustionDoesNotPush(t *testing.T) {
	binDir := t.TempDir()
	logPath, countPath := writeSyncFakeDoltFetchFlaky(t, binDir, "unexpected EOF", 99)

	out, code := runFFSyncRC(t, binDir)

	if got := fetchAttempts(t, countPath); got != 5 {
		t.Fatalf("expected the default 5-attempt budget, got %d\nout:\n%s", got, out)
	}
	if pushed(readLog(t, logPath)) {
		t.Fatalf("must NOT push when classification never succeeded.\nout:\n%s", out)
	}
	if code == 0 {
		t.Fatalf("expected non-zero exit when the fetch never converged\nout:\n%s", out)
	}
	if !strings.Contains(out, "5 attempts") {
		t.Fatalf("expected the status to report how many attempts were spent.\nout:\n%s", out)
	}
}

// TestSyncRetriesBlobNotFound: "Blob not found" is a recurring fleet-wide
// TRANSIENT, not a corruption signature, so it goes through the normal retry
// path like any other transport failure.
//
// This test exists because the first cut of this change got it wrong. One
// store was observed failing 5/5 attempts with a VARYING missing-blob hash,
// and that was read as a corrupt remote archive and given a terminal,
// no-retry branch. It did not reproduce: the same store later fetched 5/5
// clean, and the string appears 456 times across 10+ days and 17 distinct
// hashes in the fleet's dolt.log, on days when nothing was damaged. A single
// non-converging window does not license a permanent classification.
//
// Retrying is also cheap here — these attempts fail in ~3.4s, not at the ~15s
// deadline — so the cost of being wrong in this direction is far lower than
// the cost of turning a transient into a guaranteed per-cycle skip.
func TestSyncRetriesBlobNotFound(t *testing.T) {
	binDir := t.TempDir()
	logPath, countPath := writeSyncFakeDoltFetchFlaky(t,
		binDir, "Blob not found: bckv59m50r5l43o5koeiap6ecjihm36a.darc", 2)

	out, code := runFFSyncRC(t, binDir)

	if got := fetchAttempts(t, countPath); got != 3 {
		t.Fatalf("Blob not found must be retried like any transport failure: expected 3 attempts, got %d\nout:\n%s", got, out)
	}
	if !pushed(readLog(t, logPath)) {
		t.Fatalf("expected a DOLT_PUSH once the transient cleared.\nout:\n%s", out)
	}
	if code != 0 {
		t.Fatalf("expected exit 0 after the transient cleared, got %d\nout:\n%s", code, out)
	}
	if strings.Contains(out, "corrupt") {
		t.Fatalf("must not diagnose a recurring transient as corruption.\nout:\n%s", out)
	}
}

// TestSyncFirstPushSignalIsNotRetried: "invalid ref spec" / "no branches found
// in remote" are not failures at all, they are how Dolt reports a branch the
// remote does not have yet. Retrying them would trade a first push for two
// wasted round trips per cycle.
func TestSyncFirstPushSignalIsNotRetried(t *testing.T) {
	binDir := t.TempDir()
	logPath, countPath := writeSyncFakeDoltFetchFlaky(t, binDir, "fetch failed: invalid ref spec", 99)

	out, code := runFFSyncRC(t, binDir)

	if got := fetchAttempts(t, countPath); got != 1 {
		t.Fatalf("a first-push signal must not be retried: expected 1 attempt, got %d\nout:\n%s", got, out)
	}
	if !pushed(readLog(t, logPath)) {
		t.Fatalf("expected the first push to proceed.\nout:\n%s", out)
	}
	if code != 0 {
		t.Fatalf("expected exit 0 for a first push, got %d\nout:\n%s", code, out)
	}
}

// TestSyncFetchAttemptsEnvOverride: the budget is tunable, and an invalid
// value must fail loud rather than silently defaulting — the same contract the
// push/fetch timeouts already hold, for the same reason (a misconfigured bound
// that quietly becomes something else is unreviewable).
func TestSyncFetchAttemptsEnvOverride(t *testing.T) {
	binDir := t.TempDir()
	_, countPath := writeSyncFakeDoltFetchFlaky(t, binDir, "unexpected EOF", 99)

	root := repoRoot(t)
	script := filepath.Join(root, syncScript)
	cityPath := t.TempDir()
	dataDir := filepath.Join(cityPath, "data")
	if err := os.MkdirAll(filepath.Join(dataDir, "app", ".dolt"), 0o755); err != nil {
		t.Fatalf("mkdir db: %v", err)
	}
	writeSyncFakeBeadsBD(t, cityPath)
	port, cleanup := startReachableTCPListener(t)
	defer cleanup()

	run := func(attempts string) (string, int) {
		cmd := exec.Command("sh", script)
		cmd.Env = append(syncFilteredEnv(),
			"PATH="+binDir+":"+os.Getenv("PATH"),
			"GC_CITY_PATH="+cityPath,
			"GC_PACK_DIR="+root,
			"GC_DOLT_DATA_DIR="+dataDir,
			fmt.Sprintf("GC_DOLT_PORT=%d", port),
			"GC_DOLT_USER=root",
			"GC_DOLT_PASSWORD=",
			"GC_DOLT_SYNC_FETCH_RETRY_DELAY_SECS=0",
			"GC_DOLT_SYNC_FETCH_ATTEMPTS="+attempts,
		)
		out, err := cmd.CombinedOutput()
		code := 0
		ee := &exec.ExitError{}
		if errors.As(err, &ee) {
			code = ee.ExitCode()
		}
		return string(out), code
	}

	_ = os.Remove(countPath)
	out, _ := run("5")
	if got := fetchAttempts(t, countPath); got != 5 {
		t.Fatalf("expected 5 attempts from the env override, got %d\nout:\n%s", got, out)
	}

	// An all-zero / non-numeric value must abort before any database is
	// touched, exactly as the timeout bounds do.
	for _, bad := range []string{"0", "00", "abc", ""} {
		_ = os.Remove(countPath)
		out, code := run(bad)
		if code != 2 {
			t.Fatalf("GC_DOLT_SYNC_FETCH_ATTEMPTS=%q must exit 2, got %d\nout:\n%s", bad, code, out)
		}
		if got := fetchAttempts(t, countPath); got != 0 {
			t.Fatalf("GC_DOLT_SYNC_FETCH_ATTEMPTS=%q must abort before touching a db, got %d attempts\nout:\n%s", bad, got, out)
		}
	}
}

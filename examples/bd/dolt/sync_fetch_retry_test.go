//go:build unix

package dolt_test

// Pre-push fetch retry (vp-q3kp).
//
// The cost that matters is COLD vs WARM, not delta size and not store size. A
// store's first remote operation in a given sql-server lifetime must spool the
// remote's blobs; every later operation on that store in the same lifetime is
// sub-second. Against a short server read timeout the cold operation is what
// dies, and since the FETCH runs first, the push is never even attempted.
//
// Measured on a 9-store fleet 2026-08-04, read_timeout_millis=15000, dolt
// 2.2.3, git+ssh remotes. Same stores, two server lifetimes:
//
//	WARM (later in a lifetime):  hq 1.83s · vp 1.57s · vr 1.90s · vm 2.11s
//	COLD (after a restart):      hq 15.2s FAIL · vp 15.2s FAIL · vr 15.7s FAIL
//	                             vm 15.2s FAIL
//
// Attempts needed to converge FROM COLD, against git-remote-cache size:
//
//	vg 1 (5.7M) · vl 1 (12M) · vw 1 (57M) · vm 2 (342M) · vr 2 (465M)
//	va 3 (871M) · vp 4 (799M) · hq NEVER (2.1G)
//
// A fetch is read-only and idempotent, so a torn fetch loses nothing, and
// partial spool progress is retained across attempts: retrying pays the cold
// cost in installments that each fit inside the deadline. That is why retry is
// correct here and would not be for a push.
//
// Only two failures skip the retry: a first-push signal (not a failure) and
// exit 124 (the client bound already spent its whole budget). Everything else
// retries, including "Blob not found" (see TestSyncRetriesBlobNotFound). A
// store that still dies at the listener wall once the budget is spent is the
// vp-9v6f9 cold-open wall, reported terminally after the budget.
//
// Every test runs through runFFSyncRaw, the shared subprocess call site, so
// this file adds no call site to the untagged subprocess census (TESTING.md).
// runFFSyncRaw sets GC_DOLT_SYNC_FETCH_RETRY_DELAY_SECS=0 so retries do not
// wait out the production backoff.

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/orders"
)

// writeSyncFakeDoltFetchFlaky makes DOLT_FETCH fail with failMsg and exit
// failRC for the first failCount attempts and succeed afterwards, counting
// attempts in a file of its own so the count covers DOLT_FETCH calls only.
// Classification then reports ahead=2/behind=0, so a converged fetch proceeds
// to a real push.
func writeSyncFakeDoltFetchFlaky(t *testing.T, dir, failMsg string, failRC, failCount int) (logPath, countPath string) {
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
		"    if [ \"$n\" -le " + strconv.Itoa(failCount) + " ]; then\n" +
		"      printf '%s\\n' '" + failMsg + "' >&2 ; exit " + strconv.Itoa(failRC) + "\n" +
		"    fi\n" +
		"    exit 0 ;;\n" +
		"  *\"" + aheadPat + "\"*) printf 'n\\n2\\n' ; exit 0 ;;\n" +
		"  *\"" + behindPat + "\"*) printf 'n\\n0\\n' ; exit 0 ;;\n" +
		"esac\nexit 0\n"
	installFFFakeDolt(t, dir, body)
	return logPath, countPath
}

// fetchAttempts reads the fake's DOLT_FETCH counter; a missing file is zero
// attempts.
func fetchAttempts(t *testing.T, countPath string) int {
	t.Helper()
	data, err := os.ReadFile(countPath)
	if errors.Is(err, os.ErrNotExist) {
		return 0
	}
	if err != nil {
		t.Fatalf("read fetch count: %v", err)
	}
	n, err := strconv.Atoi(strings.TrimSpace(string(data)))
	if err != nil {
		t.Fatalf("parse fetch count %q: %v", string(data), err)
	}
	return n
}

// runFFSyncCode runs sync through runFFSyncRaw and returns the exit code the
// retry contract makes load-bearing: a converged retry exits 0, and an
// exhausted or terminal failure exits non-zero so the order's failure signal
// still fires.
func runFFSyncCode(t *testing.T, binDir string, extraEnv []string, args ...string) (string, int) {
	t.Helper()
	out, err := runFFSyncRaw(t, binDir, extraEnv, args...)
	if err == nil {
		return out, 0
	}
	var ee *exec.ExitError
	if !errors.As(err, &ee) {
		t.Fatalf("run sync: %v\nout:\n%s", err, out)
	}
	return out, ee.ExitCode()
}

// TestSyncRetriesTransientFetchFailureThenPushes: the fetch loses to the
// server deadline once, converges on the retry, and the push then happens.
func TestSyncRetriesTransientFetchFailureThenPushes(t *testing.T) {
	binDir := t.TempDir()
	logPath, countPath := writeSyncFakeDoltFetchFlaky(t, binDir, "unexpected EOF", 1, 1)

	out, code := runFFSyncCode(t, binDir, nil, "--db", "app")

	if got := fetchAttempts(t, countPath); got != 2 {
		t.Fatalf("fetch attempts = %d, want 2 (1 failure + 1 converged)\nout:\n%s", got, out)
	}
	if !pushed(readLog(t, logPath)) {
		t.Fatalf("expected a DOLT_PUSH after the fetch converged.\nout:\n%s\nlog:\n%s", out, readLog(t, logPath))
	}
	if code != 0 {
		t.Fatalf("exit = %d, want 0 after a converged retry\nout:\n%s", code, out)
	}
	if !strings.Contains(out, "app: fetch attempt 1/5 failed") {
		t.Fatalf("expected the failed attempt to be reported against the budget.\nout:\n%s", out)
	}
}

// TestSyncRetriesFetchTwiceThenPushes: two consecutive losses before the
// spool converges. Measured from cold, va needed 3 attempts and vp needed 4,
// so the default budget must exceed 2.
func TestSyncRetriesFetchTwiceThenPushes(t *testing.T) {
	binDir := t.TempDir()
	logPath, countPath := writeSyncFakeDoltFetchFlaky(t, binDir, "unexpected EOF", 1, 2)

	out, code := runFFSyncCode(t, binDir, nil, "--db", "app")

	if got := fetchAttempts(t, countPath); got != 3 {
		t.Fatalf("fetch attempts = %d, want 3\nout:\n%s", got, out)
	}
	if !pushed(readLog(t, logPath)) {
		t.Fatalf("expected a DOLT_PUSH after two retries.\nout:\n%s", out)
	}
	if code != 0 {
		t.Fatalf("exit = %d, want 0\nout:\n%s", code, out)
	}
}

// TestSyncFetchRetryExhaustionDoesNotPush: a fetch that never converges leaves
// the classification unknown, so nothing is pushed (gc-6ommo's fast-forward
// guarantee), the run exits non-zero, and the status reports the attempts.
func TestSyncFetchRetryExhaustionDoesNotPush(t *testing.T) {
	binDir := t.TempDir()
	logPath, countPath := writeSyncFakeDoltFetchFlaky(t, binDir, "unexpected EOF", 1, 99)

	out, code := runFFSyncCode(t, binDir, nil, "--db", "app")

	if got := fetchAttempts(t, countPath); got != 5 {
		t.Fatalf("fetch attempts = %d, want the default budget of 5\nout:\n%s", got, out)
	}
	if pushed(readLog(t, logPath)) {
		t.Fatalf("must NOT push when classification never succeeded.\nout:\n%s", out)
	}
	if code == 0 {
		t.Fatalf("expected a non-zero exit when the fetch never converged\nout:\n%s", out)
	}
	if !strings.Contains(out, "app: fetch failed (exit 1) after 5 attempts, last after ") {
		t.Fatalf("expected the status to report the attempts spent.\nout:\n%s", out)
	}
	if !strings.Contains(out, "app: unexpected EOF") {
		t.Fatalf("expected the last attempt's stderr to be relayed.\nout:\n%s", out)
	}
}

// TestSyncRetriesBlobNotFound: "Blob not found" recurs fleet-wide as a
// transient (456 occurrences, 17 distinct hashes over 10+ days, on days when
// nothing was damaged), so it is retried like any transport failure. A
// terminal branch would turn it into a guaranteed per-cycle skip.
func TestSyncRetriesBlobNotFound(t *testing.T) {
	binDir := t.TempDir()
	logPath, countPath := writeSyncFakeDoltFetchFlaky(t, binDir,
		"Blob not found: bckv59m50r5l43o5koeiap6ecjihm36a.darc", 1, 2)

	out, code := runFFSyncCode(t, binDir, nil, "--db", "app")

	if got := fetchAttempts(t, countPath); got != 3 {
		t.Fatalf("Blob not found must be retried: fetch attempts = %d, want 3\nout:\n%s", got, out)
	}
	if !pushed(readLog(t, logPath)) {
		t.Fatalf("expected a DOLT_PUSH once the transient cleared.\nout:\n%s", out)
	}
	if code != 0 {
		t.Fatalf("exit = %d, want 0 after the transient cleared\nout:\n%s", code, out)
	}
}

// TestSyncFirstPushSignalIsNotRetried: "invalid ref spec" and "no branches
// found in remote" are how Dolt reports a branch the remote does not have yet.
// They are not failures, so they are not retried and the first push proceeds.
func TestSyncFirstPushSignalIsNotRetried(t *testing.T) {
	for _, msg := range []string{"fetch failed: invalid ref spec", "fetch failed: no branches found in remote"} {
		t.Run(msg, func(t *testing.T) {
			binDir := t.TempDir()
			logPath, countPath := writeSyncFakeDoltFetchFlaky(t, binDir, msg, 1, 99)

			out, code := runFFSyncCode(t, binDir, nil, "--db", "app")

			if got := fetchAttempts(t, countPath); got != 1 {
				t.Fatalf("a first-push signal must not be retried: fetch attempts = %d, want 1\nout:\n%s", got, out)
			}
			if !pushed(readLog(t, logPath)) {
				t.Fatalf("expected the first push to proceed.\nout:\n%s", out)
			}
			if code != 0 {
				t.Fatalf("exit = %d, want 0 for a first push\nout:\n%s", code, out)
			}
		})
	}
}

// TestSyncFetchTimeoutIsNotRetried: exit 124 means the client bound already
// burned GC_DOLT_SYNC_FETCH_TIMEOUT_SECS. Another attempt would cost the same
// again and starve the stores queued behind this one.
func TestSyncFetchTimeoutIsNotRetried(t *testing.T) {
	binDir := t.TempDir()
	logPath, countPath := writeSyncFakeDoltFetchFlaky(t, binDir, "context deadline exceeded", 124, 99)

	out, code := runFFSyncCode(t, binDir, nil, "--db", "app")

	if got := fetchAttempts(t, countPath); got != 1 {
		t.Fatalf("a timed-out fetch must not be retried: fetch attempts = %d, want 1\nout:\n%s", got, out)
	}
	if pushed(readLog(t, logPath)) {
		t.Fatalf("must NOT push after a fetch timeout.\nout:\n%s", out)
	}
	if code == 0 {
		t.Fatalf("expected a non-zero exit after a fetch timeout\nout:\n%s", out)
	}
	if !strings.Contains(out, "app: fetch timed out after 60s") {
		t.Fatalf("expected the fetch-timeout status.\nout:\n%s", out)
	}
}

// TestSyncFetchAttemptsEnvOverride: the budget is tunable, and an invalid
// value fails loud before any database is touched, the same contract the
// push and fetch timeouts hold.
func TestSyncFetchAttemptsEnvOverride(t *testing.T) {
	t.Run("override", func(t *testing.T) {
		binDir := t.TempDir()
		_, countPath := writeSyncFakeDoltFetchFlaky(t, binDir, "unexpected EOF", 1, 99)
		out, code := runFFSyncCode(t, binDir, []string{"GC_DOLT_SYNC_FETCH_ATTEMPTS=2"}, "--db", "app")
		if got := fetchAttempts(t, countPath); got != 2 {
			t.Fatalf("fetch attempts = %d, want 2 from the override\nout:\n%s", got, out)
		}
		if code == 0 {
			t.Fatalf("expected a non-zero exit after the budget ran out\nout:\n%s", out)
		}
		if !strings.Contains(out, "after 2 attempts") {
			t.Fatalf("expected the status to report the overridden budget.\nout:\n%s", out)
		}
	})
	for _, bad := range []string{"0", "00", "abc", "-1", ""} {
		t.Run(fmt.Sprintf("invalid %q", bad), func(t *testing.T) {
			binDir := t.TempDir()
			_, countPath := writeSyncFakeDoltFetchFlaky(t, binDir, "unexpected EOF", 1, 99)
			out, code := runFFSyncCode(t, binDir, []string{"GC_DOLT_SYNC_FETCH_ATTEMPTS=" + bad}, "--db", "app")
			if code != 2 {
				t.Fatalf("GC_DOLT_SYNC_FETCH_ATTEMPTS=%q: exit = %d, want 2\nout:\n%s", bad, code, out)
			}
			if !strings.Contains(out, "invalid GC_DOLT_SYNC_FETCH_ATTEMPTS") {
				t.Fatalf("expected the invalid-budget error.\nout:\n%s", out)
			}
			if got := fetchAttempts(t, countPath); got != 0 {
				t.Fatalf("GC_DOLT_SYNC_FETCH_ATTEMPTS=%q must abort before touching a db: fetch attempts = %d\nout:\n%s", bad, got, out)
			}
		})
	}
}

// TestSyncFetchRetryDelayEnvValidation: the backoff accepts 0 (retry at once)
// but rejects an empty or non-numeric value before any database is touched.
func TestSyncFetchRetryDelayEnvValidation(t *testing.T) {
	for _, bad := range []string{"abc", "-1", "1.5", ""} {
		t.Run(fmt.Sprintf("invalid %q", bad), func(t *testing.T) {
			binDir := t.TempDir()
			_, countPath := writeSyncFakeDoltFetchFlaky(t, binDir, "unexpected EOF", 1, 99)
			out, code := runFFSyncCode(t, binDir, []string{"GC_DOLT_SYNC_FETCH_RETRY_DELAY_SECS=" + bad}, "--db", "app")
			if code != 2 {
				t.Fatalf("GC_DOLT_SYNC_FETCH_RETRY_DELAY_SECS=%q: exit = %d, want 2\nout:\n%s", bad, code, out)
			}
			if !strings.Contains(out, "invalid GC_DOLT_SYNC_FETCH_RETRY_DELAY_SECS") {
				t.Fatalf("expected the invalid-delay error.\nout:\n%s", out)
			}
			if got := fetchAttempts(t, countPath); got != 0 {
				t.Fatalf("GC_DOLT_SYNC_FETCH_RETRY_DELAY_SECS=%q must abort before touching a db: fetch attempts = %d\nout:\n%s", bad, got, out)
			}
		})
	}
}

// TestSyncColdOpenWallReportedAfterBudget: a wall-shaped attempt is retried
// like any transport failure, and only a budget whose last attempt still dies
// at the listener wall is reported as the vp-9v6f9 cold-open wall, with the
// attempts spent.
func TestSyncColdOpenWallReportedAfterBudget(t *testing.T) {
	binDir := t.TempDir()
	logPath := writeSyncFakeDoltColdWall(t, binDir)
	cfgPath := filepath.Join(t.TempDir(), "dolt-config.yaml")
	if err := os.WriteFile(cfgPath, []byte("listener:\n  read_timeout_millis: 2000\n"), 0o644); err != nil {
		t.Fatalf("write fake dolt-config.yaml: %v", err)
	}

	out, code := runFFSyncCode(t, binDir, []string{
		"GC_DOLT_CONFIG_FILE=" + cfgPath,
		"GC_DOLT_SYNC_FETCH_ATTEMPTS=2",
	}, "--db", "app")

	if got := strings.Count(readLog(t, logPath), "CALL DOLT_FETCH("); got != 2 {
		t.Fatalf("a wall-shaped attempt must be retried: fetch attempts = %d, want 2\nout:\n%s", got, out)
	}
	if !strings.Contains(out, "listener deadline 2s, 2 attempt(s)") {
		t.Fatalf("expected a COLD-OPEN WALL status reporting the attempts spent.\nout:\n%s", out)
	}
	if !strings.Contains(out, "NO OFF-BOX COPY:") {
		t.Fatalf("expected the no-off-box-copy summary.\nout:\n%s", out)
	}
	if code == 0 {
		t.Fatalf("expected a non-zero exit for a cold-open wall\nout:\n%s", out)
	}
}

// TestDoltRemotesPatrolTimeoutCoversFetchRetries: the sync run is sequential
// across databases, and after a server restart every store is cold at once,
// so a cycle can spend several fetch attempts per store. At the 300s exec
// default a few slow stores would exhaust the order deadline and the databases
// later in the scan would be killed before they were attempted: silent
// starvation that the emitted event cannot tell apart from a clean run.
func TestDoltRemotesPatrolTimeoutCoversFetchRetries(t *testing.T) {
	data, err := os.ReadFile(filepath.Join(repoRoot(t), "orders", "dolt-remotes-patrol.toml"))
	if err != nil {
		t.Fatalf("read order: %v", err)
	}
	o, err := orders.Parse(data)
	if err != nil {
		t.Fatalf("parse order: %v", err)
	}
	if got := o.TimeoutOrDefault(); got < 600*time.Second {
		t.Fatalf("dolt-remotes-patrol timeout = %s, want at least 600s for the fetch retry budget", got)
	}
}

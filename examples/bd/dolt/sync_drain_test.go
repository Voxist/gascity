package dolt_test

// Delivery-window drain mode (vp-p8tze / ADR-0064 D1 step 2, D3, AC2, AC4).
//
// `CALL DOLT_PUSH` returning 0 reports that *this push* succeeded — not that the
// store has nothing left to deliver. The 2026-08-04 hand-run window trusted that
// result, pushed once per store, and hq came out at ~2 commits unpushed while the
// window read as complete. `--drain` closes that gap: after a successful push it
// re-fetches and re-counts the `ahead` range, re-pushes while a residual remains
// (bounded), and reports any store it could not drive to a verified zero as a
// terminal, named, non-zero failure.
//
// These tests drive the real run.sh through the shared runFFSyncRaw call site
// (the untagged-source subprocess census is a checked ratchet — see the note on
// runFFSyncRaw), against a fake dolt whose ahead-count is STATEFUL: the pre-push
// classification and the post-push verification are distinct observations of the
// same query, and a fake that answered them identically could not tell a verified
// drain from an unverified one.

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// writeSyncFakeDoltDrain installs a fake dolt whose `ahead` answer changes with
// each call: the first firstAhead responses report aheadCount, every later one
// reports 0. With firstAhead=1 the store drains on the first push (pre-push
// classify sees a backlog, the post-push verify sees zero). With a firstAhead
// larger than the run's attempt budget the store never drains, which is the
// undrainable case D3 must report terminally.
func writeSyncFakeDoltDrain(t *testing.T, dir string, aheadCount, firstAhead int) string {
	t.Helper()
	branch := "main"
	logPath := filepath.Join(dir, "dolt.log")
	counterPath := filepath.Join(dir, "ahead.count")
	aheadPat := "dolt_log('remotes/origin/" + branch + ".." + branch + "')"
	behindPat := "dolt_log('" + branch + "..remotes/origin/" + branch + "')"
	body := fakeDoltHeader(logPath, branch) +
		"  *\"CALL DOLT_FETCH(\"*) exit 0 ;;\n" +
		"  *\"" + aheadPat + "\"*)\n" +
		"    n=$(cat " + counterPath + " 2>/dev/null || echo 0)\n" +
		"    n=$((n + 1))\n" +
		"    printf '%s' \"$n\" > " + counterPath + "\n" +
		"    if [ \"$n\" -le " + fmt.Sprintf("%d", firstAhead) + " ]; then\n" +
		"      printf 'n\\n" + fmt.Sprintf("%d", aheadCount) + "\\n'\n" +
		"    else\n" +
		"      printf 'n\\n0\\n'\n" +
		"    fi\n" +
		"    exit 0 ;;\n" +
		"  *\"" + behindPat + "\"*) printf 'n\\n0\\n' ; exit 0 ;;\n" +
		"esac\nexit 0\n"
	return installFFFakeDolt(t, dir, body)
}

// countFetches reports how many CALL DOLT_FETCH invocations reached the fake.
// The pre-push classification always issues one; the post-push verification is
// the second. It is the instrument that distinguishes "verified zero" from
// "assumed zero" — the whole point of AC2.
func countFetches(log string) int {
	return strings.Count(log, "CALL DOLT_FETCH(")
}

func TestSyncDrainVerifiesZeroBacklogAfterPush(t *testing.T) {
	binDir := t.TempDir()
	logPath := writeSyncFakeDoltDrain(t, binDir, 2, 1)
	out, err := runFFSyncRaw(t, binDir, nil, "--drain", "--db", "app")
	if err != nil {
		t.Fatalf("a store that reaches zero backlog must exit 0, got %v.\nout:\n%s", err, out)
	}
	log := readLog(t, logPath)
	if !strings.Contains(log, "CALL DOLT_PUSH('origin', 'main')") {
		t.Fatalf("expected a fast-forward push.\nout:\n%s\nlog:\n%s", out, log)
	}
	if got := countFetches(log); got != 2 {
		t.Fatalf("drain mode must re-fetch to verify the residual: want 2 fetches (classify + verify), got %d.\nlog:\n%s", got, log)
	}
	if !strings.Contains(out, "backlog 0, verified") {
		t.Fatalf("a drained store must say so explicitly.\nout:\n%s", out)
	}
	if strings.Contains(out, "BACKLOG NOT DRAINED") {
		t.Fatalf("a store that reached zero must not be reported as undrained.\nout:\n%s", out)
	}
}

func TestSyncDrainRePushesAndFailsTerminallyWhenBacklogRemains(t *testing.T) {
	binDir := t.TempDir()
	// firstAhead far beyond the attempt budget: the residual never clears, so
	// the store is undrainable and must be reported rather than retried forever.
	logPath := writeSyncFakeDoltDrain(t, binDir, 3, 99)
	out, err := runFFSyncRaw(t, binDir, []string{"GC_DOLT_SYNC_DRAIN_ATTEMPTS=2"}, "--drain", "--db", "app")
	if err == nil {
		t.Fatalf("a store that never reaches zero backlog must exit non-zero.\nout:\n%s", out)
	}
	log := readLog(t, logPath)
	if got := strings.Count(log, "CALL DOLT_PUSH("); got != 2 {
		t.Fatalf("drain must re-push up to the attempt budget and then stop: want 2 pushes, got %d.\nlog:\n%s", got, log)
	}
	// D3: the failure must name the store in the end-of-run summary, not only in
	// a per-store line that scrolls past in a multi-store run. Index (not
	// Contains) is the guard here so the slice below can never be taken from -1.
	summaryStart := strings.Index(out, "BACKLOG NOT DRAINED")
	if summaryStart < 0 {
		t.Fatalf("an undrainable store must produce a terminal record.\nout:\n%s", out)
	}
	summary := out[summaryStart:]
	if !strings.Contains(summary, "app") {
		t.Fatalf("the summary must name the undrained store.\nout:\n%s", out)
	}
	if !strings.Contains(out, "NO complete off-box copy") {
		t.Fatalf("the record must state the durability consequence, not just the count.\nout:\n%s", out)
	}
}

func TestSyncWithoutDrainKeepsSingleShotPushSemantics(t *testing.T) {
	binDir := t.TempDir()
	// Same never-draining store as the failing test above. Outside the delivery
	// window this must stay exactly as it was: one push, no verification fetch,
	// no new failure. The routine patrol is not making a durability claim, and on
	// a live city a residual arriving between push and verify is normal — failing
	// the patrol for it would be a false alarm, and paying for the extra fetch on
	// a store that is cold for this server lifetime would risk the listener wall.
	logPath := writeSyncFakeDoltDrain(t, binDir, 3, 99)
	out, err := runFFSyncRaw(t, binDir, nil, "--db", "app")
	if err != nil {
		t.Fatalf("default (non-drain) sync must keep exiting 0 after a successful push, got %v.\nout:\n%s", err, out)
	}
	log := readLog(t, logPath)
	if got := strings.Count(log, "CALL DOLT_PUSH("); got != 1 {
		t.Fatalf("non-drain mode must push exactly once: got %d.\nlog:\n%s", got, log)
	}
	if got := countFetches(log); got != 1 {
		t.Fatalf("non-drain mode must not issue the verification fetch: want 1, got %d.\nlog:\n%s", got, log)
	}
	if strings.Contains(out, "BACKLOG NOT DRAINED") || strings.Contains(out, "backlog 0, verified") {
		t.Fatalf("non-drain mode must not emit drain reporting.\nout:\n%s", out)
	}
}

// A store with no remote has no off-box copy and never will. Outside the window
// that is a benign skip; inside it, it is the P0 condition itself, and a window
// that exits 0 having stepped over it reproduces the vp-cblo shape where
// order.completed reads as evidence of a fresh backup.
func TestSyncDrainTreatsMissingRemoteAsTerminal(t *testing.T) {
	binDir := t.TempDir()
	logPath := filepath.Join(binDir, "dolt.log")
	body := "#!/bin/sh\n" +
		"printf '%s\\n' \"$*\" >> \"" + logPath + "\"\n" +
		"case \"$*\" in\n" +
		"  *\"SELECT name, url FROM dolt_remotes LIMIT 1\"*) printf 'name,url\\n' ; exit 0 ;;\n" +
		"esac\nexit 0\n"
	if err := os.WriteFile(filepath.Join(binDir, "dolt"), []byte(body), 0o755); err != nil {
		t.Fatalf("write fake dolt: %v", err)
	}

	out, err := runFFSyncRaw(t, binDir, nil, "--drain", "--db", "app")
	if err == nil {
		t.Fatalf("--drain over a store with no remote must exit non-zero.\nout:\n%s", out)
	}
	if !strings.Contains(out, "NO REMOTE") {
		t.Fatalf("the missing remote must be named as such.\nout:\n%s", out)
	}
	if !strings.Contains(out, "BACKLOG NOT DRAINED") {
		t.Fatalf("a no-remote store must reach the end-of-run terminal summary.\nout:\n%s", out)
	}

	// Same store, default mode: unchanged benign skip.
	out, err = runFFSyncRaw(t, binDir, nil, "--db", "app")
	if err != nil {
		t.Fatalf("without --drain a no-remote store must stay a benign skip, got %v.\nout:\n%s", err, out)
	}
	if !strings.Contains(out, "skipped (no remote)") {
		t.Fatalf("expected the pre-existing skip status.\nout:\n%s", out)
	}
}

func TestSyncDrainAttemptsKnobRejectsUnboundedValues(t *testing.T) {
	binDir := t.TempDir()
	writeSyncFakeDoltDrain(t, binDir, 1, 1)
	// "0" and leading-zero forms would mean "no push rounds at all", which would
	// make --drain silently deliver nothing while still reporting per-store
	// success. Same rejection rule the push/fetch timeouts already use.
	for _, bad := range []string{"0", "00", "", "abc", "-1"} {
		out, err := runFFSyncRaw(t, binDir,
			[]string{"GC_DOLT_SYNC_DRAIN_ATTEMPTS=" + bad}, "--drain", "--db", "app")
		if err == nil {
			t.Fatalf("GC_DOLT_SYNC_DRAIN_ATTEMPTS=%q must be rejected.\nout:\n%s", bad, out)
		}
		if !strings.Contains(out, "invalid GC_DOLT_SYNC_DRAIN_ATTEMPTS") {
			t.Fatalf("rejection must name the offending variable.\nout:\n%s", out)
		}
	}
}

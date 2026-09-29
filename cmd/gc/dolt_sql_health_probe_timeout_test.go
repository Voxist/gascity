package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// ga-z3c6p. The query-probe half of the ga-amol9 class of bug: op_health's
// tcp_check already distinguished "could not reach it" (die_unobservable,
// exit 3) from "reached it and it's bad" (die, exit 1). The query probe that
// runs right after tcp_check did not — a probe that hit
// managedDoltSQLCommandTimeout under the same host load that starves
// tcp_check's own `nc` was classified by managedDoltHealthOpEvidence as
// recoverEvidenceHealthOpAnswered (an observation), which skips the liveness
// check entirely and can replace a live server on nothing but slowness.

// stubSlowDoltBinary puts a `dolt` on PATH that sleeps past delay before
// exiting cleanly, so a real exec.CommandContext timeout fires through the
// genuine code path (runManagedDoltSQLContext) rather than a synthesized
// error. `sleep` takes whole seconds portably, so delay is rounded up.
func stubSlowDoltBinary(t *testing.T, delay time.Duration) {
	t.Helper()
	binDir := t.TempDir()
	sleepSeconds := int(delay/time.Second) + 1
	script := fmt.Sprintf("#!/bin/sh\nsleep %d\n", sleepSeconds)
	if err := os.WriteFile(filepath.Join(binDir, "dolt"), []byte(script), 0o755); err != nil {
		t.Fatalf("write dolt stub: %v", err)
	}
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))
}

// stubFailingDoltBinary puts a `dolt` on PATH that exits immediately with a
// non-timeout failure, standing in for a probe that ran to completion and
// reported a genuine error.
func stubFailingDoltBinary(t *testing.T, stderr string) {
	t.Helper()
	binDir := t.TempDir()
	script := "#!/bin/sh\necho " + shellQuote(stderr) + " >&2\nexit 1\n"
	if err := os.WriteFile(filepath.Join(binDir, "dolt"), []byte(script), 0o755); err != nil {
		t.Fatalf("write dolt stub: %v", err)
	}
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))
}

func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

func withShortSQLCommandTimeout(t *testing.T, timeout time.Duration) {
	t.Helper()
	old := managedDoltSQLCommandTimeout
	managedDoltSQLCommandTimeout = timeout
	t.Cleanup(func() { managedDoltSQLCommandTimeout = old })
}

func TestManagedDoltQueryProbeTimesOutAsUnobservable(t *testing.T) {
	stubSlowDoltBinary(t, 2*time.Second)
	withShortSQLCommandTimeout(t, 100*time.Millisecond)

	err := managedDoltQueryProbe("127.0.0.1", "1", "root")
	if !errors.Is(err, errManagedDoltQueryProbeTimeout) {
		t.Fatalf("managedDoltQueryProbe against a hanging dolt CLI = %v, want errManagedDoltQueryProbeTimeout", err)
	}
}

func TestManagedDoltQueryProbeAnsweredBadStaysObserved(t *testing.T) {
	stubFailingDoltBinary(t, "dolt: connection refused")
	withShortSQLCommandTimeout(t, 2*time.Second)

	err := managedDoltQueryProbe("127.0.0.1", "1", "root")
	if err == nil {
		t.Fatal("managedDoltQueryProbe against a failing dolt CLI = nil, want an error")
	}
	if errors.Is(err, errManagedDoltQueryProbeTimeout) {
		t.Fatalf("managedDoltQueryProbe against a FAST failure was classified as a timeout: %v", err)
	}
}

func TestManagedDoltWrapQueryProbeTimeoutClassifiesOnContextDeadline(t *testing.T) {
	expired, cancel := context.WithDeadline(context.Background(), time.Now().Add(-time.Second))
	defer cancel()
	<-expired.Done()
	wrapped := managedDoltWrapQueryProbeTimeout(expired, errors.New("some driver error"))
	if !errors.Is(wrapped, errManagedDoltQueryProbeTimeout) {
		t.Fatalf("wrapping against an expired context = %v, want errManagedDoltQueryProbeTimeout", wrapped)
	}

	live := context.Background()
	unwrapped := managedDoltWrapQueryProbeTimeout(live, errors.New("some driver error"))
	if errors.Is(unwrapped, errManagedDoltQueryProbeTimeout) {
		t.Fatalf("wrapping against a live context = %v, want unchanged (not a timeout)", unwrapped)
	}
}

// ---------------------------------------------------------------------
// The `gc dolt-state` cobra commands: the actual process exit code.
// ---------------------------------------------------------------------

func runDoltStateCommand(t *testing.T, args ...string) error {
	t.Helper()
	cmd := newDoltStateCmd(new(strings.Builder), new(strings.Builder))
	cmd.SetArgs(args)
	return cmd.Execute()
}

func TestDoltStateQueryProbeExitsUnobservableOnTimeout(t *testing.T) {
	stubSlowDoltBinary(t, 2*time.Second)
	withShortSQLCommandTimeout(t, 100*time.Millisecond)

	err := runDoltStateCommand(t, "query-probe", "--host", "127.0.0.1", "--port", "1")
	if got := commandExitCode(err); got != providerOpExitUnobservable {
		t.Fatalf("gc dolt-state query-probe exit code on timeout = %d, want %d (providerOpExitUnobservable)", got, providerOpExitUnobservable)
	}
}

func TestDoltStateHealthCheckExitsUnobservableOnTimeout(t *testing.T) {
	stubSlowDoltBinary(t, 2*time.Second)
	withShortSQLCommandTimeout(t, 100*time.Millisecond)

	err := runDoltStateCommand(t, "health-check", "--host", "127.0.0.1", "--port", "1")
	if got := commandExitCode(err); got != providerOpExitUnobservable {
		t.Fatalf("gc dolt-state health-check exit code on timeout = %d, want %d (providerOpExitUnobservable)", got, providerOpExitUnobservable)
	}
}

func TestDoltStateQueryProbeStillExitsOneWhenAnsweredBad(t *testing.T) {
	stubFailingDoltBinary(t, "dolt: connection refused")
	withShortSQLCommandTimeout(t, 2*time.Second)

	err := runDoltStateCommand(t, "query-probe", "--host", "127.0.0.1", "--port", "1")
	if err == nil {
		t.Fatal("gc dolt-state query-probe against a failing dolt CLI = nil error, want a failure")
	}
	if got := commandExitCode(err); got != 1 {
		t.Fatalf("gc dolt-state query-probe exit code for an answered-bad probe = %d, want 1 (unchanged)", got)
	}
}

// ---------------------------------------------------------------------
// End to end: the exit code this fix produces, run through the SAME
// recover-gate classifier and guard op_health's real callers use, proving
// the whole chain — not just this file's own sentinel — lands on decline
// against a live server, and still recovers for a genuine answered-bad
// probe.
// ---------------------------------------------------------------------

func TestQueryProbeTimeoutEndsInADeclinedRecoverAgainstALiveServer(t *testing.T) {
	// providerOpExitErrorForTest actually forks a script and exits it with
	// the given code, so this is the same shape runProviderOpWithEnvContext
	// hands managedDoltHealthOpEvidence in production when the REAL
	// op_health calls die_unobservable(exit 3) after this bead's shell-side
	// fix propagates gc dolt-state health-check's exit code through
	// load_health_check_from_gc.
	err := providerOpExitErrorForTest(t, providerOpExitUnobservable, "dolt query probe timed out (information_schema.SCHEMATA)")
	evidence := managedDoltHealthOpEvidence(context.Background(), err)
	if evidence != recoverEvidenceCallFailed {
		t.Fatalf("evidence for an exit-3 (unobservable) query-probe timeout = %v, want recoverEvidenceCallFailed", evidence)
	}

	stubRecoverGateClock(t)
	stubRecoverLiveness(t, managedDoltLivenessAlive)
	runs := countRecoverRuns(t)

	cityPath := t.TempDir()
	recoverErr := runGuardedManagedDoltRecover(context.Background(), cityPath, "script", nil, evidence)
	if !errors.Is(recoverErr, errManagedDoltRecoverDeclined) {
		t.Fatalf("runGuardedManagedDoltRecover for a timed-out query probe against a LIVE server = %v, want declined", recoverErr)
	}
	if *runs != 0 {
		t.Fatalf("recover runs = %d, want 0: a query-probe timeout must never replace a live server", *runs)
	}
}

func TestQueryProbeAnsweredBadStillRecoversAgainstALiveServer(t *testing.T) {
	// The mirror-image regression guard: a probe that ran to completion and
	// reported a genuine failure (not a timeout) must keep its existing
	// behavior — an observation, which may replace even a server this
	// guard's OWN liveness stub calls "alive" (op_health's positive
	// evidence is not required to agree with a stale/wrong liveness read;
	// that asymmetry is what closes ga-fkidk, and this bead must not touch
	// it).
	err := providerOpExitErrorForTest(t, 1, "dolt query probe failed (information_schema.SCHEMATA)")
	evidence := managedDoltHealthOpEvidence(context.Background(), err)
	if evidence != recoverEvidenceHealthOpAnswered {
		t.Fatalf("evidence for an exit-1 (answered bad) query probe = %v, want recoverEvidenceHealthOpAnswered", evidence)
	}

	stubRecoverGateClock(t)
	stubRecoverLiveness(t, managedDoltLivenessAlive)
	runs := countRecoverRuns(t)

	cityPath := t.TempDir()
	if err := runGuardedManagedDoltRecover(context.Background(), cityPath, "script", nil, evidence); err != nil {
		t.Fatalf("runGuardedManagedDoltRecover for an answered-bad query probe = %v, want it to run (this bead must not touch this path)", err)
	}
	if *runs != 1 {
		t.Fatalf("recover runs = %d, want 1: an answered-bad probe is still a genuine observation", *runs)
	}
}

// ---------------------------------------------------------------------
// The bundled shell script: pinning that the shape this Go-level fix
// depends on actually ships, mirroring
// TestBundledProviderScriptExitsUnobservableWhenTCPCheckFails's own
// convention (beads_provider_recover_gate_test.go) for the same class of
// fix at op_health's tcp_check step.
// ---------------------------------------------------------------------

func TestBundledProviderScriptPropagatesQueryProbeTimeoutAsUnobservable(t *testing.T) {
	script := filepath.Join("..", "..", "examples", "bd", "assets", "scripts", "gc-beads-bd.sh")
	src, err := os.ReadFile(script)
	if err != nil {
		t.Fatalf("bundled provider script not found at %s: %v", script, err)
	}
	text := string(src)

	if strings.Contains(text, `output=$("$gc_bin" dolt-state health-check --host "$host" --port "$DOLT_PORT" --user "$DOLT_USER" --check-read-only </dev/null 2>/dev/null) || return 1`) {
		t.Fatal("load_health_check_from_gc still collapses every gc dolt-state health-check failure to 1; a query-probe-timeout exit 3 would be lost (ga-z3c6p)")
	}
	if !strings.Contains(text, `status=$?`) {
		t.Fatal("load_health_check_from_gc no longer captures the real exit status")
	}
	if !strings.Contains(text, `return "$status"`) {
		t.Fatal("load_health_check_from_gc no longer propagates its captured status")
	}
	if !strings.Contains(text, `health_check_status=$?`) {
		t.Fatal("op_health no longer captures load_health_check_from_gc's failure status")
	}
	if !strings.Contains(text, `query_probe_status=$?`) {
		t.Fatal("op_health no longer captures do_query_probe's failure status")
	}
	if strings.Count(text, `die_unobservable "dolt query probe timed out (information_schema.SCHEMATA)"`) != 2 {
		t.Fatal("op_health no longer calls die_unobservable for a timed-out query probe on both the loader and do_query_probe branches")
	}
}

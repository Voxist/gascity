package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"os/exec"
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
//
// Clears GC_DOLT_PASSWORD: every caller of this helper means to exercise the
// CLI (dolt-fork) lane specifically, and managedDoltPassword() != "" routes
// every one of these functions to the direct (sql-driver) lane instead —
// silently testing a different code path if the ambient environment happens
// to carry that variable.
func stubSlowDoltBinary(t *testing.T, delay time.Duration) {
	t.Helper()
	t.Setenv("GC_DOLT_PASSWORD", "")
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
// reported a genuine error. See stubSlowDoltBinary's doc comment for why
// GC_DOLT_PASSWORD is cleared.
func stubFailingDoltBinary(t *testing.T, stderr string) {
	t.Helper()
	t.Setenv("GC_DOLT_PASSWORD", "")
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

// TestManagedDoltWrapSQLCommandTimeoutClassifiesOnContextDeadline is the
// read-only-step direct-lane counterpart of
// TestManagedDoltWrapQueryProbeTimeoutClassifiesOnContextDeadline: the same
// ctx.Err()-gated classification, applied to managedDoltReadOnlyStateDirect's
// failure points instead of the query probe's (ga-z3c6p).
func TestManagedDoltWrapSQLCommandTimeoutClassifiesOnContextDeadline(t *testing.T) {
	expired, cancel := context.WithDeadline(context.Background(), time.Now().Add(-time.Second))
	defer cancel()
	<-expired.Done()
	wrapped := managedDoltWrapSQLCommandTimeout(expired, errors.New("some driver error"))
	if !errors.Is(wrapped, errManagedDoltSQLCommandTimeout) {
		t.Fatalf("wrapping against an expired context = %v, want errManagedDoltSQLCommandTimeout", wrapped)
	}

	live := context.Background()
	unwrapped := managedDoltWrapSQLCommandTimeout(live, errors.New("some driver error"))
	if errors.Is(unwrapped, errManagedDoltSQLCommandTimeout) {
		t.Fatalf("wrapping against a live context = %v, want unchanged (not a timeout)", unwrapped)
	}
}

// TestManagedDoltReadOnlyStateDirectClassifiesHangAsTimeout is the direct-lane
// (GC_DOLT_PASSWORD set) counterpart of the CLI-lane read-only-step-timeout
// coverage above: a listener that accepts the TCP connection but never sends
// the MySQL handshake models a wedged server, so the driver blocks reading
// it until managedDoltReadOnlyStateDirect's own 5s context deadline fires —
// the same shape a real hung dolt server under host load produces. Real
// 5-second wait; there is no shorter knob for the direct lane's hardcoded
// timeout.
func TestManagedDoltReadOnlyStateDirectClassifiesHangAsTimeout(t *testing.T) {
	t.Setenv("GC_DOLT_PASSWORD", "x")

	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer listener.Close() //nolint:errcheck

	accepted := make(chan net.Conn, 1)
	go func() {
		conn, err := listener.Accept()
		if err == nil {
			accepted <- conn
		}
	}()
	t.Cleanup(func() {
		select {
		case conn := <-accepted:
			conn.Close() //nolint:errcheck
		default:
		}
	})

	host, port, err := net.SplitHostPort(listener.Addr().String())
	if err != nil {
		t.Fatalf("split listener addr: %v", err)
	}

	start := time.Now()
	state, stateErr := managedDoltReadOnlyStateDirect(host, port, "root")
	elapsed := time.Since(start)
	if elapsed < 4*time.Second {
		t.Fatalf("managedDoltReadOnlyStateDirect returned after %s, want it to have waited out its own 5s context deadline", elapsed)
	}
	if state != "unknown" {
		t.Fatalf("state = %q, want %q", state, "unknown")
	}
	if !errors.Is(stateErr, errManagedDoltSQLCommandTimeout) {
		t.Fatalf("err = %v, want errManagedDoltSQLCommandTimeout", stateErr)
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

// stubDoltAnswersProbeButHangsOnShowDatabases puts a `dolt` on PATH that
// answers the query probe's information_schema.SCHEMATA query immediately
// but sleeps past delay specifically on SHOW DATABASES — the read-only
// step's own first query, run only after the probe has already answered —
// so a real exec.CommandContext timeout fires on that later step alone.
// Clears GC_DOLT_PASSWORD for the same reason as stubSlowDoltBinary.
func stubDoltAnswersProbeButHangsOnShowDatabases(t *testing.T, delay time.Duration) {
	t.Helper()
	t.Setenv("GC_DOLT_PASSWORD", "")
	binDir := t.TempDir()
	sleepSeconds := int(delay/time.Second) + 1
	script := fmt.Sprintf("#!/bin/sh\ncase \"$*\" in\n  *\"SHOW DATABASES\"*)\n    sleep %d\n    ;;\nesac\nexit 0\n", sleepSeconds)
	if err := os.WriteFile(filepath.Join(binDir, "dolt"), []byte(script), 0o755); err != nil {
		t.Fatalf("write dolt stub: %v", err)
	}
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))
}

// TestManagedDoltHealthCheckReadOnlyTimeoutStaysHealthy is the ga-z3c6p fix
// at the Go level: cmd_dolt_state.go's health-check used to exit 1 when the
// read-only step timed out AFTER the query probe had already proven the
// server alive — an inconclusive later step overriding a proven-good
// observation. It must instead report ReadOnly "unknown" and succeed, the
// same way an absent user database (errManagedDoltNoUserDatabase) already
// does not fail the check.
func TestManagedDoltHealthCheckReadOnlyTimeoutStaysHealthy(t *testing.T) {
	stubDoltAnswersProbeButHangsOnShowDatabases(t, 2*time.Second)
	withShortSQLCommandTimeout(t, 100*time.Millisecond)

	report, err := managedDoltHealthCheck("127.0.0.1", "1", "root", true)
	if err != nil {
		t.Fatalf("managedDoltHealthCheck with a read-only step that times out after the probe answered = %v, want no error", err)
	}
	if !report.QueryReady {
		t.Fatal("report.QueryReady = false, want true: the query probe answered before the read-only step ever ran")
	}
	if report.ReadOnly != "unknown" {
		t.Fatalf("report.ReadOnly = %q, want %q", report.ReadOnly, "unknown")
	}
}

// TestDoltStateHealthCheckDoesNotExitOneOnReadOnlyStepTimeout is the same
// fix through the actual `gc dolt-state health-check` process exit code —
// the shape health-patrol's caller actually observes.
func TestDoltStateHealthCheckDoesNotExitOneOnReadOnlyStepTimeout(t *testing.T) {
	stubDoltAnswersProbeButHangsOnShowDatabases(t, 2*time.Second)
	withShortSQLCommandTimeout(t, 100*time.Millisecond)

	err := runDoltStateCommand(t, "health-check", "--host", "127.0.0.1", "--port", "1", "--check-read-only")
	if err != nil {
		t.Fatalf("gc dolt-state health-check with a read-only step that times out after the probe answered = %v, want success (exit 0)", err)
	}
}

// stubDoltAnswersProbeButHangsOnProcesslist mirrors
// stubDoltAnswersProbeButHangsOnShowDatabases for the connection-count step's
// own query (information_schema.PROCESSLIST) instead of the read-only step's.
func stubDoltAnswersProbeButHangsOnProcesslist(t *testing.T, delay time.Duration) {
	t.Helper()
	t.Setenv("GC_DOLT_PASSWORD", "")
	binDir := t.TempDir()
	sleepSeconds := int(delay/time.Second) + 1
	script := fmt.Sprintf("#!/bin/sh\ncase \"$*\" in\n  *\"PROCESSLIST\"*)\n    sleep %d\n    ;;\nesac\nexit 0\n", sleepSeconds)
	if err := os.WriteFile(filepath.Join(binDir, "dolt"), []byte(script), 0o755); err != nil {
		t.Fatalf("write dolt stub: %v", err)
	}
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))
}

// TestManagedDoltHealthCheckConnectionCountTimeoutStaysHealthy confirms the
// connection-count step (already non-fatal by construction: its caller only
// ever checks `err == nil` before using the result) really does stay that
// way under a real timeout, not just when the query fails fast.
func TestManagedDoltHealthCheckConnectionCountTimeoutStaysHealthy(t *testing.T) {
	stubDoltAnswersProbeButHangsOnProcesslist(t, 2*time.Second)
	withShortSQLCommandTimeout(t, 100*time.Millisecond)

	report, err := managedDoltHealthCheck("127.0.0.1", "1", "root", false)
	if err != nil {
		t.Fatalf("managedDoltHealthCheck with a connection-count step that times out = %v, want no error", err)
	}
	if !report.QueryReady {
		t.Fatal("report.QueryReady = false, want true: the query probe answered before the connection-count step ever ran")
	}
	if report.ConnectionCount != "" {
		t.Fatalf("report.ConnectionCount = %q, want empty (count unavailable is not fatal)", report.ConnectionCount)
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

// stubGCBinaryForHealthProbe writes a fake `gc` binary that answers
// `dolt-state health-check` and `dolt-state query-probe` with the given exit
// codes and nothing else, so op_health's real health-check/query-probe
// exit-code interplay can be driven from Go without a real dolt server.
func stubGCBinaryForHealthProbe(t *testing.T, healthCheckExit, queryProbeExit int) string {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "gc-stub")
	script := fmt.Sprintf("#!/bin/sh\ncase \"$1 $2\" in\n  \"dolt-state health-check\")\n    exit %d\n    ;;\n  \"dolt-state query-probe\")\n    exit %d\n    ;;\nesac\nexit 0\n", healthCheckExit, queryProbeExit)
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
		t.Fatalf("write stub gc: %v", err)
	}
	return path
}

// runOpHealthAgainstStub extracts op_health and the functions it calls
// straight out of the bundled provider script and runs the real thing as a
// bash subprocess (mirroring TestServerReachableReflectsDoltExit's own
// extractShellFunction convention), with GC_BIN pointed at a stub gc that
// answers health-check/query-probe with the given exit codes. tcp_check and
// is_remote are overridden to isolate this test to the health-check/
// query-probe exit-code decision op_health makes — TCP reachability and
// read-only detection are covered elsewhere and are not what ga-z3c6p is
// about. Returns the process's exit code.
func runOpHealthAgainstStub(t *testing.T, healthCheckExit, queryProbeExit int) int {
	t.Helper()
	script := filepath.Join("..", "..", "examples", "bd", "assets", "scripts", "gc-beads-bd.sh")
	src, err := os.ReadFile(script)
	if err != nil {
		t.Fatalf("bundled provider script not found at %s: %v", script, err)
	}
	text := string(src)

	composed := strings.Join([]string{
		"set -e",
		"tcp_check() { return 0; }",
		"is_remote() { return 0; }",
		"connect_host() { printf '127.0.0.1'; }",
		extractShellFunction(t, text, "die"),
		extractShellFunction(t, text, "die_unobservable"),
		extractShellFunction(t, text, "resolve_gc_helper_bin"),
		extractShellFunction(t, text, "load_health_check_from_gc"),
		extractShellFunction(t, text, "do_query_probe"),
		extractShellFunction(t, text, "get_connection_count"),
		extractShellFunction(t, text, "op_health"),
		"op_health",
	}, "\n")

	gcBin := stubGCBinaryForHealthProbe(t, healthCheckExit, queryProbeExit)
	cmd := exec.Command("bash", "-c", composed)
	cmd.Env = append(os.Environ(),
		"GC_BIN="+gcBin,
		"DOLT_PORT=1",
		"DOLT_USER=root",
		"DOLT_PASSWORD=",
	)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	runErr := cmd.Run()
	if runErr != nil {
		exitError := &exec.ExitError{}
		if errors.As(runErr, &exitError) {
			t.Fatalf("running op_health: %v (stderr: %s)", runErr, stderr.String())
		}
	}
	return commandExitCode(runErr)
}

// TestOpHealthPropagatesQueryProbeExitCode is the ga-z3c6p behavioral
// regression guard, replacing a prior version of this test that only
// grepped the script for the text of the fix rather than running it — a
// check that passes on broken code (the bug's own capture line,
// `query_probe_status=$?`, appears verbatim inside `do_query_probe ||
// query_probe_status=$?` too, so the grep could never tell correct code
// from the `if ! do_query_probe; then query_probe_status=$?` bug it meant
// to catch).
func TestOpHealthPropagatesQueryProbeExitCode(t *testing.T) {
	cases := []struct {
		name            string
		healthCheckExit int
		queryProbeExit  int
		wantExit        int
	}{
		{"health_check_timeout_short_circuits", 3, 0, 3},
		{"health_check_answered_bad_query_probe_times_out", 1, 3, 3},
		{"health_check_answered_bad_query_probe_answered_bad_stays_observed", 1, 1, 1},
		{"health_check_answered_bad_query_probe_healthy", 1, 0, 0},
		{"health_check_other_failure_query_probe_times_out", 2, 3, 3},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := runOpHealthAgainstStub(t, tc.healthCheckExit, tc.queryProbeExit); got != tc.wantExit {
				t.Fatalf("op_health(health-check exit %d, query-probe exit %d) = %d, want %d", tc.healthCheckExit, tc.queryProbeExit, got, tc.wantExit)
			}
		})
	}
}

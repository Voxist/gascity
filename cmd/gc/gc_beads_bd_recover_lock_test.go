package main

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// ga-iv2l2. op_recover's own stop (op_stop_impl) and cleanup
// (run_preflight_cleanup) run before op_start, which is the ONLY place that
// acquires the LOCK_FILE flock (fd 9). Two concurrent op_recover
// invocations -- two separate OS processes, so the Go-side
// admitManagedDoltRecover single-flight (an in-process sync.Map, see
// dolt_recover_gate.go) cannot see across them -- can both reach that
// unprotected stop step around the same time. Whichever one reaches
// op_start's flock second may find the first process's freshly started
// server "not reusable" and stop-then-restart it again, killing a server
// the first process just legitimately started.
//
// Found during ga-3bwmf's #227 round-3 review. Review follow-up (same
// bead): the first cut of the fix signaled "caller already holds the
// lock" via GC_DOLT_START_LOCK_HELD, an environment variable -- and gc
// passes its whole process environment through to provider ops, so any
// inherited GC_DOLT_START_LOCK_HELD=true let a plain `start`/
// `ensure-ready` call skip the lock entirely. Fixed by passing it as an
// argument ("lock-held") instead, which the environment cannot reach,
// verified by op_start itself via `flock -n 9` on the INHERITED fd
// (never re-exec'd): that succeeds only if fd 9 really is the caller's
// locked descriptor.
//
// All composed scripts run under `sh -c`, not `bash -c`: the real
// provider script's shebang is #!/bin/sh and op_start/op_recover must
// work under whatever POSIX sh gc actually invokes them with.

// recoverLockStubs composes the fixed set of stand-ins op_recover needs to
// run its stop -> cleanup -> start sequence hermetically:
//   - is_remote: false, so op_recover doesn't die() immediately.
//   - recovery_should_skip_due_to_enospc: false, so the ENOSPC short-circuit
//     never fires.
//   - tcp_check: false, so op_recover's own pre-stop read-only diagnosis
//     (irrelevant to this bead) is skipped.
//   - load_recover_managed_from_gc: forces the pure shell fallback path (no
//     gc helper binary available) -- the path this bead's race lives in.
//     Real op_recover falls through here whenever resolve_gc_helper_bin
//     can't find a gc binary; stubbing it directly is equivalent and
//     avoids needing a fake gc binary on PATH.
//   - run_preflight_cleanup: no-op.
//   - op_health: healthy or failing, per healthOK -- a losing recover
//     re-probes it after losing the lock race, and whether that probe
//     passes decides whether the loser exits 0 (someone else already
//     fixed it) or 3 (unobservable: it could not confirm health AND
//     could not get the lock to check for itself).
//   - op_stop_impl / op_start: instrumented stubs that append a
//     start/sleep/end triple to a shared, process-tagged log file instead
//     of touching a real dolt server, so two concurrent runs can be told
//     apart afterward.
func recoverLockStubs(logFile string, healthOK bool) string {
	healthBody := "return 0"
	if !healthOK {
		// Mirrors the real op_health's own die_unobservable exit on an
		// unreachable server (die_unobservable is extracted into every
		// composed script that uses this stub), not a bare failure --
		// the loser's re-probe and the final verify at the end of a
		// genuine op_recover both see exactly this exit code shape.
		healthBody = `die_unobservable "stubbed: server down mid-stop"`
	}
	return fmt.Sprintf(`
is_remote() { return 1; }
recovery_should_skip_due_to_enospc() { return 1; }
tcp_check() { return 1; }
load_recover_managed_from_gc() { GC_RECOVER_MANAGED_USED="false"; return 1; }
run_preflight_cleanup() { :; }
op_health() { %[2]s; }
op_stop_impl() {
  printf 'stop start pid=%%s\n' "$$" >> %[1]q
  sleep 0.3
  printf 'stop end pid=%%s\n' "$$" >> %[1]q
  return 0
}
op_start() {
  printf 'start start pid=%%s\n' "$$" >> %[1]q
  sleep 0.1
  printf 'start end pid=%%s\n' "$$" >> %[1]q
  return 0
}
`, logFile, healthBody)
}

// opRecoverConcurrencyScript extracts the real op_recover (plus die and
// die_unobservable, which a lock-loser's unobservable exit depends on) out
// of the bundled provider script and layers recoverLockStubs underneath it.
func opRecoverConcurrencyScript(t *testing.T, logFile string, healthOK bool) string {
	t.Helper()
	script := filepath.Join("..", "..", "examples", "bd", "assets", "scripts", "gc-beads-bd.sh")
	src, err := os.ReadFile(script)
	if err != nil {
		t.Fatalf("bundled provider script not found at %s: %v", script, err)
	}
	text := string(src)

	return strings.Join([]string{
		"set -e",
		extractShellFunction(t, text, "die"),
		extractShellFunction(t, text, "die_unobservable"),
		recoverLockStubs(logFile, healthOK),
		extractShellFunction(t, text, "op_recover"),
		"op_recover",
	}, "\n")
}

// runOpRecover runs one op_recover invocation as a real subprocess against
// its own LOCK_FILE (shared across concurrent callers via lockFile) and
// returns its exit code.
func runOpRecover(t *testing.T, composed, lockFile string) int {
	t.Helper()
	cmd := exec.Command("sh", "-c", composed)
	cmd.Env = append(os.Environ(),
		"LOCK_FILE="+lockFile,
		"DOLT_PORT=1",
	)
	runErr := cmd.Run()
	return commandExitCode(runErr)
}

// requireFlock skips the test when flock isn't on PATH -- these tests
// drive real flock contention across real subprocesses, which a host
// without the binary cannot exercise at all.
func requireFlock(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("flock"); err != nil {
		t.Skip("flock not on PATH")
	}
}

// TestConcurrentOpRecoverSerializesStopAndStart is the ga-iv2l2 behavioral
// regression guard: two op_recover invocations launched at (as close to)
// the same instant must never both run their stop/start sequence -- the
// whole point of serializing op_recover under LOCK_FILE is that a second
// concurrent recover does not get to stop the server the first one is in
// the middle of (re)starting.
//
// Before the fix, op_recover's own op_stop_impl call is not under any
// flock (only op_start's internal "Acquire exclusive start lock" block
// is), so both processes reach op_stop_impl unprotected and this test's
// log shows two "stop start" entries. After the fix, only the flock winner
// ever calls op_stop_impl/op_start; the loser's non-blocking lock attempt
// fails, it re-probes op_health (stubbed healthy here) and returns 0
// without ever touching stop/start.
func TestConcurrentOpRecoverSerializesStopAndStart(t *testing.T) {
	requireFlock(t)
	dir := t.TempDir()
	logFile := filepath.Join(dir, "recover.log")
	lockFile := filepath.Join(dir, "dolt.lock")

	composed := opRecoverConcurrencyScript(t, logFile, true)

	var wg sync.WaitGroup
	exitCodes := make([]int, 2)
	wg.Add(2)
	for i := range 2 {
		go func(i int) {
			defer wg.Done()
			exitCodes[i] = runOpRecover(t, composed, lockFile)
		}(i)
	}
	wg.Wait()

	for i, code := range exitCodes {
		// A lock loser that finds a healthy server (the op_health stub
		// here always succeeds) is expected to exit 0 (nothing left to
		// do): see TestConcurrentOpRecoverLoserExitsUnobservableWhenHealthFails
		// for the exact-exit-3 case when the re-probe itself fails.
		if code != 0 {
			t.Fatalf("op_recover run %d exited %d, want 0 (op_health stub is always healthy here)", i, code)
		}
	}

	raw, err := os.ReadFile(logFile)
	if err != nil {
		t.Fatalf("read recover log: %v", err)
	}
	log := string(raw)
	stopStarts := strings.Count(log, "stop start pid=")
	startStarts := strings.Count(log, "start start pid=")
	if stopStarts != 1 || startStarts != 1 {
		t.Fatalf("two concurrent op_recover runs produced %d stop and %d start sequences, want exactly 1 of each "+
			"(op_recover's stop/cleanup/start sequence must run under one lock, ga-iv2l2):\n%s", stopStarts, startStarts, log)
	}
}

// TestConcurrentOpRecoverLoserExitsUnobservableWhenHealthFails is the
// precise-exit-code companion to the test above, requested in review: with
// op_health stubbed FAILING (server down mid-stop) rather than healthy,
// the loser's re-probe cannot confirm health either, so it must exit
// EXACTLY 3 (die_unobservable) -- not "0 or 3", which could not tell a
// correctly-declining loser from one that took some other exit path
// entirely. Both the stop/start serialization (still exactly one of
// each) and the loser's exact exit code are asserted together.
func TestConcurrentOpRecoverLoserExitsUnobservableWhenHealthFails(t *testing.T) {
	requireFlock(t)
	dir := t.TempDir()
	logFile := filepath.Join(dir, "recover.log")
	lockFile := filepath.Join(dir, "dolt.lock")

	composed := opRecoverConcurrencyScript(t, logFile, false)

	var wg sync.WaitGroup
	exitCodes := make([]int, 2)
	wg.Add(2)
	for i := range 2 {
		go func(i int) {
			defer wg.Done()
			exitCodes[i] = runOpRecover(t, composed, lockFile)
		}(i)
	}
	wg.Wait()

	// Every run's final step is its own "Verify health" op_health call,
	// which also fails here -- so BOTH the winner (after a real
	// stop/start) and the loser (which never reaches stop/start at all)
	// exit exactly 3 in this scenario. That is expected and does not
	// weaken the test: the stop/start log counts below are what prove
	// only one of the two actually ran the sequence.
	for i, code := range exitCodes {
		if code != 3 {
			t.Fatalf("op_recover run %d exited %d, want exactly 3 (die_unobservable): op_health is stubbed failing, "+
				"so neither a winner's final verify nor a loser's lock-loss re-probe can report healthy", i, code)
		}
	}

	raw, err := os.ReadFile(logFile)
	if err != nil {
		t.Fatalf("read recover log: %v", err)
	}
	log := string(raw)
	stopStarts := strings.Count(log, "stop start pid=")
	startStarts := strings.Count(log, "start start pid=")
	if stopStarts != 1 || startStarts != 1 {
		t.Fatalf("two concurrent op_recover runs (failing op_health) produced %d stop and %d start sequences, want "+
			"exactly 1 of each -- the loser must still never touch stop/start even though it also exits 3:\n%s",
			stopStarts, startStarts, log)
	}
}

// ---------------------------------------------------------------------
// The re-entrancy half: op_start itself must trust the "lock-held"
// ARGUMENT (never the environment) instead of re-acquiring LOCK_FILE, and
// must verify that argument against the real fd 9 rather than believing it
// unconditionally -- or a caller that already holds the lock (op_recover,
// after the fix above) would collide with itself the moment it called the
// real op_start, and worse, anything that inherits gc's environment could
// forge the same bypass.
// ---------------------------------------------------------------------

// opStartReentrancyScript extracts the real op_start (plus die,
// die_unobservable and wait_for_concurrent_start_ready, which the
// lock-contention branch depends on) and stubs everything else it touches:
// resolve_gc_helper_bin/is_remote/ensure_dolt_identity as no-ops, and
// load_existing_managed_from_gc as an immediate "existing server is
// reusable" hit, so a successful lock acquisition (real or trusted via the
// argument) leads straight to op_start's own exit-0 fast path without
// needing a real dolt server.
func opStartReentrancyScript(t *testing.T) string {
	t.Helper()
	script := filepath.Join("..", "..", "examples", "bd", "assets", "scripts", "gc-beads-bd.sh")
	src, err := os.ReadFile(script)
	if err != nil {
		t.Fatalf("bundled provider script not found at %s: %v", script, err)
	}
	text := string(src)

	stubs := `
is_remote() { return 1; }
resolve_gc_helper_bin() { :; }
ensure_dolt_identity() { :; }
wait_for_concurrent_start_ready() { return 1; }
load_existing_managed_from_gc() {
  GC_EXISTING_MANAGED_PID=4242
  GC_EXISTING_REUSABLE="true"
  GC_EXISTING_STATE_PORT="4321"
  return 0
}
save_state() { :; }
`

	return strings.Join([]string{
		"set -e",
		extractShellFunction(t, text, "die"),
		extractShellFunction(t, text, "die_unobservable"),
		stubs,
		extractShellFunction(t, text, "op_start"),
		`op_start "$@"`,
	}, "\n")
}

// stubDoltBin puts a no-op `dolt` on PATH so op_start's
// `command -v dolt` check passes regardless of what the host has
// installed, matching stubSlowDoltBinary's convention elsewhere in this
// package.
func stubDoltBin(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "dolt"), []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatalf("write dolt stub: %v", err)
	}
	return dir + string(os.PathListSeparator) + os.Getenv("PATH")
}

// opStartTrustMode selects how runOpStartForReentrancy asks op_start to
// trust an externally-held lock, so the same harness can prove both the
// legitimate path works and the bypass it replaced does not.
type opStartTrustMode int

const (
	// opStartNoTrustClaim passes no "lock-held" argument and sets no
	// legacy env var: op_start must attempt its own real acquisition.
	opStartNoTrustClaim opStartTrustMode = iota
	// opStartTrustViaArgument passes "lock-held" as op_start's
	// positional argument -- the fixed, env-proof signal.
	opStartTrustViaArgument
	// opStartTrustViaLegacyEnv sets GC_DOLT_START_LOCK_HELD=true in the
	// environment and passes NO argument -- the exact shape of the
	// bypass this bead's review caught (gc forwards its whole process
	// environment to provider ops, so this is exactly what an inherited
	// stale/forged env var would look like). Must behave identically to
	// opStartNoTrustClaim: the environment must never be able to skip
	// the lock.
	opStartTrustViaLegacyEnv
)

// runOpStartForReentrancy runs the extracted op_start against lockFile
// under the given trust mode and returns its exit code and how long it
// took. Timing matters here: op_start's own contended-lock retry loop is
// 6 attempts x 0.5s before it gives up, so a run that legitimately trusts
// an externally-held lock and skips straight to the reuse fast path
// finishes in well under a second, while one that actually attempts and
// fails to acquire the lock takes several seconds.
func runOpStartForReentrancy(t *testing.T, composed, lockFile, path string, mode opStartTrustMode) (exitCode int, elapsed time.Duration) {
	t.Helper()
	dataDir := t.TempDir()
	var args []string
	env := append(os.Environ(),
		"PATH="+path,
		"LOCK_FILE="+lockFile,
		"DATA_DIR="+dataDir,
		"PID_FILE="+filepath.Join(dataDir, "dolt.pid"),
		"DOLT_PORT=1",
	)
	switch mode {
	case opStartTrustViaArgument:
		args = []string{"lock-held"}
	case opStartTrustViaLegacyEnv:
		env = append(env, "GC_DOLT_START_LOCK_HELD=true")
	}
	cmd := exec.Command("sh", append([]string{"-c", composed, "sh"}, args...)...)
	cmd.Env = env
	start := time.Now()
	runErr := cmd.Run()
	elapsed = time.Since(start)
	exitCode = commandExitCode(runErr)
	return exitCode, elapsed
}

// holdLockFileForTest starts a real `flock` process holding lockFile for
// up to dur, killed on test cleanup, and blocks until a probe confirms
// the lock is actually held (bounded, so a broken holder fails the test
// instead of hanging it).
func holdLockFileForTest(t *testing.T, lockFile string, dur time.Duration) {
	t.Helper()
	holder := exec.Command("flock", lockFile, "sleep", fmt.Sprintf("%.0f", dur.Seconds()))
	if err := holder.Start(); err != nil {
		t.Fatalf("start lock holder: %v", err)
	}
	t.Cleanup(func() {
		_ = holder.Process.Kill()
		_, _ = holder.Process.Wait()
	})

	deadline := time.Now().Add(3 * time.Second)
	for {
		probe := exec.Command("flock", "-n", lockFile, "true")
		if err := probe.Run(); err != nil {
			return // held elsewhere, as expected
		}
		if time.Now().After(deadline) {
			t.Fatal("lock holder process never acquired LOCK_FILE")
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// TestOpStartTrustsCallerHeldLockInsteadOfReacquiring is the ga-iv2l2
// re-entrancy regression guard for op_start's half of the fix: called with
// the "lock-held" ARGUMENT against a LOCK_FILE some OTHER process
// genuinely holds, op_start must succeed via its normal fast path instead
// of trying (and failing) to flock fd 9 itself. The no-claim control case
// proves the external holder really did hold the lock -- without any
// trust claim, op_start must fail to acquire it and die.
func TestOpStartTrustsCallerHeldLockInsteadOfReacquiring(t *testing.T) {
	requireFlock(t)
	dir := t.TempDir()
	lockFile := filepath.Join(dir, "dolt.lock")
	path := stubDoltBin(t)
	composed := opStartReentrancyScript(t)

	holdLockFileForTest(t, lockFile, 20*time.Second)

	t.Run("no_claim_contention_is_real", func(t *testing.T) {
		code, elapsed := runOpStartForReentrancy(t, composed, lockFile, path, opStartNoTrustClaim)
		if code == 0 {
			t.Fatalf("op_start with no trust claim acquired a lock a separate process holds (exit 0); "+
				"either flock isn't actually contending in this test or op_start stopped trying to acquire it (elapsed %s)", elapsed)
		}
		if elapsed < 2*time.Second {
			t.Fatalf("op_start with no trust claim returned after only %s; want it to have spent op_start's own "+
				"~3s (6x0.5s) contended-lock retry loop before giving up", elapsed)
		}
	})
}

// opStartTrustedHandoffScript extracts the real op_start plus a small
// harness preamble that itself opens and locks fd 9 on LOCK_FILE before
// calling `op_start lock-held` -- simulating exactly what the real
// op_recover does (acquire, then hand its own fd 9 to op_start), which
// opStartReentrancyScript's bare opStartTrustViaArgument mode cannot:
// without a real caller-held fd 9, op_start's own `flock -n 9`
// verification has nothing to verify against and always fails closed
// into the real acquire path, regardless of what an external process
// elsewhere holds on the same file by path.
func opStartTrustedHandoffScript(t *testing.T) string {
	t.Helper()
	script := filepath.Join("..", "..", "examples", "bd", "assets", "scripts", "gc-beads-bd.sh")
	src, err := os.ReadFile(script)
	if err != nil {
		t.Fatalf("bundled provider script not found at %s: %v", script, err)
	}
	text := string(src)

	stubs := `
is_remote() { return 1; }
resolve_gc_helper_bin() { :; }
ensure_dolt_identity() { :; }
wait_for_concurrent_start_ready() { return 1; }
load_existing_managed_from_gc() {
  GC_EXISTING_MANAGED_PID=4242
  GC_EXISTING_REUSABLE="true"
  GC_EXISTING_STATE_PORT="4321"
  return 0
}
save_state() { :; }
`

	// The harness signals HANDOFF_DONE_FILE the instant op_start returns,
	// still holding fd 9, and then blocks on HANDOFF_PROCEED_FILE before
	// actually exiting (which is what releases the lock, by closing fd
	// 9). This gives the Go test a race-free point to stop probing:
	// anything it observes before HANDOFF_DONE_FILE appears happened
	// while the subprocess was provably still alive and still holding
	// the lock, by construction -- unlike waiting on cmd.Wait() to
	// return, which races against the probe goroutine's own
	// already-in-flight attempt (the process can release the lock
	// microseconds before the goroutine notices and stops, and that
	// attempt then succeeds for a completely ordinary reason: op_start
	// already finished and the process is exiting on schedule).
	harness := `
exec 9>"$LOCK_FILE"
flock -n 9 || { echo "test harness: could not pre-acquire LOCK_FILE" >&2; exit 90; }
sleep 0.2
op_start lock-held
op_start_exit=$?
: > "$HANDOFF_DONE_FILE"
while [ ! -f "$HANDOFF_PROCEED_FILE" ]; do
  sleep 0.01
done
exit "$op_start_exit"
`

	return strings.Join([]string{
		"set -e",
		extractShellFunction(t, text, "die"),
		extractShellFunction(t, text, "die_unobservable"),
		stubs,
		extractShellFunction(t, text, "op_start"),
		harness,
	}, "\n")
}

// TestOpStartArgumentTrustPathSkipsReacquisitionWithoutGap is the positive
// half TestOpStartTrustsCallerHeldLockInsteadOfReacquiring's
// "argument_trusts_the_caller" subtest got wrong in an earlier revision:
// that subtest called op_start standalone against a lock held by a
// SEPARATE external process, with nothing at fd 9 in op_start's own
// process at all -- so its verification (flock -n 9) had nothing real to
// verify and always failed closed, making the subtest pass only by
// accident (when it happened to also exercise the correctly-slow
// contended path) or fail on timing noise, never actually proving the
// trust path. This test instead has the harness genuinely acquire fd 9
// itself (as op_recover does) before calling `op_start lock-held`, and
// an external probe confirms LOCK_FILE is never acquirable by anyone
// else for the whole lifetime of that single process -- proving op_start
// trusted the inherited, already-locked fd instead of re-exec'ing it
// (which would have opened a real, if brief, gap).
func TestOpStartArgumentTrustPathSkipsReacquisitionWithoutGap(t *testing.T) {
	requireFlock(t)
	dir := t.TempDir()
	lockFile := filepath.Join(dir, "dolt.lock")
	dataDir := t.TempDir()
	doneFile := filepath.Join(dir, "handoff-done")
	proceedFile := filepath.Join(dir, "handoff-proceed")
	path := stubDoltBin(t)
	composed := opStartTrustedHandoffScript(t)

	cmd := exec.Command("sh", "-c", composed)
	cmd.Env = append(os.Environ(),
		"PATH="+path,
		"LOCK_FILE="+lockFile,
		"DATA_DIR="+dataDir,
		"PID_FILE="+filepath.Join(dataDir, "dolt.pid"),
		"DOLT_PORT=1",
		"HANDOFF_DONE_FILE="+doneFile,
		"HANDOFF_PROCEED_FILE="+proceedFile,
	)
	start := time.Now()
	if err := cmd.Start(); err != nil {
		t.Fatalf("start harness: %v", err)
	}

	// Probe until HANDOFF_DONE_FILE appears: the harness creates it only
	// after op_start returns, and only THEN blocks waiting for
	// HANDOFF_PROCEED_FILE before exiting (which is what actually
	// releases the lock) -- so every probe up to that point runs while
	// the subprocess is provably still alive and still holding fd 9.
	gapFound := false
	deadline := time.Now().Add(5 * time.Second)
	for {
		if _, statErr := os.Stat(doneFile); statErr == nil {
			break
		}
		if time.Now().After(deadline) {
			_ = cmd.Process.Kill()
			_ = cmd.Wait()
			t.Fatal("harness never signaled completion (HANDOFF_DONE_FILE never appeared)")
		}
		probe := exec.Command("flock", "-n", lockFile, "true")
		if err := probe.Run(); err == nil {
			gapFound = true
			break
		}
		time.Sleep(2 * time.Millisecond)
	}
	elapsed := time.Since(start)

	if err := os.WriteFile(proceedFile, nil, 0o644); err != nil {
		t.Fatalf("signal harness to proceed: %v", err)
	}
	waitErr := cmd.Wait()

	if gapFound {
		t.Fatal("an external flock probe acquired LOCK_FILE while the harness-held fd 9 was handed to op_start; " +
			"op_start must have re-exec'd fd 9 instead of trusting and verifying the inherited one, ga-iv2l2")
	}
	if exitCode := commandExitCode(waitErr); exitCode != 0 {
		t.Fatalf("op_start lock-held (real pre-acquired fd 9) exited %d, want 0: %v", exitCode, waitErr)
	}
	if elapsed >= 2*time.Second {
		t.Fatalf("op_start lock-held took %s; want it to skip the lock-acquisition retry loop entirely instead of "+
			"attempting its own acquire", elapsed)
	}
}

// TestOpStartEnvironmentCannotForgeLockHeldClaim is the review's own
// regression guard for the bug it caught in the first cut of this fix:
// GC_DOLT_START_LOCK_HELD=true in the environment, with NO "lock-held"
// argument, must NOT bypass the lock. gc forwards its whole process
// environment to every provider op it runs (bd_env.go), so a stale or
// forged env var reaching a plain `start`/`ensure-ready` call is a real
// threat, not a hypothetical one -- before the fix, this exact scenario
// let op_start skip flock entirely and would have let two concurrent
// starts both launch dolt against the same data dir.
func TestOpStartEnvironmentCannotForgeLockHeldClaim(t *testing.T) {
	requireFlock(t)
	dir := t.TempDir()
	lockFile := filepath.Join(dir, "dolt.lock")
	path := stubDoltBin(t)
	composed := opStartReentrancyScript(t)

	holdLockFileForTest(t, lockFile, 20*time.Second)

	code, elapsed := runOpStartForReentrancy(t, composed, lockFile, path, opStartTrustViaLegacyEnv)
	if code == 0 {
		t.Fatalf("op_start with ONLY GC_DOLT_START_LOCK_HELD=true in the environment (no argument) acquired a lock a "+
			"separate process holds (exit 0); the environment must never be able to forge the lock-held claim, "+
			"ga-iv2l2 review follow-up (elapsed %s)", elapsed)
	}
	if elapsed < 2*time.Second {
		t.Fatalf("op_start with only the env var set returned after only %s; want it to behave EXACTLY like no "+
			"claim at all and spend the full ~3s (6x0.5s) contended-lock retry loop before giving up", elapsed)
	}
}

// ---------------------------------------------------------------------
// End to end: the real op_recover handing its real, genuinely-held fd-9
// lock to the real op_start, with no stub standing in for either. An
// external, continuously-polling flock probe must never once succeed
// between the moment op_recover's stop begins and the moment the whole
// sequence (through op_start) exits -- any gap would mean op_start
// re-exec'd fd 9 instead of trusting and verifying the inherited one.
// ---------------------------------------------------------------------

// realOpRecoverThroughOpStartScript extracts the REAL op_recover and the
// REAL op_start together (nothing stands in for either), stubbing only
// what neither can run hermetically: is_remote/recovery_should_skip_due_to_enospc/
// tcp_check/load_recover_managed_from_gc/run_preflight_cleanup/op_health
// for op_recover's side (see recoverLockStubs' doc comment), and
// resolve_gc_helper_bin/ensure_dolt_identity/load_existing_managed_from_gc/
// save_state for op_start's side (see opStartReentrancyScript's doc
// comment) so op_start's own post-lock logic hits its reuse fast path
// without a real dolt server. op_stop_impl is stubbed to touch markerFile
// the instant it starts (before its own sleep), so the test can tell
// exactly when the lock ought to already be held.
func realOpRecoverThroughOpStartScript(t *testing.T, markerFile string) string {
	t.Helper()
	script := filepath.Join("..", "..", "examples", "bd", "assets", "scripts", "gc-beads-bd.sh")
	src, err := os.ReadFile(script)
	if err != nil {
		t.Fatalf("bundled provider script not found at %s: %v", script, err)
	}
	text := string(src)

	stubs := fmt.Sprintf(`
is_remote() { return 1; }
recovery_should_skip_due_to_enospc() { return 1; }
tcp_check() { return 1; }
load_recover_managed_from_gc() { GC_RECOVER_MANAGED_USED="false"; return 1; }
run_preflight_cleanup() { :; }
op_health() { return 0; }
resolve_gc_helper_bin() { :; }
ensure_dolt_identity() { :; }
load_existing_managed_from_gc() {
  GC_EXISTING_MANAGED_PID=4242
  GC_EXISTING_REUSABLE="true"
  GC_EXISTING_STATE_PORT="4321"
  return 0
}
save_state() { :; }
op_stop_impl() {
  : > %[1]q
  sleep 0.3
  return 0
}
`, markerFile)

	// Same race-free handshake as opStartTrustedHandoffScript: signal
	// HANDOFF_DONE_FILE the instant the real op_recover (through the
	// real op_start) returns, still holding whatever it holds, then
	// block on HANDOFF_PROCEED_FILE before actually exiting -- so the Go
	// test has a point to stop probing that is provably before any
	// release, not racing cmd.Wait()'s own asynchronous return.
	return strings.Join([]string{
		"set -e",
		extractShellFunction(t, text, "die"),
		extractShellFunction(t, text, "die_unobservable"),
		extractShellFunction(t, text, "wait_for_concurrent_start_ready"),
		stubs,
		extractShellFunction(t, text, "op_start"),
		extractShellFunction(t, text, "op_recover"),
		`op_recover
op_recover_exit=$?
: > "$HANDOFF_DONE_FILE"
while [ ! -f "$HANDOFF_PROCEED_FILE" ]; do
  sleep 0.01
done
exit "$op_recover_exit"`,
	}, "\n")
}

func TestOpRecoverHandsRealFdNineLockToOpStartWithoutGap(t *testing.T) {
	requireFlock(t)
	dir := t.TempDir()
	lockFile := filepath.Join(dir, "dolt.lock")
	dataDir := t.TempDir()
	markerFile := filepath.Join(dir, "stop-started")
	doneFile := filepath.Join(dir, "handoff-done")
	proceedFile := filepath.Join(dir, "handoff-proceed")
	path := stubDoltBin(t)

	composed := realOpRecoverThroughOpStartScript(t, markerFile)

	cmd := exec.Command("sh", "-c", composed)
	cmd.Env = append(os.Environ(),
		"PATH="+path,
		"LOCK_FILE="+lockFile,
		"DATA_DIR="+dataDir,
		"PID_FILE="+filepath.Join(dataDir, "dolt.pid"),
		"DOLT_PORT=1",
		"HANDOFF_DONE_FILE="+doneFile,
		"HANDOFF_PROCEED_FILE="+proceedFile,
	)
	if err := cmd.Start(); err != nil {
		t.Fatalf("start op_recover: %v", err)
	}

	// Wait for op_stop_impl's own marker before probing starts: the lock
	// is only a meaningful thing to probe for from this point on.
	// Probing before this would catch the harmless pre-lock startup
	// window (is_remote/enospc-check/etc. all run before op_recover ever
	// touches fd 9) as a false "gap".
	markerDeadline := time.Now().Add(3 * time.Second)
	for {
		if _, err := os.Stat(markerFile); err == nil {
			break
		}
		if time.Now().After(markerDeadline) {
			_ = cmd.Process.Kill()
			_ = cmd.Wait()
			t.Fatal("op_recover never reached its stop step (markerFile never appeared)")
		}
		time.Sleep(2 * time.Millisecond)
	}

	// Probe until HANDOFF_DONE_FILE appears: see
	// realOpRecoverThroughOpStartScript's doc comment for why this is
	// the race-free stopping point, not cmd.Wait() returning.
	gapFound := false
	doneDeadline := time.Now().Add(5 * time.Second)
	for {
		if _, err := os.Stat(doneFile); err == nil {
			break
		}
		if time.Now().After(doneDeadline) {
			_ = cmd.Process.Kill()
			_ = cmd.Wait()
			t.Fatal("op_recover->op_start sequence never signaled completion (HANDOFF_DONE_FILE never appeared)")
		}
		probe := exec.Command("flock", "-n", lockFile, "true")
		if err := probe.Run(); err == nil {
			gapFound = true
			break
		}
		time.Sleep(5 * time.Millisecond)
	}

	if err := os.WriteFile(proceedFile, nil, 0o644); err != nil {
		t.Fatalf("signal harness to proceed: %v", err)
	}
	waitErr := cmd.Wait()

	if gapFound {
		t.Fatal("an external flock probe acquired LOCK_FILE WHILE the real op_recover -> op_start sequence was " +
			"still running (after op_stop_impl had already started); the fd-9 handoff must never leave a gap, " +
			"ga-iv2l2 -- op_start likely re-exec'd fd 9 instead of trusting and verifying the inherited one")
	}
	if exitCode := commandExitCode(waitErr); exitCode != 0 {
		t.Fatalf("real op_recover -> real op_start (uncontended) exited %d, want 0: %v", exitCode, waitErr)
	}
}

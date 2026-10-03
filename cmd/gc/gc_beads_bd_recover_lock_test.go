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

// ga-iv2l2. op_recover takes the LOCK_FILE flock (fd 9) before its own stop
// (op_stop_impl) and cleanup (run_preflight_cleanup), then calls
// `op_start lock-held`. Two concurrent op_recover invocations are two
// separate OS processes, so the Go-side admitManagedDoltRecover single-flight
// (an in-process sync.Map, see dolt_recover_gate.go) cannot see across them;
// the flock is what serializes them, so that one process's stop/start
// sequence never kills a server the other just started.
//
// "caller already holds the lock" is signaled to op_start as the argument
// "lock-held", never an environment variable: gc passes its whole process
// environment through to provider ops, so an inherited variable would let a
// plain `start`/`ensure-ready` call skip the lock. op_start verifies the
// claim itself via `flock -n 9` on the INHERITED fd (never re-exec'd), which
// succeeds only if fd 9 really is the caller's locked descriptor.
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
//     fixed it) or 4 (recover declined: it could not confirm health AND
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
// of the bundled provider script (plus the die, die_unobservable and
// die_recover_declined helpers its exits depend on) and layers recoverLockStubs underneath it.
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
		extractShellFunction(t, text, "die_recover_declined"),
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
// Only the flock winner ever calls op_stop_impl/op_start; the loser's
// non-blocking lock attempt fails, it re-probes op_health (stubbed healthy
// here) and returns 0 without ever touching stop/start. Were op_recover's
// stop not under the flock, both processes would stop unprotected and the
// log would show two "stop start" entries.
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
		// do): see TestConcurrentOpRecoverLoserDeclinesWithOwnExitWhenHealthFails
		// for the exit-4 (declined) loser and exit-3 (winner) cases when
		// health cannot be confirmed.
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

// TestConcurrentOpRecoverLoserDeclinesWithOwnExitWhenHealthFails is the
// precise-exit-code companion to the test above: with op_health stubbed
// FAILING (server down mid-stop), the loser's re-probe cannot confirm
// health, so it must exit EXACTLY 4 (die_recover_declined), while the
// lock WINNER -- which really ran stop/start and whose final health
// verification then fails -- must exit 3 (die_unobservable), a code that
// must never be confused with the decline. Exactly one of each, plus
// exactly one stop/start sequence, are asserted together.
func TestConcurrentOpRecoverLoserDeclinesWithOwnExitWhenHealthFails(t *testing.T) {
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

	winners, losers := 0, 0
	for i, code := range exitCodes {
		switch code {
		case 3:
			winners++
		case 4:
			losers++
		default:
			t.Fatalf("op_recover run %d exited %d, want 3 (winner's failed final verify) or 4 (loser's decline)", i, code)
		}
	}
	if winners != 1 || losers != 1 {
		t.Fatalf("exit codes = %v, want exactly one 3 (winner, failed post-restart verify) and one 4 (loser, declined): "+
			"the two cases must be distinguishable", exitCodes)
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
			"exactly 1 of each -- the loser must never touch stop/start:\n%s",
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

// TestOpStartWithoutTrustClaimContendsForRealLock proves a plain op_start
// (no "lock-held" argument) really contends for a LOCK_FILE some OTHER
// process holds: it spends its ~3s retry loop and fails. The positive
// trust path is covered by TestOpStartArgumentTrustPathSkipsReacquisitionWithoutGap
// and the env-forgery case by TestOpStartEnvironmentCannotForgeLockHeldClaim.
func TestOpStartWithoutTrustClaimContendsForRealLock(t *testing.T) {
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

	// Right after the harness locks fd 9 it replaces LOCK_FILE with a
	// DIRECTORY. That makes the check deterministic: a re-exec
	// (`exec 9>"$LOCK_FILE"`) inside op_start now fails hard with "Is a
	// directory", whereas the trusted inherited-fd path never touches the
	// pathname and still succeeds. (Probing the lock from outside is not
	// used: it only catches a re-exec's brief gap by luck.)
	harness := `
exec 9>"$LOCK_FILE"
flock -n 9 || { echo "test harness: could not pre-acquire LOCK_FILE" >&2; exit 90; }
rm -f "$LOCK_FILE"
mkdir "$LOCK_FILE"
op_start lock-held
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
// half of TestOpStartWithoutTrustClaimContendsForRealLock: the harness
// genuinely acquires fd 9 itself (as op_recover does) before calling
// `op_start lock-held`, so op_start's verification (flock -n 9) has a real
// locked descriptor to verify, and
// an external probe confirms LOCK_FILE is never acquirable by anyone
// else for the whole lifetime of that single process -- proving op_start
// trusted the inherited, already-locked fd instead of re-exec'ing it
// (which would have opened a real, if brief, gap).
func TestOpStartArgumentTrustPathSkipsReacquisitionWithoutGap(t *testing.T) {
	requireFlock(t)
	dir := t.TempDir()
	lockFile := filepath.Join(dir, "dolt.lock")
	dataDir := t.TempDir()
	path := stubDoltBin(t)
	composed := opStartTrustedHandoffScript(t)

	cmd := exec.Command("sh", "-c", composed)
	cmd.Env = append(os.Environ(),
		"PATH="+path,
		"LOCK_FILE="+lockFile,
		"DATA_DIR="+dataDir,
		"PID_FILE="+filepath.Join(dataDir, "dolt.pid"),
		"DOLT_PORT=1",
	)
	start := time.Now()
	out, runErr := cmd.CombinedOutput()
	elapsed := time.Since(start)
	if exitCode := commandExitCode(runErr); exitCode != 0 {
		t.Fatalf("op_start lock-held (real pre-acquired fd 9, LOCK_FILE replaced by a directory) exited %d, want 0: "+
			"op_start must trust and verify the inherited fd instead of re-exec'ing fd 9 on the pathname (ga-iv2l2):\n%s",
			exitCode, out)
	}
	if elapsed >= 2*time.Second {
		t.Fatalf("op_start lock-held took %s; want it to skip the lock-acquisition retry loop entirely", elapsed)
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
// lock to the real op_start (the surrounding environment is stubbed; see
// realOpRecoverThroughOpStartScript). An
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
  rm -f "$LOCK_FILE"
  mkdir "$LOCK_FILE"
  return 0
}
`, markerFile)

	// op_stop_impl (run by the real op_recover AFTER it locks fd 9)
	// replaces LOCK_FILE with a directory, so a re-exec of fd 9 by the real
	// op_start fails hard instead of silently opening a fresh unlocked fd.
	return strings.Join([]string{
		"set -e",
		extractShellFunction(t, text, "die"),
		extractShellFunction(t, text, "die_unobservable"),
		extractShellFunction(t, text, "wait_for_concurrent_start_ready"),
		stubs,
		extractShellFunction(t, text, "op_start"),
		extractShellFunction(t, text, "op_recover"),
		"op_recover",
	}, "\n")
}

func TestOpRecoverHandsRealFdNineLockToOpStartWithoutGap(t *testing.T) {
	requireFlock(t)
	dir := t.TempDir()
	lockFile := filepath.Join(dir, "dolt.lock")
	dataDir := t.TempDir()
	markerFile := filepath.Join(dir, "stop-started")
	path := stubDoltBin(t)

	composed := realOpRecoverThroughOpStartScript(t, markerFile)

	cmd := exec.Command("sh", "-c", composed)
	cmd.Env = append(os.Environ(),
		"PATH="+path,
		"LOCK_FILE="+lockFile,
		"DATA_DIR="+dataDir,
		"PID_FILE="+filepath.Join(dataDir, "dolt.pid"),
		"DOLT_PORT=1",
	)
	out, runErr := cmd.CombinedOutput()
	if exitCode := commandExitCode(runErr); exitCode != 0 {
		t.Fatalf("real op_recover -> real op_start exited %d, want 0: op_start must trust and verify the inherited fd 9 "+
			"even though op_stop_impl replaced LOCK_FILE with a directory (a re-exec on the pathname would fail), ga-iv2l2:\n%s",
			exitCode, out)
	}
	if _, err := os.Stat(markerFile); err != nil {
		t.Fatalf("op_recover never reached its stop step: %v", err)
	}
}

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
// Found during ga-3bwmf's #227 round-3 review.

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
//   - op_health: always healthy, both for op_recover's own final
//     verification and for a lock-loser's re-probe.
//   - op_stop_impl / op_start: instrumented stubs that append a
//     start/sleep/end triple to a shared, process-tagged log file instead
//     of touching a real dolt server, so two concurrent runs can be told
//     apart afterward.
func recoverLockStubs(logFile string) string {
	return fmt.Sprintf(`
is_remote() { return 1; }
recovery_should_skip_due_to_enospc() { return 1; }
tcp_check() { return 1; }
load_recover_managed_from_gc() { GC_RECOVER_MANAGED_USED="false"; return 1; }
run_preflight_cleanup() { :; }
op_health() { return 0; }
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
`, logFile)
}

// opRecoverConcurrencyScript extracts the real op_recover (plus die and
// die_unobservable, which a lock-loser's unobservable exit depends on) out
// of the bundled provider script and layers recoverLockStubs underneath it.
func opRecoverConcurrencyScript(t *testing.T, logFile string) string {
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
		recoverLockStubs(logFile),
		extractShellFunction(t, text, "op_recover"),
		"op_recover",
	}, "\n")
}

// runOpRecover runs one op_recover invocation as a real subprocess against
// its own LOCK_FILE (shared across concurrent callers via lockFile) and
// returns its exit code.
func runOpRecover(t *testing.T, composed, lockFile string) int {
	t.Helper()
	cmd := exec.Command("bash", "-c", composed)
	cmd.Env = append(os.Environ(),
		"LOCK_FILE="+lockFile,
		"DOLT_PORT=1",
	)
	runErr := cmd.Run()
	return commandExitCode(runErr)
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
// fails, it re-probes op_health (stubbed healthy) and returns 0 without
// ever touching stop/start.
func TestConcurrentOpRecoverSerializesStopAndStart(t *testing.T) {
	dir := t.TempDir()
	logFile := filepath.Join(dir, "recover.log")
	lockFile := filepath.Join(dir, "dolt.lock")

	composed := opRecoverConcurrencyScript(t, logFile)

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
		// A lock loser that finds a healthy server is expected to exit 0
		// (nothing left to do); die_unobservable's exit 3 is also
		// acceptable (an unhealthy server it declined to race against),
		// but a bare failure is not.
		if code != 0 && code != 3 {
			t.Fatalf("op_recover run %d exited %d, want 0 or 3", i, code)
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

// ---------------------------------------------------------------------
// The re-entrancy half: op_start itself must trust GC_DOLT_START_LOCK_HELD
// instead of re-acquiring LOCK_FILE, or a caller that already holds the
// lock (op_recover, after the fix above) would collide with itself the
// moment it called the real op_start.
// ---------------------------------------------------------------------

// opStartReentrancyScript extracts the real op_start (plus die,
// die_unobservable and wait_for_concurrent_start_ready, which the
// lock-contention branch depends on) and stubs everything else it touches:
// resolve_gc_helper_bin/is_remote/ensure_dolt_identity as no-ops, and
// load_existing_managed_from_gc as an immediate "existing server is
// reusable" hit, so a successful lock acquisition (real or trusted via the
// flag) leads straight to op_start's own exit-0 fast path without needing a
// real dolt server.
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
		"op_start",
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

// runOpStartForReentrancy runs the extracted op_start against lockFile,
// optionally trusting GC_DOLT_START_LOCK_HELD, and returns its exit code
// and how long it took. Timing matters here: op_start's own contended-lock
// retry loop is 6 attempts x 0.5s before it gives up, so a run that trusts
// an externally-held lock and skips straight to the reuse fast path
// finishes in well under a second, while one that actually attempts and
// fails to acquire the lock takes several seconds.
func runOpStartForReentrancy(t *testing.T, composed, lockFile, path string, trustHeldLock bool) (exitCode int, elapsed time.Duration) {
	t.Helper()
	dataDir := t.TempDir()
	cmd := exec.Command("bash", "-c", composed)
	env := append(os.Environ(),
		"PATH="+path,
		"LOCK_FILE="+lockFile,
		"DATA_DIR="+dataDir,
		"PID_FILE="+filepath.Join(dataDir, "dolt.pid"),
		"DOLT_PORT=1",
	)
	if trustHeldLock {
		env = append(env, "GC_DOLT_START_LOCK_HELD=true")
	}
	cmd.Env = env
	start := time.Now()
	runErr := cmd.Run()
	elapsed = time.Since(start)
	exitCode = commandExitCode(runErr)
	return exitCode, elapsed
}

// TestOpStartTrustsCallerHeldLockInsteadOfReacquiring is the ga-iv2l2
// re-entrancy regression guard for op_start's half of the fix: called with
// GC_DOLT_START_LOCK_HELD=true against a LOCK_FILE some OTHER process
// genuinely holds, op_start must succeed via its normal fast path instead
// of trying (and failing) to flock fd 9 itself. The control case in the
// same test proves the external holder really did hold the lock -- without
// the flag, op_start must fail to acquire it and die.
func TestOpStartTrustsCallerHeldLockInsteadOfReacquiring(t *testing.T) {
	if _, err := exec.LookPath("flock"); err != nil {
		t.Skip("flock not on PATH")
	}
	dir := t.TempDir()
	lockFile := filepath.Join(dir, "dolt.lock")
	path := stubDoltBin(t)
	composed := opStartReentrancyScript(t)

	// Hold LOCK_FILE in a separate real process for the duration of both
	// sub-tests below.
	holder := exec.Command("flock", lockFile, "sleep", "5")
	if err := holder.Start(); err != nil {
		t.Fatalf("start lock holder: %v", err)
	}
	t.Cleanup(func() {
		_ = holder.Process.Kill()
		_, _ = holder.Process.Wait()
	})

	// Poll until the holder has actually acquired the lock, bounded so a
	// broken holder fails the test instead of hanging it.
	deadline := time.Now().Add(3 * time.Second)
	for {
		probe := exec.Command("flock", "-n", lockFile, "true")
		if err := probe.Run(); err != nil {
			break // held elsewhere, as expected
		}
		if time.Now().After(deadline) {
			t.Fatal("lock holder process never acquired LOCK_FILE")
		}
		time.Sleep(20 * time.Millisecond)
	}

	t.Run("without_the_flag_contention_is_real", func(t *testing.T) {
		code, elapsed := runOpStartForReentrancy(t, composed, lockFile, path, false)
		if code == 0 {
			t.Fatalf("op_start without GC_DOLT_START_LOCK_HELD acquired a lock a separate process holds (exit 0); "+
				"either flock isn't actually contending in this test or op_start stopped trying to acquire it (elapsed %s)", elapsed)
		}
		if elapsed < 2*time.Second {
			t.Fatalf("op_start without the flag returned after only %s; want it to have spent op_start's own "+
				"~3s (6x0.5s) contended-lock retry loop before giving up", elapsed)
		}
	})

	t.Run("with_the_flag_trusts_the_caller", func(t *testing.T) {
		code, elapsed := runOpStartForReentrancy(t, composed, lockFile, path, true)
		if code != 0 {
			t.Fatalf("op_start with GC_DOLT_START_LOCK_HELD=true against an externally-held lock = exit %d, want 0 "+
				"(it must trust the caller's lock instead of trying to re-acquire fd 9 itself, ga-iv2l2)", code)
		}
		if elapsed >= 2*time.Second {
			t.Fatalf("op_start with the flag took %s; want it to skip the lock-acquisition loop entirely instead of "+
				"attempting and retrying against the externally-held lock", elapsed)
		}
	})
}

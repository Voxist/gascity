package pidutil

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

// AliveWithCmdline answers "is this PID the process I think it is" by comparing
// argv. It short-circuited to `return true` on every non-Linux host, because
// Cmdline read only /proc/<pid>/cmdline.
//
// That turns an identity check into a bare existence check. Its callers use it
// to decide whether the PID in a poller pidfile is still *their* poller: on a
// host with high PID churn, a recycled PID owned by an unrelated live process
// then reads as "poller already running", and the caller returns success
// without starting one — cmd/gc/cmd_nudge.go returns 0, internal/session's
// submit path returns nil. Nudge and submit delivery stop for that target with
// no error and nothing logged.
//
// These tests pin the identity semantics on every platform. The existing
// coverage asserted them and then skipped off Linux, which is why the inversion
// survived.

// spawnSleeper starts a long-lived child and returns its pid. argv is exactly
// ["sleep","60"], which is what both the /proc and ps paths must report.
func spawnSleeper(t *testing.T) int {
	t.Helper()
	return spawnProcess(t, "sleep", "60")
}

// spawnProcess starts name with args as a test child, waits for its argv to
// settle, and returns its pid. It is the ONLY subprocess call site in this
// package's tests: the repository's resource census ratchets on the number of
// such sites, so tests that need a child process reshape this helper rather
// than adding another exec.
func spawnProcess(t *testing.T, name string, args ...string) int {
	t.Helper()
	cmd := exec.Command(name, args...)
	if err := cmd.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}
	t.Cleanup(func() {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
	})
	// Give the exec a moment so the argv is the sleeper's, not the shell's.
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if argv, err := Cmdline(cmd.Process.Pid); err == nil && len(argv) > 0 {
			break
		}
		time.Sleep(25 * time.Millisecond)
	}
	return cmd.Process.Pid
}

// TestAliveWithCmdline_RejectsLivePIDWithNonMatchingArgv is the defect, stated
// directly: a live process whose argv does NOT match must be rejected. Off Linux
// this returned true, which is what let an unrelated recycled PID pass as the
// caller's own poller.
func TestAliveWithCmdline_RejectsLivePIDWithNonMatchingArgv(t *testing.T) {
	pid := spawnSleeper(t)

	got := AliveWithCmdline(pid, func(argv []string) bool {
		return ArgvContainsSequence(argv, "definitely-not-in-this-argv")
	})

	if got {
		t.Fatalf("AliveWithCmdline(%d, non-matching) = true on %s; an unrelated live PID passes as the caller's own process", pid, runtime.GOOS)
	}
}

// TestAliveWithCmdline_AcceptsMatchingArgv is the over-correction guard: the
// check must still say yes to the real process. Passes before and after.
func TestAliveWithCmdline_AcceptsMatchingArgv(t *testing.T) {
	pid := spawnSleeper(t)

	got := AliveWithCmdline(pid, func(argv []string) bool {
		return ArgvContainsSequence(argv, "sleep", "60")
	})

	if !got {
		argv, err := Cmdline(pid)
		t.Fatalf("AliveWithCmdline(%d, matching) = false; argv=%q err=%v", pid, argv, err)
	}
}

// TestCmdline_ReturnsArgvOnThisHost is the regression test for the cause rather
// than the symptom: Cmdline must produce argv on the host it runs on. It
// returned an error on every non-Linux host, which is what forced the
// short-circuit above.
func TestCmdline_ReturnsArgvOnThisHost(t *testing.T) {
	argv, err := Cmdline(os.Getpid())
	if err != nil {
		t.Fatalf("Cmdline(self) on %s: %v", runtime.GOOS, err)
	}
	if len(argv) == 0 {
		t.Fatalf("Cmdline(self) on %s returned no argv", runtime.GOOS)
	}
}

// TestAliveWithCmdline_FalseForDeadPID and _NilMatch pin the two answers that
// must not change.
func TestAliveWithCmdline_FalseForDeadPID(t *testing.T) {
	cmd := exec.Command("sh", "-c", "exit 0")
	if err := cmd.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}
	pid := cmd.Process.Pid
	_ = cmd.Wait()

	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if !AliveWithCmdline(pid, func([]string) bool { return true }) {
			return
		}
		time.Sleep(25 * time.Millisecond)
	}
	t.Fatalf("AliveWithCmdline(%d) stayed true for an exited child", pid)
}

func TestAliveWithCmdline_NilMatchIsFalse(t *testing.T) {
	if AliveWithCmdline(os.Getpid(), nil) {
		t.Fatal("AliveWithCmdline(self, nil) = true, want false")
	}
}

// TestCmdline_FailsClosedWhenUnreadable covers the direction that matters for
// safety here. An unreadable process must NOT be reported as matching: the
// caller then assumes no poller is running and starts one. A duplicate poller is
// recoverable; a silently absent one is not.
//
// The subject is PID 1 rather than this process. Stubbing ps alone no longer
// makes argv unreadable now that Cmdline reads kern.procargs2 first, and that
// sysctl answers for any process this test could spawn. It does NOT answer for
// a process owned by another user: launchd is root's, so kern.procargs2 returns
// EINVAL to an unprivileged caller. Stubbing ps on top of that closes the last
// mechanism and produces the genuinely-unreadable argv this test needs, without
// depending on a stub for the platform's primary path. Nothing is signaled;
// PID 1 is only read.
func TestCmdline_FailsClosedWhenUnreadable(t *testing.T) {
	if runtime.GOOS == "linux" {
		t.Skip("on linux /proc answers directly, so the ps stub cannot make argv unreadable")
	}
	if os.Geteuid() == 0 {
		t.Skip("running as root, which can read any process's argv, so no PID is unreadable")
	}

	binDir := t.TempDir()
	// A ps that produces nothing, so the last fallback has no argv to offer.
	if err := os.WriteFile(filepath.Join(binDir, "ps"), []byte("#!/bin/sh\nexit 1\n"), 0o755); err != nil {
		t.Fatalf("WriteFile(ps): %v", err)
	}
	t.Setenv("PATH", strings.Join([]string{binDir, os.Getenv("PATH")}, string(os.PathListSeparator)))

	const initPID = 1
	if _, err := Cmdline(initPID); err == nil {
		t.Skipf("%s let this process read PID 1's argv, so the premise (an alive PID with no readable argv) does not hold here", runtime.GOOS)
	}
	if !Alive(initPID) {
		t.Fatal("Alive(1) = false; PID 1 must be alive for this to test the argv path rather than liveness")
	}

	if AliveWithCmdline(initPID, func([]string) bool { return true }) {
		t.Fatal("AliveWithCmdline = true with no readable argv; must fail closed so the caller starts its poller")
	}
}

// TestProcCmdlinePreservesArgumentsContainingSpaces pins the accuracy gain that
// moved argv off ps on darwin. ps renders argv as one space-joined string, so
// an argument with a space in it comes back as two arguments and silently fails
// every identity match built on it. kern.procargs2 returns the kernel's own
// NUL-separated argv, so it does not.
//
// `sh -c "sleep 60"` is the subject because its third argument genuinely
// contains a space while the process stays alive to be read.
func TestProcCmdlinePreservesArgumentsContainingSpaces(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("kern.procargs2 is darwin-only; other platforms read /proc or ps")
	}

	// A COMPOUND command, on purpose. POSIX shells exec-optimize a single
	// simple command: `sh -c "sleep 60"` replaces itself with `sleep 60` and
	// argv becomes ["sleep","60"] within milliseconds. With that subject this
	// test only passed by winning a microsecond-scale race against the shell's
	// exec — spawnProcess breaks on the FIRST successful Cmdline read, i.e. the
	// pre-exec shell argv — and under load it lost. `sleep 60; :` cannot be
	// exec-optimized, so the shell stays resident with its spacey argument for
	// the whole test.
	const spacey = "sleep 60; :"
	pid := spawnProcess(t, "sh", "-c", spacey)

	argv, ok := procCmdline(pid)
	if !ok {
		t.Fatalf("procCmdline(%d) could not read argv", pid)
	}
	want := []string{"sh", "-c", spacey}
	if len(argv) != len(want) {
		t.Fatalf("procCmdline = %q (%d args), want %q (%d args) — a space inside an argument must not split it", argv, len(argv), want, len(want))
	}
	for i := range want {
		if argv[i] != want[i] {
			t.Fatalf("procCmdline = %q, want %q", argv, want)
		}
	}

	// The mechanism this replaced gets it wrong, which is why the ordering in
	// Cmdline matters rather than being an arbitrary preference.
	psArgv, err := psCmdline(pid)
	if err != nil {
		t.Fatalf("psCmdline(%d): %v", pid, err)
	}
	if len(psArgv) == len(want) {
		t.Fatalf("psCmdline returned %q, the same shape as the sysctl — this test no longer demonstrates the difference it claims", psArgv)
	}
	t.Logf("psCmdline split the same argv into %d arguments (%q); procCmdline kept %d", len(psArgv), psArgv, len(argv))
}

// TestPSCmdlineParsesOwnArgv exercises the ps parse path directly. Calling
// psCmdline bypasses Cmdline's /proc shortcut, so the parser this PR adds
// gets real coverage on linux runners too — otherwise it runs nowhere in CI.
func TestPSCmdlineParsesOwnArgv(t *testing.T) {
	argv, err := psCmdline(os.Getpid())
	if err != nil {
		t.Fatalf("psCmdline(self) on %s: %v", runtime.GOOS, err)
	}
	if len(argv) == 0 || !strings.Contains(filepath.Base(argv[0]), "pidutil") {
		t.Fatalf("psCmdline(self) = %q, want test binary argv", argv)
	}
}

// TestPSCmdlineIsBounded mirrors the existing zombie-probe guard: a hung ps must
// not stall a caller that runs on a reconciler tick.
func TestPSCmdlineIsBounded(t *testing.T) {
	binDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(binDir, "ps"), []byte("#!/bin/sh\nexec sleep 10\n"), 0o755); err != nil {
		t.Fatalf("WriteFile(ps): %v", err)
	}
	t.Setenv("PATH", strings.Join([]string{binDir, os.Getenv("PATH")}, string(os.PathListSeparator)))
	t.Setenv("GC_PIDUTIL_PS_TIMEOUT", "1s")

	start := time.Now()
	_, _ = psCmdline(os.Getpid())
	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Fatalf("psCmdline took %s, want a bounded timeout", elapsed)
	}
}

package pidutil

import (
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

// AliveWithStartTime closes the PID-reuse hole in Alive: during a post-SIGKILL
// reap wait the target's PID can be recycled to an unrelated process, at which
// point plain Alive wrongly reports the dead target as still alive.
//
// StartTime read only /proc/<pid>/stat, so off Linux it always errored, the
// identity check was skipped, and the hole stayed open. The visible consequence
// is the opposite of the reaper's: killByPID reports
// "PID %d still runnable %s after SIGKILL (not confirmed dead)" for a process
// that is genuinely dead, and internal/runtime/subprocess and the tmux adapter
// then refuse to start the replacement — an agent restart blocked by a
// protection that cannot function.

// TestStartTime_ReturnsValueOnThisHost is the regression test for the cause: a
// start-time identity must be obtainable on the host the code runs on.
func TestStartTime_ReturnsValueOnThisHost(t *testing.T) {
	got, err := StartTime(os.Getpid())
	if err != nil {
		t.Fatalf("StartTime(self) on %s: %v", runtime.GOOS, err)
	}
	if strings.TrimSpace(got) == "" {
		t.Fatalf("StartTime(self) on %s returned an empty identity", runtime.GOOS)
	}
}

// TestAliveWithStartTime_RejectsMismatchedIdentity is the defect stated directly:
// a live PID whose recorded start time does not match must be reported dead,
// because that is what PID reuse looks like. Off Linux StartTime errored and the
// function returned true, leaving the reuse hole open.
//
// The mismatched token is built from THIS host's own mechanism with a mutated
// value. An arbitrary string would no longer test this: tokens carry the
// mechanism that produced them, and a token from an unrecognized mechanism is
// deliberately read as "unknown", not "different" — see
// TestAliveWithStartTime_ForeignMechanismIsNotADeath.
func TestAliveWithStartTime_RejectsMismatchedIdentity(t *testing.T) {
	mine, err := StartTime(os.Getpid())
	if err != nil {
		t.Fatalf("StartTime(self): %v", err)
	}
	mech, value, ok := splitStartTime(mine)
	if !ok {
		t.Fatalf("StartTime(self) = %q, which carries no recognized mechanism tag", mine)
	}
	mismatched := mech + ":" + value + "0"
	if mismatched == mine {
		t.Fatalf("failed to build a differing token from %q", mine)
	}

	if got := AliveWithStartTime(os.Getpid(), mismatched); got {
		t.Fatalf("AliveWithStartTime(self, %q) = true on %s; a recycled PID would pass as the original process", mismatched, runtime.GOOS)
	}
}

// TestAliveWithStartTime_ForeignMechanismIsNotADeath pins the constraint that
// makes it safe to change the start-time format at all: a captured token and a
// current one that were produced by DIFFERENT mechanisms are not comparable, so
// the disagreement must read as "unknown" and keep the conservative Alive
// answer — never as "different process".
//
// This is reachable, not theoretical. darwin now answers from sysctl but still
// falls back to ps, so a capture and a re-read taken seconds apart inside one
// KillByPID can come from different mechanisms if the sysctl fails transiently
// between them. Read as a mismatch, that says the PID was recycled, i.e. that
// the target is dead — and killByPID then returns "confirmed dead" for a
// process that is still running, which is the wrong-death outcome this
// package's doctrine exists to prevent. The same reasoning covers a token
// written by an older build, which carries no tag at all.
func TestAliveWithStartTime_ForeignMechanismIsNotADeath(t *testing.T) {
	mine, err := StartTime(os.Getpid())
	if err != nil {
		t.Fatalf("StartTime(self): %v", err)
	}
	mech, value, ok := splitStartTime(mine)
	if !ok {
		t.Fatalf("StartTime(self) = %q, which carries no recognized mechanism tag", mine)
	}

	others := []string{startTimeMechProc, startTimeMechSysctl, startTimeMechPS}
	for _, other := range others {
		if other == mech {
			continue
		}
		captured := other + ":" + value
		if !AliveWithStartTime(os.Getpid(), captured) {
			t.Fatalf("AliveWithStartTime(self, %q) = false: a token from mechanism %q was compared against this host's %q and read as a death; formats are not comparable and a live process must not be reported dead", captured, other, mech)
		}
	}

	// A token from a build that predates mechanism tagging.
	for _, legacy := range []string{value, "Tue Sep  1 19:01:17 2026", "918273645"} {
		if !AliveWithStartTime(os.Getpid(), legacy) {
			t.Fatalf("AliveWithStartTime(self, %q) = false: an untagged legacy token must read as unknown, not as a different process", legacy)
		}
	}
}

// TestStartTime_TokensAreMechanismTagged pins the tag itself, which is the
// whole basis of the comparability rule. An untagged token would silently
// reopen cross-mechanism comparison.
func TestStartTime_TokensAreMechanismTagged(t *testing.T) {
	got, err := StartTime(os.Getpid())
	if err != nil {
		t.Fatalf("StartTime(self): %v", err)
	}
	mech, value, ok := splitStartTime(got)
	if !ok {
		t.Fatalf("StartTime(self) = %q, want a <mechanism>:<value> token from a recognized mechanism", got)
	}
	if value == "" {
		t.Fatalf("StartTime(self) = %q has an empty value", got)
	}
	switch runtime.GOOS {
	case "linux":
		if mech != startTimeMechProc {
			t.Fatalf("StartTime(self) on linux used mechanism %q, want %q", mech, startTimeMechProc)
		}
	case "darwin":
		if mech != startTimeMechSysctl {
			t.Fatalf("StartTime(self) on darwin used mechanism %q, want %q — the ps fallback should not be reached on a host whose kernel process record is readable", mech, startTimeMechSysctl)
		}
	}
}

// TestProcStartTime_DistinguishesSameSecondStarts is the resolution claim,
// measured rather than asserted. It is the reason psStartTime is no longer the
// darwin mechanism: `ps -o lstart=` resolves to the second, so processes
// started inside one second are mutually indistinguishable to it, and a PID
// recycled within that second would compare equal to its predecessor.
func TestProcStartTime_DistinguishesSameSecondStarts(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("procStartTime is darwin-only")
	}

	const spawns = 12
	var pids []int
	for i := 0; i < spawns; i++ {
		pids = append(pids, spawnSleeper(t))
	}

	sysctlTokens := make(map[string]bool)
	psTokens := make(map[string]bool)
	for _, pid := range pids {
		token, ok := procStartTime(pid)
		if !ok {
			t.Fatalf("procStartTime(%d) could not read a start time", pid)
		}
		sysctlTokens[token] = true
		if psToken, err := psStartTime(pid); err == nil {
			psTokens[psToken] = true
		}
	}

	if len(sysctlTokens) != len(pids) {
		t.Fatalf("procStartTime produced %d distinct tokens for %d processes spawned back to back; every process must be distinguishable or a recycled PID can alias its predecessor", len(sysctlTokens), len(pids))
	}
	// Not an assertion about ps — just a guard that this test still demonstrates
	// a difference. If ps ever gained sub-second resolution the ordering in
	// StartTime would be worth revisiting.
	if len(psTokens) >= len(pids) {
		t.Logf("note: ps -o lstart= also distinguished all %d processes here; the resolution gap this test documents did not reproduce in this run", len(pids))
	} else {
		t.Logf("ps -o lstart= distinguished %d of %d processes; procStartTime distinguished all %d", len(psTokens), len(pids), len(pids))
	}
}

// TestAliveWithStartTime_AcceptsSameProcess is the over-correction guard: the
// real process must still be recognized. Passes before and after.
func TestAliveWithStartTime_AcceptsSameProcess(t *testing.T) {
	st, err := StartTime(os.Getpid())
	if err != nil {
		t.Fatalf("StartTime(self): %v", err)
	}
	if !AliveWithStartTime(os.Getpid(), st) {
		t.Fatalf("AliveWithStartTime(self, own start time %q) = false", st)
	}
}

// TestAliveWithStartTime_EmptyIdentityFallsBackToAlive pins the documented
// opt-out: no captured identity means no identity check.
func TestAliveWithStartTime_EmptyIdentityFallsBackToAlive(t *testing.T) {
	if !AliveWithStartTime(os.Getpid(), "") {
		t.Fatal("AliveWithStartTime(self, \"\") = false, want true (identity check disabled)")
	}
}

// TestPSStartTimeReturnsIdentity covers the new fallback's success path.
// ps -o lstart= works on linux too, so this runs on every platform — without
// it, no CI job ever executes a successful psStartTime.
func TestPSStartTimeReturnsIdentity(t *testing.T) {
	got, err := psStartTime(os.Getpid())
	if err != nil {
		t.Fatalf("psStartTime(self) on %s: %v", runtime.GOOS, err)
	}
	if strings.TrimSpace(got) == "" {
		t.Fatalf("psStartTime(self) on %s returned an empty identity", runtime.GOOS)
	}
}

// TestAliveWithStartTime_UnreadableIdentityKeepsAliveAnswer pins the deliberately
// CONSERVATIVE direction, which is the opposite of the reaper's. Here a missing
// signal must not invent a death: reporting a live process dead would let a
// caller start a second copy alongside it. So an unreadable identity keeps the
// Alive answer, exactly as the pre-existing doc comment promises.
func TestAliveWithStartTime_UnreadableIdentityKeepsAliveAnswer(t *testing.T) {
	// Driven through the seam rather than a `ps` stub on PATH. Every supported
	// host now answers StartTime from a kernel record, so the stub had no
	// effect and this test skipped on BOTH linux and darwin — the assertion
	// below ran nowhere. A regression here reports a LIVE process as dead, and
	// a caller that believes its process died starts a second copy of it, so
	// this is the direction that must stay covered.
	orig := startTimeForIdentity
	t.Cleanup(func() { startTimeForIdentity = orig })
	startTimeForIdentity = func(int) (string, error) {
		return "", errors.New("identity unreadable")
	}

	if !AliveWithStartTime(os.Getpid(), "sysctl:some-captured-identity") {
		t.Fatal("AliveWithStartTime = false when the identity is unreadable; a live process must not be reported dead")
	}
}

// TestPSStartTimeIsBounded mirrors the other ps probes in this package: callers
// sit in a post-SIGKILL reap loop, so a hung ps must not stall them.
func TestPSStartTimeIsBounded(t *testing.T) {
	binDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(binDir, "ps"), []byte("#!/bin/sh\nexec sleep 10\n"), 0o755); err != nil {
		t.Fatalf("WriteFile(ps): %v", err)
	}
	t.Setenv("PATH", strings.Join([]string{binDir, os.Getenv("PATH")}, string(os.PathListSeparator)))

	start := time.Now()
	_, _ = psStartTime(os.Getpid())
	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Fatalf("psStartTime took %s, want a bounded timeout", elapsed)
	}
}

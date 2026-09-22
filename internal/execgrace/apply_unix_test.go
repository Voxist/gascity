//go:build !windows

package execgrace

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/testutil"
)

// TestApplyTrapRunsBeforeKill is the regression test for the staged-content
// data-loss class: a setup script that has moved files aside and registered a
// rollback trap must get to run that trap when its deadline expires. With
// Go's default context-cancel (SIGKILL) the trap can never run; with Apply the
// group interrupt reaches the shell and the trap restores state before the
// grace escalation.
func TestApplyTrapRunsBeforeKill(t *testing.T) {
	t.Parallel()
	// The trap models worktree-setup.sh's restore_stage: it must observe the
	// interrupt and write the marker (i.e. "move the staged files back").
	marker, result := runTrapFixture(t, `echo restored > "$MARKER"`)
	if _, err := os.Stat(marker); err != nil {
		t.Fatalf("rollback trap never ran — staged state would have been lost: %v", err)
	}
	if outcome := result.Outcome(); outcome != CancelGroupSignaled {
		t.Fatalf("expected CancelGroupSignaled, got %v", outcome)
	}
}

// TestApplyForceKillsUncooperative proves the grace escalation: a command that
// ignores the interrupt must still die within WaitDelay rather than hanging
// the caller forever.
func TestApplyForceKillsUncooperative(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()

	cmd := exec.CommandContext(ctx, "sh", "-c", `trap '' INT TERM; sleep 30`)
	result := Apply(cmd, 1*time.Second)

	start := time.Now()
	err := cmd.Run()
	elapsed := time.Since(start)
	if err == nil {
		t.Fatal("expected the canceled command to report an error")
	}
	// Deadline (200ms) + grace (1s) + slack. Well under sleep 30.
	if elapsed > 10*time.Second {
		t.Fatalf("uncooperative command outlived the grace escalation: %v", elapsed)
	}
	// The group signal is still delivered successfully here — the ignoring
	// process just doesn't act on it. That makes this CancelGroupSignaled,
	// not CancelForceKilled: os/exec's own WaitDelay escalation (SIGKILL) is
	// what actually ends the process, running independently of — and after —
	// our cmd.Cancel closure, which already reported delivery. CancelForceKilled
	// is reserved for when interruptProcessGroup itself fails to deliver and
	// *our* fallback cmd.Process.Kill() is what fires (see
	// TestApplyForceKilledWhenGroupSignalFails).
	if outcome := result.Outcome(); outcome != CancelGroupSignaled {
		t.Fatalf("expected CancelGroupSignaled (signal delivered; os/exec's own WaitDelay kill finishes the job), got %v", outcome)
	}
}

// TestApplyAcceptedFlag proves the delivered-cancellation flag contract that
// internal/runtime/exec's cancellation-wins error mapping depends on.
func TestApplyAcceptedFlag(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithTimeout(context.Background(), 150*time.Millisecond)
	defer cancel()

	cmd := exec.CommandContext(ctx, "sh", "-c", `sleep 30`)
	result := Apply(cmd, 2*time.Second)
	if err := cmd.Run(); err == nil {
		t.Fatal("expected the canceled command to report an error")
	}
	if !result.Delivered() {
		t.Fatal("accepted flag must record the delivered cancellation")
	}

	// A command that finishes on its own must not set the flag. (Cancel
	// requires a context-created command even when the context never fires.)
	cmd2 := exec.CommandContext(context.Background(), "sh", "-c", "true")
	result2 := Apply(cmd2, 2*time.Second)
	if err := cmd2.Run(); err != nil {
		t.Fatalf("healthy command failed: %v", err)
	}
	if result2.Delivered() {
		t.Fatal("accepted flag must stay false when the command completes normally")
	}
}

// TestApplyDoesNotShootTheRollbackTrapsChildren pins the one-interrupt-then-
// quiet-grace contract: after the single group SIGINT, NOTHING may signal the
// group again until WaitDelay's force-kill — a rollback trap's own children
// (an mv or find in a restore_stage-style rollback) run inside that grace,
// and any re-signal heuristic that fires during it aborts the rollback
// mid-flight and strands staged state. Two re-signal designs (until leader
// death; until the pre-signal child cohort changed) were reviewed into
// retirement for exactly this; the trap here does its work through a child
// process slow enough that any re-signal within the grace window kills it,
// so the marker is only written if the quiet-grace contract holds.
func TestApplyDoesNotShootTheRollbackTrapsChildren(t *testing.T) {
	t.Parallel()
	// The trap does its restore through a child process (sleep) slow enough
	// that any signal to the group inside the grace window kills it (default
	// INT disposition) and aborts the trap body before the marker write — so
	// the marker proves the quiet-grace contract.
	marker, _ := runTrapFixture(t, `sleep 0.5 && echo restored > "$MARKER"`)
	if _, err := os.Stat(marker); err != nil {
		t.Fatalf("rollback trap's child was killed by a re-signal — the rollback was aborted mid-flight: %v", err)
	}
}

// waitForReadiness is the one polling site in this package, at a true
// black-box boundary: the fixture is a shell adapter whose only completion
// signal for "the foreground child is now observable" is the readiness file
// its background subshell writes (adding a pipe would change the fixture
// contract). Boundary owner: the adapter script's readiness marker. It polls
// on a bounded ticker up to testutil.ExecRaceTimeout (the documented floor
// for timers racing a subprocess start) and fails with the last observed
// state: a command exit before readiness, a stat error, or the timeout.
func waitForReadiness(t *testing.T, ready string, done <-chan error) {
	t.Helper()
	ticker := time.NewTicker(10 * time.Millisecond)
	defer ticker.Stop()
	deadline := time.After(testutil.ExecRaceTimeout)
	for {
		if _, err := os.Stat(ready); err == nil {
			return
		} else if !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("stat readiness marker %s: %v", ready, err)
		}
		select {
		case err := <-done:
			t.Fatalf("command exited before readiness marker %s appeared: %v", ready, err)
		case <-deadline:
			t.Fatalf("readiness marker %s did not appear within %s", ready, testutil.ExecRaceTimeout)
		case <-ticker.C:
		}
	}
}

// runTrapFixture is the package's single trap-fixture runner (and its one
// os/exec site): a shell whose INT trap runs trapBody then exits 130, with a
// foreground `sleep 30` as the child cancellation must reach. Readiness is
// written by a background subshell only once that sleep is OBSERVABLE via
// pgrep -P — writing it before the fork let cancellation land inside the
// shell's ~1-2ms blocked-signal fork window (the pre-exec child misses the
// group SIGINT, the deferred trap loses to the WaitDelay force-kill); that
// window is the fixture's own artifact, closed here by construction. The
// trailing ':' is load-bearing: bash 3.2 skips the INT trap when the racing
// command sits in tail position of a -c script (verified 10/10 vs 0/10).
// Returns the marker path the trap body may write via $MARKER, and the
// CancelResult Apply attached to the command.
func runTrapFixture(t *testing.T, trapBody string) (string, *CancelResult) {
	t.Helper()
	dir := t.TempDir()
	marker := filepath.Join(dir, "restored")
	ready := filepath.Join(dir, "ready")

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	script := "trap '" + trapBody + `; exit 130' INT TERM
( i=0; until pgrep -P $$ "sleep" >/dev/null 2>&1; do i=$((i+1)); [ "$i" -gt 2000 ] && exit 1; done; : > "$READY" ) &
sleep 30
:`
	cmd := exec.CommandContext(ctx, "sh", "-c", script)
	cmd.Env = append(os.Environ(), "MARKER="+marker, "READY="+ready)
	result := Apply(cmd, 5*time.Second)

	if err := cmd.Start(); err != nil {
		t.Fatalf("start: %v", err)
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	waitForReadiness(t, ready, done)
	cancel()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("expected the canceled command to report an error")
		}
	case <-time.After(testutil.ExecRaceTimeout):
		t.Fatal("command never exited after cancellation")
	}
	return marker, result
}

// TestApplyLeaderSignaledOnlyWhenGetpgidFails proves the leader-only signal
// fallback: when the process group cannot be resolved, Apply must still
// interrupt the process leader directly, and record that no group signal
// was ever attempted.
//
// Not parallel: overrides the package-level getpgid seam.
func TestApplyLeaderSignaledOnlyWhenGetpgidFails(t *testing.T) {
	origGetpgid := getpgid
	getpgid = func(_ int) (int, error) {
		return 0, syscall.EINVAL
	}
	defer func() { getpgid = origGetpgid }()

	ctx, cancel := context.WithTimeout(context.Background(), 150*time.Millisecond)
	defer cancel()

	cmd := exec.CommandContext(ctx, "sleep", "30")
	result := Apply(cmd, 2*time.Second)

	if err := cmd.Run(); err == nil {
		t.Fatal("expected the canceled command to report an error")
	}
	if !result.Delivered() {
		t.Fatal("accepted flag must record the delivered cancellation")
	}
	if outcome := result.Outcome(); outcome != CancelLeaderSignaledOnly {
		t.Fatalf("expected CancelLeaderSignaledOnly, got %v", outcome)
	}
}

// TestApplyForceKilledWhenGroupSignalFails proves the last-resort fallback:
// when the process-group signal itself fails outright (not just "already
// gone"), Apply must fall back to killing the leader directly rather than
// leaving the command to run out the clock on WaitDelay.
//
// Not parallel: overrides the package-level killProcessGroup seam.
func TestApplyForceKilledWhenGroupSignalFails(t *testing.T) {
	origKill := killProcessGroup
	killProcessGroup = func(_ int, _ syscall.Signal) error {
		return syscall.EPERM
	}
	defer func() { killProcessGroup = origKill }()

	ctx, cancel := context.WithTimeout(context.Background(), 150*time.Millisecond)
	defer cancel()

	cmd := exec.CommandContext(ctx, "sleep", "30")
	result := Apply(cmd, 2*time.Second)

	if err := cmd.Run(); err == nil {
		t.Fatal("expected the canceled command to report an error")
	}
	if !result.Delivered() {
		t.Fatal("accepted flag must record the delivered cancellation")
	}
	if outcome := result.Outcome(); outcome != CancelForceKilled {
		t.Fatalf("expected CancelForceKilled, got %v", outcome)
	}

	status, ok := cmd.ProcessState.Sys().(syscall.WaitStatus)
	if !ok {
		t.Fatalf("expected a syscall.WaitStatus, got %T", cmd.ProcessState.Sys())
	}
	if !status.Signaled() || status.Signal() != syscall.SIGKILL {
		t.Fatalf("expected the process to be killed by SIGKILL, got signaled=%v signal=%v", status.Signaled(), status.Signal())
	}
}

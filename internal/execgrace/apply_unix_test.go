//go:build !windows

package execgrace

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
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
	dir := t.TempDir()
	marker := filepath.Join(dir, "restored")
	ready := filepath.Join(dir, "ready")

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// The trap models worktree-setup.sh's restore_stage: it must observe the
	// interrupt and write the marker (i.e. "move the staged files back").
	// Cancellation fires only once the foreground sleep is OBSERVABLE (the
	// background subshell writes readiness when it appears): a fixed-delay
	// cancel raced the shell's fork window under parallel test load, where
	// the pre-exec child misses the group SIGINT and the deferred trap
	// loses to the WaitDelay force-kill.
	script := `trap 'echo restored > "$MARKER"; exit 130' INT TERM
( i=0; until pgrep -P $$ "sleep" >/dev/null 2>&1; do i=$((i+1)); [ "$i" -gt 2000 ] && exit 1; done; : > "$READY" ) &
sleep 30
:`
	cmd := exec.CommandContext(ctx, "sh", "-c", script)
	cmd.Env = append(os.Environ(), "MARKER="+marker, "READY="+ready)
	Apply(cmd, 5*time.Second)

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
	case <-time.After(10 * time.Second):
		t.Fatal("command never exited after cancellation")
	}
	if _, err := os.Stat(marker); err != nil {
		t.Fatalf("rollback trap never ran — staged state would have been lost: %v", err)
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
	Apply(cmd, 1*time.Second)

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
}

// TestApplyAcceptedFlag proves the delivered-cancellation flag contract that
// internal/runtime/exec's cancellation-wins error mapping depends on.
func TestApplyAcceptedFlag(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithTimeout(context.Background(), 150*time.Millisecond)
	defer cancel()

	cmd := exec.CommandContext(ctx, "sh", "-c", `sleep 30`)
	accepted := Apply(cmd, 2*time.Second)
	if err := cmd.Run(); err == nil {
		t.Fatal("expected the canceled command to report an error")
	}
	if !accepted.Load() {
		t.Fatal("accepted flag must record the delivered cancellation")
	}

	// A command that finishes on its own must not set the flag. (Cancel
	// requires a context-created command even when the context never fires.)
	cmd2 := exec.CommandContext(context.Background(), "sh", "-c", "true")
	accepted2 := Apply(cmd2, 2*time.Second)
	if err := cmd2.Run(); err != nil {
		t.Fatalf("healthy command failed: %v", err)
	}
	if accepted2.Load() {
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
	dir := t.TempDir()
	marker := filepath.Join(dir, "restored")
	ready := filepath.Join(dir, "ready")

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// The trap models restore_stage: a non-builtin child (sleep) doing real
	// work inside the grace window, then the state restore. Any signal to
	// the group during the grace kills that sleep (default INT disposition)
	// and aborts the trap body before the marker write — so the marker
	// proves the quiet-grace contract. Readiness is written by a background
	// subshell only once the foreground sleep is OBSERVABLE, closing the
	// shell's fork window so the single interrupt is always deliverable
	// (same construction as the exec provider's start-cancellation test).
	// The trailing ':' is load-bearing: bash 3.2 skips the INT trap when
	// the racing command sits in tail position of a -c script (verified
	// 10/10 vs 0/10).
	script := `trap 'sleep 0.5 && echo restored > "$MARKER"; exit 130' INT TERM
( i=0; until pgrep -P $$ "sleep" >/dev/null 2>&1; do i=$((i+1)); [ "$i" -gt 2000 ] && exit 1; done; : > "$READY" ) &
sleep 30
:`
	cmd := exec.CommandContext(ctx, "sh", "-c", script)
	cmd.Env = append(os.Environ(), "MARKER="+marker, "READY="+ready)
	Apply(cmd, 5*time.Second)

	if err := cmd.Start(); err != nil {
		t.Fatalf("start: %v", err)
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()

	waitForReadiness(t, ready, done)

	cancel()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("command never exited after cancellation")
	}
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

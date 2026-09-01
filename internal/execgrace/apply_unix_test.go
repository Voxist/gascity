//go:build !windows

package execgrace

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"
)

// TestApplyTrapRunsBeforeKill is the regression test for the staged-content
// data-loss class: a setup script that has moved files aside and registered a
// rollback trap must get to run that trap when its deadline expires. With
// Go's default context-cancel (SIGKILL) the trap can never run; with Apply the
// group interrupt reaches the shell and the trap restores state before the
// grace escalation.
func TestApplyTrapRunsBeforeKill(t *testing.T) {
	t.Parallel()
	marker := filepath.Join(t.TempDir(), "restored")

	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()

	// The trap models worktree-setup.sh's restore_stage: it must observe the
	// interrupt and write the marker (i.e. "move the staged files back").
	script := `trap 'echo restored > "$MARKER"; exit 130' INT TERM; sleep 30`
	cmd := exec.CommandContext(ctx, "sh", "-c", script)
	cmd.Env = append(os.Environ(), "MARKER="+marker)
	Apply(cmd, 5*time.Second)

	if err := cmd.Run(); err == nil {
		t.Fatal("expected the canceled command to report an error")
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

// TestApplyResignalsWhenTheFirstInterruptIsLost is the regression test for the
// lost-first-interrupt race (the TestProvider_StartCancellationInterrupts-
// ForegroundChild flake): cancellation can fire in the window where the shell
// has committed to forking its foreground child with signals blocked — the
// pre-exec child never observes the SIGINT, the shell defers its trap behind
// the child's full runtime, and the WaitDelay force-kill wins, so the rollback
// trap never runs. A single interrupt is a one-shot race; Apply must keep
// re-signaling the group during the grace window until the process is
// observed dead, so delivery converges.
//
// The fixture makes the lost first signal deterministic instead of a fork-
// window coin flip: the inner shell IGNORES INT for 500ms (modeling the
// blocked-signal window), then restores the default disposition and blocks in
// a long foreground sleep. The first interrupt lands entirely inside the
// ignore window and is provably lost; only a re-signal after the window can
// kill the sleep and let the outer trap run.
func TestApplyResignalsWhenTheFirstInterruptIsLost(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	marker := filepath.Join(dir, "interrupted")
	ready := filepath.Join(dir, "ready")

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	script := `trap 'echo interrupted > "$MARKER"; exit 130' INT TERM
sh -c 'trap "" INT; : > "$READY"; sleep 0.5; trap - INT; sleep 30'
:`
	// The trailing ':' is load-bearing: bash 3.2 treats the LAST command of a
	// -c script specially, and with the inner shell in tail position the outer
	// trap reliably never runs when the child dies of SIGINT (verified 10/10
	// failing without the no-op vs 0/10 with it, independent of this
	// package's re-signaling). The no-op keeps the fixture testing signal
	// re-delivery, not bash's tail-position quirk.
	cmd := exec.CommandContext(ctx, "sh", "-c", script)
	cmd.Env = append(os.Environ(), "MARKER="+marker, "READY="+ready)
	Apply(cmd, 5*time.Second)

	done := make(chan error, 1)
	if err := cmd.Start(); err != nil {
		t.Fatalf("start: %v", err)
	}
	go func() { done <- cmd.Wait() }()

	deadline := time.After(5 * time.Second)
	for {
		if _, err := os.Stat(ready); err == nil {
			break
		}
		select {
		case err := <-done:
			t.Fatalf("command exited before readiness: %v", err)
		case <-deadline:
			t.Fatal("timed out waiting for readiness marker")
		case <-time.After(5 * time.Millisecond):
		}
	}

	cancel() // lands inside the inner shell's ignore window: this SIGINT is lost
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("command never exited after cancellation")
	}
	if _, err := os.Stat(marker); err != nil {
		t.Fatalf("rollback trap never ran — the lost first interrupt was not re-sent: %v", err)
	}
}

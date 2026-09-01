//go:build !windows

package execgrace

import (
	"errors"
	"os"
	"os/exec"
	"syscall"
	"time"
)

// setProcessGroup puts the command in its own process group so a cooperative
// cancellation can be delivered to the whole group — reaching any foreground
// child (for example a long-running git checkout under a setup shell) that
// would otherwise keep the shell from running its rollback trap before the
// forced kill.
func setProcessGroup(cmd *exec.Cmd) {
	if cmd.SysProcAttr == nil {
		cmd.SysProcAttr = &syscall.SysProcAttr{}
	}
	cmd.SysProcAttr.Setpgid = true
}

// interruptProcessGroup sends os.Interrupt to the command's process group so a
// foreground child receives it alongside the shell leader. It preserves the
// os.ErrProcessDone signal the caller special-cases: an already-exited target
// reports ErrProcessDone rather than a spurious failure. If the group id cannot
// be resolved it falls back to signaling the leader directly.
func interruptProcessGroup(cmd *exec.Cmd) error {
	if cmd.Process == nil {
		return os.ErrProcessDone
	}
	pgid, err := syscall.Getpgid(cmd.Process.Pid)
	if err != nil {
		return cmd.Process.Signal(os.Interrupt)
	}
	if killErr := syscall.Kill(-pgid, syscall.SIGINT); killErr != nil {
		if errors.Is(killErr, syscall.ESRCH) {
			return os.ErrProcessDone
		}
		return killErr
	}
	return nil
}

// resignalInterval paces the re-delivery loop below. It must be comfortably
// smaller than the grace window (Apply's callers use >= 2s) so a lost first
// interrupt is re-sent several times before the WaitDelay force-kill.
const resignalInterval = 100 * time.Millisecond

// resignalUntilDone keeps re-sending the group interrupt until the command's
// leader is observed dead or the budget (the WaitDelay grace window) expires.
//
// A single interrupt is a one-shot race: cancellation can land in the window
// where the shell has committed to forking its foreground child with signals
// blocked — the pre-exec child never observes the SIGINT, the shell defers
// its trap behind the child's full runtime, and the force-kill wins, so the
// rollback trap never runs (measured at 13/100 on a quiet host for the
// exec-provider start test before this loop existed). Re-sending converges:
// the first tick after the window kills the child, the shell reaps it and
// runs its trap inside the grace budget.
//
// Liveness is probed through cmd.Process.Signal(0), which os/exec guards
// against reaped processes, so the loop cannot signal a recycled pid group
// after Wait has returned; a group whose members are all gone stops the loop
// via ESRCH. The budget bound keeps the goroutine's lifetime finite even if
// the process ignores every signal — the WaitDelay force-kill owns that case.
func resignalUntilDone(cmd *exec.Cmd, budget time.Duration) {
	if budget <= 0 {
		return
	}
	go func() {
		deadline := time.Now().Add(budget)
		ticker := time.NewTicker(resignalInterval)
		defer ticker.Stop()
		for range ticker.C {
			if time.Now().After(deadline) {
				return
			}
			if err := cmd.Process.Signal(syscall.Signal(0)); err != nil {
				return // leader reaped or gone; never signal a recyclable pgid
			}
			if err := interruptProcessGroup(cmd); err != nil {
				return
			}
		}
	}()
}

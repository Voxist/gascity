//go:build !windows

package execgrace

import (
	"errors"
	"os"
	"os/exec"
	"strconv"
	"strings"
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

// resignalInterval paces the re-delivery loop below; the effective interval
// is capped at a quarter of the grace budget (floor 20ms) so a small budget
// still gets several re-sends instead of silently none.
const resignalInterval = 100 * time.Millisecond

// preSignalCohort snapshots the leader's direct children BEFORE the first
// interrupt is sent. This set defines "the group has not reacted yet": while
// every snapshot pid is still alive and no new direct child has appeared,
// the interrupt was provably lost (a foreground child with the default INT
// disposition dies on delivery, and a shell that received it would have
// moved on — reaping the child or spawning trap children). The moment the
// set changes in either direction, re-signaling must stop: a death means
// the signal landed and the shell may be running its rollback trap, and a
// new pid IS that trap's child — shooting it aborts the rollback mid-flight
// and strands staged state (found by review of the first re-signal design,
// which re-signaled until the leader died and killed trap children on every
// slow rollback).
//
// An empty snapshot (shell between commands, or cancellation landing in the
// microsecond fork window before the child is visible) disables re-signaling
// and degrades to the original single-interrupt semantics; that residual
// window is accepted and documented rather than papered over.
func preSignalCohort(cmd *exec.Cmd) map[int]bool {
	if cmd.Process == nil {
		return nil
	}
	return directChildren(cmd.Process.Pid)
}

// directChildren lists the live direct children of pid via pgrep -P. A
// missing pgrep or a no-match exit yields an empty set, which callers treat
// as "cannot observe: do not re-signal" — the conservative degradation.
func directChildren(pid int) map[int]bool {
	out, err := exec.Command("pgrep", "-P", strconv.Itoa(pid)).Output()
	if err != nil {
		return nil
	}
	kids := make(map[int]bool)
	for _, f := range strings.Fields(string(out)) {
		if n, err := strconv.Atoi(f); err == nil {
			kids[n] = true
		}
	}
	return kids
}

func sameCohort(a, b map[int]bool) bool {
	if len(a) != len(b) {
		return false
	}
	for pid := range a {
		if !b[pid] {
			return false
		}
	}
	return true
}

// resignalWhileUnchanged re-sends the group interrupt while the pre-signal
// cohort is provably intact — the lost-first-interrupt state — and stops
// permanently the moment the group visibly reacts (any cohort pid dies or a
// new direct child appears), so a running rollback trap and its children are
// never signaled. A single interrupt is otherwise a one-shot race:
// cancellation can land in the window where the shell has committed to
// forking its foreground child with signals blocked, the pre-exec child
// never observes the SIGINT, the shell defers its trap behind the child's
// full runtime, and the WaitDelay force-kill wins (measured at 13/100 on a
// quiet host for the exec-provider start test before re-signaling existed).
//
// Liveness of the leader is probed through cmd.Process.Signal(0), which
// os/exec guards against reaped processes; the probe-to-kill gap is a
// residual TOCTOU (a reap plus pid-group recycling between two lines) that
// this design narrows but does not close — the cohort rule stops the loop
// after one or two ticks in every non-lost case, so the gap is crossed far
// fewer times than a fixed re-signal loop would. The budget bound keeps the
// goroutine finite when a process ignores everything; the force-kill owns
// that case.
func resignalWhileUnchanged(cmd *exec.Cmd, budget time.Duration, cohort map[int]bool) {
	if budget <= 0 || len(cohort) == 0 {
		return
	}
	leader := cmd.Process.Pid
	interval := resignalInterval
	if cap := budget / 4; cap < interval {
		interval = cap
	}
	if interval < 20*time.Millisecond {
		interval = 20 * time.Millisecond
	}
	go func() {
		deadline := time.Now().Add(budget)
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		for range ticker.C {
			if time.Now().After(deadline) {
				return
			}
			if err := cmd.Process.Signal(syscall.Signal(0)); err != nil {
				return // leader reaped or gone
			}
			if !sameCohort(cohort, directChildren(leader)) {
				return // the group reacted: a rollback trap may be running
			}
			if err := interruptProcessGroup(cmd); err != nil {
				return
			}
		}
	}()
}

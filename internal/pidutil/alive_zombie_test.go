//go:build !windows

package pidutil

import (
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

// TestAlive_ZombieIsDeadWithoutPS is the regression test for ga-yeyt3.
//
// Alive's last-resort zombie probe fork/execs `ps -o stat=` under a 100ms
// deadline and reads every failure as "not a zombie", i.e. as alive. On a host
// saturated with fork/exec that deadline is missed often — measured at 20 of
// 300 calls, worst case 2.7s, on a 16-core Mac at load ~150 — so a process that
// had already died read as alive. Callers polling Alive for a child to exit
// (terminateManagedDoltPIDGuarded, the managed-Dolt scope watchdog reap) then
// ran their full grace to the deadline and reported the exit as never having
// happened.
//
// Making ps unusable reproduces that saturation deterministically: with the
// stub on PATH, Alive must still report a signaled, unreaped child as dead,
// because it reads the kernel's own process record (/proc on linux, a
// sysctl(kern.proc.pid) on darwin) before it ever considers ps.
func TestAlive_ZombieIsDeadWithoutPS(t *testing.T) {
	// spawnSleeper's cleanup reaps the child, so it stays a zombie — signaled
	// but unwaited — for the whole of this test body, which is the state Alive
	// has to get right.
	pid := spawnSleeper(t)

	if !Alive(pid) {
		t.Fatalf("Alive(%d) = false for a running child", pid)
	}

	if err := syscall.Kill(pid, syscall.SIGKILL); err != nil {
		t.Fatalf("kill child: %v", err)
	}

	binDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(binDir, "ps"), []byte("#!/bin/sh\nexec sleep 30\n"), 0o755); err != nil {
		t.Fatalf("WriteFile(ps stub): %v", err)
	}
	t.Setenv("PATH", strings.Join([]string{binDir, os.Getenv("PATH")}, string(os.PathListSeparator)))

	// The kill is asynchronous, so wait on the condition with a bound rather
	// than a fixed delay: with the fix this settles in microseconds, and a
	// regression shows up as the deadline expiring, not as a slower pass.
	deadline := time.NewTimer(5 * time.Second)
	defer deadline.Stop()
	poll := time.NewTicker(5 * time.Millisecond)
	defer poll.Stop()
	for Alive(pid) {
		select {
		case <-poll.C:
		case <-deadline.C:
			t.Fatalf("Alive(%d) still true 5s after SIGKILL with ps unusable; the zombie probe fell back to ps and failed open", pid)
		}
	}
}

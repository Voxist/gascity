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

// TestAlive_ZombieIsDeadWithoutPS is the regression test for ga-yeyt3.
//
// Alive's last-resort zombie probe fork/execs `ps -o stat=` under a 100ms
// deadline and reads every failure as "not a zombie", i.e. as alive. On a host
// saturated with fork/exec that deadline is missed often — measured at 20 of
// 300 calls, worst case 2.7s, on a 16-core Mac at load ~150 — so a process that
// had already died read as alive. Callers polling Alive for a child to exit
// (terminateManagedDoltPIDGuarded, the scope watchdog reap) then ran their full
// grace to the deadline and reported the exit as never having happened.
//
// Making ps unusable reproduces that saturation deterministically: with the
// stub on PATH, Alive must still report a signaled, unreaped child as dead,
// because it reads the kernel's own process record (/proc on linux, a
// sysctl(kern.proc.pid) on darwin) before it ever considers ps.
func TestAlive_ZombieIsDeadWithoutPS(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX zombie semantics required")
	}

	child := exec.Command("sleep", "60")
	if err := child.Start(); err != nil {
		t.Fatalf("start child: %v", err)
	}
	pid := child.Process.Pid
	reaped := false
	t.Cleanup(func() {
		if reaped {
			return
		}
		_ = child.Process.Kill()
		_ = child.Wait()
	})

	if !Alive(pid) {
		t.Fatalf("Alive(%d) = false for a running child", pid)
	}

	// Kill without reaping: the child stays in the process table as a zombie
	// until Wait, which is the state Alive has to get right.
	if err := child.Process.Kill(); err != nil {
		t.Fatalf("kill child: %v", err)
	}

	binDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(binDir, "ps"), []byte("#!/bin/sh\nexec sleep 30\n"), 0o755); err != nil {
		t.Fatalf("WriteFile(ps stub): %v", err)
	}
	t.Setenv("PATH", strings.Join([]string{binDir, os.Getenv("PATH")}, string(os.PathListSeparator)))

	// The kill is asynchronous, so bound the wait rather than sleeping a fixed
	// amount: with the fix this settles in microseconds, and a regression here
	// shows up as the deadline expiring, not as a slower pass.
	deadline := time.Now().Add(5 * time.Second)
	for Alive(pid) {
		if time.Now().After(deadline) {
			t.Fatalf("Alive(%d) still true 5s after SIGKILL with ps unusable; the zombie probe fell back to ps and failed open", pid)
		}
		time.Sleep(5 * time.Millisecond)
	}

	_ = child.Wait()
	reaped = true
	if Alive(pid) {
		t.Fatalf("Alive(%d) = true after the child was reaped", pid)
	}
}

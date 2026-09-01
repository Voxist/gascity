//go:build darwin

package pidutil

import (
	"errors"
	"fmt"
	"syscall"

	"golang.org/x/sys/unix"
)

// darwinSZOMB is SZOMB from <sys/proc.h> (SIDL=1, SRUN=2, SSLEEP=3, SSTOP=4,
// SZOMB=5). x/sys/unix does not export the p_stat constants.
const darwinSZOMB = 5

// procStateDead reports whether pid is no longer a running process — reaped, or
// a zombie awaiting its parent's wait — by reading the kernel process record
// directly with sysctl(kern.proc.pid). known is false only when that record
// cannot be read for a PID that still exists, which leaves the caller on its
// portable fallback.
//
// This exists because the ps-based fallback is not usable as a liveness oracle
// on a loaded host. It fork/execs ps under a 100ms deadline and treats every
// failure as "not a zombie", i.e. as alive. Measured on a 16-core Mac at load
// ~150, `ps -o stat= -p PID` exceeded 100ms in 20 of 300 calls with a 2.7s
// worst case, so ~7% of probes reported a dead process as alive — which is
// exactly the window the managed-Dolt terminate paths poll in. The sysctl is a
// single syscall with no deadline and no child process: 10,000 calls take 185ms
// (~18us each), and it cannot be starved by fork/exec contention.
func procStateDead(pid int) (dead, known bool) {
	kp, err := unix.SysctlKinfoProc("kern.proc.pid", pid)
	if err == nil && kp != nil {
		return kp.Proc.P_stat == darwinSZOMB, true
	}
	// No process record. That is how darwin answers for a PID that has already
	// been reaped (the sysctl returns zero bytes, surfaced as EIO), but confirm
	// with kill(0) rather than assume: a transient sysctl failure against a live
	// process must report "unknown" and fall back, not declare it dead.
	if sigErr := syscall.Kill(pid, 0); errors.Is(sigErr, syscall.ESRCH) {
		return true, true
	}
	return false, false
}

// procStartTime returns pid's start time as "<sec>.<usec>" from the same kernel
// process record. It replaces a `ps -o lstart=` subprocess that carries the same
// starvation problem as procStateDead's, and whose one-second resolution cannot
// distinguish a PID recycled within the same second from the original process.
func procStartTime(pid int) (string, bool) {
	kp, err := unix.SysctlKinfoProc("kern.proc.pid", pid)
	if err != nil || kp == nil {
		return "", false
	}
	return fmt.Sprintf("%d.%06d", kp.Proc.P_starttime.Sec, kp.Proc.P_starttime.Usec), true
}

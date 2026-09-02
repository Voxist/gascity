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
//
// Measured on this host at load ~119: `ps -p PID -o lstart=` averages 6.9ms
// (p95 10.3ms, max 24.3ms) against 162us (p95 335us, max 550us) for the sysctl,
// and across 60 back-to-back spawns the sysctl produced 60 distinct tokens
// where lstart produced 1 — 59 of the 60 processes were mutually
// indistinguishable to the mechanism this replaces.
//
// The caller tags this value with startTimeMechSysctl; see StartTime for why a
// token may never be compared against one produced by another mechanism.
func procStartTime(pid int) (string, bool) {
	kp, err := unix.SysctlKinfoProc("kern.proc.pid", pid)
	if err != nil || kp == nil {
		return "", false
	}
	return fmt.Sprintf("%d.%06d", kp.Proc.P_starttime.Sec, kp.Proc.P_starttime.Usec), true
}

// sysctlChildPIDs returns the live direct children of parent from the kernel
// process table via sysctl(kern.proc.all), replacing a `ps -axo pid=,ppid=`
// fork/exec. ok is false when the table cannot be read, which leaves the caller
// on its portable ps fallback.
//
// Named for its mechanism because ChildPIDs now has two kernel readers ahead of
// ps: procChildPIDs reads /proc/<pid>/task/<tid>/children (#5427) and answers on
// linux, this one answers on darwin where there is no /proc to read.
//
// kern.proc.ppid would answer this more narrowly, but darwin's
// sysctlnametomib does not resolve that subname, so this filters the full
// table. It costs ~8ms for ~1250 processes and still beats forking ps, which
// must build and format the same table in a child process.
//
// Unlike the ps path there is no enumeration helper of our own in the result:
// the syscall does not create a process, so a caller checking its own children
// never has to exclude a probe that masqueraded as a leaked child.
func sysctlChildPIDs(parent int) ([]int, bool) {
	all, err := unix.SysctlKinfoProcSlice("kern.proc.all")
	if err != nil {
		return nil, false
	}
	var children []int
	for i := range all {
		if int(all[i].Eproc.Ppid) == parent {
			children = append(children, int(all[i].Proc.P_pid))
		}
	}
	return children, true
}

// procCmdline returns pid's argv exactly, from sysctl(kern.procargs2).
//
// This is the mechanism the psCmdline doc comment describes as "KERN_PROCARGS2
// via cgo, which is not worth it". It needs no cgo — unix.SysctlRaw resolves
// the name — and it removes the accepted limitation that comment records: ps
// renders argv as a single space-joined string, so an argument containing a
// space is split in two and silently fails the match. This returns the
// kernel's own NUL-separated argv, so spaces inside an argument survive.
//
// Both sides of this merge grew a kern.procargs2 reader independently — the
// fork's procCmdline (ga-4l4me) and upstream's platformCmdline/parseProcArgs2
// (#5216, "preserve Darwin argv boundaries"). One buffer parser is kept,
// upstream's, because it is build-tag free and therefore unit-testable on the
// linux runners too (procargs2_test.go); this wrapper keeps the fork's
// (argv, ok) contract, which is what lets Cmdline fall back to ps instead of
// surfacing a sysctl failure as an error. parseProcArgs2 carries the same
// allocation guard the fork's parser had — it bounds argc by the buffer length
// before sizing the slice, so a malformed record claiming 0x7FFFFFFF arguments
// cannot allocate a multi-GB header.
//
// ok is false when the buffer cannot be read or does not hold argc complete
// arguments — the caller then falls back to ps rather than returning a
// truncated argv, because a short argv fails the identity match and callers
// read a failed match as "not my process".
func procCmdline(pid int) ([]string, bool) {
	buf, err := unix.SysctlRaw("kern.procargs2", pid)
	if err != nil {
		return nil, false
	}
	argv, err := parseProcArgs2(buf)
	if err != nil {
		return nil, false
	}
	argv = NormalizeArgv(argv)
	if len(argv) == 0 {
		return nil, false
	}
	return argv, true
}

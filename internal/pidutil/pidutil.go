// Package pidutil contains small process helpers shared across GC packages.
package pidutil

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"
)

const (
	psZombieTimeout  = 100 * time.Millisecond
	childEnumTimeout = 1 * time.Second
	// psStartTimeTimeout bounds the portable start-time probe. Callers sit in a
	// post-SIGKILL reap loop, so a hung ps must not stall them.
	psStartTimeTimeout = 1 * time.Second
)

// psCmdlineTimeout bounds the portable argv probe. Callers run on reconciler
// ticks, so a hung ps must not stall them; a timeout yields no argv, which the
// identity check treats as "cannot confirm" and rejects.
const psCmdlineTimeout = time.Second

// Alive reports whether a PID exists and is not a zombie.
//
// The zombie check reads /proc where it exists, a kernel process record via
// sysctl on darwin, and only then falls back to a ps subprocess. The fallback
// is last because it fails open — a ps that misses its deadline reports "not a
// zombie", i.e. alive — and on a host saturated with fork/exec that happens
// often enough to make callers that poll Alive for a process to exit flaky.
func Alive(pid int) bool {
	if pid <= 0 {
		return false
	}
	err := syscall.Kill(pid, 0)
	if err != nil && !errors.Is(err, syscall.EPERM) {
		return false
	}
	statPath := filepath.Join("/proc", strconv.Itoa(pid), "stat")
	data, err := os.ReadFile(statPath)
	if err != nil {
		if dead, known := procStateDead(pid); known {
			return !dead
		}
		return !psReportsZombie(pid)
	}
	fields := strings.Fields(string(data))
	if len(fields) >= 3 && fields[2] == "Z" {
		return false
	}
	return true
}

// Start-time identity tokens are tagged with the mechanism that produced them.
// The value formats are mutually unintelligible — clock ticks since boot, a
// microsecond epoch pair, a wall-clock date — so a token may only ever be
// compared against another token from the SAME mechanism. The tag is what makes
// that checkable instead of assumed: see sameStartIdentity.
const (
	startTimeMechProc   = "proc"     // /proc/<pid>/stat field 22, clock ticks since boot
	startTimeMechSysctl = "sysctl"   // darwin kern.proc.pid p_starttime, "<sec>.<usec>"
	startTimeMechPS     = "pslstart" // ps -o lstart=, wall-clock to the second
)

// splitStartTime returns the mechanism and value of a tagged token. ok is false
// for anything this package did not produce — an untagged token written by an
// older build, or a tag from a mechanism this build does not know. Both are
// "unknown", never "different"; see sameStartIdentity.
//
// The value may itself contain colons (ps renders a clock time), so this cuts
// at the first one only.
func splitStartTime(token string) (mech, value string, ok bool) {
	mech, value, found := strings.Cut(token, ":")
	if !found || value == "" {
		return "", "", false
	}
	switch mech {
	case startTimeMechProc, startTimeMechSysctl, startTimeMechPS:
		return mech, value, true
	}
	return "", "", false
}

// sameStartIdentity reports whether captured and current describe the same
// process start.
//
// It answers true — "still the original process", the conservative direction —
// for every comparison it cannot actually make: an untagged or unrecognized
// token on either side, or two tokens from different mechanisms. Only a
// same-mechanism disagreement is reported as a different process.
//
// That asymmetry is the whole point. A false "different" is read by callers as
// PID reuse, i.e. as proof that the original process is dead, and acting on
// that kills or abandons live work. A false "same" costs only the pre-existing
// conservative answer the code had before any identity check existed. So a
// format mismatch must degrade to "unknown", and "unknown" must resolve the way
// this package's stated doctrine resolves it: "the consequence of a miss is the
// pre-existing conservative answer rather than a wrong death."
//
// This is not hypothetical bookkeeping. On darwin StartTime now answers from
// sysctl but still falls back to ps, so a capture and a re-read taken seconds
// apart inside one KillByPID can legitimately come from different mechanisms if
// the sysctl fails transiently in between. Comparing those two values directly
// yields "different" for a process that never went anywhere, and the reap loop
// then reports a live process as confirmed dead.
func sameStartIdentity(captured, current string) bool {
	capturedMech, capturedValue, capturedOK := splitStartTime(captured)
	currentMech, currentValue, currentOK := splitStartTime(current)
	if !capturedOK || !currentOK || capturedMech != currentMech {
		return true
	}
	return capturedValue == currentValue
}

// StartTime returns a PID's start time as an opaque, mechanism-tagged token
// used to disambiguate a recycled PID from the original target. The kernel
// never reuses a (pid, start time) pair for the lifetime of a boot, so a
// changed start time on the same PID proves the original process is gone and an
// unrelated one now holds the number.
//
// Three mechanisms answer, in descending order of preference:
//
//	proc      /proc/<pid>/stat field 22 (starttime, clock ticks since boot)
//	sysctl    darwin kern.proc.pid p_starttime, to the microsecond
//	pslstart  `ps -p PID -o lstart=`, to the second
//
// It returns an error only when none of them can answer; callers treat that as
// "no identity signal available" and fall back to plain liveness.
//
// The token is opaque and callers must not parse it. They must also not assume
// two tokens are comparable: the tag exists so sameStartIdentity can refuse to
// compare values from different mechanisms rather than reporting them as a
// different process. A token captured by an older build carries no tag and is
// therefore treated as unknown, which is the safe reading during an upgrade.
//
// ps is last for the reason given on Alive, plus one of its own: `ps -o lstart=`
// resolves only to the second, so two processes occupying the same recycled PID
// within one second produce the same token and the reuse guard aliases them.
// Measured here, 60 processes spawned back to back yielded 60 distinct sysctl
// tokens and 1 distinct lstart token. The sysctl path reports microseconds and
// has no such window.
//
// The comm field (field 2) of /proc/<pid>/stat is wrapped in parens and may
// itself contain spaces and parens, so parsing anchors on the final ')' and
// counts fields from there: field 3 (state) is the first token after "') '",
// making field 22 (starttime) the token at index 19 of that suffix.
func StartTime(pid int) (string, error) {
	if pid <= 0 {
		return "", fmt.Errorf("pidutil: invalid PID %d", pid)
	}
	data, err := os.ReadFile(filepath.Join("/proc", strconv.Itoa(pid), "stat"))
	if err != nil {
		if token, ok := procStartTime(pid); ok {
			return startTimeMechSysctl + ":" + token, nil
		}
		token, psErr := psStartTime(pid)
		if psErr != nil {
			return "", psErr
		}
		return startTimeMechPS + ":" + token, nil
	}
	stat := string(data)
	rparen := strings.LastIndexByte(stat, ')')
	if rparen < 0 || rparen+2 >= len(stat) {
		return "", fmt.Errorf("pidutil: malformed stat for PID %d", pid)
	}
	fields := strings.Fields(stat[rparen+2:])
	const starttimeIndexAfterComm = 19 // field 22 minus fields 1-3 offset
	if len(fields) <= starttimeIndexAfterComm {
		return "", fmt.Errorf("pidutil: stat for PID %d has %d post-comm fields, want > %d", pid, len(fields), starttimeIndexAfterComm)
	}
	return startTimeMechProc + ":" + fields[starttimeIndexAfterComm], nil
}

// AliveWithStartTime reports whether pid is alive AND still the same process
// identified by startTime. It closes the PID-reuse hole in Alive: during a
// post-SIGKILL reap wait the target's PID can be reaped and recycled to an
// unrelated new process inside the window, at which point plain Alive would
// wrongly report the (dead) target as still alive.
//
// An empty startTime disables the identity check and falls back to Alive — used
// when the original start time could not be captured before the wait. A
// startTime that disagrees with the current one, read by the same mechanism,
// means the PID was recycled: the original target is dead, so this returns
// false. Every other outcome — an unreadable current start time, or two tokens
// that cannot be compared — keeps the conservative Alive answer rather than
// inventing a death. See sameStartIdentity.
func AliveWithStartTime(pid int, startTime string) bool {
	if !Alive(pid) {
		return false
	}
	if startTime == "" {
		return true
	}
	current, err := startTimeForIdentity(pid)
	if err != nil {
		return true
	}
	return sameStartIdentity(startTime, current)
}

// startTimeForIdentity is the seam the unreadable-identity test drives.
//
// It exists because that case can no longer be produced from the outside: the
// test used to stub `ps` on PATH, but every supported host now answers
// StartTime from a kernel record (/proc on linux, sysctl on darwin), so the
// stub stopped having any effect and the test silently skipped on BOTH
// platforms — leaving "an unreadable identity must keep the Alive answer"
// asserted in no CI lane at all. That is the dangerous direction to leave
// uncovered: a regression there reports a live process as dead, and a caller
// that believes its process died will start a second copy of it.
var startTimeForIdentity = StartTime

// AliveWithCmdline reports whether a PID exists, is not a zombie, and its
// command line satisfies match.
//
// It used to return true unconditionally off Linux, because Cmdline read only
// /proc. That turned an identity check into a bare existence check on those
// hosts: callers use this to decide whether the PID in a pidfile is still THEIR
// process, so a recycled PID owned by an unrelated live process passed the
// check, and the caller skipped work it should have done. Cmdline is portable
// now, so the platform branch is gone.
//
// An unreadable argv yields false — never a match. Callers treat "not my
// process" as "do the work", which is the recoverable direction.
func AliveWithCmdline(pid int, match func([]string) bool) bool {
	if !Alive(pid) {
		return false
	}
	if match == nil {
		return false
	}
	argv, err := Cmdline(pid)
	if err != nil {
		return false
	}
	return match(argv)
}

// ArgvContainsSequence reports whether argv contains seq contiguously.
func ArgvContainsSequence(argv []string, seq ...string) bool {
	if len(seq) == 0 {
		return true
	}
	if len(argv) < len(seq) {
		return false
	}
	for i := 0; i <= len(argv)-len(seq); i++ {
		ok := true
		for j := range seq {
			if argv[i+j] != seq[j] {
				ok = false
				break
			}
		}
		if ok {
			return true
		}
	}
	return false
}

// ArgvHasFlagValue reports whether argv contains flag with value, either as
// "--flag value" or "--flag=value".
func ArgvHasFlagValue(argv []string, flag, value string) bool {
	if flag == "" || value == "" {
		return false
	}
	for i, arg := range argv {
		if arg == flag && i+1 < len(argv) && argv[i+1] == value {
			return true
		}
		if strings.HasPrefix(arg, flag+"=") && strings.TrimPrefix(arg, flag+"=") == value {
			return true
		}
	}
	return false
}

// Cmdline returns a PID's command line, normalized through NormalizeArgv.
// It reads /proc/<pid>/cmdline where available, then the kernel process
// arguments via sysctl(kern.procargs2) on darwin, and only then falls back to
// ps, which is how the rest of this repo already reads another process's argv
// (see the ps -o args= call sites in cmd/gc and internal/runtime/tmux).
// It returns an error when no mechanism can read the process record.
//
// The sysctl sits ahead of ps for accuracy as much as for speed: it returns the
// kernel's own NUL-separated argv, so an argument containing a space survives,
// where ps hands back one space-joined string that splits such an argument in
// two and silently fails the identity match.
func Cmdline(pid int) ([]string, error) {
	data, err := os.ReadFile(filepath.Join("/proc", strconv.Itoa(pid), "cmdline"))
	if err != nil {
		if argv, ok := procCmdline(pid); ok {
			return argv, nil
		}
		return psCmdline(pid)
	}
	trimmed := strings.TrimRight(string(data), "\x00")
	if trimmed == "" {
		return nil, nil
	}
	return NormalizeArgv(strings.Split(trimmed, "\x00")), nil
}

// NormalizeArgv returns argv with empty and whitespace-only arguments
// dropped — the rule Cmdline applies to /proc command lines. Callers
// comparing a configured argv against Cmdline output must pass the
// configured side through this helper first so both sides share the same
// argument shape.
func NormalizeArgv(argv []string) []string {
	out := make([]string, 0, len(argv))
	for _, arg := range argv {
		if strings.TrimSpace(arg) == "" {
			continue
		}
		out = append(out, arg)
	}
	return out
}

// ChildPIDs returns the pids of all live direct child processes of parent,
// read from the kernel process table via sysctl on darwin and enumerated
// portably via `ps -axo pid=,ppid=` elsewhere, so it works on darwin as well
// as linux. It returns an error when enumeration itself fails or times out, so
// callers can tell "enumeration ran and found nothing" apart from "enumeration
// did not run" — collapsing the two into an empty slice would let an
// unavailable check masquerade as a clean result.
//
// ps is itself alive, and a child of the caller, at the instant it captures
// the process table — so a caller checking its own children (parent ==
// os.Getpid(), the pattern this package's callers use for self leak checks)
// always sees ps's own transient pid/ppid row alongside any real children.
// The enumeration helper's own pid is excluded below so it can never
// masquerade as a leaked child. The sysctl path has no such problem to solve:
// it reads the table in-process and spawns nothing that could be mistaken for
// a leak.
func ChildPIDs(parent int) ([]int, error) {
	if children, ok := procChildPIDs(parent); ok {
		return children, nil
	}
	return psChildPIDs(parent)
}

// psChildPIDs enumerates parent's direct children with `ps -axo pid=,ppid=`.
// It is the last resort, reached only on a host whose kernel process table
// cannot be read directly. Kept as a named function rather than inlined into
// ChildPIDs so its error and self-exclusion contracts stay directly testable
// on every platform, including the ones where ChildPIDs itself never reaches
// it — the same reason psCmdline and psStartTime are tested directly.
func psChildPIDs(parent int) ([]int, error) {
	ctx, cancel := context.WithTimeout(context.Background(), childEnumTimeout)
	defer cancel()

	cmd := exec.CommandContext(ctx, "ps", "-axo", "pid=,ppid=")
	out, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("pidutil: ps enumeration failed: %w", err)
	}
	selfPID := -1
	if cmd.Process != nil {
		selfPID = cmd.Process.Pid
	}

	var children []int
	for _, line := range strings.Split(string(out), "\n") {
		fields := strings.Fields(line)
		if len(fields) != 2 {
			continue
		}
		pid, errPID := strconv.Atoi(fields[0])
		ppid, errPPID := strconv.Atoi(fields[1])
		if errPID != nil || errPPID != nil {
			continue
		}
		if pid == selfPID {
			continue
		}
		if ppid == parent {
			children = append(children, pid)
		}
	}
	return children, nil
}

// psStartTime reads a PID's start time with ps. It is the last resort, reached
// only on a host with neither /proc nor a readable kernel process record.
//
// Each mechanism returns a different format — /proc gives jiffies since boot,
// sysctl a microsecond epoch pair, ps a wall-clock date. That used to be
// justified by the observation that only one mechanism could answer on a given
// host, so a capture and a re-read always agreed on format. That is no longer
// true: darwin can answer from either sysctl or here, so StartTime tags every
// token with its mechanism and sameStartIdentity refuses to compare across
// them. The values are still never compared across hosts.
//
// One granularity limitation, and the reason this is now the fallback rather
// than the darwin mechanism: ps -o lstart= has one-second resolution, so a PID
// recycled within the same second as its predecessor started would compare
// equal and the reuse would go undetected. Measured here, 60 processes spawned
// back to back all reported the same lstart. The consequence of such a miss is
// the pre-existing conservative answer rather than a wrong death, which is why
// this remained usable as a fallback while it was the only portable option.
func psStartTime(pid int) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), psStartTimeTimeout)
	defer cancel()

	out, err := exec.CommandContext(ctx, "ps", "-p", strconv.Itoa(pid), "-o", "lstart=").Output()
	if err != nil {
		return "", fmt.Errorf("reading start time for pid %d via ps: %w", pid, err)
	}
	identity := strings.TrimSpace(string(out))
	if identity == "" {
		return "", fmt.Errorf("no start time reported for pid %d", pid)
	}
	return identity, nil
}

// psCmdline reads a PID's argv with ps. It is the last resort, reached only on
// a host with neither /proc nor a readable kern.procargs2 record.
//
// One accepted limitation, and the reason this is now the fallback: ps renders
// argv as a single space-joined string, so an argument containing a space is
// split into two. The matchers in this package compare flags and their values
// (ArgvContainsSequence, ArgvHasFlagValue), and the identifiers they match on —
// session names, targets — do not contain spaces. A mis-split argv fails the
// match, and failing the match is the safe direction for every caller, which is
// what made this tolerable as the only portable option. procCmdline reads argv
// exactly on darwin through kern.procargs2, which needs no cgo.
//
// -ww asks ps for full width, since a truncated argv fails the match on BSD ps.
func psCmdline(pid int) ([]string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), psCmdlineTimeout)
	defer cancel()

	out, err := exec.CommandContext(ctx, "ps", "-ww", "-o", "args=", "-p", strconv.Itoa(pid)).Output()
	if err != nil {
		return nil, fmt.Errorf("reading argv for pid %d via ps: %w", pid, err)
	}
	fields := strings.Fields(string(out))
	if len(fields) == 0 {
		return nil, fmt.Errorf("no argv reported for pid %d", pid)
	}
	return NormalizeArgv(fields), nil
}

func psReportsZombie(pid int) bool {
	ctx, cancel := context.WithTimeout(context.Background(), psZombieTimeout)
	defer cancel()

	out, err := exec.CommandContext(ctx, "ps", "-o", "stat=", "-p", strconv.Itoa(pid)).Output()
	if err != nil {
		return false
	}
	state := strings.TrimSpace(string(out))
	return strings.HasPrefix(state, "Z")
}

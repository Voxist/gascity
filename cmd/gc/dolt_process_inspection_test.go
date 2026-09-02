package main

import (
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"
)

func TestProcessArgsFromPSReturnsWhenPSHangs(t *testing.T) {
	binDir := t.TempDir()
	psPath := filepath.Join(binDir, "ps")
	if err := os.WriteFile(psPath, []byte("#!/bin/sh\nexec sleep 10\n"), 0o755); err != nil {
		t.Fatalf("WriteFile(ps): %v", err)
	}
	t.Setenv("PATH", strings.Join([]string{binDir, os.Getenv("PATH")}, string(os.PathListSeparator)))

	start := time.Now()
	_, err := processArgsFromPS(os.Getpid(), 100*time.Millisecond)
	if err == nil {
		t.Fatalf("processArgsFromPS succeeded with a hanging ps")
	}
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Fatalf("processArgsFromPS took %s, want bounded timeout", elapsed)
	}
}

func TestFindPortHolderPIDUsesProcBeforeLsof(t *testing.T) {
	if _, err4 := os.Stat("/proc/net/tcp"); err4 != nil {
		if _, err6 := os.Stat("/proc/net/tcp6"); err6 != nil {
			t.Skip("requires Linux /proc TCP tables")
		}
	}

	listener := listenOnRandomPort(t)
	defer func() { _ = listener.Close() }()
	port := listener.Addr().(*net.TCPAddr).Port

	binDir := t.TempDir()
	psPath := filepath.Join(binDir, "lsof")
	if err := os.WriteFile(psPath, []byte("#!/bin/sh\nexec sleep 2\n"), 0o755); err != nil {
		t.Fatalf("WriteFile(lsof): %v", err)
	}
	t.Setenv("PATH", strings.Join([]string{binDir, os.Getenv("PATH")}, string(os.PathListSeparator)))

	start := time.Now()
	pid := findPortHolderPID(strconv.Itoa(port))
	if pid != os.Getpid() {
		t.Fatalf("findPortHolderPID(%d) = %d, want current pid %d", port, pid, os.Getpid())
	}
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Fatalf("findPortHolderPID took %s, want /proc path before lsof", elapsed)
	}
}

func TestPIDsFromPlainPortLsofOutput(t *testing.T) {
	output := fmt.Sprintf(`COMMAND   PID USER   FD   TYPE DEVICE SIZE/OFF NODE NAME
dolt    %d user   12u  IPv4 0x1234      0t0  TCP *:3306 (LISTEN)
`, os.Getpid())
	pids := pidsFromPlainPortLsofOutput(output, "3306")
	if len(pids) != 1 || pids[0] != os.Getpid() {
		t.Fatalf("pidsFromPlainPortLsofOutput() = %v, want [%d]", pids, os.Getpid())
	}
}

func TestProcessCWDFromLsofParsesNameRecord(t *testing.T) {
	binDir := t.TempDir()
	lsofPath := filepath.Join(binDir, "lsof")
	if err := os.WriteFile(lsofPath, []byte("#!/bin/sh\nprintf 'p123\\nfcwd\\nn/private/var/folders/example/.beads/dolt\\n'\n"), 0o755); err != nil {
		t.Fatalf("WriteFile(lsof): %v", err)
	}
	t.Setenv("PATH", strings.Join([]string{binDir, os.Getenv("PATH")}, string(os.PathListSeparator)))

	cwd, ok := processCWDFromLsof(123)
	if !ok {
		t.Fatal("processCWDFromLsof did not find cwd")
	}
	if !samePath(cwd, "/var/folders/example/.beads/dolt") {
		t.Fatalf("processCWDFromLsof = %q, want path equivalent to /var/folders/example/.beads/dolt", cwd)
	}
}

func TestCWDFromPlainLsofOutput(t *testing.T) {
	output := `COMMAND   PID USER   FD   TYPE DEVICE SIZE/OFF NODE NAME
dolt      123 user  cwd    DIR   1,4       96  42 /private/tmp/gc-city/.beads/dolt
`
	cwd, ok := cwdFromPlainLsofOutput(output)
	if !ok {
		t.Fatal("cwdFromPlainLsofOutput did not find cwd")
	}
	if !samePath(cwd, "/tmp/gc-city/.beads/dolt") {
		t.Fatalf("cwdFromPlainLsofOutput = %q, want path equivalent to /tmp/gc-city/.beads/dolt", cwd)
	}
}

func TestCWDFromPlainLsofOutputPreservesSpacesInPath(t *testing.T) {
	output := `COMMAND   PID USER   FD   TYPE DEVICE SIZE/OFF NODE NAME
dolt      123 user  cwd    DIR   1,4       96  42 /tmp/my city/.beads/dolt
`
	cwd, ok := cwdFromPlainLsofOutput(output)
	if !ok {
		t.Fatal("cwdFromPlainLsofOutput did not find cwd")
	}
	if cwd != "/tmp/my city/.beads/dolt" {
		t.Fatalf("cwdFromPlainLsofOutput = %q, want full spaced path", cwd)
	}
}

func TestDeletedDataInodeTargetsFromLsofParsesNameRecords(t *testing.T) {
	binDir := t.TempDir()
	lsofPath := filepath.Join(binDir, "lsof")
	if err := os.WriteFile(lsofPath, []byte("#!/bin/sh\nprintf 'p123\\nn/private/var/folders/example/.beads/dolt/held.db (deleted)\\nn/private/var/folders/example/.beads/dolt/hq/.dolt/noms/LOCK (deleted)\\n'\n"), 0o755); err != nil {
		t.Fatalf("WriteFile(lsof): %v", err)
	}
	t.Setenv("PATH", strings.Join([]string{binDir, os.Getenv("PATH")}, string(os.PathListSeparator)))

	targets := deletedDataInodeTargetsFromLsof(123)
	if len(targets) != 2 {
		t.Fatalf("deletedDataInodeTargetsFromLsof returned %d targets, want 2: %#v", len(targets), targets)
	}
	if !pathWithinOrSame(targets[0], "/var/folders/example/.beads/dolt") {
		t.Fatalf("target %q should be within canonical data dir", targets[0])
	}
	if !benignManagedDeletedInodeTarget(targets[1]) {
		t.Fatalf("target %q should be treated as benign noms lock", targets[1])
	}
}

func TestDeletedDataInodeTargetsFromFormattedLsofIgnoresLiveNameRecords(t *testing.T) {
	targets := deletedDataInodeTargetsFromFormattedLsofOutput("p123\nn/private/tmp/gc-city/.beads/dolt/active.db\n")
	if len(targets) != 0 {
		t.Fatalf("deletedDataInodeTargetsFromFormattedLsofOutput returned live targets: %#v", targets)
	}
}

func TestDeletedDataInodeTargetsFromFormattedLsofUsesZeroLinkCount(t *testing.T) {
	targets := deletedDataInodeTargetsFromFormattedLsofOutput("p123\nf4\nn/private/tmp/gc-city/.beads/dolt/held.db\nk0\n")
	if len(targets) != 1 {
		t.Fatalf("deletedDataInodeTargetsFromFormattedLsofOutput returned %d targets, want 1: %#v", len(targets), targets)
	}
	if !samePath(targets[0], "/tmp/gc-city/.beads/dolt/held.db") {
		t.Fatalf("target = %q, want held.db", targets[0])
	}
}

func TestDeletedDataInodeTargetsFromPlainLsofOutputPreservesSpacesInPath(t *testing.T) {
	output := `COMMAND   PID USER   FD   TYPE DEVICE SIZE/OFF NODE NAME
dolt      123 user  cwd    DIR   1,4       96  42 /tmp/my city/.beads/dolt
dolt      123 user    5u   REG   1,4     4096  99 /tmp/my city/.beads/dolt/held.db (deleted)
`
	targets := deletedDataInodeTargetsFromPlainLsofOutput(output)
	if len(targets) != 1 {
		t.Fatalf("deletedDataInodeTargetsFromPlainLsofOutput returned %d targets, want 1: %#v", len(targets), targets)
	}
	if targets[0] != "/tmp/my city/.beads/dolt/held.db" {
		t.Fatalf("target = %q, want full spaced path", targets[0])
	}
}

// The ga-yeyt3 regression: a port NUMBER can be held by several processes at
// once, because the same number bound to different local addresses (127.0.0.1
// and ::1, or a second interface address) is a distinct bind the kernel
// accepts. The concurrent fast-tier shards produce exactly that collision —
// they all draw ephemeral ports from one small shared range — and the managed
// Dolt ownership checks used to collapse the holder set to one arbitrary
// element and compare it for equality against the PID they cared about. When
// the arbitrary element was the stranger, a live managed Dolt was reported as
// not owning its own port and the caller silently fell back to a stale one.

// TestPIDsFromLsofPIDListKeepsEveryHolder pins the set at the parser, where it
// is deterministic and needs no second process: a two-PID listing must survive
// as two PIDs.
func TestPIDsFromLsofPIDListKeepsEveryHolder(t *testing.T) {
	stranger, mine := os.Getppid(), os.Getpid()
	if stranger <= 0 || stranger == mine {
		t.Skip("need two distinct live PIDs to represent two holders")
	}

	// lsof lists holders in process-table order, so the stranger comes first
	// whenever it started first. That ordering used to decide the verdict.
	pids := pidsFromLsofPIDList(fmt.Sprintf("%d\n%d\n", stranger, mine))
	if len(pids) != 2 || !slices.Contains(pids, mine) {
		t.Fatalf("pidsFromLsofPIDList = %v, want both holders including our pid %d", pids, mine)
	}
}

// TestPIDsFromPlainPortLsofOutputKeepsEveryHolder is the same guarantee for the
// fallback parser, which reads lsof's human listing rather than -t.
func TestPIDsFromPlainPortLsofOutputKeepsEveryHolder(t *testing.T) {
	stranger, mine := os.Getppid(), os.Getpid()
	if stranger <= 0 || stranger == mine {
		t.Skip("need two distinct live PIDs to represent two holders")
	}

	output := fmt.Sprintf(`COMMAND   PID USER   FD   TYPE DEVICE SIZE/OFF NODE NAME
other   %d user   12u  IPv6 0x1234      0t0  TCP [::1]:3306 (LISTEN)
dolt    %d user   13u  IPv4 0x5678      0t0  TCP 127.0.0.1:3306 (LISTEN)
`, stranger, mine)
	pids := pidsFromPlainPortLsofOutput(output, "3306")
	if len(pids) != 2 || !slices.Contains(pids, mine) {
		t.Fatalf("pidsFromPlainPortLsofOutput = %v, want both holders including our pid %d", pids, mine)
	}
}

// TestPortHeldByPIDReportsUnknownForAnUnheldPort pins the third state: no
// holder at all is "unknown", which callers must not read as "someone else
// holds it".
// TestPortHeldByPIDReportsAKnownNegativeForAnUnheldPort pins the split between
// "checked, nobody listens" and "could not check".
//
// An earlier draft of this test asserted unknown here, because the probe
// collapsed both cases into an empty []int. That collapse is not survivable
// once a caller acts on unknown: cmd_doctor_drift treats unknown as blocking
// (a live pid we cannot prove is NOT holding the port must not be repinned
// over), so an unheld port reported as unknown made
// TestDoltDriftCheckTreatsLivePIDWithoutMatchingPortAsStale fail — a live pid
// on a port it does not hold is exactly the stale case, not a blocking one.
//
// An empty listing from a probe that RAN is a real negative. Unknown is
// reserved for a probe that could not answer: no lsof and no /proc, or LISTEN
// rows that exist while no readable /proc/<pid>/fd maps to them (a holder under
// another uid). That last case is the one the doctor's sibling helper reports
// as unknown, and it is not constructible portably here, so it is pinned by the
// drift-check tests rather than by this unit.
func TestPortHeldByPIDReportsAKnownNegativeForAnUnheldPort(t *testing.T) {
	port := reserveRandomTCPPort(t)
	held, known := portHeldByPID(strconv.Itoa(port), os.Getpid())
	if held {
		t.Fatalf("portHeldByPID(%d, self) = held=true on a port nobody is listening on", port)
	}
	if !known {
		t.Fatalf("portHeldByPID(%d, self) = known=false on a port nobody is listening on; a probe that ran and found no holders is a real negative, not an undetermined one — callers that treat unknown as blocking cannot distinguish an unused port from a failed probe", port)
	}
}

// TestPortHeldByPIDReportsUnknownWhenNoProbeCanAnswer pins the other half: with
// no port to ask about there is nothing to determine, and the answer must not
// read as a confident "not held".
func TestPortHeldByPIDReportsUnknownWhenNoProbeCanAnswer(t *testing.T) {
	if held, known := portHeldByPID("", os.Getpid()); held || known {
		t.Fatalf("portHeldByPID(\"\", self) = (held=%v, known=%v), want (false, false)", held, known)
	}
	if held, known := portHeldByPID("1", 0); held || known {
		t.Fatalf("portHeldByPID(\"1\", 0) = (held=%v, known=%v), want (false, false)", held, known)
	}
}

// TestPortHolderPIDsFromLsofTimeoutIsUnknownNotEmpty pins the difference
// between "lsof found nobody" and "lsof was killed at its deadline". Both used
// to come back as (nil, true) — a checked-empty listing — so a starved probe
// read as a confident "nobody holds this port", which for the drift check means
// a live rig-local Dolt can be repinned over. A deadline hit must be UNKNOWN.
//
// Driven through the timeout seam with a deadline no lsof can meet, against a
// port we actually hold, so a false "known: not held" cannot hide behind a
// genuinely empty port.
func TestPortHolderPIDsFromLsofTimeoutIsUnknownNotEmpty(t *testing.T) {
	if _, err := exec.LookPath("lsof"); err != nil {
		t.Skip("lsof not installed")
	}
	port := reserveRandomTCPPort(t)
	ln, err := listenOnAddr(net.JoinHostPort("127.0.0.1", strconv.Itoa(port)))
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer ln.Close() //nolint:errcheck // test cleanup

	pids, known := portHolderPIDsFromLsofWithTimeout(time.Nanosecond, strconv.Itoa(port))
	if known {
		t.Fatalf("portHolderPIDsFromLsofWithTimeout(1ns) = (pids=%v, known=true); a deadline hit is not a listing and must not read as a confident answer", pids)
	}
	if len(pids) != 0 {
		t.Fatalf("portHolderPIDsFromLsofWithTimeout(1ns) returned pids %v alongside unknown", pids)
	}

	// And with a real deadline the same probe reports us as the holder, so the
	// unknown above is the timeout, not the probe being broken.
	pids, known = portHolderPIDsFromLsofWithTimeout(lsofCommandTimeout, strconv.Itoa(port))
	if !known || !slices.Contains(pids, os.Getpid()) {
		t.Fatalf("portHolderPIDsFromLsofWithTimeout(real) = (pids=%v, known=%v), want self among holders with known=true", pids, known)
	}
}

// TestPortHolderPIDsReportsEveryHolderOnTheRealProbe is the end-to-end shape
// against the actual lsof/proc probe: a separate process holds the port number
// on 127.0.0.1 while we hold the same number on ::1. Both must be reported, and
// we must be recognized as a holder of our own port even though a stranger also
// holds it.
func TestPortHolderPIDsReportsEveryHolderOnTheRealProbe(t *testing.T) {
	port := reserveRandomTCPPort(t)
	ours, err := listenOnAddr(net.JoinHostPort("::1", strconv.Itoa(port)))
	if err != nil {
		t.Skipf("cannot bind [::1]:%d (no IPv6 loopback?): %v", port, err)
	}
	defer ours.Close() //nolint:errcheck // test cleanup

	// startTCPListenerProcessInDir binds 127.0.0.1 on the same number; it also
	// carries the slow-process skip, so this runs in the cmd-gc-process tier.
	stranger := startTCPListenerProcessInDir(t, port, t.TempDir())

	// Assert on the whole set, not just our membership: which element an
	// equality check would have picked depends on process-table order, so only
	// "both holders are reported" pins the property independently of ordering.
	deadline := time.NewTimer(5 * time.Second)
	defer deadline.Stop()
	poll := time.NewTicker(50 * time.Millisecond)
	defer poll.Stop()
	for {
		holders := portHolderPIDs(strconv.Itoa(port))
		held, known := portHeldByPID(strconv.Itoa(port), os.Getpid())
		if known && held && slices.Contains(holders, stranger.Process.Pid) {
			return
		}
		select {
		case <-poll.C:
		case <-deadline.C:
			t.Fatalf("portHolderPIDs(%d) = %v, want both our pid %d and the stranger %d; portHeldByPID = (held=%v, known=%v)",
				port, holders, os.Getpid(), stranger.Process.Pid, held, known)
		}
	}
}

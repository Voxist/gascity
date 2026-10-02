package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/doctor"
	"github.com/gastownhall/gascity/internal/pidutil"
)

// installWatchdogLivenessCity builds a city whose managed-Dolt layout lives
// under a tmpdir, mirroring installListenerDeadlineCity's env-override
// convention so this check is exercised the same way production resolves
// the layout.
func installWatchdogLivenessCity(t *testing.T) (string, managedDoltRuntimeLayout) {
	t.Helper()
	cityPath := t.TempDir()
	packStateDir := filepath.Join(cityPath, "pack-state")
	if err := os.MkdirAll(packStateDir, 0o755); err != nil {
		t.Fatalf("mkdir pack state dir: %v", err)
	}
	t.Setenv("GC_PACK_STATE_DIR", packStateDir)
	layout, err := resolveManagedDoltRuntimeLayout(cityPath)
	if err != nil {
		t.Fatalf("resolve layout: %v", err)
	}
	// Every test below stands in a plain `sleep`/shell process for the
	// managed dolt server (there is no real dolt binary in a unit test), so
	// the real argv-matching pidLooksLikeDoltSQLServer would reject every
	// one of them as "PID reuse". Stub it here; the one test that exercises
	// the real function (TestPidLooksLikeDoltSQLServerRejectsAnUnrelatedProcess)
	// restores it first.
	prevLooksLikeDolt := pidLooksLikeDoltSQLServer
	t.Cleanup(func() { pidLooksLikeDoltSQLServer = prevLooksLikeDolt })
	pidLooksLikeDoltSQLServer = func(int) bool { return true }
	return cityPath, layout
}

// writeWatchdogLivenessState records a running managed-Dolt server at pid,
// with watchdog recorded exactly as dolt_start_managed.go would at spawn
// time (started.WatchdogPID > 0) — every test here cares about a RUNNING
// recording (the not-running case is covered directly in
// TestDoltWatchdogLivenessCheckIsQuietWithNoRuntimeState, which writes no
// state at all).
func writeWatchdogLivenessState(t *testing.T, layout managedDoltRuntimeLayout, pid int, watchdog bool) {
	t.Helper()
	data, err := json.Marshal(doltRuntimeState{Running: true, PID: pid, Port: 48770, DataDir: layout.DataDir, Watchdog: watchdog})
	if err != nil {
		t.Fatalf("marshal runtime state: %v", err)
	}
	if err := os.MkdirAll(filepath.Dir(layout.StateFile), 0o755); err != nil {
		t.Fatalf("mkdir state dir: %v", err)
	}
	if err := os.WriteFile(layout.StateFile, data, 0o644); err != nil {
		t.Fatalf("write runtime state: %v", err)
	}
}

func runWatchdogLivenessCheck(t *testing.T, cityPath string) *doctor.CheckResult {
	t.Helper()
	return newDoltWatchdogLivenessCheck(cityPath, &config.City{}).Run(nil)
}

// spawnOrphanedChild starts a long-lived process under an intermediate
// "watchdog" shell, kills only the shell, and returns the child's PID once
// it has been reparented — exactly the shape ga-3bwmf's leading hypothesis
// describes: a watchdog that dies right after handing off its child leaves
// that child orphaned to ppid 1 (or the platform's subreaper), alive, with
// no supervisor of its own.
func spawnOrphanedChild(t *testing.T) int {
	t.Helper()
	watchdog := exec.Command("sh", "-c", "sleep 60 & echo $!; wait")
	stdout, err := watchdog.StdoutPipe()
	if err != nil {
		t.Fatalf("stdout pipe: %v", err)
	}
	if err := watchdog.Start(); err != nil {
		t.Fatalf("start watchdog shell: %v", err)
	}
	reader := bufio.NewReader(stdout)
	line, err := reader.ReadString('\n')
	if err != nil {
		_ = watchdog.Process.Kill()
		t.Fatalf("read child pid from watchdog: %v", err)
	}
	childPID, err := strconv.Atoi(strings.TrimSpace(line))
	if err != nil {
		_ = watchdog.Process.Kill()
		t.Fatalf("parse child pid %q: %v", line, err)
	}
	t.Cleanup(func() {
		_ = syscall.Kill(childPID, syscall.SIGKILL)
	})
	// Killing the shell (not the process group) leaves the backgrounded
	// sleep running, now parentless until init/launchd adopts it.
	if err := watchdog.Process.Kill(); err != nil {
		t.Fatalf("kill watchdog shell: %v", err)
	}
	_ = watchdog.Wait()

	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if ppid, err := pidutil.ParentPIDOf(childPID); err == nil {
			subreaperPID := pidutil.DetectUserSubreaperPID(os.Getpid())
			if pidutil.IsReparentedOrphan(ppid, subreaperPID) {
				return childPID
			}
		}
		time.Sleep(20 * time.Millisecond)
	}
	// This host's reparenting model adopted the child under neither pid 1
	// nor a detected systemd --user subreaper — some other live process (a
	// non-systemd session manager, a container init, etc.) — which this
	// test's own reparented-orphan test cannot reason about either. Skip
	// rather than fail: it says nothing about the check under test, only
	// about this host's process topology.
	t.Skipf("pid %d was never observed reparented (host subreaper model not recognized by pidutil.IsReparentedOrphan)", childPID)
	return 0
}

func TestDoltWatchdogLivenessCheckFlagsAnOrphanedServer(t *testing.T) {
	cityPath, layout := installWatchdogLivenessCity(t)
	childPID := spawnOrphanedChild(t)
	writeWatchdogLivenessState(t, layout, childPID, true)

	got := runWatchdogLivenessCheck(t, cityPath)
	if got.Status != doctor.StatusError {
		t.Fatalf("status = %v, want error (result %+v)", got.Status, got)
	}
	if !strings.Contains(got.Message, fmt.Sprintf("%d", childPID)) {
		t.Errorf("message %q does not name the orphaned pid %d", got.Message, childPID)
	}
	if !strings.Contains(strings.Join(got.Details, " "), "ppid=") {
		t.Errorf("details %v do not name the observed ppid", got.Details)
	}
}

func TestDoltWatchdogLivenessCheckPassesForASupervisedServer(t *testing.T) {
	cityPath, layout := installWatchdogLivenessCity(t)
	// The test binary itself is a live, non-reparented parent of this child —
	// standing in for a live scope watchdog.
	cmd := exec.Command("sleep", "5")
	if err := cmd.Start(); err != nil {
		t.Fatalf("start sleep: %v", err)
	}
	t.Cleanup(func() {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
	})
	writeWatchdogLivenessState(t, layout, cmd.Process.Pid, true)

	got := runWatchdogLivenessCheck(t, cityPath)
	if got.Status != doctor.StatusOK {
		t.Fatalf("status = %v, want ok (result %+v)", got.Status, got)
	}
}

func TestDoltWatchdogLivenessCheckIsQuietWithoutManagedDolt(t *testing.T) {
	cityPath, _ := installWatchdogLivenessCity(t)
	t.Setenv("GC_BEADS", "exec:/bin/true")
	got := runWatchdogLivenessCheck(t, cityPath)
	if got.Status != doctor.StatusOK {
		t.Fatalf("status = %v, want ok (result %+v)", got.Status, got)
	}
	if !strings.Contains(got.Message, "not using managed Dolt topology") {
		t.Errorf("message = %q, want the not-applicable record", got.Message)
	}
}

func TestDoltWatchdogLivenessCheckIsQuietWithNoRuntimeState(t *testing.T) {
	cityPath, _ := installWatchdogLivenessCity(t)
	got := runWatchdogLivenessCheck(t, cityPath)
	if got.Status != doctor.StatusOK {
		t.Fatalf("status = %v, want ok (result %+v)", got.Status, got)
	}
}

func TestDoltWatchdogLivenessCheckIsQuietWhenRecordedPIDIsDead(t *testing.T) {
	cityPath, layout := installWatchdogLivenessCity(t)
	// A recently-reaped PID: dead-PID classification is a different check's
	// lane (dolt-topology/dolt-drift).
	writeWatchdogLivenessState(t, layout, deadPID(t), true)

	got := runWatchdogLivenessCheck(t, cityPath)
	if got.Status != doctor.StatusOK {
		t.Fatalf("status = %v, want ok (result %+v)", got.Status, got)
	}
}

// TestDoltWatchdogLivenessCheckIsQuietWhenWatchdogDisabled covers the
// direct-spawn case (GC_DOLT_SCOPE_WATCHDOG=0 AT START TIME): the check
// reads Watchdog=false from runtime state — the durable fact recorded when
// this server started, not a re-read of the current env, which could have
// changed since (ga-3bwmf review) — and declines to judge an orphaned PID
// that was never supposed to have a watchdog in the first place.
func TestDoltWatchdogLivenessCheckIsQuietWhenWatchdogDisabled(t *testing.T) {
	cityPath, layout := installWatchdogLivenessCity(t)
	childPID := spawnOrphanedChild(t)
	writeWatchdogLivenessState(t, layout, childPID, false)

	got := runWatchdogLivenessCheck(t, cityPath)
	if got.Status != doctor.StatusOK {
		t.Fatalf("status = %v, want ok when the recorded state says this server was started without a watchdog (result %+v)", got.Status, got)
	}
}

// TestDoltWatchdogLivenessCheckIgnoresCurrentEnvAtCheckTime pins the exact
// review finding: GC_DOLT_SCOPE_WATCHDOG at the moment `gc doctor` runs must
// have NO effect on the verdict for an already-recorded server. A server
// recorded as watchdog=true but now orphaned is still a genuine finding even
// if the operator has since flipped the env off (that env change affects
// only FUTURE starts).
func TestDoltWatchdogLivenessCheckIgnoresCurrentEnvAtCheckTime(t *testing.T) {
	cityPath, layout := installWatchdogLivenessCity(t)
	t.Setenv("GC_DOLT_SCOPE_WATCHDOG", "0")
	childPID := spawnOrphanedChild(t)
	writeWatchdogLivenessState(t, layout, childPID, true)

	got := runWatchdogLivenessCheck(t, cityPath)
	if got.Status != doctor.StatusError {
		t.Fatalf("status = %v, want error: a watchdog=true recording must still be judged regardless of the CURRENT env (result %+v)", got.Status, got)
	}
}

// TestPidLooksLikeDoltSQLServerRejectsAnUnrelatedProcess covers the LOW
// review item: a recorded PID that has been reused by an unrelated process
// must not be judged for supervision at all. This exercises the REAL
// default implementation directly (every other test in this file stubs it,
// standing in a plain `sleep` for lack of a real dolt binary).
func TestPidLooksLikeDoltSQLServerRejectsAnUnrelatedProcess(t *testing.T) {
	cmd := exec.Command("sleep", "5")
	if err := cmd.Start(); err != nil {
		t.Fatalf("start sleep: %v", err)
	}
	t.Cleanup(func() {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
	})
	// A load-timing race, not a flaky assertion: this failed on CI (shard
	// 7, run 36657665083) with pidLooksLikeDoltSQLServer(sleep pid) = true.
	// The exact mechanism by which a read shortly after Start() can still
	// come back unable to identify the child's real argv is not settled --
	// review round 3 (L4) found the original comment here overclaimed an
	// "empty /proc read" explanation Linux's fork/exec semantics don't
	// obviously support -- but the observed failure and
	// pidLooksLikeDoltSQLServer's own documented "failed or unreadable is
	// not a mismatch" fallback (which answers true whenever it cannot
	// positively identify argv) together are enough to fix regardless of
	// the precise cause: wait until the child's own cmdline actually reads
	// back "sleep" -- proving its real argv is observable -- before
	// asserting on it.
	pid := cmd.Process.Pid
	awaitCond(t, func() bool {
		argv, err := pidutil.Cmdline(pid)
		return err == nil && len(argv) > 0 && filepath.Base(argv[0]) == "sleep"
	}, "child process's cmdline to read back \"sleep\" (post-exec)")
	if pidLooksLikeDoltSQLServer(pid) {
		t.Fatalf("pidLooksLikeDoltSQLServer(sleep pid) = true, want false (argv is not \"dolt sql-server\")")
	}
	if !pidLooksLikeDoltSQLServer(0) {
		t.Error("pidLooksLikeDoltSQLServer(0) = false, want true: a failed Cmdline read must not manufacture a mismatch")
	}
}

// runWriteProviderAdoption invokes the real `gc dolt-state write-provider`
// command against layout.StateFile for pid — exactly what gc-beads-bd.sh's
// kickstart-adoption path runs (write-provider has no --watchdog flag, so
// every such call carries the zero value) — and returns the state that
// ends up on disk.
func runWriteProviderAdoption(t *testing.T, layout managedDoltRuntimeLayout, pid int) doltRuntimeState {
	t.Helper()
	var stdout, stderr bytes.Buffer
	cmd := newDoltStateCmd(&stdout, &stderr)
	cmd.SetOut(&stdout)
	cmd.SetErr(&stderr)
	cmd.SetArgs([]string{
		"write-provider",
		"--file", layout.StateFile,
		"--pid", strconv.Itoa(pid),
		"--running", "true",
		"--port", "48770",
		"--data-dir", layout.DataDir,
	})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("gc dolt-state write-provider: %v (stderr: %s)", err, stderr.String())
	}
	state, err := readDoltRuntimeStateFile(layout.StateFile)
	if err != nil {
		t.Fatalf("read rewritten state: %v", err)
	}
	return state
}

// TestDoltWatchdogLivenessSurvivesWriteProviderAdoptionWithSamePID pins the
// MEDIUM review item: an adoption rewrite for the SAME already-running,
// already-supervised pid must not silently clear the durable Watchdog fact
// this check depends on (preservedDoltWatchdog, dolt_runtime_publication.go).
// Before that fix, `gc dolt-state write-provider` — which has no --watchdog
// flag — made this check go blind (report ok, "nothing to check") for an
// orphaned, watchdog=true server exactly after a kickstart adopted it: the
// scenario ga-3bwmf's own doc comment describes.
func TestDoltWatchdogLivenessSurvivesWriteProviderAdoptionWithSamePID(t *testing.T) {
	cityPath, layout := installWatchdogLivenessCity(t)
	childPID := spawnOrphanedChild(t)
	writeWatchdogLivenessState(t, layout, childPID, true)

	rewritten := runWriteProviderAdoption(t, layout, childPID)
	if !rewritten.Watchdog {
		t.Fatal("write-provider adoption rewrite for the SAME pid cleared Watchdog; it must be preserved")
	}

	got := runWatchdogLivenessCheck(t, cityPath)
	if got.Status != doctor.StatusError {
		t.Fatalf("status after adoption rewrite = %v, want error (the check must not go blind): result %+v", got.Status, got)
	}
}

// TestDoltWatchdogLivenessWriteProviderClearsWatchdogOnPIDChange pins the
// mirror-image case: a write-provider rewrite for a DIFFERENT pid (a genuine
// restart, adopted without yet knowing whether the new process has a
// watchdog) must NOT inherit the old pid's Watchdog=true. That fact is fixed
// at spawn time (dolt_start_managed.go) and does not carry across pids —
// dolt_port_selection.go's repair path reassigning PID is the other place
// this same "clear on PID change" behavior matters.
func TestDoltWatchdogLivenessWriteProviderClearsWatchdogOnPIDChange(t *testing.T) {
	_, layout := installWatchdogLivenessCity(t)
	const oldPID = 999999
	writeWatchdogLivenessState(t, layout, oldPID, true)

	newPID := os.Getpid()
	if newPID == oldPID {
		t.Fatal("test setup collision: this process's own pid equals oldPID")
	}
	rewritten := runWriteProviderAdoption(t, layout, newPID)
	if rewritten.Watchdog {
		t.Fatal("write-provider rewrite for a DIFFERENT pid inherited the old pid's Watchdog=true; it must be cleared")
	}
}

func TestDoltWatchdogLivenessCheckMetadata(t *testing.T) {
	c := newDoltWatchdogLivenessCheck("/city", nil)
	if c.Name() != "dolt-watchdog-liveness" {
		t.Errorf("name = %q", c.Name())
	}
	if c.CanFix() {
		t.Error("CanFix must be false: repair needs an operator-supervised Dolt restart")
	}
	if c.WarmupEligible() {
		t.Error("WarmupEligible must be false: the check shells out to inspect a PID's parent")
	}
	if err := c.Fix(nil); err != nil {
		t.Errorf("Fix = %v, want nil", err)
	}
}

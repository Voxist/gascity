package main

import (
	"bufio"
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
	return cityPath, layout
}

// writeWatchdogLivenessState records a running managed-Dolt server at pid —
// every test here cares about a RUNNING recording (the not-running case is
// covered directly in TestDoltWatchdogLivenessCheckIsQuietWithNoRuntimeState,
// which writes no state at all).
func writeWatchdogLivenessState(t *testing.T, layout managedDoltRuntimeLayout, pid int) {
	t.Helper()
	data, err := json.Marshal(doltRuntimeState{Running: true, PID: pid, Port: 48770, DataDir: layout.DataDir})
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
	t.Fatalf("pid %d was never observed reparented after its watchdog shell died", childPID)
	return 0
}

func TestDoltWatchdogLivenessCheckFlagsAnOrphanedServer(t *testing.T) {
	cityPath, layout := installWatchdogLivenessCity(t)
	childPID := spawnOrphanedChild(t)
	writeWatchdogLivenessState(t, layout, childPID)

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
	writeWatchdogLivenessState(t, layout, cmd.Process.Pid)

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
	writeWatchdogLivenessState(t, layout, deadPID(t))

	got := runWatchdogLivenessCheck(t, cityPath)
	if got.Status != doctor.StatusOK {
		t.Fatalf("status = %v, want ok (result %+v)", got.Status, got)
	}
}

func TestDoltWatchdogLivenessCheckIsQuietWhenWatchdogDisabled(t *testing.T) {
	cityPath, layout := installWatchdogLivenessCity(t)
	t.Setenv("GC_DOLT_SCOPE_WATCHDOG", "0")
	childPID := spawnOrphanedChild(t)
	writeWatchdogLivenessState(t, layout, childPID)

	got := runWatchdogLivenessCheck(t, cityPath)
	if got.Status != doctor.StatusOK {
		t.Fatalf("status = %v, want ok when the watchdog is deliberately disabled (result %+v)", got.Status, got)
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

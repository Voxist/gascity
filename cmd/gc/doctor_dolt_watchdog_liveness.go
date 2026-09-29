package main

import (
	"encoding/json"
	"fmt"
	"os"

	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/doctor"
	"github.com/gastownhall/gascity/internal/pidutil"
)

// doltWatchdogLivenessCheck asserts that the managed Dolt server gc's own
// runtime state says is running is still supervised by a live scope
// watchdog (dolt_scope_watchdog.go).
//
// ga-3bwmf, live on voxist-city 2026-09-29 (ga-xuapz). A clean `launchctl
// kickstart` of the supervisor produced a managed dolt (pid 45560) with no
// scope watchdog attached at all: dolt.log never got a "supervising dolt
// sql-server pid 45560" line, and `ps` showed ppid=1. The scope watchdog is
// a supervisor-of-one by design (dolt_scope_watchdog.go's
// runManagedDoltScopeWatchdog returns, does not restart, when its child
// exits or is signaled) — so once it is gone for any reason, the server it
// leaves behind is silently back in the exact "orphaned to ppid 1, runs
// forever, nothing reaps it when the scope is deleted" state the watchdog
// exists to prevent (that file's own header: 314 such orphans accumulated
// pre-watchdog, ~44GB RSS). Nothing else in gc notices, because every other
// health signal this project has — TCP reachability, a query probe, the
// recorded PID being alive — is satisfied by an orphaned-but-otherwise-fine
// server. This check adds the one signal that catches it: whether the
// recorded PID's OS parent is still the watchdog process, using the same
// reparented-orphan test drain_ack_escalation.go already relies on for a
// materially identical question (was THIS process's live parent replaced by
// a subreaper).
//
// The exact mechanism by which the watchdog goes missing without a log
// trace is not settled (see ga-3bwmf's notes) — a race in the handshake
// window is the leading hypothesis. This check does not try to diagnose or
// repair that race; it only makes the resulting unsupervised state visible
// instead of silent, the way dolt-listener-deadline makes a config leak
// visible instead of silent.
type doltWatchdogLivenessCheck struct {
	cityPath string
	cfg      *config.City
}

func newDoltWatchdogLivenessCheck(cityPath string, cfg *config.City) *doltWatchdogLivenessCheck {
	return &doltWatchdogLivenessCheck{cityPath: cityPath, cfg: cfg}
}

// Name implements doctor.Check.
func (*doltWatchdogLivenessCheck) Name() string { return "dolt-watchdog-liveness" }

// CanFix implements doctor.Check. Re-attaching supervision to an already
// running, unsupervised server means either killing and restarting it (a
// live work-ledger restart) or inventing a way to adopt an existing PID
// under a fresh watchdog that does not exist today — both operator-
// supervised actions, not something `gc doctor --fix` may do behind the
// operator's back. See dolt-listener-deadline's identical reasoning.
func (*doltWatchdogLivenessCheck) CanFix() bool { return false }

// Fix implements doctor.Check.
func (*doltWatchdogLivenessCheck) Fix(_ *doctor.CheckContext) error { return nil }

// WarmupEligible implements doctor.Check. It shells out to inspect a PID's
// parent (a ps fork on darwin when the kernel-record reader is unavailable),
// so it stays out of `gc start` warm-up like dolt-listener-deadline.
func (*doltWatchdogLivenessCheck) WarmupEligible() bool { return false }

// Run implements doctor.Check.
func (c *doltWatchdogLivenessCheck) Run(_ *doctor.CheckContext) *doctor.CheckResult {
	name := c.Name()
	if c.cfg == nil || !workspaceUsesManagedBdStoreContract(c.cityPath, c.cfg.Rigs) {
		return okCheck(name, "not using managed Dolt topology")
	}
	// Deliberately NOT managedDoltScopeWatchdogEnabled(): that function folds
	// in "are we running inside a go test binary" (managedDoltTestModeEnabled),
	// which exists to keep unit tests that spawn managed-dolt fixtures from
	// interposing a real watchdog — a concern about what a FRESH start does,
	// not about whether an ALREADY-RUNNING recorded PID is supervised, which
	// is what this check reads. Consulting only the raw env var is both the
	// semantically correct question here and what makes this check's own
	// logic exercisable under `go test` at all.
	if !managedDoltScopeWatchdogEnabledFor(false, os.Getenv(managedDoltScopeWatchdogEnv)) {
		// An operator who opted out of the watchdog (GC_DOLT_SCOPE_WATCHDOG=0)
		// gets a directly-spawned server with no watchdog BY DESIGN — ppid 1
		// there is expected, not a finding.
		return okCheck(name, "scope watchdog disabled (GC_DOLT_SCOPE_WATCHDOG=0); nothing to check")
	}
	layout, err := resolveManagedDoltRuntimeLayout(c.cityPath)
	if err != nil {
		return okCheck(name, "no managed Dolt runtime layout for this city")
	}
	data, err := os.ReadFile(layout.StateFile)
	if err != nil {
		return okCheck(name, "no managed Dolt runtime state recorded yet")
	}
	var state doltRuntimeState
	if err := json.Unmarshal(data, &state); err != nil {
		return warnCheck(name,
			fmt.Sprintf("managed Dolt runtime state is malformed (%s)", layout.StateFile),
			"delete the stale state file and restart: `gc dolt stop && gc dolt start`",
			[]string{err.Error()})
	}
	if !state.Running || state.PID <= 0 {
		return okCheck(name, "no managed Dolt server recorded as running")
	}
	if !pidAlive(state.PID) {
		// A dead recorded PID is a different finding (dolt-topology/dolt-drift's
		// lane); this check answers only "is a LIVE recorded server supervised".
		return okCheck(name, fmt.Sprintf("recorded pid %d is not alive", state.PID))
	}

	ppid, err := pidutil.ParentPIDOf(state.PID)
	if err != nil {
		return warnCheck(name,
			fmt.Sprintf("could not determine the parent of managed Dolt pid %d", state.PID),
			"retry `gc doctor`; a persistent failure may mean this host lacks /proc, sysctl process records, and a usable ps",
			[]string{err.Error()})
	}
	subreaperPID := pidutil.DetectUserSubreaperPID(os.Getpid())
	if pidutil.IsReparentedOrphan(ppid, subreaperPID) {
		return errorCheck(name,
			fmt.Sprintf("managed Dolt pid %d has no live scope watchdog (reparented, ppid %d)", state.PID, ppid),
			"the server is running unsupervised and will not be reaped even after its scope is deleted; "+
				"restart it under gc's own supervision: `gc dolt stop && gc dolt start` (ga-3bwmf)",
			[]string{fmt.Sprintf("pid=%d ppid=%d detected_subreaper=%d", state.PID, ppid, subreaperPID)})
	}
	return okCheck(name, fmt.Sprintf("managed Dolt pid %d is supervised (parent pid %d)", state.PID, ppid))
}

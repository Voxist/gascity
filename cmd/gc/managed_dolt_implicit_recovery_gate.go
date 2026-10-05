package main

import (
	"fmt"
	"log"
	"sync"
	"sync/atomic"

	"github.com/gastownhall/gascity/internal/beads"
)

// This file is the one gate on IMPLICIT managed-Dolt recovery: the starts that
// an ordinary store-touching command (gc hook, gc ready, gc mail, a launchd
// helper calling gc or bd) triggers as a side effect of resolving its bd
// environment. Such a command has no lifecycle intent, so it must not revive a
// managed Dolt that an operator deliberately stopped (ga-lc97s, vp-zyx9q).
//
// Recovery is permitted when ANY of these holds:
//   - the process declared lifecycle intent (gc start, init, rig add, gc dolt
//     start/recover, gc beads health, and the long-lived supervisor and
//     controller, which drive infrastructure by definition);
//   - the city's controller answers a ping, so the city is running or starting
//     and self-healing from the CLI is the intended behavior.
//
// It is denied when the scope is under a MIGRATION-FREEZE regardless.
//
// Liveness is queried live (controllerAlive); no status file is written.
// Lifecycle intent is a process-level flag, not a context value, because the
// bd runner's recovery closure (recoverManagedBDCommand) runs under
// context.Background() and has no caller context to inherit one from.

var lifecycleIntent atomic.Bool

// declareLifecycleIntent marks this process as one whose job includes starting
// the city's managed Dolt. Call it only from lifecycle entry points.
func declareLifecycleIntent() { lifecycleIntent.Store(true) }

// resetLifecycleIntentForTest restores the flag to its prior value when the
// test ends and clears it for the test's duration.
func resetLifecycleIntentForTest(t interface{ Cleanup(func()) }) {
	prior := lifecycleIntent.Swap(false)
	t.Cleanup(func() { lifecycleIntent.Store(prior) })
}

// Seams for the live-state queries.
var (
	implicitRecoveryControllerAlive = controllerAlive
	implicitRecoveryMigrationFrozen = beads.MigrationFrozen
	implicitRecoveryRegistered      = func(cityPath string) bool {
		_, ok, err := registeredCityEntry(cityPath)
		return err == nil && ok
	}
)

// implicitRecoveryDeniedError is returned when an implicit recovery is refused.
type implicitRecoveryDeniedError struct {
	cityPath string
	reason   string
	next     string
}

func (e *implicitRecoveryDeniedError) Error() string {
	return fmt.Sprintf("city %q: %s; managed Dolt was not started: %s", e.cityPath, e.reason, e.next)
}

// implicitManagedDoltRecoveryCheck returns nil when an implicit managed-Dolt
// recovery may proceed, otherwise an *implicitRecoveryDeniedError. A denial is
// logged once per process per city and reason.
func implicitManagedDoltRecoveryCheck(cityPath string) error {
	if implicitRecoveryMigrationFrozen(cityPath) {
		return denyImplicitRecovery(cityPath, "a MIGRATION-FREEZE is active",
			"clear the freeze marker once the migration window closes")
	}
	if lifecycleIntent.Load() || implicitRecoveryControllerAlive(cityPath) != 0 {
		return nil
	}
	next := fmt.Sprintf("run gc register %s && gc start", cityPath)
	if implicitRecoveryRegistered(cityPath) {
		next = "run gc start"
	}
	return denyImplicitRecovery(cityPath, "city is stopped", next)
}

var implicitRecoveryDenialLogged sync.Map

func denyImplicitRecovery(cityPath, reason, next string) error {
	if _, seen := implicitRecoveryDenialLogged.LoadOrStore(cityPath+"\x00"+reason, struct{}{}); !seen {
		logImplicitRecoveryDenial(cityPath, reason, next)
	}
	return &implicitRecoveryDeniedError{cityPath: cityPath, reason: reason, next: next}
}

// logImplicitRecoveryDenial is a seam so tests can count denial records.
var logImplicitRecoveryDenial = func(cityPath, reason, next string) {
	log.Printf("gc: INFO: refusing implicit managed dolt recovery for city %q: %s; %s", cityPath, reason, next)
}

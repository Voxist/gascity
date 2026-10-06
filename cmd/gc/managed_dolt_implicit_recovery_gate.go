package main

import (
	"errors"
	"fmt"
	"log"
	"os"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

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
//   - the city's controller is not confirmed stopped. Liveness is three-valued:
//     a ping that is answered, or a socket that accepts but answers slowly, or
//     a probe that fails for any reason other than "nothing is listening", all
//     count as running or unknown and are allowed. Only a refused connection
//     or a missing socket is a confirmed stop. A host under starvation must not
//     read as a stopped city;
//   - the city is registered with a live supervisor, which covers the window
//     between registration and the controller binding its socket.
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

// controllerLiveness is the three-valued answer to "is this city's controller
// running?".
type controllerLiveness int

const (
	// controllerLivenessUnknown: the probe could not tell. Treated as allow.
	controllerLivenessUnknown controllerLiveness = iota
	// controllerLivenessRunning: the controller answered.
	controllerLivenessRunning
	// controllerLivenessStopped: nothing is listening on the controller socket.
	controllerLivenessStopped
)

// probeControllerLiveness pings the city's controller socket and classifies
// the result. Only ECONNREFUSED (a stale socket file) and a missing socket
// confirm a stop; a timeout or any other failure is unknown.
func probeControllerLiveness(cityPath string) controllerLiveness {
	_, err := sendControllerCommandWithTimeouts(cityPath, "ping", 500*time.Millisecond, 500*time.Millisecond, 2*time.Second)
	return classifyControllerPingError(err)
}

// classifyControllerPingError maps a ping outcome to a liveness verdict.
func classifyControllerPingError(err error) controllerLiveness {
	switch {
	case err == nil:
		return controllerLivenessRunning
	case errors.Is(err, syscall.ECONNREFUSED), errors.Is(err, os.ErrNotExist):
		return controllerLivenessStopped
	default:
		return controllerLivenessUnknown
	}
}

// Seams for the live-state queries.
var (
	implicitRecoveryControllerLiveness = probeControllerLiveness
	implicitRecoveryMigrationFrozen    = beads.MigrationFrozen
	implicitRecoverySupervisorAlive    = func() bool { return supervisorAliveHook() != 0 }
	implicitRecoveryRegistered         = func(cityPath string) bool {
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
	if lifecycleIntent.Load() || implicitRecoveryControllerLiveness(cityPath) != controllerLivenessStopped {
		return nil
	}
	registered := implicitRecoveryRegistered(cityPath)
	if registered && implicitRecoverySupervisorAlive() {
		return nil
	}
	next := fmt.Sprintf("run gc register %s && gc start", cityPath)
	if registered {
		next = "run gc start"
	}
	return denyImplicitRecovery(cityPath, "city is stopped", next)
}

// recoverManagedBDCommandForScope gates the bd runner's transport-retry recover
// on the freeze state of the scope the command ran in, which may be a rig
// outside the city tree, before the city-level gate in recoverManagedBDCommand.
func recoverManagedBDCommandForScope(cityPath, scopeRoot string) error {
	if scopeRoot != "" && implicitRecoveryMigrationFrozen(scopeRoot) {
		return fmt.Errorf("%w: %w", errManagedDoltRecoverDeclined,
			denyImplicitRecovery(scopeRoot, "a MIGRATION-FREEZE is active", "clear the freeze marker once the migration window closes"))
	}
	return recoverManagedBDCommand(cityPath)
}

// implicitRecoveryDenialLogInterval bounds how often one (scope, reason) pair is
// logged, so a long-lived process still records a later freeze or stop.
const implicitRecoveryDenialLogInterval = 10 * time.Minute

var (
	implicitRecoveryDenialMu     sync.Mutex
	implicitRecoveryDenialLogged = map[string]time.Time{}
	implicitRecoveryNow          = time.Now
)

func denyImplicitRecovery(cityPath, reason, next string) error {
	key := cityPath + "\x00" + reason
	now := implicitRecoveryNow()
	implicitRecoveryDenialMu.Lock()
	last, seen := implicitRecoveryDenialLogged[key]
	shouldLog := !seen || now.Sub(last) >= implicitRecoveryDenialLogInterval
	if shouldLog {
		implicitRecoveryDenialLogged[key] = now
	}
	implicitRecoveryDenialMu.Unlock()
	if shouldLog {
		logImplicitRecoveryDenial(cityPath, reason, next)
	}
	return &implicitRecoveryDeniedError{cityPath: cityPath, reason: reason, next: next}
}

// logImplicitRecoveryDenial is a seam so tests can count denial records.
var logImplicitRecoveryDenial = func(cityPath, reason, next string) {
	log.Printf("gc: INFO: refusing implicit managed dolt recovery for city %q: %s; %s", cityPath, reason, next)
}

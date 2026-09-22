package main

import (
	"errors"
	"sync"
)

// Managed-Dolt lifecycle ownership (ga-fjr5f).
//
// The controller owns the managed Dolt server's lifecycle (SDK
// self-sufficiency). Before this, ANY gc process that found the server down
// restarted it implicitly — the bd env resolution's health check and the bd
// runner's transport recovery both run the provider "recover" op — and the
// restart ran in that process's own tree and session. An agent's SessionStart
// hook (`gc prime --hook`) was such a process, and so was a `gc doctor` whose
// checks open a store: the server they started, and the scope watchdog
// supervising it, belonged to the agent session, and the session's teardown
// killed it. The next agent wake restarted it, and the loop repeated.
//
// Implicit recovery is reserved for the processes that have claimed a city's
// lifecycle: the supervisor booting that city, its city runtime, and the
// explicit `gc start` (not `gc start --dry-run`, which any caller may run).
// Every other process reports the store unavailable; the controller's own next
// read recovers it. That includes `gc doctor` and `gc beads health`, and so
// the beads-health order, which the controller runs as a separate process: it
// reports, it does not restart. Explicit lifecycle commands
// (`gc dolt-state start-managed` / `recover-managed`, which the provider
// script runs as the controller's child) are not gated.
//
// The claim is per CITY, not per process: a supervisor hosts several cities,
// and a runtime that failed init owns nothing (CityRuntime.ownedCity). A
// process may only recover the servers of the cities it actually brought up.
//
// The server and its scope watchdog also carry no session identity
// (doltServerEnv), so no session's orphan sweep can reap them, and both start
// in their own session (managedDoltDetachedSysProcAttr).

// managedDoltLifecycleClaims holds the cities whose managed Dolt lifecycle
// this process owns, keyed by normalized city path. A claim is never
// released: a process that brought a city up owns it for its life.
var managedDoltLifecycleClaims sync.Map

// errManagedDoltLifecycleNotOwned is returned instead of implicitly recovering
// the managed Dolt server from a process that does not own its lifecycle.
var errManagedDoltLifecycleNotOwned = errors.New(
	"managed dolt is unavailable; the controller recovers it — wait, or if no controller is running use `gc start` (check with `gc supervisor status`) (ga-fjr5f)")

// claimManagedDoltLifecycle marks this process as the owner of one city's
// managed Dolt lifecycle. Only the supervisor's per-city boot, that city's
// runtime and `gc start` call it.
func claimManagedDoltLifecycle(cityPath string) {
	managedDoltLifecycleClaims.Store(normalizePathForCompare(cityPath), true)
}

// managedDoltImplicitRecoveryAllowed reports whether this process may restart
// the city's managed Dolt server as a side effect of a read or health check.
func managedDoltImplicitRecoveryAllowed(cityPath string) bool {
	_, ok := managedDoltLifecycleClaims.Load(normalizePathForCompare(cityPath))
	return ok
}

// managedDoltImplicitRecoveryDecision reports whether this process may
// restart the city's managed Dolt server right now, as the side effect of a
// failed read or health check: nil to proceed, or the reason not to.
//
// It deliberately says nothing about whether the server is live but slow.
// Refusing to replace a live server is the transport-recover path's own
// guard (ga-amol9); a second opinion here would be the duplicate mechanism
// this gate exists to avoid.
func managedDoltImplicitRecoveryDecision(cityPath string) error {
	if !managedDoltImplicitRecoveryAllowed(cityPath) {
		return errManagedDoltLifecycleNotOwned
	}
	return nil
}

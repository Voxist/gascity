package main

import (
	"errors"
	"sync/atomic"
)

// Managed-Dolt lifecycle ownership (ga-fjr5f).
//
// The controller owns the managed Dolt server's lifecycle (SDK
// self-sufficiency). Before this, ANY gc process that found the server down
// restarted it implicitly — the bd env resolution's health check and the bd
// runner's transport recovery both run the provider "recover" op — and the
// restart ran in that process's own tree and session. An agent's SessionStart
// hook (`gc prime --hook`) was such a process: the server it started, and the
// scope watchdog supervising it, belonged to the agent session, and the
// session's teardown killed the server. The next agent wake restarted it, and
// the loop repeated.
//
// Implicit recovery is now reserved for processes that have claimed the
// lifecycle: the supervisor, a controller's city runtime, and the explicit
// `gc start` lifecycle command. Every other process reports the store
// unavailable; the controller's own next read recovers it. Explicit lifecycle
// commands (`gc dolt-state start-managed` / `recover-managed`, which the
// provider script runs as the controller's child) are not gated.

// managedDoltLifecycleOwner is set once a process claims the lifecycle. It is
// never cleared: a process that owns the lifecycle owns it for its life.
var managedDoltLifecycleOwner atomic.Bool

// errManagedDoltLifecycleNotOwned is returned instead of implicitly recovering
// the managed Dolt server from a process that does not own its lifecycle.
var errManagedDoltLifecycleNotOwned = errors.New(
	"managed dolt is unavailable; its lifecycle belongs to the controller, which recovers it (ga-fjr5f)")

// claimManagedDoltLifecycle marks this process as the managed Dolt lifecycle
// owner. Only the supervisor, a controller's city runtime and `gc start` call it.
func claimManagedDoltLifecycle() {
	managedDoltLifecycleOwner.Store(true)
}

// managedDoltImplicitRecoveryAllowed reports whether this process may restart
// the managed Dolt server as a side effect of a read or health check.
func managedDoltImplicitRecoveryAllowed() bool {
	return managedDoltLifecycleOwner.Load()
}

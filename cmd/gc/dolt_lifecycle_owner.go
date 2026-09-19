package main

import (
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"time"
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
	"managed dolt is unavailable and only the controller restarts it: run `gc start` (or wait for the controller to recover it) (ga-fjr5f)")

// errManagedDoltAliveButUnresponsive is returned instead of replacing a
// managed Dolt server whose process is alive and whose port accepts
// connections but which is not answering — slow, not dead.
var errManagedDoltAliveButUnresponsive = errors.New(
	"managed dolt is alive but not answering; not replacing a live server")

// managedDoltLiveUnresponsiveGrace is how long a live managed Dolt server
// (process alive, port accepting connections) must stay unresponsive before
// even the lifecycle owner replaces it. Under host saturation a healthy server
// answers slowly — reconciles of 40s+ were measured — and replacing it then
// turns a slow store into a dead one plus a cold start (ga-fjr5f).
const managedDoltLiveUnresponsiveGrace = 2 * time.Minute

// managedDoltUnresponsiveEpisode is one city's run of live-but-unresponsive
// observations, kept in the long-lived controller's memory only: it describes
// the current process's observations, and a restart correctly starts over.
type managedDoltUnresponsiveEpisode struct {
	first, last time.Time
}

var (
	managedDoltUnresponsiveMu       sync.Mutex
	managedDoltUnresponsiveEpisodes = map[string]managedDoltUnresponsiveEpisode{}

	// managedDoltRecoveryNow and managedDoltServerAliveFn are seams for tests.
	managedDoltRecoveryNow   = time.Now
	managedDoltServerAliveFn = managedDoltServerAlive
)

// managedDoltServerAlive reports whether the city's recorded managed Dolt
// server is alive. A valid provider state already means the recorded process
// is alive, its port is reachable and the process owns that port
// (validDoltRuntimeState), so no further probe is needed.
func managedDoltServerAlive(cityPath string) bool {
	_, ok := readValidProviderManagedDoltState(cityPath)
	return ok
}

// managedDoltImplicitRecoveryDecision reports whether this process may
// restart the managed Dolt server right now, as the side effect of a failed
// read or health check. It returns nil to proceed, or the reason not to:
//   - errManagedDoltLifecycleNotOwned when this process does not own the
//     lifecycle;
//   - errManagedDoltAliveButUnresponsive while a live server has been
//     unresponsive for less than managedDoltLiveUnresponsiveGrace.
//
// Observations further apart than the grace start a new episode, so a server
// that recovered in between is not replaced on its next slow answer; a server
// that is no longer alive ends the episode.
func managedDoltImplicitRecoveryDecision(cityPath string) error {
	if !managedDoltImplicitRecoveryAllowed() {
		return errManagedDoltLifecycleNotOwned
	}
	key := normalizePathForCompare(cityPath)
	managedDoltUnresponsiveMu.Lock()
	defer managedDoltUnresponsiveMu.Unlock()
	if !managedDoltServerAliveFn(cityPath) {
		delete(managedDoltUnresponsiveEpisodes, key)
		return nil
	}
	now := managedDoltRecoveryNow()
	episode, ok := managedDoltUnresponsiveEpisodes[key]
	if !ok || now.Sub(episode.last) > managedDoltLiveUnresponsiveGrace {
		episode = managedDoltUnresponsiveEpisode{first: now}
	}
	episode.last = now
	managedDoltUnresponsiveEpisodes[key] = episode
	if dwell := now.Sub(episode.first); dwell < managedDoltLiveUnresponsiveGrace {
		return fmt.Errorf("%w: unresponsive for %s of the %s grace (ga-fjr5f)",
			errManagedDoltAliveButUnresponsive, dwell.Round(time.Second), managedDoltLiveUnresponsiveGrace)
	}
	// Sustained: the episode stays escalated until the server is gone or the
	// observations lapse, so the recovery's own follow-up reads may recover
	// too.
	return nil
}

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

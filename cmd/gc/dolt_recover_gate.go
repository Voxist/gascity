package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"
)

// This file is the single guarded route to the managed-Dolt "recover"
// provider operation, which stops the current server and starts a
// replacement. Two rules hold at that one choke point (ga-amol9):
//
//  1. At most one recover per city per providerRecoverCooldown.
//  2. A failure to OBSERVE is never sufficient evidence to destroy a
//     running server.
//
// Rule 2 is the load-bearing one. Every probe gc can make — a TCP dial,
// lsof, ps, reading a state file — fails under exactly the host
// saturation that also makes a healthy server look slow, and each of
// those failures used to collapse to "not alive". On 2026-09-22 that
// cost the city four store restarts in seventy minutes: gc's own core
// beads-health order replaced a slow-but-alive Dolt after three minutes
// of context-deadline order failures, and dolt.log recorded "pid 11354
// exited cleanly" and "supervising pid 22745" in the same second.
//
// So the liveness answer here is three-valued, and only a CONFIRMED
// death authorizes a replacement.

// managedDoltRecoverOpName is the provider script operation that stops
// the running managed Dolt and starts a replacement.
//
// runGuardedManagedDoltRecover is the only place in cmd/gc allowed to
// hand this to the provider runner. Both historical call sites — the bd
// runner's recoverManagedBDCommand and healthBeadsProviderContext — now
// route through it, so neither the cooldown nor the liveness check can
// be skipped by taking a different route to the same effect.
// TestManagedDoltRecoverOpHasASingleCallSite enforces that structurally.
const managedDoltRecoverOpName = "recover"

// errManagedDoltRecoverDeclined is returned by
// runGuardedManagedDoltRecover when the guard refuses to run the
// recover op. It is a refusal to act, not a failure of the store:
// callers that have a cheaper remedy (the bd runner's retry) should
// still take it.
var errManagedDoltRecoverDeclined = errors.New("managed dolt recover declined")

// managedDoltRecoverEvidence is what a caller knows about the server it
// is proposing to replace.
type managedDoltRecoverEvidence int

const (
	// recoverEvidenceCallFailed means the caller only knows that its own
	// call did not come back: a bd transport timeout, an unresolvable
	// endpoint, or a health op that was killed at its deadline. A read
	// that timed out against a merely slow server is indistinguishable
	// from one against a dead server, so this evidence never authorizes
	// replacing a live one — it may only START a Dolt that is not there.
	recoverEvidenceCallFailed managedDoltRecoverEvidence = iota

	// recoverEvidenceHealthOpAnswered means the provider's own health op
	// ran to completion and reported the server unhealthy: its query
	// probe returned an error, or the server is in read-only mode. That
	// is positive evidence of unhealthiness which a live pid holding its
	// port does not refute, so it may replace a live server.
	//
	// This is deliberately NOT collapsed into the case above. A
	// wedged-but-listening Dolt answers every liveness probe gc has and
	// would otherwise be unrecoverable forever (ga-fkidk); op_health is
	// the only observer that can still tell it is broken.
	recoverEvidenceHealthOpAnswered
)

// mustProveDeath reports whether this evidence has to clear the
// three-valued liveness check before a replacement is allowed.
func (e managedDoltRecoverEvidence) mustProveDeath() bool {
	return e != recoverEvidenceHealthOpAnswered
}

// managedDoltHealthOpEvidence classifies what a FAILED provider health
// op actually proves.
//
// runProviderOpWithEnvContext wraps context.DeadlineExceeded when the op
// is killed at its deadline and otherwise reports the script's own
// stderr, so the two cases are cleanly separable. A health op that was
// killed observed nothing at all — on a saturated host that is the
// common case, not the rare one — and must not be read as a verdict on
// the server.
func managedDoltHealthOpEvidence(ctx context.Context, err error) managedDoltRecoverEvidence {
	if err == nil {
		return recoverEvidenceCallFailed
	}
	if ctx != nil && ctx.Err() != nil {
		return recoverEvidenceCallFailed
	}
	if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled) {
		return recoverEvidenceCallFailed
	}
	if strings.Contains(err.Error(), "signal: killed") {
		return recoverEvidenceCallFailed
	}
	return recoverEvidenceHealthOpAnswered
}

// managedDoltLiveness is the three-valued answer to "is there a managed
// Dolt here that would be destroyed by a recover?".
type managedDoltLiveness int

const (
	// managedDoltLivenessUnknown: a probe could not complete. Not
	// evidence of death, and never grounds for a replacement.
	managedDoltLivenessUnknown managedDoltLiveness = iota
	// managedDoltLivenessAlive: the pid is alive and holds its port.
	managedDoltLivenessAlive
	// managedDoltLivenessConfirmedDead: nothing is serving here, on a
	// probe that completed.
	managedDoltLivenessConfirmedDead
)

func (l managedDoltLiveness) String() string {
	switch l {
	case managedDoltLivenessAlive:
		return "alive"
	case managedDoltLivenessConfirmedDead:
		return "confirmed-dead"
	default:
		return "unknown"
	}
}

// managedDoltReplaceDialBudget is the TCP dial budget used ONLY when
// deciding whether to destroy a running server, and only as a fallback
// when the port-holder probe could not complete.
//
// doltPortReachable's 250ms is right for REPORTING health and wrong for
// this: a decision to replace a live server deserves a longer look than
// a decision to print a status line, and 250ms is comfortably inside the
// stall this guard exists to survive. The budget is a fallback rather
// than the primary signal because portHeldByPID answers from the process
// table, which a slow server cannot fail. A dial that times out here
// still yields UNKNOWN, never confirmed-dead.
var managedDoltReplaceDialBudget = 3 * time.Second

// lastBeadsProviderRecoverMu serializes the read-decide-write of
// lastBeadsProviderRecover so concurrent callers cannot both observe a
// stale timestamp and both admit a recover. The health patrol ticks
// once, but the bd-runner path is hit by every in-flight bd call at
// once, which is exactly the storm the cooldown exists to bound.
var lastBeadsProviderRecoverMu sync.Mutex

// admitManagedDoltRecover is the single throttle for managed-dolt
// recover attempts on a city. It admits at most one recover per
// providerRecoverCooldown, recording the admitted attempt so the next
// caller inside the window is refused, and reports whether the caller
// may proceed.
//
// The bd-runner path previously recovered on every recoverable
// transport error with no rate limit at all, so a city whose bd reads
// kept timing out could restart its managed dolt once per failed call
// (ga-amol9). It now shares this one window with the health path.
//
// An admitted call burns the window whether or not the recover then
// succeeds, which is the conservative direction for a throttle whose
// job is to break a restart loop. A recover DECLINED on liveness never
// reaches here, so a refusal to replace a live server cannot delay a
// later, genuine recovery.
func admitManagedDoltRecover(cityPath string) bool {
	cityKey := normalizePathForCompare(cityPath)
	now := providerRecoverNow()
	lastBeadsProviderRecoverMu.Lock()
	defer lastBeadsProviderRecoverMu.Unlock()
	if v, loaded := lastBeadsProviderRecover.Load(cityKey); loaded {
		if last, ok := v.(time.Time); ok && now.Sub(last) < providerRecoverCooldown() {
			return false
		}
	}
	lastBeadsProviderRecover.Store(cityKey, now)
	return true
}

// managedDoltRecoverRunner performs the provider recover operation.
//
// It is a var ONLY so tests can fake the provider script. The guard sits
// above it in runGuardedManagedDoltRecover, so faking this seam never
// bypasses the cooldown or the liveness check — which is the point of
// having one route rather than a gate at each call site.
var managedDoltRecoverRunner = func(ctx context.Context, script string, environ []string) error {
	return runProviderOpWithEnvContext(ctx, script, environ, managedDoltRecoverOpName)
}

// runGuardedManagedDoltRecover is the ONE route to the managed-Dolt
// recover op. It applies the liveness check (for evidence that cannot
// prove death on its own) and then the per-city cooldown, and only then
// runs the provider operation.
//
// A refusal returns an error wrapping errManagedDoltRecoverDeclined and
// names the reason, so the decision is legible in the logs of whichever
// process made it — which, on 2026-09-22, was an order process nobody
// expected to be restarting servers at all.
func runGuardedManagedDoltRecover(ctx context.Context, cityPath, script string, environ []string, evidence managedDoltRecoverEvidence) error {
	if evidence.mustProveDeath() {
		liveness, reason := managedDoltReplaceLiveness(cityPath)
		if liveness != managedDoltLivenessConfirmedDead {
			log.Printf("gc: declining managed dolt recover for %s: liveness %s (%s)", cityPath, liveness, reason)
			return fmt.Errorf("%w: server liveness is %s (%s); only a confirmed-dead server may be replaced on this evidence",
				errManagedDoltRecoverDeclined, liveness, reason)
		}
	}
	if !admitManagedDoltRecover(cityPath) {
		return fmt.Errorf("%w: another recover was admitted for this city within the last %s",
			errManagedDoltRecoverDeclined, providerRecoverCooldown())
	}
	if ctx == nil {
		ctx = context.Background()
	}
	return managedDoltRecoverRunner(ctx, script, environ)
}

// managedDoltReplaceLiveness answers whether cityPath has a managed Dolt
// that a recover would destroy, and returns a human-readable reason for
// the log line.
//
// It is a var only so the guard's own decision table can be driven in a
// test; the implementation is managedDoltReplaceLivenessWith.
var managedDoltReplaceLiveness = func(cityPath string) (managedDoltLiveness, string) {
	return managedDoltReplaceLivenessWith(cityPath, defaultManagedDoltLivenessProbes())
}

// managedDoltLivenessProbes are the host observations the replace
// decision rests on.
//
// They are injectable because the outcomes that matter most here are the
// ones where a probe FAILS — a starved lsof, an unreadable /proc entry,
// a dial that times out — and those cannot be produced on demand from
// the outside. A guard whose "could not check" branches are untested is
// a guard whose only interesting behavior is untested.
type managedDoltLivenessProbes struct {
	lifecycleOwned func(cityPath string) (bool, error)
	pidAlive       func(pid int) bool
	portHeldByPID  func(port string, pid int) (held, known bool)
	portHolders    func(port string) ([]int, bool)
	portReachable  func(port string, budget time.Duration) bool
}

func defaultManagedDoltLivenessProbes() managedDoltLivenessProbes {
	return managedDoltLivenessProbes{
		lifecycleOwned: managedDoltLifecycleOwned,
		pidAlive:       pidAlive,
		portHeldByPID:  portHeldByPID,
		portHolders:    portHolderPIDsKnown,
		portReachable:  doltPortReachableWithin,
	}
}

// managedDoltReplaceLivenessWith is the liveness decision itself.
//
// It reuses the existing runtime-state machinery
// (validDoltRuntimeStateIdentity, pidAlive, portHeldByPID) but keeps
// each probe's "could not check" distinct from its "checked, negative"
// instead of collapsing both to false the way validDoltRuntimeState
// does. portHolderPIDsKnown already draws exactly that line one layer
// down — an lsof killed at its deadline is not a listing at all — and
// this is the same discipline applied to the replace decision.
func managedDoltReplaceLivenessWith(cityPath string, probes managedDoltLivenessProbes) (managedDoltLiveness, string) {
	owned, err := probes.lifecycleOwned(cityPath)
	if err != nil {
		return managedDoltLivenessUnknown, fmt.Sprintf("managed dolt ownership probe failed: %v", err)
	}
	if !owned {
		// gc runs no managed Dolt for this city, so there is no managed
		// server here for a recover to destroy. Whatever the provider's
		// recover op does for an external store is that store's concern,
		// and refusing here would only break it.
		return managedDoltLivenessConfirmedDead, "city has no gc-managed dolt runtime"
	}

	data, err := os.ReadFile(managedDoltStatePath(cityPath))
	if err != nil {
		if os.IsNotExist(err) {
			return managedDoltAbsentStateLiveness(cityPath, probes)
		}
		return managedDoltLivenessUnknown, fmt.Sprintf("runtime state unreadable: %v", err)
	}
	var state doltRuntimeState
	if err := json.Unmarshal(data, &state); err != nil {
		return managedDoltLivenessUnknown, fmt.Sprintf("runtime state malformed: %v", err)
	}
	if !state.Running || state.PID <= 0 || state.Port <= 0 {
		return managedDoltLivenessConfirmedDead, "runtime state records no running server"
	}
	if _, ok := validDoltRuntimeStateIdentity(state, cityPath); !ok {
		// The state names a server, but gc cannot confirm it is THIS
		// city's: a data dir that does not match, or a layout that would
		// not resolve. Not knowing whose server it is, is not knowing
		// that it is dead.
		return managedDoltLivenessUnknown, "runtime state identity could not be resolved"
	}
	if !probes.pidAlive(state.PID) {
		// kill(pid, 0) answering ESRCH is a real answer and, unlike a
		// dial or an lsof, is not something host saturation can take
		// away. This is the case a recover exists for.
		return managedDoltLivenessConfirmedDead, fmt.Sprintf("pid %d is gone", state.PID)
	}

	port := strconv.Itoa(state.Port)
	held, known := probes.portHeldByPID(port, state.PID)
	if !known {
		return managedDoltUnheldPortLiveness(state.PID, port, probes)
	}
	if held {
		return managedDoltLivenessAlive, fmt.Sprintf("pid %d is alive and holds port %s", state.PID, port)
	}
	if holders, holdersKnown := probes.portHolders(port); holdersKnown && len(holders) > 0 {
		return managedDoltLivenessConfirmedDead,
			fmt.Sprintf("port %s is held by %v, not by recorded pid %d", port, holders, state.PID)
	}
	// A complete, empty listing: the recorded pid is alive but is not
	// serving on its port. Nothing here answers clients, so there is no
	// live server to protect.
	return managedDoltLivenessConfirmedDead,
		fmt.Sprintf("pid %d is alive but nothing listens on port %s", state.PID, port)
}

// managedDoltUnheldPortLiveness handles the case where the port-holder
// probe itself could not complete — a starved lsof, an unreadable /proc
// entry. This is the ga-kozi8 shape, where the supervisor's own probe
// children were being killed ~14 times per ten minutes, so the input to
// the replace decision was frequently "I could not look".
//
// A dial is the only remaining evidence, and it is consulted with the
// raised replace-path budget. Reachable means alive; unreachable means
// unknown, never dead.
func managedDoltUnheldPortLiveness(pid int, port string, probes managedDoltLivenessProbes) (managedDoltLiveness, string) {
	if probes.portReachable(port, managedDoltReplaceDialBudget) {
		return managedDoltLivenessAlive,
			fmt.Sprintf("port-holder probe failed, but pid %d is alive and port %s answers within %s", pid, port, managedDoltReplaceDialBudget)
	}
	return managedDoltLivenessUnknown,
		fmt.Sprintf("pid %d is alive and the port-holder probe for %s could not complete", pid, port)
}

// managedDoltAbsentStateLiveness classifies a city with no published
// runtime state.
//
// Absent state is the normal "nothing has been started here" case and
// must stay recoverable, or the guard would break genuine recovery. But
// absent state with something still listening on the recorded port is a
// publication that has not caught up, not an empty city, and destroying
// that listener is precisely the mistake this guard exists to prevent.
func managedDoltAbsentStateLiveness(cityPath string, probes managedDoltLivenessProbes) (managedDoltLiveness, string) {
	port := strings.TrimSpace(recordedCompatDoltPort(cityPath))
	if port == "" {
		return managedDoltLivenessConfirmedDead, "no runtime state and no recorded port: nothing is running"
	}
	holders, known := probes.portHolders(port)
	if !known {
		return managedDoltLivenessUnknown,
			fmt.Sprintf("no runtime state, and the port-holder probe for recorded port %s could not complete", port)
	}
	if len(holders) > 0 {
		return managedDoltLivenessUnknown,
			fmt.Sprintf("no runtime state, but recorded port %s is held by %v", port, holders)
	}
	return managedDoltLivenessConfirmedDead,
		fmt.Sprintf("no runtime state and nothing listens on recorded port %s", port)
}

// recordedCompatDoltPort reads the .beads/dolt-server.port compatibility
// mirror WITHOUT the reconciliation side effects of currentDoltPort,
// which deletes the file when runtime state is missing. A liveness probe
// must not mutate the state it is reading.
func recordedCompatDoltPort(cityPath string) string {
	data, err := os.ReadFile(filepath.Join(cityPath, ".beads", "dolt-server.port"))
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(data))
}

// doltPortReachableWithin is doltPortReachable with the dial budget
// supplied by the caller, so the replace path can look longer than the
// health-reporting path does.
func doltPortReachableWithin(port string, budget time.Duration) bool {
	if strings.TrimSpace(port) == "" {
		return false
	}
	conn, err := net.DialTimeout("tcp", net.JoinHostPort("127.0.0.1", port), budget)
	if err != nil {
		return false
	}
	_ = conn.Close()
	return true
}

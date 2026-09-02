package main

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
)

// Bounds the single store probe that gates store-dependent doctor checks (#5064).
const doctorBeadStorePreflightTimeout = 5 * time.Second

// City + per-rig store checks skipped on outage-shaped preflight; keep in sync with buildDoctorChecks.
const (
	// 14, not 13: the fork's prDeliveryDoctorCheck is a city store check and
	// lives inside buildDoctorChecks' storeOK block alongside upstream's.
	// This constant is the drift lock the preflight test asserts against.
	doctorCityStoreCheckCount   = 14
	doctorPerRigStoreCheckCount = 3
)

// City-scoped store probe before store-dependent checks (also used at gc start warmup). Tests override.
var doctorBeadStorePreflight = defaultDoctorBeadStorePreflight

func defaultDoctorBeadStorePreflight(cityPath string, _ func(string) (beads.Store, error)) error {
	ctx, cancel := context.WithTimeout(context.Background(), doctorBeadStorePreflightTimeout)
	defer cancel()
	if err := ctx.Err(); err != nil {
		return err
	}
	// NoRecovery + context-bound runner (O(1) list; process-group kill on timeout).
	env, err := bdRuntimeEnvWithErrorRecoveryContext(ctx, cityPath, false)
	if err != nil {
		return err
	}
	_, err = beads.ExecCommandRunnerWithEnvContext(ctx, env)(cityPath, "bd", "list", "--json", "--limit", "1")
	return err
}

// True for live store outages (breaker/conn/timeout), not missing/uninitialized stores.
//
// Superset of the transport shapes in bdTransportRetryableError
// (cmd/gc/bd_env.go) plus store-pool exhaustion. Deliberately separate:
// that list drives retry, this one drives check omission. Keep them from
// drifting when new bd/Dolt error shapes appear.
func isBeadStoreUnreachable(err error) bool {
	if err == nil {
		return false
	}
	msg := strings.ToLower(err.Error())
	for _, sig := range []string{
		"dolt circuit breaker is open",
		"server appears down",
		"dolt server unreachable",
		"dolt server not reachable",
		"max waiting connections",
		"client rejected",
		"too many connections",
		"connection refused",
		"dial tcp",
		"bad connection",
		"invalid connection",
		"connection reset",
		"broken pipe",
		"i/o timeout",
		"timed out after",
		"context deadline exceeded",
		"unexpected eof",
		"use of closed network connection",
	} {
		if strings.Contains(msg, sig) {
			return true
		}
	}
	return false
}

// doctorCityStoreCheckCountForEnv is the city store-gated check count for the
// CURRENT env shape: the base constant, plus the city
// CustomTypesPreflightCheck that buildDoctorChecks registers only outside
// GC_DOLT=skip. Every earlier drift-lock test ran under GC_DOLT=skip, so the
// env-gated registration was invisible to the sync guard and a production
// outage under-reported how many checks it withheld.
func doctorCityStoreCheckCountForEnv() int {
	if gcDoltSkip() {
		return doctorCityStoreCheckCount
	}
	return doctorCityStoreCheckCount + 1
}

// beadStorePreflightSkipCount counts the store-gated checks buildDoctorChecks
// would have registered for this city and rig set, using the SAME predicates
// the registrations themselves use (gcDoltSkip for the env shape,
// rigUsesManagedBdStoreContract for the per-rig custom-types preflight), so
// the outage banner cannot drift from production by construction. Pinned in
// both env shapes by TestBeadStorePreflightSkipCountMatchesBothEnvShapes.
func beadStorePreflightSkipCount(cityPath string, activeRigs []config.Rig) int {
	count := doctorCityStoreCheckCountForEnv() + doctorPerRigStoreCheckCount*len(activeRigs)
	if gcDoltSkip() {
		return count
	}
	for _, rig := range activeRigs {
		if rigUsesManagedBdStoreContract(cityPath, rig) {
			count++ // rig custom-types-preflight, managed-bdstore rigs only
		}
	}
	return count
}

func beadStorePreflightSkipMessage(skipCount, rigCount int, probeErr error) string {
	// City-scoped probe: skip is a city-outage gate (per-rig endpoints may differ).
	base := fmt.Sprintf(
		"bead store unreachable — skipped %d store checks (%d city, %d rigs); city store was probed (per-rig endpoints, including doltlite, may differ)",
		skipCount, doctorCityStoreCheckCountForEnv(), rigCount,
	)
	if probeErr == nil {
		return base
	}
	return fmt.Sprintf("%s: %v", base, probeErr)
}

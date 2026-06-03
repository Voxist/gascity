package main

import (
	"context"
	"fmt"
	"io"
	"strings"
)

// managedDoltServingDataDirFn is the seam used by tests to stub the live
// @@datadir read without a running Dolt server.
var managedDoltServingDataDirFn = managedDoltServingDataDir

// managedDoltServingDataDir asks the managed Dolt SQL server bound to the
// city's managed port for its active data directory (`SELECT @@datadir`). Host
// and user are left empty so runManagedDoltSQLContext applies the canonical
// managed defaults (managedDoltConnectHost + root) and the managed password,
// identically to the other health probes. The query is bounded by ctx.
func managedDoltServingDataDir(ctx context.Context, port string) (string, error) {
	out, err := runManagedDoltSQLContext(ctx, "", port, "", "-r", "csv", "-q", "SELECT @@datadir")
	if err != nil {
		return "", err
	}
	return firstManagedDoltCSVValue(out), nil
}

// firstManagedDoltCSVValue returns the first data cell of a single-column CSV
// result (the row after the header), with surrounding quotes/whitespace
// stripped. Empty string if there is no data row.
func firstManagedDoltCSVValue(out string) string {
	lines := strings.Split(strings.ReplaceAll(out, "\r\n", "\n"), "\n")
	row := 0
	for _, line := range lines {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		row++
		if row == 1 {
			continue // header (@@datadir)
		}
		return strings.Trim(line, `"`)
	}
	return ""
}

// managedDoltDataDirMismatchFn is the seam used by the reconciler gate (and its
// tests) so the steady-state hold logic can be exercised without a live Dolt.
var managedDoltDataDirMismatchFn = managedDoltDataDirMismatch

// managedDoltDataDirMismatch reports whether the managed Dolt server currently
// bound to this city's port is serving a DIFFERENT data directory than gc
// expects — i.e. a foreign/"squatter" Dolt has taken the port.
//
// It returns true ONLY on a confirmed mismatch: a successful @@datadir read
// whose value differs from the expected ${cityPath}/.beads/dolt. Any inability
// to determine identity (not a bd-store city, no resolvable managed port, query
// error, unparseable result) returns false — failing OPEN so a transient SQL
// hiccup never wedges the fleet. Genuine store-unreachability is already handled
// by the demand-read error path (storeQueryPartial); this check covers the case
// the partial path cannot see: a *successful* read of the *wrong* store.
//
// Probe errors are logged to stderr (best-effort) so a persistent fail-open is
// observable rather than silent.
//
// Motivation: the 2026-06-01 fleet-drain incident (gastownhall/gascity#2930) —
// managed Dolt died (ENOSPC), a standalone bd Dolt squatted the vacated port,
// and the reconciler read zero demand from the wrong store and drained every
// min=0 pool with no respawn.
func managedDoltDataDirMismatch(ctx context.Context, cityPath string, stderr io.Writer) bool {
	if !cityUsesBdStoreContract(cityPath) {
		return false
	}
	layout, err := resolveManagedDoltRuntimeLayout(cityPath)
	if err != nil || strings.TrimSpace(layout.DataDir) == "" {
		return false
	}
	port := strings.TrimSpace(currentResolvableManagedDoltPort(cityPath))
	if port == "" {
		return false
	}
	return managedDoltDataDirMismatchForConn(ctx, layout.DataDir, port, stderr)
}

// managedDoltDataDirMismatchForConn is the seam-backed core: it reads the live
// @@datadir over the managed connection and compares it to expectedDataDir.
// Split out from managedDoltDataDirMismatch so the read+compare logic is unit
// testable without satisfying the bd-store-contract / port-resolution gates.
func managedDoltDataDirMismatchForConn(ctx context.Context, expectedDataDir, port string, stderr io.Writer) bool {
	serving, err := managedDoltServingDataDirFn(ctx, port)
	if err != nil {
		if stderr != nil {
			fmt.Fprintf(stderr, "managed dolt identity probe failed (treating as healthy): %v\n", err) //nolint:errcheck // best-effort stderr
		}
		return false
	}
	return dataDirIsMismatch(serving, expectedDataDir)
}

// dataDirIsMismatch reports whether two data-dir paths refer to different
// directories. Either side empty → false (cannot conclude a mismatch; fail
// open). Comparison is symlink/abs-normalized via samePath.
func dataDirIsMismatch(serving, expected string) bool {
	serving = strings.TrimSpace(serving)
	expected = strings.TrimSpace(expected)
	if serving == "" || expected == "" {
		return false
	}
	return !samePath(serving, expected)
}

// storeIdentityHold reports whether this reconcile tick must hold (suppress the
// drain) because the managed Dolt store identity cannot be trusted. It is gated
// to the only situation a squatter can drain: no assigned work yet running
// sessions exist. A non-empty work set proves the store is live and ours; no
// running sessions means there is nothing to protect this tick — so neither
// pays the @@datadir probe.
func storeIdentityHold(ctx context.Context, cityPath string, assignedWorkCount, openSessionCount int, stderr io.Writer) bool {
	if assignedWorkCount != 0 || openSessionCount == 0 {
		return false
	}
	return managedDoltDataDirMismatchFn(ctx, cityPath, stderr)
}

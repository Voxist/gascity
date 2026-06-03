package main

import (
	"os"
	"strings"
)

// managedDoltServingDataDirFn is the seam used by tests to stub the live
// @@datadir read without a running Dolt server.
var managedDoltServingDataDirFn = managedDoltServingDataDir

// managedDoltServingDataDir asks the managed Dolt SQL server bound to host:port
// for its active data directory (`SELECT @@datadir`). It reuses runManagedDoltSQL
// so the managed password path is honored identically to the other health
// probes.
func managedDoltServingDataDir(host, port, user string) (string, error) {
	out, err := runManagedDoltSQL(host, port, user, "-r", "csv", "-q", "SELECT @@datadir")
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
// Motivation: the 2026-06-01 fleet-drain incident (gastownhall/gascity#2930) —
// managed Dolt died (ENOSPC), a standalone bd Dolt squatted the vacated port,
// and the reconciler read zero demand from the wrong store and drained every
// min=0 pool with no respawn.
func managedDoltDataDirMismatch(cityPath string) bool {
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
	host := strings.TrimSpace(os.Getenv("GC_DOLT_HOST"))
	if host == "" {
		host = "127.0.0.1"
	}
	user := strings.TrimSpace(os.Getenv("GC_DOLT_USER"))
	if user == "" {
		user = "root"
	}
	serving, err := managedDoltServingDataDirFn(host, port, user)
	if err != nil {
		return false
	}
	return dataDirIsMismatch(serving, layout.DataDir)
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

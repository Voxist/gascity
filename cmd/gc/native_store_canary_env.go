package main

import (
	"fmt"
	"strings"
)

// nativeStoreCanaryServerModeValue is the value projected for
// BEADS_DOLT_SERVER_MODE when a scope's native-store canary is enabled. It
// routes the upstream beads open path through server mode (IsDoltServerMode()
// true) instead of falling through to an embedded — and potentially empty —
// database, which is the silent-misroute the post-open identity assertion
// exists to catch.
const nativeStoreCanaryServerModeValue = "1"

// applyNativeStoreCanaryEnv mutates the scope's resolved beads env so the native
// store opens against the managed Dolt server when the canary is enabled for the
// scope.
//
// When enabled is false this is a no-op: the env is returned byte-for-byte
// unchanged so an unlisted scope's projection is unaffected (the lever is purely
// additive and defaults OFF).
//
// When enabled is true it requires the managed-server host and port to already
// be resolved into BEADS_DOLT_SERVER_HOST/PORT (mirrored from the live handle by
// the surrounding projection — never the port file) and projects
// BEADS_DOLT_SERVER_MODE so beads routes to server mode. If the endpoint is
// unresolvable it returns a loud error rather than silently letting the open
// fall through to an empty embedded database.
func applyNativeStoreCanaryEnv(env map[string]string, scopeName string, enabled bool) error {
	if !enabled {
		return nil
	}
	if env == nil {
		return fmt.Errorf("native-store canary for scope %q: env map is nil", scopeName)
	}
	host := strings.TrimSpace(env["BEADS_DOLT_SERVER_HOST"])
	port := strings.TrimSpace(env["BEADS_DOLT_SERVER_PORT"])
	if host == "" || port == "" {
		return fmt.Errorf("native-store canary for scope %q: managed Dolt server endpoint is unresolvable (host=%q port=%q); refusing to open the native store against an embedded database", scopeName, host, port)
	}
	env["BEADS_DOLT_SERVER_MODE"] = nativeStoreCanaryServerModeValue
	return nil
}

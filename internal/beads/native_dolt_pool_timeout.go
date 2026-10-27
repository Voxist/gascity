package beads

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"gopkg.in/yaml.v3"
)

// nativePoolReadTimeoutEnvKey is the library's per-I/O read deadline override
// for pooled server connections.
const nativePoolReadTimeoutEnvKey = "BEADS_DOLT_POOL_READ_TIMEOUT"

// nativePoolReadTimeoutConfigKey is the workspace config.yaml spelling of the
// same override, which the library reads after the env rung.
const nativePoolReadTimeoutConfigKey = "dolt.pool-read-timeout"

// withNativePoolReadTimeoutDefault raises the library's per-I/O read deadline
// to nativeReadRetryBudget for one open, unless the operator chose a deadline
// through the env or the workspace config.yaml. The caller holds
// nativeDoltOpenEnvMu (it runs inside an open window); the returned func
// restores the env.
//
// THE PER-ATTEMPT DEADLINE MUST NOT BE SHORTER THAN THE BUDGET. The library
// defaults the deadline to 10s, and withReadRetryContext classifies the
// resulting "i/o timeout" as transient: it reconnects and re-issues the read,
// up to the 90s budget. A read that is slow rather than lost is therefore sent
// again every ~10s while the server keeps executing each copy the client has
// abandoned, so a loaded server is handed several times the work it was
// already failing to finish (ga-s69mk.7: a startup wisp listing executed ~8x
// per read). With the deadline at the budget a slow read runs once, still
// bounded by the budget's context; a lost connection (a killed or rebound
// server) still fails at once with a connection error and is retried as
// before.
func withNativePoolReadTimeoutDefault(beadsDir string) (func(), error) {
	if _, set := os.LookupEnv(nativePoolReadTimeoutEnvKey); set || workspaceConfigSets(beadsDir, nativePoolReadTimeoutConfigKey) {
		return func() {}, nil
	}
	if err := os.Setenv(nativePoolReadTimeoutEnvKey, nativeReadRetryBudget.String()); err != nil {
		return nil, fmt.Errorf("projecting native Dolt open env %s: %w", nativePoolReadTimeoutEnvKey, err)
	}
	return func() { _ = os.Unsetenv(nativePoolReadTimeoutEnvKey) }, nil
}

// workspaceConfigSets reports whether beadsDir's config.yaml sets the dotted
// key, as the library's config lookup finds it: an exact top-level key, or the
// same path through nested maps. An unreadable or unparseable file sets
// nothing.
func workspaceConfigSets(beadsDir, key string) bool {
	data, err := os.ReadFile(filepath.Join(beadsDir, "config.yaml"))
	if err != nil {
		return false
	}
	var root map[string]any
	if yaml.Unmarshal(data, &root) != nil {
		return false
	}
	if root[key] != nil {
		return true
	}
	node := root
	parts := strings.Split(key, ".")
	for _, part := range parts[:len(parts)-1] {
		next, ok := node[part].(map[string]any)
		if !ok {
			return false
		}
		node = next
	}
	return node[parts[len(parts)-1]] != nil
}

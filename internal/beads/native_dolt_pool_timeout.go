package beads

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

// nativePoolReadTimeoutEnvKey is the library's per-I/O read deadline override
// for pooled server connections.
const nativePoolReadTimeoutEnvKey = "BEADS_DOLT_POOL_READ_TIMEOUT"

// nativePoolReadTimeoutConfigKey is the workspace config.yaml spelling of the
// same override, which the library reads after the env rung.
const nativePoolReadTimeoutConfigKey = "dolt.pool-read-timeout"

// nativePoolReadTimeoutDefault is the per-I/O read deadline gc's native opens
// use when the operator configured none. It sits far above the slowest
// healthy-but-loaded read measured for ga-s69mk.7 (11-13s with 16 concurrent
// whole-ledger listings) and below nativeReadRetryBudget, so a read on a
// silently dead peer still times out early enough to reconnect and retry once
// inside the budget.
const nativePoolReadTimeoutDefault = 60 * time.Second

// withNativePoolReadTimeoutDefault raises the library's per-I/O read deadline
// to nativePoolReadTimeoutDefault for one open, unless the operator configured
// a deadline the library will actually use, through the env or the workspace
// config.yaml. The caller holds nativeDoltOpenEnvMu (it runs inside an open
// window); the returned func restores the env exactly as it was.
//
// THE PER-ATTEMPT DEADLINE MUST BE LONG RELATIVE TO A SLOW READ. The library
// defaults the deadline to 10s, and withReadRetryContext classifies the
// resulting "i/o timeout" as transient: it reconnects and re-issues the read,
// up to the 90s budget. A read that is slow rather than lost is therefore sent
// again every ~10s while the server keeps executing each copy the client has
// abandoned, so a loaded server is handed several times the work it was
// already failing to finish, and the repeated timeouts trip the library's
// circuit breaker (ga-s69mk.7: a startup wisp listing executed ~8x per read).
// With the longer deadline a slow read runs once; a lost connection (a killed
// or rebound server) still fails at once with a connection error and is
// retried as before.
//
// Only the two rungs gc's in-process open reaches are consulted. The library's
// global viper rung is initialized by cmd/bd alone, so it is empty here.
func withNativePoolReadTimeoutDefault(beadsDir string) (func(), error) {
	previous, envSet := os.LookupEnv(nativePoolReadTimeoutEnvKey)
	if envSet && libraryPoolTimeoutUsable(previous) {
		return func() {}, nil
	}
	// An unusable env value makes the library fall through to the config rung.
	if libraryPoolTimeoutUsable(workspaceConfigString(beadsDir, nativePoolReadTimeoutConfigKey)) {
		return func() {}, nil
	}
	if err := os.Setenv(nativePoolReadTimeoutEnvKey, nativePoolReadTimeoutDefault.String()); err != nil {
		return nil, fmt.Errorf("projecting native Dolt open env %s: %w", nativePoolReadTimeoutEnvKey, err)
	}
	return func() {
		if envSet {
			_ = os.Setenv(nativePoolReadTimeoutEnvKey, previous)
			return
		}
		_ = os.Unsetenv(nativePoolReadTimeoutEnvKey)
	}, nil
}

// libraryPoolTimeoutUsable reports whether the library's duration parser
// (beads internal/storage/dolt parseTimeout) yields a deadline from raw rather
// than falling back to its default: a positive time.ParseDuration value, or a
// positive bare number of seconds.
func libraryPoolTimeoutUsable(raw string) bool {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return false
	}
	if d, err := time.ParseDuration(raw); err == nil {
		return d > 0
	}
	d, err := time.ParseDuration(raw + "s")
	return err == nil && d > 0
}

// workspaceConfigString returns beadsDir's config.yaml value for the dotted
// key, as the library's config lookup (beads internal/config
// GetStringFromDir) finds and renders it: an exact top-level key wins over the
// same path through nested maps, and a non-string value is formatted with %v.
// A missing key, or an unreadable or unparseable file, yields "".
func workspaceConfigString(beadsDir, key string) string {
	data, err := os.ReadFile(filepath.Join(beadsDir, "config.yaml"))
	if err != nil {
		return ""
	}
	var root map[string]any
	if yaml.Unmarshal(data, &root) != nil {
		return ""
	}
	if val, ok := root[key]; ok {
		return configScalarString(val)
	}
	node := root
	parts := strings.Split(key, ".")
	for _, part := range parts[:len(parts)-1] {
		next, ok := node[part].(map[string]any)
		if !ok {
			return ""
		}
		node = next
	}
	val, ok := node[parts[len(parts)-1]]
	if !ok {
		return ""
	}
	return configScalarString(val)
}

// configScalarString renders a config.yaml value the way the library does.
func configScalarString(val any) string {
	if s, ok := val.(string); ok {
		return s
	}
	return fmt.Sprintf("%v", val)
}

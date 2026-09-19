package integration

import (
	"strings"
	"testing"
)

// inheritedEnvScrubVars are the ambient variables integrationEnvFor strips
// before building a child gc/bd environment. Most of them point a process at a
// particular city, rig, bead store or Dolt server; a fleet agent shell carries
// the LIVE city's values for all of them.
//
// managedDoltRuntimeLayoutVars are listed separately because they are the ones
// that name the live managed-Dolt server's own files (ga-cflrh). A child that
// inherits GC_DOLT_PID_FILE / GC_DOLT_STATE_FILE / GC_PACK_STATE_DIR resolves
// the live server's pid and state as its own, passes the managed-Dolt
// ownership check against it, and can stop it or rewrite its state. cmd/gc's
// TestMain already scrubs every GC_* variable; internal/testenv's
// LeakVectorVars carries the same names for every other test binary.
var managedDoltRuntimeLayoutVars = []string{
	"GC_PACK_STATE_DIR",
	"GC_DOLT_CONFIG_FILE",
	"GC_DOLT_DATA_DIR",
	"GC_DOLT_PID_FILE",
	"GC_DOLT_STATE_FILE",
	"GC_DOLT_LOCK_FILE",
	"GC_DOLT_LOG_FILE",
	"GC_DOLT_MANAGED_LOCAL",
	"GC_DOLT_ARCHIVE_LEVEL",
	"GC_DOLT_AUTO_GC_ENABLED",
	"GC_DOLT_MAX_CONNECTIONS",
	"GC_DOLT_READ_TIMEOUT_MILLIS",
	"GC_DOLT_WRITE_TIMEOUT_MILLIS",
	"GC_DOLT_LOCK_RELEASE_TIMEOUT_MS",
	"GC_BEADS_BACKEND",
}

var inheritedEnvScrubVars = append([]string{
	"GC_BEADS",
	"BEADS_DIR",
	"GC_BEADS_SCOPE_ROOT",
	"GC_DOLT",
	"PATH",
	"GC_HOME",
	"GC_DIR",
	"GC_CITY",
	"GC_CITY_PATH",
	"GC_CITY_ROOT",
	"GC_CITY_RUNTIME_DIR",
	"GC_AGENT",
	"GC_RIG",
	"GC_RIG_ROOT",
	"GC_TEMPLATE",
	"GC_SESSION_NAME",
	"XDG_RUNTIME_DIR",
	"DOLT_ROOT_PATH",
	"BEADS_ACTOR",
	"GC_DOLT_HOST",
	"GC_DOLT_PORT",
	"GC_DOLT_USER",
	"GC_DOLT_PASSWORD",
	"BEADS_DOLT_SERVER_HOST",
	"BEADS_DOLT_SERVER_PORT",
	"BEADS_DOLT_SERVER_USER",
	"BEADS_DOLT_HOST",
	"BEADS_DOLT_PORT",
	"BEADS_DOLT_USER",
	"BEADS_DOLT_DATABASE",
	"BEADS_DOLT_DATA_DIR",
	"BEADS_DOLT_PASSWORD",
	"GC_SUPERVISOR_ENV",
	"GC_SUPERVISOR_PRESERVE_SESSIONS_ON_SIGNAL",
	"GC_SUPERVISOR_LOG_TEE",
	"DOLT_HOST",
	"DOLT_PORT",
	"DOLT_USER",
	"DOLT_PASSWORD",
	"BEADS_DOLT_AUTO_START",
}, managedDoltRuntimeLayoutVars...)

// scrubInheritedEnv returns env without any inheritedEnvScrubVars entry.
func scrubInheritedEnv(env []string) []string {
	for _, name := range inheritedEnvScrubVars {
		env = filterEnv(env, name)
	}
	return env
}

// filterEnv returns env with the named variable removed.
func filterEnv(env []string, name string) []string {
	prefix := name + "="
	result := make([]string, 0, len(env))
	for _, e := range env {
		if len(e) >= len(prefix) && e[:len(prefix)] == prefix {
			continue
		}
		result = append(result, e)
	}
	return result
}

// TestScrubInheritedEnvDropsManagedDoltRuntimeLayout is the ga-cflrh guard: a
// fleet shell's live managed-Dolt layout must never reach an integration
// child's environment, while unrelated variables pass through untouched.
func TestScrubInheritedEnvDropsManagedDoltRuntimeLayout(t *testing.T) {
	var env []string
	for _, name := range managedDoltRuntimeLayoutVars {
		env = append(env, name+"=/live/city/"+name)
	}
	env = append(env, "GC_FAST_UNIT=keep", "LANG=C")

	got := scrubInheritedEnv(env)

	for _, entry := range got {
		name, _, _ := strings.Cut(entry, "=")
		for _, leaked := range managedDoltRuntimeLayoutVars {
			if name == leaked {
				t.Errorf("%s reached the child env: %q", name, entry)
			}
		}
	}
	for _, want := range []string{"GC_FAST_UNIT=keep", "LANG=C"} {
		found := false
		for _, entry := range got {
			if entry == want {
				found = true
			}
		}
		if !found {
			t.Errorf("%q was scrubbed but should pass through; got %v", want, got)
		}
	}
}

// TestManagedDoltRuntimeLayoutVarsCoverTheIncidentSet pins the names ga-cflrh
// named, so shrinking the list cannot pass silently.
func TestManagedDoltRuntimeLayoutVarsCoverTheIncidentSet(t *testing.T) {
	for _, name := range []string{
		"GC_PACK_STATE_DIR",
		"GC_DOLT_CONFIG_FILE",
		"GC_DOLT_DATA_DIR",
		"GC_DOLT_PID_FILE",
		"GC_DOLT_STATE_FILE",
		"GC_DOLT_LOCK_FILE",
		"GC_DOLT_LOG_FILE",
	} {
		found := false
		for _, v := range managedDoltRuntimeLayoutVars {
			if v == name {
				found = true
			}
		}
		if !found {
			t.Errorf("%s missing from managedDoltRuntimeLayoutVars", name)
		}
	}
}

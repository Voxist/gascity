//go:build integration

package integration

// Regression guards for ga-32bb2: integration runs planted launchd supervisors
// in the operator's real ~/Library/LaunchAgents. GC_HOME isolation does not
// reach launchd, so each plist outlived its test, was reloaded with a GC_HOME
// that no longer held a supervisor.toml, and bound the default API port — the
// production supervisor's — taking the fleet's controller offline.

import (
	"os"
	"os/user"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"
)

const supervisorPlistPrefix = "com.gascity.supervisor."

func realLaunchAgentsDirForTest(t *testing.T) string {
	t.Helper()
	lu, err := user.LookupId(strconv.Itoa(os.Getuid()))
	if err != nil || strings.TrimSpace(lu.HomeDir) == "" {
		t.Skipf("cannot resolve the real home dir: %v", err)
	}
	return filepath.Join(lu.HomeDir, "Library", "LaunchAgents")
}

func supervisorPlistNames(t *testing.T, dir string) map[string]bool {
	t.Helper()
	names := map[string]bool{}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return names
	}
	for _, entry := range entries {
		if name := entry.Name(); strings.HasPrefix(name, supervisorPlistPrefix) && strings.HasSuffix(name, ".plist") {
			names[name] = true
		}
	}
	return names
}

// TestIntegrationEnvRedirectsLaunchAgentsAndRefusesLaunchctl pins the two
// properties every gc subprocess env must have: plists go to a per-GC_HOME
// dir, and the launchctl on PATH refuses to touch the real launchd domain.
func TestIntegrationEnvRedirectsLaunchAgentsAndRefusesLaunchctl(t *testing.T) {
	gcHome, _, env := newIsolatedEnvRoot(t, false)
	got := parseEnvList(env)

	want := filepath.Join(gcHome, "LaunchAgents")
	if got[launchAgentsDirEnv] != want {
		t.Fatalf("%s = %q, want %q", launchAgentsDirEnv, got[launchAgentsDirEnv], want)
	}
	if realDir := realLaunchAgentsDirForTest(t); strings.HasPrefix(got[launchAgentsDirEnv], realDir) {
		t.Fatalf("%s = %q is inside the real LaunchAgents dir %q", launchAgentsDirEnv, got[launchAgentsDirEnv], realDir)
	}

	var launchctl string
	for _, dir := range filepath.SplitList(got["PATH"]) {
		candidate := filepath.Join(dir, "launchctl")
		if info, err := os.Stat(candidate); err == nil && !info.IsDir() {
			launchctl = candidate
			break
		}
	}
	if launchctl != filepath.Join(integrationToolBinDir, "launchctl") {
		t.Fatalf("first launchctl on the subprocess PATH = %q, want the refusal shim in %q", launchctl, integrationToolBinDir)
	}
	if out, err := runCommand("", env, 5*time.Second, launchctl, "list"); err == nil {
		t.Fatalf("launchctl shim succeeded; it must refuse every call\n%s", out)
	}

	cfg, err := os.ReadFile(filepath.Join(gcHome, "supervisor.toml"))
	if err != nil {
		t.Fatalf("reading supervisor.toml: %v", err)
	}
	if !strings.Contains(string(cfg), "port = ") || strings.Contains(string(cfg), "port = 8372") {
		t.Fatalf("supervisor.toml must pin a reserved non-default port:\n%s", cfg)
	}
}

// TestSupervisorInstallNeverWritesRealLaunchAgents drives the real binary's
// install path the way newIsolatedCommandEnv's permissive launchctl shim did
// when the leaked plists were planted: launchctl "succeeds", so the plist is
// kept. It must land under the injected dir, never the operator's.
func TestSupervisorInstallNeverWritesRealLaunchAgents(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("launchd install path is darwin-only; Linux installs a systemd user unit")
	}
	gcHome, _, env := newIsolatedEnvRoot(t, false)
	realDir := realLaunchAgentsDirForTest(t)
	before := supervisorPlistNames(t, realDir)
	// Backstop for a regression: remove only plists this test planted (new
	// since the snapshot and naming this test's GC_HOME). The permissive shim
	// below means none of them was ever loaded into launchd.
	t.Cleanup(func() {
		for name := range supervisorPlistNames(t, realDir) {
			path := filepath.Join(realDir, name)
			data, err := os.ReadFile(path)
			if before[name] || err != nil || !strings.Contains(string(data), gcHome) {
				continue
			}
			if err := os.Remove(path); err == nil {
				t.Errorf("removed leaked plist %s", path)
			}
		}
	})

	shimDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(shimDir, "launchctl"), []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatalf("writing permissive launchctl shim: %v", err)
	}
	env = replaceEnv(env, "PATH", prependPath(shimDir, parseEnvList(env)["PATH"]))

	out, err := runCommand("", env, 30*time.Second, gcBinary, "supervisor", "install")
	if err != nil {
		t.Fatalf("gc supervisor install: %v\n%s", err, out)
	}

	injected := supervisorPlistNames(t, filepath.Join(gcHome, "LaunchAgents"))
	if len(injected) != 1 {
		t.Fatalf("plists under the injected dir = %v, want exactly one\n%s", injected, out)
	}
	for name := range supervisorPlistNames(t, realDir) {
		if !before[name] {
			t.Fatalf("gc supervisor install wrote %s into the real LaunchAgents dir %s", name, realDir)
		}
	}
}

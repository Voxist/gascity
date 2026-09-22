package main

import (
	"bytes"
	"errors"
	"os"
	"os/user"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/supervisor"
)

// Regression guards for ga-32bb2: tests installed launchd supervisors into the
// operator's real ~/Library/LaunchAgents (GC_HOME isolation does not reach
// launchd), launchd reloaded them after their GC_HOME was deleted, and each
// one fell back to the default API port — the production supervisor's port.

// useDefaultSupervisorLaunchAgentsDir restores the production resolver (env
// override, else $HOME/Library/LaunchAgents) for a test that sets HOME to a
// temp dir and asserts on the default path. TestMain pins the seam to the test
// temp root; this undoes that for the calling test only.
func useDefaultSupervisorLaunchAgentsDir(t *testing.T) {
	t.Helper()
	t.Setenv(supervisorLaunchAgentsDirEnv, "")
	prev := supervisorLaunchAgentsDir
	supervisorLaunchAgentsDir = defaultSupervisorLaunchAgentsDir
	t.Cleanup(func() { supervisorLaunchAgentsDir = prev })
}

// realLaunchAgentsDir is the operator's real LaunchAgents dir, resolved from
// the passwd database so a test-scoped HOME cannot mask it.
func realLaunchAgentsDir(t *testing.T) string {
	t.Helper()
	lu, err := user.LookupId(strconv.Itoa(os.Getuid()))
	if err != nil || strings.TrimSpace(lu.HomeDir) == "" {
		t.Skipf("cannot resolve the real home dir: %v", err)
	}
	return filepath.Join(lu.HomeDir, "Library", "LaunchAgents")
}

func pathIsUnder(path, dir string) bool {
	rel, err := filepath.Rel(filepath.Clean(dir), filepath.Clean(path))
	return err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}

// TestSupervisorLaunchdPathsNeverResolveToRealLaunchAgentsInTests is the
// invariant that would have caught ga-32bb2: even with HOME pinned to the real
// home (which the install path's HOME-override guard requires), every launchd
// plist path the package resolves stays under the harness-injected dir.
func TestSupervisorLaunchdPathsNeverResolveToRealLaunchAgentsInTests(t *testing.T) {
	pinRealHome(t)
	t.Setenv("GC_HOME", t.TempDir())
	realDir := realLaunchAgentsDir(t)

	for name, path := range map[string]string{
		"dir":    supervisorLaunchAgentsDir(),
		"plist":  supervisorLaunchdPlistPath(),
		"legacy": legacySupervisorLaunchdPlistPath(),
	} {
		if pathIsUnder(path, realDir) {
			t.Errorf("%s path %q resolves under the real LaunchAgents dir %q; tests must never reach it", name, path, realDir)
		}
	}
}

func TestSupervisorLaunchAgentsDirHonorsOverride(t *testing.T) {
	useDefaultSupervisorLaunchAgentsDir(t)
	dir := t.TempDir()
	t.Setenv(supervisorLaunchAgentsDirEnv, dir)
	t.Setenv("GC_HOME", t.TempDir())

	if got := supervisorLaunchAgentsDir(); got != dir {
		t.Fatalf("supervisorLaunchAgentsDir() = %q, want %q", got, dir)
	}
	if got, want := supervisorLaunchdPlistPath(), filepath.Join(dir, supervisorLaunchdLabel()+".plist"); got != want {
		t.Fatalf("supervisorLaunchdPlistPath() = %q, want %q", got, want)
	}
	if got, want := legacySupervisorLaunchdPlistPath(), filepath.Join(dir, defaultSupervisorLaunchdLabel+".plist"); got != want {
		t.Fatalf("legacySupervisorLaunchdPlistPath() = %q, want %q", got, want)
	}
}

// TestSupervisorLaunchAgentsDirDefaultsToHome pins production behavior: with
// no override the plist lives in $HOME/Library/LaunchAgents.
func TestSupervisorLaunchAgentsDirDefaultsToHome(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	useDefaultSupervisorLaunchAgentsDir(t)

	if got, want := supervisorLaunchAgentsDir(), filepath.Join(home, "Library", "LaunchAgents"); got != want {
		t.Fatalf("supervisorLaunchAgentsDir() = %q, want %q", got, want)
	}
}

// TestInstallSupervisorLaunchdWritesOnlyUnderInjectedDir drives the real
// install path with the real HOME and asserts the plist lands in the injected
// dir and nowhere in the operator's LaunchAgents.
func TestInstallSupervisorLaunchdWritesOnlyUnderInjectedDir(t *testing.T) {
	pinRealHome(t)
	t.Setenv("GC_HOME", t.TempDir())
	t.Setenv("XDG_RUNTIME_DIR", t.TempDir())
	// Drive the production resolver through its env override, the path the
	// integration and acceptance harnesses use for gc subprocesses.
	useDefaultSupervisorLaunchAgentsDir(t)
	injected := t.TempDir()
	t.Setenv(supervisorLaunchAgentsDirEnv, injected)

	label := supervisorLaunchdLabel()
	realPlist := filepath.Join(realLaunchAgentsDir(t), label+".plist")
	if _, err := os.Stat(realPlist); err == nil {
		t.Fatalf("precondition: %s already exists", realPlist)
	}
	// Registered before install so a failed assertion still removes anything
	// this test planted in the real dir; the label is unique to this GC_HOME.
	t.Cleanup(func() {
		if err := os.Remove(realPlist); err == nil {
			t.Errorf("removed leaked plist %s", realPlist)
		} else if !errors.Is(err, os.ErrNotExist) {
			t.Errorf("removing %s: %v", realPlist, err)
		}
	})

	var launchctlCalls [][]string
	oldRun := supervisorLaunchctlRun
	supervisorLaunchctlRun = func(args ...string) error {
		launchctlCalls = append(launchctlCalls, append([]string(nil), args...))
		return nil
	}
	t.Cleanup(func() { supervisorLaunchctlRun = oldRun })

	data, err := buildSupervisorServiceData()
	if err != nil {
		t.Fatalf("buildSupervisorServiceData: %v", err)
	}
	var stdout, stderr bytes.Buffer
	if code := installSupervisorLaunchd(data, &stdout, &stderr); code != 0 {
		t.Fatalf("installSupervisorLaunchd = %d; stderr=%q", code, stderr.String())
	}
	t.Cleanup(func() { _ = os.Remove(filepath.Join(injected, label+".plist")) })

	if _, err := os.Stat(filepath.Join(injected, label+".plist")); err != nil {
		t.Fatalf("plist not written under the injected dir: %v", err)
	}
	if _, err := os.Stat(realPlist); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("install touched the real LaunchAgents: stat %s = %v", realPlist, err)
	}
	for _, call := range launchctlCalls {
		for _, arg := range call {
			if strings.HasSuffix(arg, ".plist") && !pathIsUnder(arg, injected) {
				t.Fatalf("launchctl %v references a plist outside the injected dir", call)
			}
		}
	}
}

// TestTestHarnessDefaultPortError: a test supervisor whose config does not pin
// a port must refuse to start rather than fall back to the production default
// port; an explicit port, or no test harness, is left alone.
func TestTestHarnessDefaultPortError(t *testing.T) {
	t.Setenv("GC_HOME", t.TempDir())

	t.Setenv(managedDoltTestModeEnv, "1")
	err := testHarnessDefaultPortError(supervisor.Section{})
	if err == nil || !strings.Contains(err.Error(), "refusing to bind the default API port 8372") {
		t.Fatalf("unset port under a test harness: err = %v, want default-port refusal", err)
	}
	if err := testHarnessDefaultPortError(supervisor.Section{Port: 41234}); err != nil {
		t.Fatalf("explicit port under a test harness: err = %v, want nil", err)
	}

	t.Setenv(managedDoltTestModeEnv, "")
	if err := testHarnessDefaultPortError(supervisor.Section{}); err != nil {
		t.Fatalf("unset port outside a test harness (production): err = %v, want nil", err)
	}
}

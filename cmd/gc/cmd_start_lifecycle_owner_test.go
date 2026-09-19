package main

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"

	"github.com/gastownhall/gascity/internal/citylayout"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/runtime"
)

// startLifecycleOwnerTestCity scaffolds a file-provider city whose session
// provider is a fake, so doStartStandalone runs end to end in-process.
func startLifecycleOwnerTestCity(t *testing.T) string {
	t.Helper()
	cityPath := t.TempDir()
	clearInheritedBeadsEnv(t)
	requireNoLeakedDoltAfterForPaths(t, cityPath)
	t.Chdir(t.TempDir())
	if err := os.MkdirAll(filepath.Join(cityPath, citylayout.RuntimeRoot), 0o755); err != nil {
		t.Fatalf("scaffold runtime root: %v", err)
	}
	cityTOML := "[workspace]\nname = \"owner-city\"\n\n[beads]\nprovider = \"file\"\n"
	if err := os.WriteFile(filepath.Join(cityPath, "city.toml"), []byte(cityTOML), 0o644); err != nil {
		t.Fatalf("write city.toml: %v", err)
	}
	oldBuild := buildSessionProviderByName
	t.Cleanup(func() { buildSessionProviderByName = oldBuild })
	buildSessionProviderByName = func(_ *config.City, _ string, _ config.SessionConfig, _, _ string) (runtime.Provider, error) {
		return runtime.NewFake(), nil
	}
	return cityPath
}

// TestStartStandaloneClaimsTheManagedDoltLifecycle pins the third claimant
// of the managed Dolt lifecycle (ga-fjr5f): `gc start` in standalone mode is
// the explicit lifecycle command, so the process running it must be allowed to
// recover the server it starts.
func TestStartStandaloneClaimsTheManagedDoltLifecycle(t *testing.T) {
	ownManagedDoltLifecycleForTest(t, false)
	cityPath := startLifecycleOwnerTestCity(t)

	var stdout, stderr bytes.Buffer
	if code := doStartStandalone([]string{cityPath}, false, &stdout, &stderr); code != 0 {
		t.Fatalf("doStartStandalone exit = %d, want 0\nstdout:\n%s\nstderr:\n%s", code, stdout.String(), stderr.String())
	}
	if !managedDoltImplicitRecoveryAllowed() {
		t.Fatal("gc start did not claim the managed Dolt lifecycle; the process that starts the " +
			"server could not recover it (ga-fjr5f)")
	}
}

// TestStartDryRunNeitherClaimsNorStartsTheBeadStore pins review finding F2:
// `gc start --dry-run` is a preview any caller may run — an agent included —
// so it must not claim the managed Dolt lifecycle, and it must not start the
// bead store it only describes.
func TestStartDryRunNeitherClaimsNorStartsTheBeadStore(t *testing.T) {
	ownManagedDoltLifecycleForTest(t, false)
	cityPath := startLifecycleOwnerTestCity(t)
	prevDryRun := dryRunMode
	dryRunMode = true
	t.Cleanup(func() { dryRunMode = prevDryRun })

	var stdout, stderr bytes.Buffer
	if code := doStartStandalone([]string{cityPath}, false, &stdout, &stderr); code != 0 {
		t.Fatalf("doStartStandalone --dry-run exit = %d, want 0\nstdout:\n%s\nstderr:\n%s", code, stdout.String(), stderr.String())
	}
	if managedDoltImplicitRecoveryAllowed() {
		t.Fatal("gc start --dry-run claimed the managed Dolt lifecycle; an agent running a preview " +
			"would become able to restart the city's server in its own session (ga-fjr5f)")
	}
	if _, err := os.Stat(filepath.Join(cityPath, ".beads")); !os.IsNotExist(err) {
		t.Fatalf("gc start --dry-run started the bead store (.beads stat err = %v); a preview has no side effects", err)
	}
}

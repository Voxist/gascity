package main

import (
	"errors"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/config"
)

// Tests for the degrade-not-fatal rig init behavior introduced by vp-cz7o.13.
// A single rig's bead-store init failure (e.g. ensure-project-id L1/L3
// mismatch) must not abort the fleet — it must be logged as offline and
// healthy rigs must proceed normally.

func TestStartBeadsLifecycleDegradeNotFatal_OneRigFails(t *testing.T) {
	cityPath := t.TempDir()
	goodRigPath := t.TempDir()
	badRigPath := t.TempDir()

	origEnsure := startBeadsLifecycleEnsureProvider
	origInit := startBeadsLifecycleInitAndHookDir
	t.Cleanup(func() {
		startBeadsLifecycleEnsureProvider = origEnsure
		startBeadsLifecycleInitAndHookDir = origInit
	})

	startBeadsLifecycleEnsureProvider = func(_ string) error { return nil }

	var initializedDirs []string
	startBeadsLifecycleInitAndHookDir = func(_, dir, _ string) error {
		if dir == badRigPath {
			return errors.New("ensure-project-id: L1/L3 project_id mismatch (vp-cz7o.13 reproducer)")
		}
		initializedDirs = append(initializedDirs, dir)
		return nil
	}

	cfg := &config.City{
		Workspace: config.Workspace{Name: "test-city"},
		Rigs: []config.Rig{
			{Name: "good-rig", Path: goodRigPath},
			{Name: "bad-rig", Path: badRigPath},
		},
	}

	var stderrBuf strings.Builder
	err := startBeadsLifecycle(cityPath, "test-city", cfg, &stderrBuf)

	if err != nil {
		t.Fatalf("startBeadsLifecycle returned error, want nil (degrade-not-fatal): %v", err)
	}

	var foundGood bool
	for _, d := range initializedDirs {
		if d == goodRigPath {
			foundGood = true
		}
	}
	if !foundGood {
		t.Error("good-rig was not initialized; want it to proceed despite bad-rig failure")
	}

	out := stderrBuf.String()
	if !strings.Contains(out, "bad-rig") {
		t.Errorf("stderr = %q, want 'bad-rig' mentioned", out)
	}
	if !strings.Contains(out, "offline") {
		t.Errorf("stderr = %q, want 'offline' mentioned", out)
	}
}

func TestStartBeadsLifecycleDegradeNotFatal_AllRigsFail(t *testing.T) {
	cityPath := t.TempDir()
	badRigPath := t.TempDir()

	origEnsure := startBeadsLifecycleEnsureProvider
	origInit := startBeadsLifecycleInitAndHookDir
	t.Cleanup(func() {
		startBeadsLifecycleEnsureProvider = origEnsure
		startBeadsLifecycleInitAndHookDir = origInit
	})

	startBeadsLifecycleEnsureProvider = func(_ string) error { return nil }
	startBeadsLifecycleInitAndHookDir = func(_, dir, _ string) error {
		if dir == badRigPath {
			return errors.New("ensure-project-id: mismatch")
		}
		return nil // city init succeeds
	}

	cfg := &config.City{
		Workspace: config.Workspace{Name: "test-city"},
		Rigs:      []config.Rig{{Name: "bad-rig", Path: badRigPath}},
	}

	var stderrBuf strings.Builder
	err := startBeadsLifecycle(cityPath, "test-city", cfg, &stderrBuf)

	if err != nil {
		t.Fatalf("startBeadsLifecycle returned error even with all rigs degraded, want nil: %v", err)
	}
	if !strings.Contains(stderrBuf.String(), "bad-rig") {
		t.Errorf("stderr = %q, want 'bad-rig' mentioned", stderrBuf.String())
	}
}

func TestStartBeadsLifecycleDegradeNotFatal_CityInitFail_StillPropagates(t *testing.T) {
	// City-level init failure IS still fatal — only rig failures are demoted.
	cityPath := t.TempDir()

	origEnsure := startBeadsLifecycleEnsureProvider
	origInit := startBeadsLifecycleInitAndHookDir
	t.Cleanup(func() {
		startBeadsLifecycleEnsureProvider = origEnsure
		startBeadsLifecycleInitAndHookDir = origInit
	})

	startBeadsLifecycleEnsureProvider = func(_ string) error { return nil }
	startBeadsLifecycleInitAndHookDir = func(_, dir, _ string) error {
		if dir == cityPath {
			return errors.New("city bead store init failed")
		}
		return nil
	}

	cfg := &config.City{Workspace: config.Workspace{Name: "test-city"}}
	err := startBeadsLifecycle(cityPath, "test-city", cfg, &strings.Builder{})
	if err == nil {
		t.Fatal("startBeadsLifecycle returned nil for city-level failure, want error")
	}
	if !strings.Contains(err.Error(), "init city beads") {
		t.Errorf("err = %v, want 'init city beads' in message", err)
	}
}

// ---------------------------------------------------------------------------
// rigFilteredCityConfig unit tests
// ---------------------------------------------------------------------------

func TestRigFilteredCityConfig_ExcludesNamedRig(t *testing.T) {
	cfg := &config.City{
		Rigs: []config.Rig{
			{Name: "alpha", Path: "/a"},
			{Name: "beta", Path: "/b"},
			{Name: "gamma", Path: "/c"},
		},
	}
	filtered := rigFilteredCityConfig(cfg, []string{"beta"})
	if len(filtered.Rigs) != 2 {
		t.Fatalf("want 2 rigs, got %d: %v", len(filtered.Rigs), filtered.Rigs)
	}
	for _, r := range filtered.Rigs {
		if r.Name == "beta" {
			t.Error("'beta' should have been filtered out")
		}
	}
}

func TestRigFilteredCityConfig_OriginalUnmodified(t *testing.T) {
	cfg := &config.City{
		Rigs: []config.Rig{
			{Name: "alpha", Path: "/a"},
			{Name: "beta", Path: "/b"},
		},
	}
	_ = rigFilteredCityConfig(cfg, []string{"alpha"})
	if len(cfg.Rigs) != 2 {
		t.Errorf("original cfg.Rigs was modified, want 2 rigs got %d", len(cfg.Rigs))
	}
}

func TestRigFilteredCityConfig_AllDegraded(t *testing.T) {
	cfg := &config.City{
		Rigs: []config.Rig{{Name: "only", Path: "/r"}},
	}
	filtered := rigFilteredCityConfig(cfg, []string{"only"})
	if len(filtered.Rigs) != 0 {
		t.Errorf("want 0 rigs after filtering all, got %d", len(filtered.Rigs))
	}
}

func TestRigFilteredCityConfig_NoDegraded_SameRigs(t *testing.T) {
	cfg := &config.City{
		Rigs: []config.Rig{
			{Name: "alpha", Path: "/a"},
			{Name: "beta", Path: "/b"},
		},
	}
	filtered := rigFilteredCityConfig(cfg, nil)
	if len(filtered.Rigs) != 2 {
		t.Errorf("want 2 rigs (none degraded), got %d", len(filtered.Rigs))
	}
}

func TestRigFilteredCityConfig_PreservesOtherFields(t *testing.T) {
	cfg := &config.City{
		Workspace: config.Workspace{Name: "my-city"},
		Rigs:      []config.Rig{{Name: "r", Path: "/r"}},
	}
	filtered := rigFilteredCityConfig(cfg, []string{"r"})
	if filtered.Workspace.Name != "my-city" {
		t.Errorf("Workspace.Name = %q, want %q", filtered.Workspace.Name, "my-city")
	}
}

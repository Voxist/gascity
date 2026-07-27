package main

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/gastownhall/gascity/internal/configedit"
)

// TestAssertRigPathWithinCity pins the containment property after the v1.4.0
// resync collapsed the old two-pass check (lexical, then symlink-aware) into a
// single pass that canonicalizes both sides.
//
// The old first pass compared a raw cityPath against an already-canonicalized
// target, which produced false rejections once resolveStoreScopeRoot began
// resolving symlinks. Collapsing the passes fixed that, but it also removed a
// defense-in-depth layer guarding a remote-API dir-create/file-plant primitive,
// and nothing named pinned the rejection behavior. This test does.
func TestAssertRigPathWithinCity(t *testing.T) {
	city := t.TempDir()
	outside := t.TempDir()
	if err := os.MkdirAll(filepath.Join(city, "rigs"), 0o755); err != nil {
		t.Fatal(err)
	}

	t.Run("contained existing path is allowed", func(t *testing.T) {
		target := filepath.Join(city, "rigs")
		if err := assertRigPathWithinCity(city, resolveStoreScopeRoot(city, target)); err != nil {
			t.Fatalf("contained path rejected: %v", err)
		}
	})

	// The regression the resync introduced: the rig dir does not exist yet, so
	// EvalSymlinks fails on it and only the city side could be canonicalized.
	t.Run("contained absent path is allowed", func(t *testing.T) {
		target := filepath.Join(city, "rigs", "not-created-yet")
		if err := assertRigPathWithinCity(city, resolveStoreScopeRoot(city, target)); err != nil {
			t.Fatalf("absent contained path rejected: %v", err)
		}
	})

	t.Run("dot-dot escape is rejected", func(t *testing.T) {
		target := filepath.Join(city, "..", "escaped")
		err := assertRigPathWithinCity(city, resolveStoreScopeRoot(city, target))
		if err == nil || !errors.Is(err, configedit.ErrValidation) {
			t.Fatalf("../ escape not rejected: %v", err)
		}
	})

	t.Run("absolute path outside the city is rejected", func(t *testing.T) {
		err := assertRigPathWithinCity(city, resolveStoreScopeRoot(city, outside))
		if err == nil || !errors.Is(err, configedit.ErrValidation) {
			t.Fatalf("outside absolute path not rejected: %v", err)
		}
	})

	// The case the symlink-aware pass was originally added for: a "../"-free
	// path that still escapes through a symlinked ancestor.
	t.Run("escape through a symlinked ancestor is rejected", func(t *testing.T) {
		link := filepath.Join(city, "link")
		if err := os.Symlink(outside, link); err != nil {
			t.Skipf("symlink unsupported: %v", err)
		}
		target := filepath.Join(link, "rig")
		err := assertRigPathWithinCity(city, resolveStoreScopeRoot(city, target))
		if err == nil || !errors.Is(err, configedit.ErrValidation) {
			t.Fatalf("symlinked-ancestor escape not rejected: %v", err)
		}
	})
}

package main

import (
	"bytes"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/fsnotify/fsnotify"
	"github.com/gastownhall/gascity/internal/config"
)

// TestIsChmodOnlyConfigWatchEvent pins the op filter: a bare Chmod is
// dropped, and any op that combines Chmod with a content or directory change
// still reloads.
func TestIsChmodOnlyConfigWatchEvent(t *testing.T) {
	for _, tc := range []struct {
		op   fsnotify.Op
		want bool
	}{
		{fsnotify.Chmod, true},
		{fsnotify.Write, false},
		{fsnotify.Create, false},
		{fsnotify.Rename, false},
		{fsnotify.Remove, false},
		{fsnotify.Chmod | fsnotify.Write, false},
		{fsnotify.Chmod | fsnotify.Create, false},
		{fsnotify.Chmod | fsnotify.Rename, false},
		{fsnotify.Chmod | fsnotify.Remove, false},
	} {
		if got := isChmodOnlyConfigWatchEvent(tc.op); got != tc.want {
			t.Errorf("isChmodOnlyConfigWatchEvent(%v) = %v, want %v", tc.op, got, tc.want)
		}
	}
}

// TestWatchConfigDirs_IgnoresChmodOnlyEvents pins vp-h1jgt end to end on the
// real fsnotify backend: a mode or timestamp change on an unchanged city.toml
// does not mark config dirty, and a content write after it still does.
func TestWatchConfigDirs_IgnoresChmodOnlyEvents(t *testing.T) {
	dir := t.TempDir()
	tomlPath := filepath.Join(dir, "city.toml")
	if err := os.WriteFile(tomlPath, []byte("[workspace]\nname = \"test\"\n"), 0o644); err != nil {
		t.Fatalf("WriteFile(city.toml): %v", err)
	}

	var dirty atomic.Bool
	pokeCh := make(chan struct{}, 1)
	var stderr bytes.Buffer
	cleanup := watchConfigTargets([]config.WatchTarget{{Path: dir, DiscoverConventions: true}}, testConfigDebounce, &dirty, newLegacyWake(pokeCh, nil), &stderr)
	defer cleanup()

	select {
	case <-pokeCh:
	default:
	}
	dirty.Store(false)

	if err := os.Chmod(tomlPath, 0o600); err != nil {
		t.Fatalf("Chmod(city.toml): %v", err)
	}
	when := time.Now().Add(-time.Hour)
	if err := os.Chtimes(tomlPath, when, when); err != nil {
		t.Fatalf("Chtimes(city.toml): %v", err)
	}
	// Negative-assertion window: asserts no watcher poke arrives for a
	// Chmod-only event (vp-h1jgt exclusion).
	select {
	case <-pokeCh:
		t.Fatalf("unexpected watcher poke after chmod/chtimes on city.toml; stderr=%q", stderr.String())
	case <-time.After(250 * time.Millisecond):
	}
	if dirty.Load() {
		t.Fatalf("dirty flag set after chmod/chtimes on city.toml; stderr=%q", stderr.String())
	}

	if err := os.WriteFile(tomlPath, []byte("[workspace]\nname = \"test-v2\"\n"), 0o644); err != nil {
		t.Fatalf("rewrite city.toml: %v", err)
	}
	awaitClose(t, pokeCh, "watcher poke after city.toml rewrite following chmod")
	if !dirty.Load() {
		t.Fatalf("dirty flag not set after city.toml rewrite; stderr=%q", stderr.String())
	}
}

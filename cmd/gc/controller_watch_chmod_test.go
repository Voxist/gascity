package main

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/fsnotify/fsnotify"
	"github.com/gastownhall/gascity/internal/config"
)

// TestIsChmodOnlyConfigWatchEvent pins which ops go through the attribute
// check: only a bare Chmod; any op that combines Chmod with a content or
// directory change always reloads.
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

// TestConfigWatchAttrStampsDropOnlyUnchangedFiles pins the check a bare Chmod
// event goes through: it is noise only when the file's ctime, mtime, size and
// mode match the last stamp, which a page-in leaves alone. A truncate with no
// write, a file turned unreadable or readable, a touch, an unseeded path and
// a removed file all count as changes (vp-h1jgt).
func TestConfigWatchAttrStampsDropOnlyUnchangedFiles(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "city.toml")
	if err := os.WriteFile(path, []byte("[workspace]\nname = \"test\"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	attrs := newConfigWatchAttrStamps()
	attrs.seed(dir)
	expect := func(step string, want bool) {
		t.Helper()
		if got := attrs.unchanged(path); got != want {
			t.Fatalf("%s: unchanged = %v, want %v", step, got, want)
		}
	}
	expect("seeded, untouched", true)
	expect("untouched again", true)

	if err := os.Truncate(path, 0); err != nil {
		t.Fatal(err)
	}
	expect("truncated to zero", false)
	expect("after the truncate was recorded", true)

	if err := os.Chmod(path, 0o000); err != nil {
		t.Fatal(err)
	}
	expect("made unreadable", false)
	if err := os.Chmod(path, 0o644); err != nil {
		t.Fatal(err)
	}
	expect("made readable again", false)

	when := time.Now().Add(-time.Hour)
	if err := os.Chtimes(path, when, when); err != nil {
		t.Fatal(err)
	}
	expect("touched", false)

	fresh := filepath.Join(dir, "pack.toml")
	if err := os.WriteFile(fresh, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	if attrs.unchanged(fresh) {
		t.Fatal("unchanged(unseeded path) = true, want false")
	}
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	expect("removed", false)
}

// TestConfigWatchAttrStampsStayBounded pins the record's size limit: past it
// the record starts over instead of growing.
func TestConfigWatchAttrStampsStayBounded(t *testing.T) {
	path := filepath.Join(t.TempDir(), "city.toml")
	if err := os.WriteFile(path, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	attrs := newConfigWatchAttrStamps()
	for i := 0; i < maxConfigWatchAttrStamps; i++ {
		attrs.seen[fmt.Sprintf("/synthetic/%d", i)] = configWatchAttrStamp{}
	}
	attrs.record(path)
	if len(attrs.seen) != 1 {
		t.Fatalf("len(seen) = %d after recording past the limit, want 1 (the record starts over)", len(attrs.seen))
	}
}

// TestWatchConfigDirs_ChmodOnlyAttributeChangesReload pins vp-h1jgt end to end
// on the real fsnotify backend: changes that can reach the watcher as a bare
// Chmod (kqueue reports a truncate with no write that way, and chmod and
// utimes always) still mark config dirty and poke.
func TestWatchConfigDirs_ChmodOnlyAttributeChangesReload(t *testing.T) {
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

	when := time.Now().Add(-time.Hour)
	for _, step := range []struct {
		name   string
		change func() error
	}{
		{"truncate to zero", func() error { return os.Truncate(tomlPath, 0) }},
		{"made unreadable", func() error { return os.Chmod(tomlPath, 0o000) }},
		{"made readable", func() error { return os.Chmod(tomlPath, 0o644) }},
		{"touched", func() error { return os.Chtimes(tomlPath, when, when) }},
	} {
		select {
		case <-pokeCh:
		default:
		}
		dirty.Store(false)
		if err := step.change(); err != nil {
			t.Fatalf("%s: %v", step.name, err)
		}
		awaitClose(t, pokeCh, "watcher poke after "+step.name)
		if !dirty.Load() {
			t.Fatalf("dirty flag not set after %s; stderr=%q", step.name, stderr.String())
		}
	}
}

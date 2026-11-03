package main

import (
	"bytes"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/config"
)

// TestShouldIgnoreConfigWatchEventIgnoresVCSAgentAndScratchDirs: .git, .omc
// and .scratch churn constantly in a live city root (every git command, every
// agent tool write, every scratch file) and none of them is config. Matching is
// by path component, so look-alikes such as .gitignore or .github still count.
func TestShouldIgnoreConfigWatchEventIgnoresVCSAgentAndScratchDirs(t *testing.T) {
	root := filepath.Join(string(filepath.Separator), "city")
	for _, path := range []string{
		filepath.Join(root, ".git"),
		filepath.Join(root, ".git", "index.lock"),
		filepath.Join(root, ".omc"),
		filepath.Join(root, ".omc", "state", "hud.json"),
		filepath.Join(root, ".scratch"),
		filepath.Join(root, ".scratch", "session", "probe.go"),
		filepath.Join(root, ".gc", "events.jsonl"),
		filepath.Join(root, ".beads", "issues.jsonl"),
	} {
		if !shouldIgnoreConfigWatchEvent(path) {
			t.Errorf("shouldIgnoreConfigWatchEvent(%q) = false, want true", path)
		}
	}
	for _, path := range []string{
		filepath.Join(root, "city.toml"),
		filepath.Join(root, ".gitignore"),
		filepath.Join(root, ".github", "workflows", "ci.yml"),
		filepath.Join(root, ".scratchpad.toml"),
		filepath.Join(root, "agents", "mayor", "prompt.md"),
	} {
		if shouldIgnoreConfigWatchEvent(path) {
			t.Errorf("shouldIgnoreConfigWatchEvent(%q) = true, want false", path)
		}
	}
}

// TestWatchConfigDirs_CityRootIgnoresFilesThatAreNotConfigInputs: the city root
// is watched shallowly, and fsnotify reports every entry in it. A root that has
// collected scratch files (one live city had ~1,900) otherwise wakes the
// reloader on each of their writes, and every reload re-parses the whole
// config. With Inputs listed, only those files, convention roots and anything
// under a recursive root count.
func TestWatchConfigDirs_CityRootIgnoresFilesThatAreNotConfigInputs(t *testing.T) {
	dir := t.TempDir()
	tomlPath := filepath.Join(dir, "city.toml")
	if err := os.WriteFile(tomlPath, []byte("[workspace]\nname = \"test\"\n"), 0o644); err != nil {
		t.Fatalf("WriteFile(city.toml): %v", err)
	}
	stray := filepath.Join(dir, ".pr42_review_head.go")
	if err := os.WriteFile(stray, []byte("package scratch\n"), 0o644); err != nil {
		t.Fatalf("seed stray file: %v", err)
	}

	target := config.WatchTarget{
		Path:                dir,
		DiscoverConventions: true,
		Inputs: []string{
			tomlPath,
			filepath.Join(dir, "pack.toml"),
			filepath.Join(dir, "packs.lock"),
		},
	}
	var dirty atomic.Bool
	pokeCh := make(chan struct{}, 1)
	var stderr bytes.Buffer
	cleanup := watchConfigTargets([]config.WatchTarget{target}, testConfigDebounce, &dirty, newLegacyWake(pokeCh, nil), &stderr)
	defer cleanup()

	drain := func() {
		select {
		case <-pokeCh:
		default:
		}
		dirty.Store(false)
	}
	expectQuiet := func(what string) {
		t.Helper()
		select {
		case <-pokeCh:
			t.Fatalf("unexpected watcher poke after %s; stderr=%q", what, stderr.String())
		case <-time.After(250 * time.Millisecond):
		}
		if dirty.Load() {
			t.Fatalf("dirty flag set after %s; stderr=%q", what, stderr.String())
		}
	}
	expectPoke := func(what string) {
		t.Helper()
		awaitClose(t, pokeCh, "watcher poke after "+what)
		if !dirty.Load() {
			t.Fatalf("dirty flag not set after %s; stderr=%q", what, stderr.String())
		}
	}

	drain()
	if err := os.WriteFile(stray, []byte("package scratch // edited\n"), 0o644); err != nil {
		t.Fatalf("rewrite stray file: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "notes.txt"), []byte("new scratch\n"), 0o644); err != nil {
		t.Fatalf("create scratch file: %v", err)
	}
	expectQuiet("scratch files in the city root changed")

	// A listed input that did not exist yet still counts when it appears.
	drain()
	if err := os.WriteFile(filepath.Join(dir, "pack.toml"), []byte("[pack]\nname = \"test\"\nschema = 2\n"), 0o644); err != nil {
		t.Fatalf("create pack.toml: %v", err)
	}
	expectPoke("pack.toml was created")

	drain()
	if err := os.WriteFile(tomlPath, []byte("[workspace]\nname = \"test-v2\"\n"), 0o644); err != nil {
		t.Fatalf("rewrite city.toml: %v", err)
	}
	expectPoke("city.toml was rewritten")

	// Convention roots are not listed as Inputs but are still config.
	drain()
	if err := os.MkdirAll(filepath.Join(dir, "agents"), 0o755); err != nil {
		t.Fatalf("MkdirAll(agents): %v", err)
	}
	expectPoke("agents/ was created")
}

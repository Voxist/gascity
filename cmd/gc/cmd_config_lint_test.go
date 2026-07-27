package main

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"
)

// gc config lint (vc-quqf): the runtime load degrades unresolvable
// [[patches.agent]] targets to warnings (vc-9wa: one bad patch must not
// brick the city); lint is where that same problem fails loudly pre-commit.

func TestConfigLintFailsOnUnresolvedAgentPatch(t *testing.T) {
	clearGCEnv(t)
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, ".gc"), 0o755); err != nil {
		t.Fatalf("MkdirAll(.gc): %v", err)
	}
	writeCityToml(t, dir, `[workspace]
name = "demo"

[[patches.agent]]
dir = "ghost-rig"
name = "ghost-agent"
suspended = true
`)

	var stdout, stderr bytes.Buffer
	code := run([]string{"config", "lint", "--city", dir}, &stdout, &stderr)
	if code == 0 {
		t.Fatalf("run(config lint) = 0, want non-zero; stdout=%q stderr=%q", stdout.String(), stderr.String())
	}
	got := stderr.String()
	for _, want := range []string{"patches.agent[0]", "ghost-agent", "ghost-rig"} {
		if !bytes.Contains([]byte(got), []byte(want)) {
			t.Errorf("stderr = %q, want it to contain %q", got, want)
		}
	}
}

func TestConfigLintPassesCleanConfig(t *testing.T) {
	clearGCEnv(t)
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, ".gc"), 0o755); err != nil {
		t.Fatalf("MkdirAll(.gc): %v", err)
	}
	writeCityToml(t, dir, "[workspace]\nname = \"demo\"\n")

	var stdout, stderr bytes.Buffer
	code := run([]string{"config", "lint", "--city", dir}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("run(config lint) = %d, want 0; stderr=%q", code, stderr.String())
	}
	if !bytes.Contains(stdout.Bytes(), []byte("ok")) {
		t.Errorf("stdout = %q, want ok line", stdout.String())
	}
}

func TestConfigLintFailsOnLoadError(t *testing.T) {
	clearGCEnv(t)
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, ".gc"), 0o755); err != nil {
		t.Fatalf("MkdirAll(.gc): %v", err)
	}
	writeCityToml(t, dir, "[[[not toml")

	var stdout, stderr bytes.Buffer
	code := run([]string{"config", "lint", "--city", dir}, &stdout, &stderr)
	if code == 0 {
		t.Fatalf("run(config lint) = 0 on unparseable city.toml, want non-zero; stdout=%q", stdout.String())
	}
}

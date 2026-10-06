package main

import (
	"bytes"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

const migrationFreezeMarkerName = "MIGRATION-FREEZE"

// TestManagedCityRefusesImplicitDoltRecoveryInChildProcess runs the test binary
// as a real `gc` process (the testscript re-exec path) with lifecycle intent
// withheld, so the implicit-recovery gate is active exactly as it is in
// production. A stopped or frozen city must be refused before the provider
// script runs, which only a real child can prove.
func TestManagedCityRefusesImplicitDoltRecoveryInChildProcess(t *testing.T) {
	runChildGC := func(t *testing.T, city string, args ...string) (string, int) {
		t.Helper()
		exe, err := os.Executable()
		if err != nil {
			t.Fatal(err)
		}
		bin := filepath.Join(shortSocketTempDir(t, "gc-bin-"), "gc")
		if err := os.Symlink(exe, bin); err != nil {
			t.Fatal(err)
		}
		cmd := exec.Command(bin, args...)
		cmd.Dir = city
		cmd.Env = append(os.Environ(),
			"GC_HOME="+shortSocketTempDir(t, "gc-home-"),
			"XDG_RUNTIME_DIR="+shortSocketTempDir(t, "gc-xdg-"),
			"GC_BEADS=bd",
			"GC_DOLT=managed",
			"GC_SESSION=fake",
			"GC_BOOTSTRAP=skip",
			noLifecycleIntentTestEnv+"=1",
		)
		var out bytes.Buffer
		cmd.Stdout, cmd.Stderr = &out, &out
		code := 0
		if err := cmd.Run(); err != nil {
			var ee *exec.ExitError
			if !errors.As(err, &ee) {
				t.Fatalf("running child gc: %v", err)
			}
			code = ee.ExitCode()
		}
		return out.String(), code
	}

	// providerStubLog installs a provider script that records every invocation
	// and returns the path of its log.
	providerStubLog := func(t *testing.T, city string) string {
		t.Helper()
		log := filepath.Join(t.TempDir(), "provider.log")
		script := gcBeadsBdScriptPath(city)
		if err := os.MkdirAll(filepath.Dir(script), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(script, []byte("#!/bin/sh\necho \"$@\" >> "+log+"\nexit 1\n"), 0o755); err != nil {
			t.Fatal(err)
		}
		return log
	}

	t.Run("stopped", func(t *testing.T) {
		city := shortSocketTempDir(t, "gc-city-")
		writeManagedBdCityFixture(t, city)
		log := providerStubLog(t, city)

		out, code := runChildGC(t, city, "--city", city, "bd", "list")
		if code == 0 {
			t.Fatalf("gc bd list against a stopped managed city succeeded:\n%s", out)
		}
		if !strings.Contains(out, "city is stopped") {
			t.Fatalf("output does not say the city is stopped:\n%s", out)
		}
		if data, err := os.ReadFile(log); err == nil {
			t.Fatalf("provider script ran against a stopped city (start/recover revived Dolt):\n%s", data)
		}
	})

	t.Run("frozen", func(t *testing.T) {
		city := shortSocketTempDir(t, "gc-city-")
		writeManagedBdCityFixture(t, city)
		log := providerStubLog(t, city)
		marker := filepath.Join(city, ".beads", migrationFreezeMarkerName)
		if err := os.WriteFile(marker, []byte("tester\t2026-10-01T00:00:00Z\tschema window\n"), 0o644); err != nil {
			t.Fatal(err)
		}

		out, code := runChildGC(t, city, "--city", city, "bd", "list")
		if code == 0 {
			t.Fatalf("gc bd list against a frozen managed city succeeded:\n%s", out)
		}
		if !strings.Contains(out, "MIGRATION-FREEZE") {
			t.Fatalf("output does not name the freeze:\n%s", out)
		}
		if data, err := os.ReadFile(log); err == nil {
			t.Fatalf("provider script ran against a frozen city:\n%s", data)
		}
	})
}

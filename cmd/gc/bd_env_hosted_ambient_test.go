package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestHostedCityRunnersWithholdAmbientBeadsEnv pins the hosted-city runner
// routing at its two production entry points. Upstream routes both through
// beadsCommandRunnerForHostedCity so the exact hosted beads-workspace binding
// runs bd WITHOUT the inherited BEADS_* namespace (ExecCommandRunnerWithEnv
// WithoutAmbientBeads strips it from the base env and deletes blank
// overrides). The 2026-08-31 merge kept the helper but dropped every
// production call site, so ambient BEADS_* flowed into bd subprocesses on
// hosted cities — e.g. an inherited BEADS_DOLT credential/TLS override
// reaching the hosted binding as an explicit value.
func TestHostedCityRunnersWithholdAmbientBeadsEnv(t *testing.T) {
	disableManagedDoltRecoveryForTest(t)
	clearInheritedCityRoutingEnv(t)
	stubHostedBeadsCredentialExecutable(t, "/opt/current gc/bin/gc")
	t.Setenv(registryCredentialProviderEnv, `["/opt/gasworks","credential-provider"]`)
	cityPath := writeHostedBeadsCity(t, "https://beads.example/workspaces/infra", "gasworks", false)

	// The ambient var a hosted binding must never inherit.
	t.Setenv("BEADS_AMBIENT_PROBE_R4", "leaked")

	// A fake bd that dumps its environment.
	stubDir := t.TempDir()
	stub := filepath.Join(stubDir, "bd")
	if err := os.WriteFile(stub, []byte("#!/bin/sh\nenv\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", stubDir+string(os.PathListSeparator)+os.Getenv("PATH"))

	workDir := t.TempDir()

	t.Run("context runner", func(t *testing.T) {
		out, err := bdContextCommandRunnerForCity(cityPath)(workDir, "bd", "probe")
		if err != nil {
			t.Fatalf("runner: %v\n%s", err, out)
		}
		if strings.Contains(string(out), "BEADS_AMBIENT_PROBE_R4=leaked") {
			t.Fatalf("ambient BEADS_* leaked into the hosted city's bd subprocess via bdContextCommandRunnerForCity")
		}
	})

	t.Run("managed-retry runner", func(t *testing.T) {
		envFn := func(_ string) (map[string]string, error) { return map[string]string{}, nil }
		out, err := bdCommandRunnerWithManagedRetryErr(cityPath, envFn)(workDir, "bd", "probe")
		if err != nil {
			t.Fatalf("runner: %v\n%s", err, out)
		}
		if strings.Contains(string(out), "BEADS_AMBIENT_PROBE_R4=leaked") {
			t.Fatalf("ambient BEADS_* leaked into the hosted city's bd subprocess via bdCommandRunnerWithManagedRetryErr")
		}
	})
}

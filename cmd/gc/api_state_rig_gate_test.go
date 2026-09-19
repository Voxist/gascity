package main

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
)

// TestControllerStateBuildStoresGatesRigCaches pins that every rig cache the
// controller builds reads its scope's transport breaker as its availability
// gate — the join that makes the reconciler skip a degraded rig and serve its
// last-good snapshot. It was wired when the breaker landed (e017841b3) and
// later lost from buildStores, leaving only the city cache gated.
func TestControllerStateBuildStoresGatesRigCaches(t *testing.T) {
	t.Setenv("GC_BEADS", "")
	t.Setenv("GC_BEADS_SCOPE_ROOT", "")

	prevOpen := controllerStateOpenRigStoreAtForCity
	t.Cleanup(func() { controllerStateOpenRigStoreAtForCity = prevOpen })

	cityDir := t.TempDir()
	rigDir := filepath.Join(cityDir, "frontend")
	if err := os.MkdirAll(filepath.Join(rigDir, ".beads"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cityDir, "city.toml"), []byte(`[workspace]
name = "demo"

[beads]
provider = "file"

[beads.resilience]
consecutive_failures = 3
open_base = "1h"
open_max = "1h"
`), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(rigDir, ".beads", "metadata.json"), []byte(`{"database":"dolt","backend":"dolt","dolt_mode":"embedded","dolt_database":"fe"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	controllerStateOpenRigStoreAtForCity = func(_ context.Context, _ beads.StoreOpenOptions) (beads.StoreOpenResult, error) {
		return beads.StoreOpenResult{
			Store:      beads.NewMemStore(),
			Diagnostic: beads.BeadsDiagnostic{Store: "NativeDoltStore", NativeStoreEligible: true},
		}, nil
	}
	cfg := &config.City{
		Workspace: config.Workspace{Name: "demo"},
		Rigs:      []config.Rig{{Name: "frontend", Path: rigDir, Prefix: "fe"}},
	}

	cs := &controllerState{cityPath: cityDir, cfg: cfg}
	stores := cs.buildStores(cfg)
	cached, ok := underlyingPolicyStoreForTest(stores["frontend"]).(*beads.CachingStore)
	if !ok {
		t.Fatalf("frontend store = %T, want caching store", underlyingPolicyStoreForTest(stores["frontend"]))
	}
	if cached.Degraded() {
		t.Fatal("precondition: a fresh rig cache reads as degraded")
	}

	degradeScope(t, cityDir, rigDir)
	if !cached.Degraded() {
		t.Fatal("the rig scope's breaker is open but its cache does not read as degraded — " +
			"the rig cache has no availability gate, so its reconciler keeps paying for a " +
			"scope the breaker already declared unavailable")
	}
}

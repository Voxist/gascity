package main

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
)

// vc-ny00 relies on the scope's transport breaker (internal/resilience,
// [beads.resilience]) as the ONE degrade/skip decision. These tests pin the
// two joins that reliance needs and that no other test reaches.

// TestTickBoundReadTimeoutCountsAgainstTheScopeBreaker is the #118
// (ga-2bo4m) property for L1's bound: a tick-context bd read killed at the
// tick read bound — not at the 120s non-tick bound — is a wedged-backend
// timeout, so it counts against the scope's transport breaker, and the scope
// fails fast once the breaker trips.
//
// It is driven through the real bd runner AND the real exec layer, against a
// bd that never answers, because the property spans both: the exec layer
// must reap the read at the tick bound with the per-command timeout shape
// (not the caller-deadline shape, which is deliberately not counted), and the
// runner must classify that shape as a transport failure.
func TestTickBoundReadTimeoutCountsAgainstTheScopeBreaker(t *testing.T) {
	t.Setenv("GC_BEADS", "bd")
	t.Setenv("BD_BIN", "")
	cityPath := writeBreakerTestCity(t, "")

	binDir := t.TempDir()
	callsFile := filepath.Join(binDir, "calls")
	script := "#!/bin/sh\necho call >> '" + callsFile + "'\nexec sleep 30\n"
	if err := os.WriteFile(filepath.Join(binDir, "bd"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))
	origRecover := recoverManagedBDCommand
	recoverManagedBDCommand = func(string) error { return nil }
	t.Cleanup(func() { recoverManagedBDCommand = origRecover })

	const tickBound = 300 * time.Millisecond
	beads.SetTickReadTimeoutForTest(t, tickBound)
	prev := beads.SetReconcilerTickTrigger("patrol")
	defer beads.RestoreReconcilerTickTrigger(prev)

	runner := breakerTestRunner(cityPath)
	scope := t.TempDir()
	for i := 0; i < 3; i++ {
		start := time.Now()
		_, err := runner(scope, "bd", "list", "--json")
		elapsed := time.Since(start)
		if err == nil || errors.Is(err, beads.ErrStoreUnavailable) {
			t.Fatalf("read %d: err = %v, want the tick-bound timeout before the trip threshold", i+1, err)
		}
		if !strings.Contains(err.Error(), "timed out after "+tickBound.String()) {
			t.Fatalf("read %d: err = %v, want it reaped at the tick bound %s", i+1, err, tickBound)
		}
		if elapsed > 10*time.Second {
			t.Fatalf("read %d took %s; a tick-context read must be reaped at the tick bound, "+
				"not the 120s non-tick bound", i+1, elapsed)
		}
	}

	if _, err := runner(scope, "bd", "list", "--json"); !errors.Is(err, beads.ErrStoreUnavailable) {
		t.Fatalf("after 3 tick-bound timeouts err = %v, want ErrStoreUnavailable — the tick "+
			"bound's timeouts must trip the scope breaker, or the tick keeps paying the bound "+
			"per read for a store already known not to answer", err)
	}
	raw, err := os.ReadFile(callsFile)
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Count(string(raw), "call"); got != 3 {
		t.Fatalf("bd was spawned %d time(s), want 3 — a timeout is not retried, and an open "+
			"breaker spawns no subprocess", got)
	}
}

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

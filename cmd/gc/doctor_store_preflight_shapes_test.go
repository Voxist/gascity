package main

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
)

// TestBeadStorePreflightSkipCountMatchesBothEnvShapes closes the gap that let
// the skip-banner count drift from production: every prior drift-lock test set
// GC_DOLT=skip, and CustomTypesPreflightCheck registers inside the storeOK
// blocks only OUTSIDE that shape (city unconditionally, rigs only with a
// managed-bdstore contract) — so the sync guard never saw the production
// shape, and a real outage under-reported how many checks were withheld
// (e.g. 15 city withheld, 14 reported).
//
// The invariant is the same one TestBuildDoctorChecks_RigStoreNameSetPreflight
// pins for the skip shape: healthy-minus-outage name delta == skipCount - 1
// (the -1 is the bead-store-preflight entry the outage build adds). Here it
// must hold in BOTH env shapes.
func TestBeadStorePreflightSkipCountMatchesBothEnvShapes(t *testing.T) {
	for _, shape := range []struct {
		name   string
		gcDolt string
	}{
		{name: "skip shape", gcDolt: "skip"},
		{name: "dolt shape (production)", gcDolt: ""},
	} {
		t.Run(shape.name, func(t *testing.T) {
			cityDir := t.TempDir()
			if err := os.WriteFile(filepath.Join(cityDir, "city.toml"), []byte("[workspace]\nname = \"demo\"\n"), 0o644); err != nil {
				t.Fatal(err)
			}
			t.Setenv("GC_DOLT", shape.gcDolt)

			cfg := &config.City{
				Workspace: config.Workspace{Name: "demo"},
				Rigs: []config.Rig{
					{Name: "alpha", Path: "alpha", Prefix: "al"},
					{Name: "beta", Path: "beta", Prefix: "be"},
				},
			}
			opts := buildDoctorChecksOpts{
				ControllerRunning:    true,
				SkipCityDoltCheck:    true,
				SkipManagedDoltCheck: true,
				SkipRigDoltChecks:    true,
			}

			old := doctorBeadStorePreflight
			t.Cleanup(func() { doctorBeadStorePreflight = old })

			doctorBeadStorePreflight = func(string, func(string) (beads.Store, error)) error { return nil }
			healthy := doctorCheckNames(buildDoctorChecks(cityDir, cfg, nil, opts))

			doctorBeadStorePreflight = func(string, func(string) (beads.Store, error)) error {
				return errors.New("connection refused")
			}
			outage := doctorCheckNames(buildDoctorChecks(cityDir, cfg, nil, opts))

			wantDelta := beadStorePreflightSkipCount(cityDir, cfg.Rigs) - 1
			if got := len(healthy) - len(outage); got != wantDelta {
				t.Fatalf("healthy-outage name delta = %d, want %d (skipCount-1); healthy=%d outage=%d — "+
					"the skip banner would misreport how many checks an outage withholds in this env shape",
					got, wantDelta, len(healthy), len(outage))
			}
		})
	}
}

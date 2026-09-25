package main

import (
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/beads/contract"
	"github.com/gastownhall/gascity/internal/fsys"
)

// A doltlite city runs no Dolt server and no proxy: its store is the embedded
// engine under .beads/embeddeddolt. The proxied-local default must not reach it.
//
// The failure this pins is quiet. Since the default flipped, a scope with no
// persisted dolt_mode canonicalizes to "proxied-server", and that rule did not
// look at the backend — so a doltlite city came out of `gc init` carrying
// dolt_mode "proxied-server", which is exactly the shape the R1 classifier
// reads as a bd-owned proxied scope. gc then treated it as provider-owned and
// bd raised a proxy plus a Dolt child (at its own 30s idle timeout, since gc
// never asked for a resident one) over a workspace that is supposed to have
// neither.

func writeDoltliteCityScaffold(t *testing.T, cityPath string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Join(cityPath, ".beads"), 0o700); err != nil {
		t.Fatal(err)
	}
	cityConfig := "[workspace]\nname = \"doltlite-city\"\nprefix = \"dl\"\n\n[beads]\nprovider = \"bd\"\nbackend = \"doltlite\"\n"
	if err := os.WriteFile(filepath.Join(cityPath, "city.toml"), []byte(cityConfig), 0o644); err != nil {
		t.Fatal(err)
	}
}

func readScopeMetadata(t *testing.T, scopeRoot string) contract.MetadataState {
	t.Helper()
	state, ok, err := contract.LoadMetadataState(fsys.OSFS{}, filepath.Join(scopeRoot, ".beads", "metadata.json"))
	if err != nil {
		t.Fatalf("load metadata for %s: %v", scopeRoot, err)
	}
	if !ok {
		t.Fatalf("no canonical metadata under %s", scopeRoot)
	}
	return state
}

func TestFreshScopeCanonicalDoltModeKeepsDoltliteOffTheProxiedDefault(t *testing.T) {
	cityPath := t.TempDir()
	writeDoltliteCityScaffold(t, cityPath)

	if got := freshScopeCanonicalDoltMode(cityPath); got != "server" {
		t.Fatalf("freshScopeCanonicalDoltMode(doltlite city) = %q, want server", got)
	}
}

// TestFreshScopeCanonicalDoltModeDefaultsDoltCitiesToDirectServer was
// TestFreshScopeCanonicalDoltModeStillDefaultsDoltCitiesToProxied, asserting
// origin's af7ad8a0f default ("proxied-local bd-owned Dolt by default on
// beads v1.3.0-rc.2"). This fork does not adopt that default (ga-m07q9):
// config.Proxied stays defaulting false, and a scope with no persisted
// dolt_mode and no other signal resolves to an ordinary gc-managed Dolt
// server. Adapted (2026-09-25 resync) rather than dropped -- a direct,
// function-level assertion on freshScopeCanonicalDoltMode for a plain
// non-doltlite dolt city is still worth having alongside its sibling
// TestFreshScopeCanonicalDoltModeKeepsDoltliteOffTheProxiedDefault, just
// with the correct expectation.
func TestFreshScopeCanonicalDoltModeDefaultsDoltCitiesToDirectServer(t *testing.T) {
	cityPath := t.TempDir()
	if err := os.MkdirAll(filepath.Join(cityPath, ".beads"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cityPath, "city.toml"), []byte("[workspace]\nname = \"dolt-city\"\nprefix = \"gc\"\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	if got := freshScopeCanonicalDoltMode(cityPath); got != "server" {
		t.Fatalf("freshScopeCanonicalDoltMode(fresh dolt city) = %q, want server", got)
	}
}

func TestNormalizeCanonicalBdScopeFilesForInitKeepsDoltliteOffTheProxiedDefault(t *testing.T) {
	cityPath := t.TempDir()
	writeDoltliteCityScaffold(t, cityPath)

	if err := normalizeCanonicalBdScopeFilesForInit(cityPath, cityPath, "dl", "dl"); err != nil {
		t.Fatalf("normalizeCanonicalBdScopeFilesForInit: %v", err)
	}

	if got := readScopeMetadata(t, cityPath).DoltMode; !strings.EqualFold(got, "server") {
		t.Fatalf("doltlite city dolt_mode = %q, want server (the mode it had before the proxied default)", got)
	}
}

// The rig path is the same decision, and a rig is where the damage compounds:
// a proxied binding on a rig gets its own proxy root and its own Dolt child.
func TestNormalizeCanonicalBdScopeFilesForInitKeepsDoltliteRigsOffTheProxiedDefault(t *testing.T) {
	cityPath := t.TempDir()
	writeDoltliteCityScaffold(t, cityPath)
	rigPath := filepath.Join(cityPath, "rigs", "frontend")
	if err := os.MkdirAll(rigPath, 0o755); err != nil {
		t.Fatal(err)
	}

	if err := normalizeCanonicalBdScopeFilesForInit(cityPath, rigPath, "fe", "fe"); err != nil {
		t.Fatalf("normalizeCanonicalBdScopeFilesForInit: %v", err)
	}

	if got := readScopeMetadata(t, rigPath).DoltMode; strings.EqualFold(got, "proxied-server") {
		t.Fatalf("doltlite rig dolt_mode = %q, which gives it a proxy root of its own", got)
	}
}

// The classifier is the thing that actually spawns a proxy, so assert on it
// rather than only on the string that feeds it.
func TestDoltliteScopeIsNotProviderOwnedProxied(t *testing.T) {
	cityPath := t.TempDir()
	writeDoltliteCityScaffold(t, cityPath)

	if err := normalizeCanonicalBdScopeFilesForInit(cityPath, cityPath, "dl", "dl"); err != nil {
		t.Fatalf("normalizeCanonicalBdScopeFilesForInit: %v", err)
	}
	owned, err := scopeBindingIsProviderOwnedProxied(cityPath)
	if err != nil {
		t.Fatalf("scopeBindingIsProviderOwnedProxied: %v", err)
	}
	if owned {
		t.Fatal("a doltlite city classified as a bd-owned proxied scope")
	}
}

// TestNormalizeCanonicalBdScopeFilesDefaultsFreshDoltCityToDirectServer is
// the ga-m07q9 invariant, asserted on disk rather than on any one constant
// or function: a freshly initialized scope's persisted dolt_mode is
// "server", not "proxied-server", unless the scope is explicitly marked
// proxied. This fork does not adopt beads v1.3.0-rc.2's proxied-by-default
// topology (af7ad8a0f) -- config.Proxied stays defaulting false, and a scope
// with no persisted dolt_mode and no other signal must resolve to an
// ordinary gc-managed Dolt server.
//
// freshScopeCanonicalDoltMode governs config.yaml's dolt.mode mirror;
// defaultFreshScopeDoltMode governs metadata.json's dolt_mode field written
// directly at scope-canonicalization time. Two artifacts, one policy, and a
// fix to one does not imply the other is fixed -- this asserts both, through
// normalizeCanonicalBdScopeFiles, the reconciliation path gc start/reload
// and `gc rig add` actually run (cmd/gc/beads_provider_lifecycle.go:315,
// cmd/gc/api_state.go:2590, cmd/gc/cmd_rig.go:340), not through
// normalizeCanonicalBdScopeFilesForInit (a different, already-gated path
// used by `gc init` directly).
func TestNormalizeCanonicalBdScopeFilesDefaultsFreshDoltCityToDirectServer(t *testing.T) {
	cityPath := t.TempDir()
	if err := os.MkdirAll(filepath.Join(cityPath, ".beads"), 0o700); err != nil {
		t.Fatal(err)
	}
	cityToml := "[workspace]\nname = \"dolt-city\"\nprefix = \"gc\"\n\n[[rigs]]\nname = \"frontend\"\npath = \"rigs/frontend\"\nprefix = \"fe\"\n"
	if err := os.WriteFile(filepath.Join(cityPath, "city.toml"), []byte(cityToml), 0o644); err != nil {
		t.Fatal(err)
	}
	rigPath := filepath.Join(cityPath, "rigs", "frontend")
	if err := os.MkdirAll(rigPath, 0o755); err != nil {
		t.Fatal(err)
	}

	cfg, err := loadCityConfig(cityPath, io.Discard)
	if err != nil {
		t.Fatalf("loadCityConfig: %v", err)
	}

	if err := normalizeCanonicalBdScopeFiles(cityPath, cfg, io.Discard); err != nil {
		t.Fatalf("normalizeCanonicalBdScopeFiles: %v", err)
	}

	if got := readScopeMetadata(t, cityPath).DoltMode; !strings.EqualFold(got, "server") {
		t.Errorf("city metadata.json dolt_mode = %q, want server", got)
	}
	if cfgState, ok, err := contract.ReadConfigState(fsys.OSFS{}, filepath.Join(cityPath, ".beads", "config.yaml")); err != nil {
		t.Errorf("read city config.yaml: %v", err)
	} else if ok && strings.EqualFold(cfgState.DoltMode, "proxied-server") {
		t.Errorf("city config.yaml dolt.mode = %q, want anything but proxied-server", cfgState.DoltMode)
	}

	if got := readScopeMetadata(t, rigPath).DoltMode; !strings.EqualFold(got, "server") {
		t.Errorf("rig metadata.json dolt_mode = %q, want server", got)
	}
	if cfgState, ok, err := contract.ReadConfigState(fsys.OSFS{}, filepath.Join(rigPath, ".beads", "config.yaml")); err != nil {
		t.Errorf("read rig config.yaml: %v", err)
	} else if ok && strings.EqualFold(cfgState.DoltMode, "proxied-server") {
		t.Errorf("rig config.yaml dolt.mode = %q, want anything but proxied-server", cfgState.DoltMode)
	}
}

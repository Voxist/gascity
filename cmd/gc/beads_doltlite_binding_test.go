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

// TestPreAndPostInitScopeDoltModeHonorExplicitProxiedOptIn is ga-m07q9's other
// half: an explicit --beads-transport=proxied opt-in must still resolve to
// "proxied-server", for both a city and a rig, at both the pre-init and
// post-init metadata writes.
//
// The fix above (defaultFreshScopeDoltMode: "proxied-server" -> "server")
// went further than the no-signal case it targeted. preInitScopeDoltMode and
// postInitScopeDoltMode both read that same constant in their PROXIED
// branches -- reached only once scopeUsesProxiedDoltMode /
// scopeInitUsesProxiedDoltMode has already classified the scope as proxied
// by an explicit signal, never the ambient default -- so flipping the
// constant's value silently broke both call sites: they started reporting
// "server" for a scope this same init pass had just told bd to create with
// --proxied-server. Neither the per-hunk conflict screen nor the file audit
// caught this; only re-tracing every reader of the constant did. The durable
// fix is a second constant, explicitProxiedDoltMode, dedicated to the
// already-classified-proxied branches so a future edit to the no-signal
// default cannot silently retake this path -- this test pins that split the
// way TestNormalizeCanonicalBdScopeFilesDefaultsFreshDoltCityToDirectServer
// pins the no-signal default: on the actual functions and the actual
// written artifact, not on either constant.
//
// This reproduces the regression: temporarily changing
// preInitScopeDoltMode's and postInitScopeDoltMode's proxied branches back
// to `return defaultFreshScopeDoltMode` (reverting explicitProxiedDoltMode)
// makes every assertion below fail with "server" instead of
// "proxied-server" -- verified by hand before this test was written, and the
// mechanism this test exists to catch if it ever recurs.
func TestPreAndPostInitScopeDoltModeHonorExplicitProxiedOptIn(t *testing.T) {
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

	// An explicit --beads-transport=proxied opt-in journals a pending scope
	// in the providerScopeInitializing state with Intent.Transport ==
	// "proxied" -- the real signal scopeUsesProxiedDoltMode's FIRST check
	// reads (beads_provider_lifecycle.go:474), before the ambient default is
	// ever consulted. This is what `gc init --beads-transport proxied` and
	// `gc rig add --beads-transport proxied` actually record, and it holds
	// (state stays providerScopeInitializing) across exactly the window both
	// preInitScopeDoltMode and postInitScopeDoltMode run in.
	for _, scope := range []string{cityPath, rigPath} {
		if err := persistProviderScopeOwnership(cityPath, scope, providerScopeIntent{Transport: "proxied", Target: "local"}); err != nil {
			t.Fatalf("persistProviderScopeOwnership(%s): %v", scope, err)
		}
	}

	for _, tc := range []struct {
		scope string
		label string
	}{
		{cityPath, "city"},
		{rigPath, "rig"},
	} {
		// Pure function-level checks first, with no metadata written yet:
		// both the pre-init stamp (initAndHookDir's caller,
		// beads_provider_lifecycle.go:889/891) and the post-init stamp
		// (finalizeCanonicalBdScopeInit, beads_provider_lifecycle.go:2132)
		// must independently resolve the opt-in to proxied.
		preMode := preInitScopeDoltMode(cityPath, tc.scope)
		if preMode != "proxied-server" {
			t.Errorf("%s: preInitScopeDoltMode = %q, want proxied-server", tc.label, preMode)
		}
		postMode := postInitScopeDoltMode(cityPath, tc.scope)
		if postMode != "proxied-server" {
			t.Errorf("%s: postInitScopeDoltMode = %q, want proxied-server", tc.label, postMode)
		}

		// Artifact-level: the ACTUAL write finalizeCanonicalBdScopeInit makes
		// once bd init has completed (enforceCanonicalScopeMetadataForInit,
		// fed by postInitScopeDoltMode) must persist "proxied-server" to
		// metadata.json, not silently downgrade to the no-signal default.
		if err := enforceCanonicalScopeMetadataForInit(fsys.OSFS{}, tc.scope, "hq", postMode); err != nil {
			t.Fatalf("%s: enforceCanonicalScopeMetadataForInit: %v", tc.label, err)
		}
		if got := readScopeMetadata(t, tc.scope).DoltMode; !strings.EqualFold(got, "proxied-server") {
			t.Errorf("%s metadata.json dolt_mode = %q, want proxied-server", tc.label, got)
		}

		// config.yaml must still never carry "proxied-server" literally (D1:
		// bd records the proxied binding in metadata.json only, and
		// canonicalConfigDoltMode is what scrubs it before any config.yaml
		// write) -- but the RESOLVED mode a later reconciliation pass would
		// compute from what was just persisted must say proxied, not the
		// no-signal default. persistedScopeDoltMode is exactly what
		// scopeUsesProxiedDoltMode itself consults first.
		if got := persistedScopeDoltMode(tc.scope); got != "proxied-server" {
			t.Errorf("%s: persistedScopeDoltMode after write = %q, want proxied-server", tc.label, got)
		}
		if got := canonicalConfigDoltMode(persistedScopeDoltMode(tc.scope)); got != "" {
			t.Errorf("%s: canonicalConfigDoltMode(persisted) = %q, want empty (never written to config.yaml literally)", tc.label, got)
		}
	}
}

// TestPersistFreshProviderOwnershipDefaultsFreshCityAndRigToDirectServer is a
// B1 regression guard (2026-09-27 independent review of resync PR #215): the
// two invariant tests above build a city and normalize its scope files
// directly (normalizeCanonicalBdScopeFiles), which never goes through the
// actual `gc init` journal. That gap is exactly why B1 shipped undetected:
// applySelectorToCityConfig and providerOwnershipIntent
// (cmd/gc/init_hosted_dolt.go) still passed contract.InitIntent{Transport:
// "proxied", Target: "local"} as ResolveInitIntent's provider-default
// argument -- upstream's rejected topology (ga-m07q9) -- so a PLAIN `gc init`
// with no selector journaled the city (and every fresh rig `gc init`
// initializes alongside it) as pending proxied/local, and
// scopeUsesProxiedDoltMode's FIRST check reads that journal entry before any
// ga-m07q9 guard ever runs. persistFreshProviderOwnership is the real entry
// point `gc init` calls to write that journal; postInitScopeDoltMode is the
// real function initDefaultRigBdStore consults to decide `bd init --server`
// vs `--proxied-server`. This test exercises both together, for the city and
// a rig `gc init` would initialize with it, so the invariant is pinned on
// the actual init path rather than on a scope-file-only shortcut.
//
// ga-wuda3: the fix that gave the direct/local fallback its correct FLAVOR
// still left a no-signal `gc init` journaling the city (and every rig
// configured alongside it) as provider-owned -- a third leak of the same
// rejected upstream default, this time over WHETHER a fresh scope is
// provider-owned at all rather than which flavor it is. A city.toml's rigs
// share the city's one managed Dolt server exactly like TestCleanInstall-
// TutorialPath expects; `gc rig add` on that city read the journal this test
// used to assert existed and started a second, unseeded Dolt server per rig.
// See ga-m07q9's notes for the full audit.
func TestPersistFreshProviderOwnershipLeavesFreshCityAndRigGCManaged(t *testing.T) {
	cityPath := t.TempDir()
	if err := os.MkdirAll(filepath.Join(cityPath, ".beads"), 0o700); err != nil {
		t.Fatal(err)
	}
	cityToml := "[workspace]\nname = \"dolt-city\"\nprefix = \"gc\"\n[beads]\nprovider = \"bd\"\n\n[[rigs]]\nname = \"frontend\"\npath = \"rigs/frontend\"\nprefix = \"fe\"\n"
	if err := os.WriteFile(filepath.Join(cityPath, "city.toml"), []byte(cityToml), 0o644); err != nil {
		t.Fatal(err)
	}
	rigPath := filepath.Join(cityPath, "rigs", "frontend")
	if err := os.MkdirAll(rigPath, 0o755); err != nil {
		t.Fatal(err)
	}

	// No selector requested: this is the no-signal `gc init` a brand new city
	// gets by default. ga-m07q9 says this must stay on the classic gc-managed
	// lifecycle -- one shared Dolt server -- not silently become
	// provider-owned just because ResolveInitIntent needed some fallback
	// value to hand back.
	if err := persistFreshProviderOwnership(cityPath, hostedDoltInitOptions{}); err != nil {
		t.Fatalf("persistFreshProviderOwnership: %v", err)
	}

	if entry, owned, err := providerScopeOwnership(cityPath, cityPath); err != nil || owned {
		t.Errorf("city journaled intent = (%+v, %t, %v), want not owned (classic gc-managed)", entry, owned, err)
	}
	if entry, owned, err := providerScopeOwnership(cityPath, rigPath); err != nil || owned {
		t.Errorf("rig journaled intent = (%+v, %t, %v), want not owned (classic gc-managed)", entry, owned, err)
	}

	// Classic gc-managed scopes also run `bd init --server`, so this stays
	// "server" regardless of provider ownership -- it is the journal above,
	// not this transport flavor, that distinguishes the two lifecycles.
	if got := postInitScopeDoltMode(cityPath, cityPath); got != "server" {
		t.Errorf("city postInitScopeDoltMode = %q, want server", got)
	}
	if got := postInitScopeDoltMode(cityPath, rigPath); got != "server" {
		t.Errorf("rig postInitScopeDoltMode = %q, want server", got)
	}
}

// TestPersistFreshProviderOwnershipHonorsExplicitSelector is
// TestPersistFreshProviderOwnershipLeavesFreshCityAndRigGCManaged's
// complement: an explicit --beads-transport/--beads-target selector must
// still produce a provider-owned city and rig exactly as before. ga-m07q9's
// no-signal default never overrides a real signal.
func TestPersistFreshProviderOwnershipHonorsExplicitSelector(t *testing.T) {
	cityPath := t.TempDir()
	if err := os.MkdirAll(filepath.Join(cityPath, ".beads"), 0o700); err != nil {
		t.Fatal(err)
	}
	cityToml := "[workspace]\nname = \"dolt-city\"\nprefix = \"gc\"\n[beads]\nprovider = \"bd\"\n\n[[rigs]]\nname = \"frontend\"\npath = \"rigs/frontend\"\nprefix = \"fe\"\n"
	if err := os.WriteFile(filepath.Join(cityPath, "city.toml"), []byte(cityToml), 0o644); err != nil {
		t.Fatal(err)
	}
	rigPath := filepath.Join(cityPath, "rigs", "frontend")
	if err := os.MkdirAll(rigPath, 0o755); err != nil {
		t.Fatal(err)
	}

	opts := hostedDoltInitOptions{Transport: "direct", Target: "local"}
	if err := persistFreshProviderOwnership(cityPath, opts); err != nil {
		t.Fatalf("persistFreshProviderOwnership: %v", err)
	}

	if entry, owned, err := providerScopeOwnership(cityPath, cityPath); err != nil || !owned || entry.Intent.Transport != "direct" {
		t.Errorf("city journaled intent = (%+v, %t, %v), want direct transport, owned", entry, owned, err)
	}
	if entry, owned, err := providerScopeOwnership(cityPath, rigPath); err != nil || !owned || entry.Intent.Transport != "direct" {
		t.Errorf("rig journaled intent = (%+v, %t, %v), want direct transport, owned", entry, owned, err)
	}
}

// TestPersistFreshProviderOwnershipResumesPendingRecordWithNoNewSignal covers
// constraint #3 (persisted state wins): a `gc init` interrupted after an
// earlier, explicit-selector pass already journaled the city as pending
// provider ownership must not be "healed" back to gc-managed just because
// the retry supplies no selector of its own.
func TestPersistFreshProviderOwnershipResumesPendingRecordWithNoNewSignal(t *testing.T) {
	cityPath := t.TempDir()
	if err := os.MkdirAll(filepath.Join(cityPath, ".beads"), 0o700); err != nil {
		t.Fatal(err)
	}
	cityToml := "[workspace]\nname = \"dolt-city\"\nprefix = \"gc\"\n[beads]\nprovider = \"bd\"\n"
	if err := os.WriteFile(filepath.Join(cityPath, "city.toml"), []byte(cityToml), 0o644); err != nil {
		t.Fatal(err)
	}

	// Simulate an earlier, explicit-selector `gc init` pass that journaled the
	// city as pending provider ownership and was then interrupted before bd
	// committed metadata.
	if err := persistProviderScopeOwnership(cityPath, cityPath, providerScopeIntent{Transport: "direct", Target: "local"}); err != nil {
		t.Fatal(err)
	}

	// The retry supplies no selector at all -- a plain `gc init` re-run.
	if err := persistFreshProviderOwnership(cityPath, hostedDoltInitOptions{}); err != nil {
		t.Fatalf("persistFreshProviderOwnership: %v", err)
	}

	if entry, owned, err := providerScopeOwnership(cityPath, cityPath); err != nil || !owned || entry.Intent.Transport != "direct" {
		t.Errorf("city journaled intent = (%+v, %t, %v), want direct transport, owned (persisted state wins)", entry, owned, err)
	}
}

// TestEnsureFreshRigProviderOwnershipDefaultsEmbeddedCityRigToDirectServer is
// B1's other half: a fresh rig added under an ALREADY provider-owned city
// whose own backend is embedded Dolt (no server transport for the rig to
// inherit) went through providerOwnershipIntentFromPersistedCity's separate
// "embedded" branch (cmd/gc/beads_scope_ownership.go), which had its own,
// independent copy of the same rejected proxied/local default. This exercises
// that branch through ensureFreshRigProviderOwnership, the real entry point a
// `gc rig add` (or a second `gc init` pass) under an established city runs.
func TestEnsureFreshRigProviderOwnershipDefaultsEmbeddedCityRigToDirectServer(t *testing.T) {
	cityPath := t.TempDir()
	rigPath := filepath.Join(cityPath, "rigs", "fresh")
	if err := os.MkdirAll(filepath.Join(cityPath, ".beads"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(rigPath, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cityPath, ".beads", "metadata.json"), []byte(`{"backend":"dolt","dolt_mode":"embedded"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cityPath, ".beads", "config.yaml"), []byte("gc.endpoint_origin: managed_city\ndolt.mode: embedded\ndolt.auto-start: false\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cityPath, "city.toml"), []byte("[workspace]\nname = \"demo\"\n[beads]\nprovider = \"bd\"\n[[rigs]]\nname = \"fresh\"\npath = \"rigs/fresh\"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	// Mark the city itself provider-owned and ready: an embedded city with no
	// ownership journal or handoff marker is NOT provider-owned
	// (cityScopeProviderOwned), which routes a fresh rig onto the legacy
	// path instead and never reaches the branch under test. A provider-owned
	// embedded city is the scenario providerOwnershipIntentFromPersistedCity's
	// embedded branch actually exists for.
	if err := persistProviderScopeOwnership(cityPath, cityPath, providerScopeIntent{Transport: "direct", Target: "local"}); err != nil {
		t.Fatal(err)
	}
	if err := markProviderScopeOwnershipReady(cityPath, cityPath); err != nil {
		t.Fatal(err)
	}

	cfg, err := loadCityConfig(cityPath, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	if err := ensureFreshRigProviderOwnership(cityPath, cfg); err != nil {
		t.Fatalf("ensureFreshRigProviderOwnership: %v", err)
	}

	entry, owned, err := providerScopeOwnership(cityPath, rigPath)
	if err != nil || !owned || entry.Intent.Transport != "direct" {
		t.Fatalf("rig journaled intent under embedded city = (%+v, %t, %v), want direct transport", entry, owned, err)
	}
	if got := postInitScopeDoltMode(cityPath, rigPath); got != "server" {
		t.Errorf("rig postInitScopeDoltMode under embedded city = %q, want server", got)
	}
}

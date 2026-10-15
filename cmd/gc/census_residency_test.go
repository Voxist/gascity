package main

import (
	"path/filepath"
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/coordclass"
	"github.com/gastownhall/gascity/internal/storeref"
)

// candidateRefs renders one census leg set as its refs, which is the half a
// consumer persists and matches on.
func candidateRefs(candidates []classStoreCandidate) []string {
	out := make([]string, 0, len(candidates))
	for _, c := range candidates {
		out = append(out, c.ref)
	}
	return out
}

func equalRefs(got, want []string) bool {
	if len(got) != len(want) {
		return false
	}
	for i := range got {
		if got[i] != want[i] {
			return false
		}
	}
	return true
}

// THE MIGRATION PIN, T0 half. A city that relocates nothing must get exactly the
// leg set coordClassStoreCandidates built: the leading store first, then the
// configured rigs, in each arm's own ref vocabulary. Byte-identical is the
// acceptance criterion for every legacy deployment.
func TestCensusCandidatesAreByteIdenticalOnASingleStoreCity(t *testing.T) {
	cfg := residencyTestConfig()
	work := beads.NewMemStore()
	rigs := map[string]beads.Store{"alpha": beads.NewMemStore(), "bravo": beads.NewMemStore()}

	bare, err := censusStoreCandidates("", cfg, work, rigs, nil, censusRefBare)
	if err != nil {
		t.Fatalf("censusStoreCandidates(bare): %v", err)
	}
	if got, want := candidateRefs(bare), []string{"", "alpha", "bravo"}; !equalRefs(got, want) {
		t.Fatalf("assigned-work arm refs = %v, want %v", got, want)
	}
	if bare[0].store != work {
		t.Fatal("the leading candidate is not the store the arm was handed")
	}

	scoped, err := censusStoreCandidates("", cfg, work, rigs, nil, censusRefScoped)
	if err != nil {
		t.Fatalf("censusStoreCandidates(scoped): %v", err)
	}
	if got, want := candidateRefs(scoped), []string{"city:test-city", "rig:alpha", "rig:bravo"}; !equalRefs(got, want) {
		t.Fatalf("unassigned-routed arm refs = %v, want %v", got, want)
	}

	sessions, err := sessionCensusStoreCandidates("", cfg, work, rigs, nil)
	if err != nil {
		t.Fatalf("sessionCensusStoreCandidates: %v", err)
	}
	if got, want := candidateRefs(sessions), []string{"city:test-city", "rig:alpha", "rig:bravo"}; !equalRefs(got, want) {
		t.Fatalf("session arm refs = %v, want %v", got, want)
	}
}

// The suspension frame, at the census seam this time: a suspended rig is not a
// leg. The control is the same call with no frame.
func TestCensusCandidatesExcludeSuspendedRigs(t *testing.T) {
	cfg := residencyTestConfig()
	work := beads.NewMemStore()
	rigs := map[string]beads.Store{"alpha": beads.NewMemStore(), "bravo": beads.NewMemStore()}
	suspended := map[string]bool{filepath.Clean("/tmp/bravo"): true}

	got, err := censusStoreCandidates("", cfg, work, rigs, suspended, censusRefBare)
	if err != nil {
		t.Fatalf("censusStoreCandidates: %v", err)
	}
	if want := []string{"", "alpha"}; !equalRefs(candidateRefs(got), want) {
		t.Fatalf("refs with bravo suspended = %v, want %v", candidateRefs(got), want)
	}
	control, err := censusStoreCandidates("", cfg, work, rigs, nil, censusRefBare)
	if err != nil {
		t.Fatalf("censusStoreCandidates (control): %v", err)
	}
	if want := []string{"", "alpha", "bravo"}; !equalRefs(candidateRefs(control), want) {
		t.Fatalf("control refs = %v, want %v — the exclusion above proves nothing if no frame also drops bravo", candidateRefs(control), want)
	}
}

// A store map entry config no longer declares is not a leg, exactly as before:
// the pre-resolver builder walked cfg.Rigs and looked each name up.
func TestCensusCandidatesSkipUndeclaredRigStores(t *testing.T) {
	cfg := residencyTestConfig()
	rigs := map[string]beads.Store{"alpha": beads.NewMemStore(), "ghost": beads.NewMemStore()}
	got, err := censusStoreCandidates("", cfg, beads.NewMemStore(), rigs, nil, censusRefBare)
	if err != nil {
		t.Fatalf("censusStoreCandidates: %v", err)
	}
	if want := []string{"", "alpha"}; !equalRefs(candidateRefs(got), want) {
		t.Fatalf("refs = %v, want %v — a rig store city.toml does not declare is not a census leg", candidateRefs(got), want)
	}
}

// THE D6 FLIP, at the unit seam. A controller that registered its work store
// gets the binding as a leg BESIDE it, under its own class ref — not instead of
// it. Before this slice the arm was handed the binding as its "city" leg and the
// work store was in no leg at all.
func TestCensusCandidatesNameTheWorkStoreAndTheBindingSeparately(t *testing.T) {
	cityPath := t.TempDir()
	cfg := residencyTestConfig()
	work := beads.NewMemStore()
	binding := beads.NewMemStore()
	routes := splitRoutes(binding)
	registerResidencyRoutes(cityPath, routes, func() beads.Store { return work })
	t.Cleanup(func() { unregisterResidencyRoutes(cityPath, routes) })

	// The arm is handed the SESSIONS store, which on a converged split is the
	// binding — exactly what buildDesiredState passes.
	got, err := censusStoreCandidates(cityPath, cfg, binding, nil, nil, censusRefBare)
	if err != nil {
		t.Fatalf("censusStoreCandidates: %v", err)
	}
	if want := []string{"", string(storeref.ClassRef(wholeSplitClasses()))}; !equalRefs(candidateRefs(got), want) {
		t.Fatalf("split-city census refs = %v, want %v — the binding must be a DISTINCT leg beside the work store (D6)", candidateRefs(got), want)
	}
	if got[0].store != work {
		t.Fatal("the work leg is not the registered city work store; the binding is still standing in for it")
	}
	if got[1].store != binding {
		t.Fatal("the second leg is not the binding")
	}
}

// A refused city gets no census at all: every infrastructure class answers with
// the refusal that names the remedy, and a work-only sweep reads the ledger the
// classes were moved off.
func TestCensusCandidatesRefuseARefusedCity(t *testing.T) {
	cityPath := t.TempDir()
	routes := splitRoutes(refusedClassStore{err: standingStorageRefusal{err: errStorageRefusedForTest{}}})
	registerResidencyRoutes(cityPath, routes, func() beads.Store { return beads.NewMemStore() })
	t.Cleanup(func() { unregisterResidencyRoutes(cityPath, routes) })

	if _, err := censusStoreCandidates(cityPath, residencyTestConfig(), beads.NewMemStore(), nil, nil, censusRefBare); err == nil {
		t.Fatal("a refused city produced a census leg set — that is the work-only answer that looks like success")
	}
	// Control: the same call on a city that relocates nothing succeeds, so the
	// refusal above is the refusal and not a constructor that always errors.
	if _, err := censusStoreCandidates("", residencyTestConfig(), beads.NewMemStore(), nil, nil, censusRefBare); err != nil {
		t.Fatalf("a single-store city was refused a census: %v", err)
	}
}

// The binding ref must read back as CITY scope through every normalizer that
// compares scopes, or the census gains the leg and drops its rows in the same
// commit.
func TestBindingRefNormalizesOntoTheCityScope(t *testing.T) {
	ref := string(storeref.ClassRef(wholeSplitClasses()))
	if got := normalizeDemandStoreRef(ref); got != "city" {
		t.Errorf("normalizeDemandStoreRef(%q) = %q, want \"city\"", ref, got)
	}
	if got := normalizeIdleClaimStoreRef(ref); got != "city" {
		t.Errorf("normalizeIdleClaimStoreRef(%q) = %q, want \"city\"", ref, got)
	}
	if got, ok := canonicalContinuationClaimStoreRef("test-city", ref); !ok || got != "city:test-city" {
		t.Errorf("canonicalContinuationClaimStoreRef(%q) = (%q, %v), want (\"city:test-city\", true)", ref, got, ok)
	}
	if !rootStoreRefMatchesCandidate("city:test-city", ref) {
		t.Error("a city-scope root_store_ref no longer matches the binding candidate; every binding-resident routed row just left the demand scan")
	}
	// A class binding is a genuinely distinct physical store, not a duplicate
	// view of a rig or city work store — `gc storage migrate` relocates rows
	// INTO it, it never aliases a rig's own file. So it is authoritative for a
	// RIG-scoped root too, on the strength of the filter's one call site:
	// collectOpenUnassignedRoutedWork resolves its legs through
	// routedWorkStoreCandidates on the RUNTIME plane, and storeref.Narrow drops
	// every leg but the binding once a plan touches one — no rig leg survives
	// to defer to. Rejecting it here is exactly the duplicate-view rejection
	// this filter is FOR, and the binding is never a duplicate view.
	//
	// That holds for runtime-plane callers only: `gc storage migrate` retains
	// the source, so an un-narrowed reconcile-plane leg set would still offer a
	// relocated row through both the binding and the legacy leg.
	if !rootStoreRefMatchesCandidate("rig:alpha", ref) {
		t.Error("a rig-scoped root_store_ref did not match the binding candidate; a migrated rig-rooted control row would be permanently invisible to demand and route repair")
	}
	// Control: two independent LEGACY (non-class) candidates still gate on
	// rig identity, or the class-binding carve-out has flattened the whole
	// comparison into a tautology.
	if rootStoreRefMatchesCandidate("rig:alpha", "rig:bravo") {
		t.Error("a rig-scoped root_store_ref matched an unrelated rig candidate")
	}
	// Control: the carve-out is for the BINDING, not for city scope. A legacy
	// city candidate reads as the same scope as a binding (storeref.ScopeRigContext
	// answers "" for both), but it is a duplicate view of a rig file store in
	// legacy unscoped mode, so it must keep deferring to the named rig. Widening
	// the carve-out to "any city-scope candidate" would pass every other
	// assertion here and re-collapse that duplicate view.
	if rootStoreRefMatchesCandidate("rig:alpha", "city:test-city") {
		t.Error("a rig-scoped root_store_ref matched the legacy city candidate; the class-binding carve-out leaked onto city scope")
	}
}

// wholeSplitClasses is the class set this build's storage boot admits: all five
// infrastructure classes on one binding. Derived rather than spelled, so the
// ref stays the constructors' answer and not a literal a test can drift from.
func wholeSplitClasses() []coordclass.Class {
	classes := make([]coordclass.Class, 0, 5)
	for _, c := range coordclass.Classes() {
		if c.IsInfrastructure() {
			classes = append(classes, c)
		}
	}
	return classes
}

// The generated work_query's federation bit is a PROJECTION of the city's
// shape: a binding leg in Plan(RoutedWork), or rig spread — claimable rows on a
// bound rig the work-dir command cannot read. The rows below pin each half on
// its own before the combined cities, so neither can go vacuous behind the
// other, and the collision/unbound rows pin the same rules the reader's leg
// assembly applies (rigLegsExcludingCityName, openStandaloneRigStores).
func TestQueryTopologyProjectsTheRoutedWorkPlan(t *testing.T) {
	cfg := residencyTestConfig() // rigs alpha and bravo, both bound
	rigLess := &config.City{Workspace: config.Workspace{Name: "test-city"}}

	// A city that relocates nothing and declares no rigs: no binding leg and no
	// rig legs, so `bd ready` is enough and the generated command is
	// byte-identical to the one it always ran.
	if routedWorkNeedsFederatedReader("", rigLess) {
		t.Error("a single-store city with no rigs was told to use the federated reader; its generated query is supposed to be unchanged")
	}

	// RIG SPREAD ALONE (vp-bzeg D3): a bound rig is a bd workspace, but it is
	// not the one the work_query runs from, so its claimable rows are invisible
	// to `bd ready` whatever their tier. The reader must federate.
	if !routedWorkNeedsFederatedReader("", cfg) {
		t.Error("a city with bound rigs was told `bd ready` is enough; the query runs from the city work directory and reads exactly one store, so the rigs' claimable rows are invisible to it")
	}

	// A rig keyed under the CITY'S OWN NAME is no leg — the same collision
	// rigLegsExcludingCityName drops from an opened leg map — so it alone is
	// not spread.
	collide := &config.City{
		Workspace: config.Workspace{Name: "test-city"},
		Rigs:      []config.Rig{{Name: "test-city", Path: "/tmp/self"}},
	}
	if routedWorkNeedsFederatedReader("", collide) {
		t.Error("a city whose only rig is keyed under the city's own name was told to federate; the reader drops that collision, so the predicate naming a leg the reader will not build is a needless federation")
	}

	// An UNBOUND rig (declared but empty Path) is skipped by the reader's leg
	// opener, so it alone is not spread either.
	unbound := &config.City{
		Workspace: config.Workspace{Name: "test-city"},
		Rigs:      []config.Rig{{Name: "ghost", Path: ""}},
	}
	if routedWorkNeedsFederatedReader("", unbound) {
		t.Error("a city whose only rig is unbound (no path) was told to federate; the reader skips that rig, so there is no store for the reader to reach that `bd ready` cannot")
	}

	split := t.TempDir()
	routes := splitRoutes(beads.NewMemStore())
	registerResidencyRoutes(split, routes, nil)
	t.Cleanup(func() { unregisterResidencyRoutes(split, routes) })
	if !routedWorkNeedsFederatedReader(split, cfg) {
		t.Error("a split city was told `bd ready` is enough; on such a city that reads the work ledger and answers with the copies the migration retained — a short array indistinguishable from \"no work\"")
	}

	// THE DELETED-[storage] TRAP. Plan REFUSES over a refused topology, and the
	// projection must read that as "federate", not as "no split". The federated
	// reader is the only one that carries the refusal to the operator; `bd ready`
	// would quietly read the ledger the classes were moved off, forever.
	refused := t.TempDir()
	refusedRoutes := splitRoutes(refusedClassStore{err: standingStorageRefusal{err: errStorageRefusedForTest{}}})
	registerResidencyRoutes(refused, refusedRoutes, nil)
	t.Cleanup(func() { unregisterResidencyRoutes(refused, refusedRoutes) })
	if !routedWorkNeedsFederatedReader(refused, cfg) {
		t.Error("a REFUSED city was given the single-store reader — \"no work forever\", which is the exact trap the resolver's refusal exists to surface")
	}
}

// A nil work store is not a topology to plan over, and returning one nil
// candidate the arms skip is what the pre-resolver builder did. No legs, no
// error.
func TestCensusCandidatesAnswerNoLegsForANilStore(t *testing.T) {
	got, err := censusStoreCandidates("", &config.City{}, nil, nil, nil, censusRefBare)
	if err != nil {
		t.Fatalf("a nil work store produced an error: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("got %d candidates for a nil work store, want none", len(got))
	}
}

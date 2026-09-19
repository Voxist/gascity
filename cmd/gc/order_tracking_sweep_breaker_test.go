package main

import (
	"bytes"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
)

// Tick-path isolation for the order-tracking sweep (vc-ny00): a scope the
// transport breaker holds open is dropped from the sweep, loudly, and every
// other scope is swept as before.

// prefixedStore is a MemStore-backed beads.Store standing in for one scope's
// bead store; prefix only makes failures readable.
type prefixedStore struct {
	beads.Store
	prefix string
}

func newPrefixedStore(prefix string) *prefixedStore {
	return &prefixedStore{Store: beads.NewMemStore(), prefix: prefix}
}

// degradeScope opens a scope's REAL transport breaker the way the bd runner
// does: consecutive bd calls killed at their per-command deadline, each
// classified and recorded through the production helpers (ga-2bo4m). The city comes from
// writeBreakerTestCity (three failures trip, one-hour open), so the verdict
// cannot expire mid-test.
func degradeScope(t *testing.T, cityPath, scopeRoot string) {
	t.Helper()
	breaker := bdScopeBreaker(cityPath, scopeRoot)
	timeout := fmt.Errorf("bd list: timed out after %s", 2*time.Minute)
	for i := 0; i < 3; i++ {
		recordBdBreakerOutcome(breaker, bdInvocationTimedOut("bd", timeout))
	}
	if breaker.Available() {
		t.Fatalf("precondition: scope %q breaker did not open", scopeRoot)
	}
}

// sweepScoped wraps a store the way orderTrackingSweepStoresFromTargets does,
// so the filter sees the production shape.
func sweepScoped(store beads.Store, label, scopeRoot string) beads.Store {
	return orderTrackingSweepScopedStore{Store: store, label: label, key: scopeRoot, breakerScope: scopeRoot}
}

// TestDegradedStoresAreDroppedFromTheTickSweep pins skip-and-announce for a
// degraded scope and isolation for the rest.
//
// dispatchOrders and both order-tracking sweep watchdogs run in the SAME
// serial tick body. On 2026-09-06 that body serialized behind stores that
// could not answer, and the observable result was 20 cooldown orders going
// stale simultaneously while the supervisor logged nothing for 15 minutes.
// A scope the transport breaker has already declared unavailable must not be
// walked again by the sweep on the tick path.
func TestDegradedStoresAreDroppedFromTheTickSweep(t *testing.T) {
	cityPath := writeBreakerTestCity(t, "")
	sickRoot, wellRoot := t.TempDir(), t.TempDir()
	degradeScope(t, cityPath, sickRoot)

	sick := sweepScoped(newPrefixedStore("vcnysick"), `rig "sick"`, sickRoot)
	well := sweepScoped(newPrefixedStore("vcnywell"), `rig "well"`, wellRoot)

	var stderr bytes.Buffer
	kept := filterDegradedSweepStores(cityPath, []beads.Store{sick, well}, &stderr, "test")

	if len(kept) != 1 {
		t.Fatalf("kept %d store(s), want 1 — the degraded store must be dropped", len(kept))
	}
	if got := kept[0].(orderTrackingSweepScopedStore).breakerScope; got != wellRoot {
		t.Fatalf("kept store scope = %q, want the healthy %q", got, wellRoot)
	}
	if !strings.Contains(stderr.String(), `rig "sick"`) {
		t.Fatalf("the skip was not announced; a silent skip leaves the operator "+
			"with no reason for the sweep's reduced scope.\nstderr: %s", stderr.String())
	}
}

// TestHealthyStoresSurviveTheSweepFilterUntouched is the other half of the
// isolation property: a degraded store must not take the fleet with it.
func TestHealthyStoresSurviveTheSweepFilterUntouched(t *testing.T) {
	cityPath := writeBreakerTestCity(t, "")
	stores := []beads.Store{
		sweepScoped(newPrefixedStore("vcnya"), `rig "a"`, t.TempDir()),
		sweepScoped(newPrefixedStore("vcnyb"), `rig "b"`, t.TempDir()),
	}
	var stderr bytes.Buffer
	kept := filterDegradedSweepStores(cityPath, stores, &stderr, "test")

	if len(kept) != 2 {
		t.Fatalf("kept %d of 2 healthy stores", len(kept))
	}
	if stderr.Len() != 0 {
		t.Fatalf("a sweep with no degraded store announced something: %s", stderr.String())
	}
}

// TestSweepFilterKeepsStoresItCannotIdentify pins the fail-safe direction.
// "Unknown scope" must never mean "skip": a store-type change that stopped
// carrying its scope root would otherwise silently disable both watchdogs,
// which is the stale-tracking jam (#2168) reintroduced by accident. The
// orders binding serves every scope and names none, so it is kept too.
func TestSweepFilterKeepsStoresItCannotIdentify(t *testing.T) {
	cityPath := writeBreakerTestCity(t, "")
	degradeScope(t, cityPath, cityPath)

	anonymous := beads.NewMemStore()
	ordersBinding := sweepScoped(beads.NewMemStore(), "orders binding", "")
	kept := filterDegradedSweepStores(cityPath, []beads.Store{anonymous, ordersBinding}, nil, "test")
	if len(kept) != 2 {
		t.Fatalf("kept %d of 2 stores with no identifiable scope; unknown must mean keep", len(kept))
	}
}

// sweepStoresForConfig builds the sweep's stores through the production target
// and scoping path, with an in-memory store standing in for each scope.
func sweepStoresForConfig(t *testing.T, cityPath string, cfg *config.City) []beads.Store {
	t.Helper()
	stores, err := orderTrackingSweepStoresFromTargets(orderTrackingSweepTargetsForConfig(cityPath, cfg),
		func(orderTrackingSweepTarget) (beads.Store, error) { return beads.NewMemStore(), nil })
	if err != nil {
		t.Fatalf("building sweep stores: %v", err)
	}
	return stores
}

func sweepLabels(stores []beads.Store) []string {
	var labels []string
	for _, s := range stores {
		labels = append(labels, s.(orderTrackingSweepScopedStore).label)
	}
	return labels
}

// TestSweepFilterReadsTheBreakerOfASymlinkedRig pins that the filter asks the
// breaker that actually trips. buildStores keys a rig's cache gate and bd
// runner by resolveStoreScopeRoot (symlinks and /private collapsed); a filter
// keyed by the rig path as configured would read a different, never-tripped
// breaker for a symlinked rig and never skip it.
func TestSweepFilterReadsTheBreakerOfASymlinkedRig(t *testing.T) {
	cityPath := writeBreakerTestCity(t, "")
	realRig := filepath.Join(cityPath, "realrig")
	if err := os.MkdirAll(realRig, 0o755); err != nil {
		t.Fatal(err)
	}
	linkRig := filepath.Join(cityPath, "rig")
	if err := os.Symlink(realRig, linkRig); err != nil {
		t.Fatal(err)
	}
	resolved := resolveStoreScopeRoot(cityPath, linkRig)
	if filepath.Clean(resolved) == filepath.Clean(linkRig) {
		t.Fatalf("precondition: resolveStoreScopeRoot(%q) did not resolve the symlink", linkRig)
	}
	cfg := &config.City{Rigs: []config.Rig{{Name: "linked", Path: linkRig, Prefix: "lk"}}}

	degradeScope(t, cityPath, resolved)
	var stderr bytes.Buffer
	kept := filterDegradedSweepStores(cityPath, sweepStoresForConfig(t, cityPath, cfg), &stderr, "test")

	if got := sweepLabels(kept); len(got) != 1 || got[0] != "city" {
		t.Fatalf("kept %v, want only the city — the symlinked rig's breaker is open under %q "+
			"and the sweep must skip it", got, resolved)
	}
	if !strings.Contains(stderr.String(), `rig "linked"`) {
		t.Fatalf("the skip of the symlinked rig was not announced.\nstderr: %s", stderr.String())
	}
}

// TestSweepFilterReadsTheCityBreakerAtTheCityPath is the counterweight: the
// city's cache gate and bd runner key its breaker by the city path as given,
// unresolved, so the filter must too. Resolving it would miss the city's own
// breaker whenever the city is reached through a symlink.
func TestSweepFilterReadsTheCityBreakerAtTheCityPath(t *testing.T) {
	realCity := writeBreakerTestCity(t, "")
	cityPath := filepath.Join(t.TempDir(), "citylink")
	if err := os.Symlink(realCity, cityPath); err != nil {
		t.Fatal(err)
	}

	degradeScope(t, cityPath, cityPath)
	kept := filterDegradedSweepStores(cityPath, sweepStoresForConfig(t, cityPath, &config.City{}), io.Discard, "test")

	if len(kept) != 0 {
		t.Fatalf("kept %v, want the city dropped — its breaker is open under the city path %q",
			sweepLabels(kept), cityPath)
	}
}

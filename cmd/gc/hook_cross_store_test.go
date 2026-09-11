package main

import (
	"errors"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/config"
)

var errTestStoreTimeout = errors.New("store timed out")

// TestRigScopedHookRig is the core of the rig-scope hook fix: a rig-scoped agent
// ("<rig>/<name>") must resolve to its own rig so the hook also queries that
// rig's store, where its routed work lives. City-scoped identities (no "/") and
// unknown rigs resolve to "" so no spurious store is added.
func TestRigScopedHookRig(t *testing.T) {
	cfg := &config.City{Rigs: []config.Rig{{Name: "voxist-web"}, {Name: "voxist-api"}}}
	cases := []struct {
		name     string
		identity string
		want     string
	}{
		{"rig-scoped known rig", "voxist-web/voxist.executor", "voxist-web"},
		{"rig-scoped other known rig", "voxist-api/voxist.reviewer", "voxist-api"},
		{"rig-scoped unknown rig", "hq/voxist.executor", ""},
		{"city-scoped (no slash)", "voxist.architect", ""},
		{"empty identity", "", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := rigScopedHookRig(cfg, tc.identity); got != tc.want {
				t.Fatalf("rigScopedHookRig(%q) = %q, want %q", tc.identity, got, tc.want)
			}
		})
	}
	if got := rigScopedHookRig(nil, "voxist-web/x"); got != "" {
		t.Fatalf("rigScopedHookRig(nil, ...) = %q, want \"\"", got)
	}
}

// TestAppendOneRigHookStoreSkipsUnknownInput guards the best-effort contract:
// an unknown rig, empty rig, or nil cfg/agent must leave the store list
// unchanged (and must not reach hookQueryEnv), so a stray GC_AGENT prefix can
// never add a bogus store or wedge the hook.
func TestAppendOneRigHookStoreSkipsUnknownInput(t *testing.T) {
	cfg := &config.City{Rigs: []config.Rig{{Name: "voxist-web"}}}
	a := &config.Agent{Name: "voxist.executor"}
	base := []hookStore{{dir: "own"}}

	for _, tc := range []struct {
		name    string
		cfg     *config.City
		agent   *config.Agent
		rigName string
	}{
		{"unknown rig", cfg, a, "nope"},
		{"empty rig", cfg, a, ""},
		{"nil cfg", nil, a, "voxist-web"},
		{"nil agent", cfg, nil, "voxist-web"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := appendOneRigHookStore(base, t.TempDir(), tc.cfg, tc.agent, tc.rigName, nil)
			if len(got) != len(base) {
				t.Fatalf("appendOneRigHookStore added a store for %s: len=%d, want %d", tc.name, len(got), len(base))
			}
		})
	}
}

func TestBestStoreWithWorkReturnsTheOnlyStoreThatHasWork(t *testing.T) {
	stores := []hookStore{{dir: "city"}, {dir: "riga"}, {dir: "rigb"}}
	var calls []string
	run := func(_, dir string, _ []string) (string, error) {
		calls = append(calls, dir)
		if dir == "riga" {
			return `[{"id":"va-1"}]`, nil
		}
		return `[]`, nil
	}
	out, gotStore, err := bestStoreWithWork("q", stores, stores[0], run)
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if out != `[{"id":"va-1"}]` {
		t.Fatalf("out = %q, want riga work", out)
	}
	if gotStore.dir != "riga" {
		t.Fatalf("store.dir = %q, want riga", gotStore.dir)
	}
	// Every store is consulted: selection is a comparison, not a first hit.
	if len(calls) != 3 || calls[0] != "city" || calls[1] != "riga" || calls[2] != "rigb" {
		t.Fatalf("calls = %v, want [city riga rigb]", calls)
	}
}

// TestBestStoreWithWorkPrefersHigherPriorityInALaterStore is the regression this
// selection change exists for: the agent's own store is first in the slice and
// has ready work, so first-hit selection returned it and the rig-routed P0 was
// unreachable no matter how urgent it was.
func TestBestStoreWithWorkPrefersHigherPriorityInALaterStore(t *testing.T) {
	stores := []hookStore{{dir: "city"}, {dir: "riga"}}
	run := func(_, dir string, _ []string) (string, error) {
		if dir == "city" {
			return `[{"id":"ci-1","priority":2}]`, nil
		}
		return `[{"id":"va-1","priority":0}]`, nil
	}
	out, gotStore, err := bestStoreWithWork("q", stores, stores[0], run)
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if gotStore.dir != "riga" {
		t.Fatalf("store.dir = %q, want riga (P0 must beat the own store's P2)", gotStore.dir)
	}
	if out != `[{"id":"va-1","priority":0}]` {
		t.Fatalf("out = %q, want riga work", out)
	}
}

// TestBestStoreWithWorkDoesNotInvertTheBug guards the other direction: a
// higher-priority candidate in the agent's OWN store must still win. A fix
// that simply preferred the federated store would pass the regression test
// above and be just as wrong.
//
// (An "equal priority keeps slice order" case used to live here too,
// asserting that a tie always resolved to the own store. That assertion WAS
// the permanent-starvation bug ga-kbbg9a exists to fix — see
// TestBestStoreWithWorkRotatesExactTies and
// TestBestStoreWithWorkRepeatedTiesVisitEveryStoreOverTime below for its
// replacement.)
func TestBestStoreWithWorkDoesNotInvertTheBug(t *testing.T) {
	stores := []hookStore{{dir: "city"}, {dir: "riga"}}
	run := func(_, dir string, _ []string) (string, error) {
		if dir == "city" {
			return `[{"id":"ci-1","priority":1}]`, nil
		}
		return `[{"id":"va-1","priority":3}]`, nil
	}
	_, gotStore, err := bestStoreWithWork("q", stores, stores[0], run)
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if gotStore.dir != "city" {
		t.Fatalf("store.dir = %q, want city (P1 in own store beats rig P3)", gotStore.dir)
	}
}

// withHookTieBreakClock pins bestStoreWithWork's tie-break clock for the
// duration of the test, restoring the real clock on cleanup.
func withHookTieBreakClock(t *testing.T, now time.Time) {
	t.Helper()
	orig := hookTieBreakClock
	hookTieBreakClock = func() time.Time { return now }
	t.Cleanup(func() { hookTieBreakClock = orig })
}

// TestBestStoreWithWorkRotatesExactTies is the fix ga-kbbg9a exists for: an
// exact rank tie resolved to the same store on every call because the
// selection loop only replaced its incumbent on a STRICT improvement — ties
// left the first-seen store (stores[0], the agent's own store) as the
// incumbent forever, starving every other tied store regardless of how many
// hook calls ran. Pinning the tie-break clock to two different instants
// proves the winner now depends on the clock rather than always being the
// first store in the slice.
func TestBestStoreWithWorkRotatesExactTies(t *testing.T) {
	stores := []hookStore{{dir: "city"}, {dir: "riga"}}
	run := func(_, dir string, _ []string) (string, error) {
		if dir == "city" {
			return `[{"id":"ci-1","priority":1}]`, nil
		}
		return `[{"id":"va-1","priority":1}]`, nil
	}

	withHookTieBreakClock(t, time.Unix(0, 0))
	_, gotCity, err := bestStoreWithWork("q", stores, stores[0], run)
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if gotCity.dir != "city" {
		t.Fatalf("store.dir = %q, want city at tie-break clock offset 0", gotCity.dir)
	}

	withHookTieBreakClock(t, time.Unix(0, 1))
	_, gotRiga, err := bestStoreWithWork("q", stores, stores[0], run)
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if gotRiga.dir != "riga" {
		t.Fatalf("store.dir = %q, want riga at tie-break clock offset 1 — an exact tie must not always resolve to the same store", gotRiga.dir)
	}
}

// TestBestStoreWithWorkRepeatedTiesVisitEveryStoreOverTime is the direct
// regression test for the reported symptom: gc hook run repeatedly against an
// UNCHANGED four-way exact tie (mirroring the bead's live measurement — city,
// gascity, cairn, and beads all at tier=routed/P1) must eventually surface
// every tied store, not just the first one in the slice, forever.
func TestBestStoreWithWorkRepeatedTiesVisitEveryStoreOverTime(t *testing.T) {
	stores := []hookStore{{dir: "city"}, {dir: "gascity"}, {dir: "cairn"}, {dir: "beads"}}
	run := func(_, dir string, _ []string) (string, error) {
		return `[{"id":"tied-` + dir + `","priority":1}]`, nil
	}

	orig := hookTieBreakClock
	defer func() { hookTieBreakClock = orig }()

	seen := map[string]bool{}
	for i := 0; i < len(stores); i++ {
		offset := int64(i)
		hookTieBreakClock = func() time.Time { return time.Unix(0, offset) }
		_, gotStore, err := bestStoreWithWork("q", stores, stores[0], run)
		if err != nil {
			t.Fatalf("call %d: err: %v", i, err)
		}
		seen[gotStore.dir] = true
	}

	if len(seen) <= 1 {
		t.Fatalf("repeated calls against an unchanged tie only ever selected %v — starvation is still permanent", seen)
	}
}

// TestBestStoreWithWorkDoesNotRotateOnACoResidentDuplicateID is the
// regression test for a gap in ga-kbbg9a's own rotation: a bead migrated
// with `gc storage migrate` (copies, never deletes) is visible as ready from
// MORE than one store under the SAME id — the exact same row, not two tied
// pieces of work. An id-blind tie-break can rotate onto a later store ahead
// of the primary even though nothing about the work differs, which breaks
// the rig-first-city-last fan-out order TestClassEscalationWaitsForEveryWorkLeg
// and TestClassEscalationStillReachesABindingOnlyBead
// (hook_claim_class_fanout_test.go) depend on. Two DIFFERENT ids at the same
// rank must still rotate (TestBestStoreWithWorkRotatesExactTies) — only a
// shared id must not.
func TestBestStoreWithWorkDoesNotRotateOnACoResidentDuplicateID(t *testing.T) {
	stores := []hookStore{{dir: "riga"}, {dir: "city"}}
	run := func(_, _ string, _ []string) (string, error) {
		// Same id, same rank, from every store: a co-resident duplicate, not
		// two different pieces of work.
		return `[{"id":"dup-1","assignee":"worker-1"}]`, nil
	}

	// Offset 1 is exactly the clock value TestBestStoreWithWorkRotatesExactTies
	// uses to prove rotation moves off the first store; here the tied
	// candidates share an id, so it must NOT move off riga.
	withHookTieBreakClock(t, time.Unix(0, 1))
	_, got, err := bestStoreWithWork("q", stores, stores[0], run)
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if got.dir != "riga" {
		t.Fatalf("store.dir = %q, want riga: a tie between two copies of the SAME bead id must keep slice order, not rotate", got.dir)
	}
}

// TestHookTieBreakIndex pins the rotation formula directly: it must stay
// within [0, n) and must not collapse to a constant across varying clock
// values, which is exactly what would silently reintroduce the starvation
// bug.
func TestHookTieBreakIndex(t *testing.T) {
	for _, tc := range []struct {
		n    int
		nano int64
		want int
	}{
		{2, 0, 0},
		{2, 1, 1},
		{2, 2, 0},
		{4, 0, 0},
		{4, 3, 3},
		{4, 4, 0},
	} {
		got := hookTieBreakIndex(tc.n, time.Unix(0, tc.nano))
		if got != tc.want {
			t.Fatalf("hookTieBreakIndex(%d, unix-nano %d) = %d, want %d", tc.n, tc.nano, got, tc.want)
		}
	}
}

// TestBestStoreWithWorkRanksTierAheadOfPriority pins that priority is compared
// WITHIN a tier, never across one: the three-tier work_query means crash
// recovery and pre-assigned work outrank routed work regardless of number.
func TestBestStoreWithWorkRanksTierAheadOfPriority(t *testing.T) {
	for _, tc := range []struct {
		name    string
		cityRow string
		rigRow  string
		wantDir string
	}{
		{
			name:    "in_progress in a rig store beats a routed P0 in the own store",
			cityRow: `[{"id":"ci-1","priority":0}]`,
			rigRow:  `[{"id":"va-1","priority":3,"status":"in_progress","assignee":"me"}]`,
			wantDir: "riga",
		},
		{
			name:    "assigned beats routed at worse priority",
			cityRow: `[{"id":"ci-1","priority":0}]`,
			rigRow:  `[{"id":"va-1","priority":3,"assignee":"me"}]`,
			wantDir: "riga",
		},
		{
			name:    "within the routed tier, priority decides",
			cityRow: `[{"id":"ci-1","priority":3}]`,
			rigRow:  `[{"id":"va-1","priority":0}]`,
			wantDir: "riga",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			stores := []hookStore{{dir: "city"}, {dir: "riga"}}
			run := func(_, dir string, _ []string) (string, error) {
				if dir == "city" {
					return tc.cityRow, nil
				}
				return tc.rigRow, nil
			}
			_, gotStore, err := bestStoreWithWork("q", stores, stores[0], run)
			if err != nil {
				t.Fatalf("err: %v", err)
			}
			if gotStore.dir != tc.wantDir {
				t.Fatalf("store.dir = %q, want %q", gotStore.dir, tc.wantDir)
			}
		})
	}
}

// TestBestStoreWithWorkShortCircuitsOwnInProgress pins the resume carve-out:
// this session's own interrupted work is unconditional, so the primary store's
// in_progress row is taken without consulting any federated store at all.
func TestBestStoreWithWorkShortCircuitsOwnInProgress(t *testing.T) {
	stores := []hookStore{{dir: "city"}, {dir: "riga"}}
	var calls []string
	run := func(_, dir string, _ []string) (string, error) {
		calls = append(calls, dir)
		if dir == "city" {
			return `[{"id":"ci-1","priority":3,"status":"in_progress","assignee":"me"}]`, nil
		}
		return `[{"id":"va-1","priority":0,"status":"in_progress","assignee":"me"}]`, nil
	}
	_, gotStore, err := bestStoreWithWork("q", stores, stores[0], run)
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if gotStore.dir != "city" {
		t.Fatalf("store.dir = %q, want city (own in_progress work is unconditional)", gotStore.dir)
	}
	if len(calls) != 1 || calls[0] != "city" {
		t.Fatalf("calls = %v, want [city] — the resume path must not query rig stores", calls)
	}
}

// TestBestStoreWithWorkDegradesToFirstHitOnUnrankableOutput pins the degradation
// rule: a work_query that does not emit a JSON array of objects cannot be
// compared, so selection falls back to the pre-existing first-hit behavior
// rather than reordering on a comparison that was never made.
func TestBestStoreWithWorkDegradesToFirstHitOnUnrankableOutput(t *testing.T) {
	stores := []hookStore{{dir: "city"}, {dir: "riga"}}
	run := func(_, dir string, _ []string) (string, error) {
		if dir == "city" {
			return "va-1 some plain-text row", nil
		}
		return `[{"id":"va-2","priority":0}]`, nil
	}
	_, gotStore, err := bestStoreWithWork("q", stores, stores[0], run)
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if gotStore.dir != "city" {
		t.Fatalf("store.dir = %q, want city (unrankable output degrades to first-hit)", gotStore.dir)
	}
}

// TestBestHookCandidateRank exercises the ranking primitive directly, including
// the wire-shape distinction that motivates hookDefaultCandidatePriority: bd's
// priority is *int with omitempty, so an ABSENT priority must not be read as P0.
func TestBestHookCandidateRank(t *testing.T) {
	for _, tc := range []struct {
		name  string
		ready string
		want  hookCandidateRank
		ok    bool
	}{
		{"routed with priority", `[{"id":"a","priority":1}]`, hookCandidateRank{tier: hookTierRouted, priority: 1}, true},
		{"absent priority is not P0", `[{"id":"a"}]`, hookCandidateRank{tier: hookTierRouted, priority: hookDefaultCandidatePriority}, true},
		{"assignee lifts the tier", `[{"id":"a","assignee":"me","priority":3}]`, hookCandidateRank{tier: hookTierAssigned, priority: 3}, true},
		{"in_progress is the top tier", `[{"id":"a","assignee":"me","status":"in_progress","priority":3}]`, hookCandidateRank{tier: hookTierInProgress, priority: 3}, true},
		{"blank assignee stays routed", `[{"id":"a","assignee":"  ","priority":1}]`, hookCandidateRank{tier: hookTierRouted, priority: 1}, true},
		{"best of several rows wins", `[{"id":"a","priority":3},{"id":"b","priority":0}]`, hookCandidateRank{tier: hookTierRouted, priority: 0}, true},
		{"empty array is unrankable", `[]`, hookCandidateRank{}, false},
		{"non-JSON is unrankable", `not json`, hookCandidateRank{}, false},
		{"array of non-objects is unrankable", `["a"]`, hookCandidateRank{}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, _, ok := bestHookCandidateRank(tc.ready)
			if ok != tc.ok {
				t.Fatalf("ok = %v, want %v", ok, tc.ok)
			}
			if ok && got != tc.want {
				t.Fatalf("rank = %+v, want %+v", got, tc.want)
			}
		})
	}
}

// TestBestHookCandidateRankParsesAge pins the D2 wire input: the best row's
// created_at becomes the rank's age, and an unparseable or absent created_at
// leaves hasAge false (the tiebreak then does not apply to that row).
func TestBestHookCandidateRankParsesAge(t *testing.T) {
	rank, _, ok := bestHookCandidateRank(`[{"id":"a","priority":1,"created_at":"2026-08-13T09:00:00Z"}]`)
	if !ok {
		t.Fatalf("ok = false, want rankable")
	}
	want := time.Date(2026, 8, 13, 9, 0, 0, 0, time.UTC)
	if !rank.hasAge || !rank.age.Equal(want) {
		t.Fatalf("age = %v (hasAge %v), want %v", rank.age, rank.hasAge, want)
	}

	rank, _, ok = bestHookCandidateRank(`[{"id":"a","priority":1,"created_at":"not-a-date"}]`)
	if !ok || rank.hasAge {
		t.Fatalf("unparseable created_at must leave hasAge false; got %+v ok=%v", rank, ok)
	}
	rank, _, ok = bestHookCandidateRank(`[{"id":"a","priority":1}]`)
	if !ok || rank.hasAge {
		t.Fatalf("absent created_at must leave hasAge false; got %+v ok=%v", rank, ok)
	}
}

// TestBestStoreWithWorkD1ProbeRefinesRank is the ADR-0076 regression fixture
// (acceptance criterion 2): the agent's own store's window is full of aged P3s
// while its real best routed work is a P1 outside the window — the measured
// 2026-08-13 shape, where the busiest store advertised the WORST rank. Without
// the probe the own store loses to a quiet rig's P2; with the D1 probe the
// store's advertised rank comes from the probe's best-by-priority row and it
// must win.
func TestBestStoreWithWorkD1ProbeRefinesRank(t *testing.T) {
	stores := []hookStore{{dir: "city"}, {dir: "riga"}}
	agedP3 := `[{"id":"ci-aged-1","priority":3,"created_at":"2026-07-01T00:00:00Z"}]`
	freshP1 := `[{"id":"ci-fresh-1","priority":1,"created_at":"2026-08-13T00:00:00Z"}]`
	rigP2 := `[{"id":"va-1","priority":2,"created_at":"2026-08-01T00:00:00Z"}]`
	run := func(command, dir string, _ []string) (string, error) {
		if command == "probe" {
			if dir == "city" {
				return freshP1, nil // the P1 the window hides
			}
			return rigP2, nil
		}
		if dir == "city" {
			return agedP3, nil // window: aged P3s only
		}
		return rigP2, nil
	}
	// Without the probe: riga's P2 beats the city window's P3 (the bug).
	_, got, err := bestStoreWithWork("q", stores, stores[0], run)
	if err != nil || got.dir != "riga" {
		t.Fatalf("pre-D1 shape: store.dir = %q err %v, want riga", got.dir, err)
	}
	// With the probe: city's hidden P1 must be ranked and win.
	_, got, err = bestStoreWithWork("q", stores, stores[0], run, hookStoreRankOptions{RankProbeCommand: "probe"})
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if got.dir != "city" {
		t.Fatalf("store.dir = %q, want city — the probe's P1 must outrank riga's P2", got.dir)
	}
}

// TestBestStoreWithWorkProbeFailureKeepsWindowRank pins C3: a probe that
// errors or returns garbage is best-effort. The store keeps its window rank
// and selection proceeds — a flaky probe must never wedge the hook.
func TestBestStoreWithWorkProbeFailureKeepsWindowRank(t *testing.T) {
	stores := []hookStore{{dir: "city"}, {dir: "riga"}}
	run := func(command, dir string, _ []string) (string, error) {
		if command == "probe" {
			if dir == "city" {
				return "", errTestStoreTimeout // primary probe fails
			}
			return "not json", nil // rig probe unparseable
		}
		if dir == "city" {
			return `[{"id":"ci-1","priority":2}]`, nil
		}
		return `[{"id":"va-1","priority":1}]`, nil
	}
	_, got, err := bestStoreWithWork("q", stores, stores[0], run, hookStoreRankOptions{RankProbeCommand: "probe"})
	if err != nil {
		t.Fatalf("a failed probe must not error the hook: %v", err)
	}
	if got.dir != "riga" {
		t.Fatalf("store.dir = %q, want riga (window ranks stand when probes fail)", got.dir)
	}
}

// TestBestStoreWithWorkBreaksExactTiesOnAge is ADR-0076 acceptance criterion
// 3: two stores tying on (tier, priority) select the one with the OLDER best
// candidate — age is ADR-0035's anti-starvation axis, and positional ties are
// how one early-registered rig owned the front door for 34 days.
func TestBestStoreWithWorkBreaksExactTiesOnAge(t *testing.T) {
	stores := []hookStore{{dir: "city"}, {dir: "riga"}}
	run := func(_, dir string, _ []string) (string, error) {
		if dir == "city" {
			return `[{"id":"ci-1","priority":1,"created_at":"2026-08-10T00:00:00Z"}]`, nil
		}
		return `[{"id":"va-1","priority":1,"created_at":"2026-07-08T00:00:00Z"}]`, nil // older
	}
	withHookTieBreakClock(t, time.Unix(0, 1)) // the offset that would rotate an unresolved tie to riga anyway
	_, got, err := bestStoreWithWork("q", stores, stores[0], run)
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if got.dir != "riga" {
		t.Fatalf("store.dir = %q, want riga — the older best candidate must win the tie, not the clock", got.dir)
	}

	// Equal KNOWN ages keep slice order (ADR-0076 D2), even at a clock offset
	// that would otherwise rotate.
	run = func(_, dir string, _ []string) (string, error) {
		return `[{"id":"tied-` + dir + `","priority":1,"created_at":"2026-08-01T00:00:00Z"}]`, nil
	}
	_, got, err = bestStoreWithWork("q", stores, stores[0], run)
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if got.dir != "city" {
		t.Fatalf("store.dir = %q, want city — equal known ages keep slice order", got.dir)
	}
}

// TestBestStoreWithWorkCollectsSelectionStats pins the D3 telemetry: counts
// are per store consulted, the total is the sum of ready rows, and the
// selected store is recorded.
func TestBestStoreWithWorkCollectsSelectionStats(t *testing.T) {
	stores := []hookStore{{dir: "city"}, {dir: "riga"}, {dir: "rigb"}}
	run := func(_, dir string, _ []string) (string, error) {
		switch dir {
		case "city":
			return `[]`, nil
		case "riga":
			return `[{"id":"va-1","priority":2},{"id":"va-2","priority":2}]`, nil
		default:
			return `[{"id":"vb-1","priority":1}]`, nil
		}
	}
	var stats hookSelectionStats
	_, got, err := bestStoreWithWork("q", stores, stores[0], run, hookStoreRankOptions{Stats: &stats})
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if got.dir != "rigb" {
		t.Fatalf("store.dir = %q, want rigb (P1)", got.dir)
	}
	if stats.StoresWithReadyWork != 2 {
		t.Fatalf("StoresWithReadyWork = %d, want 2", stats.StoresWithReadyWork)
	}
	if stats.TotalReadyCandidates != 3 {
		t.Fatalf("TotalReadyCandidates = %d, want 3", stats.TotalReadyCandidates)
	}
	if stats.SelectedStore != "rigb" {
		t.Fatalf("SelectedStore = %q, want rigb", stats.SelectedStore)
	}
}

func TestBestStoreWithWorkReturnsLastWhenNoneHasWork(t *testing.T) {
	stores := []hookStore{{dir: "city"}, {dir: "riga"}}
	run := func(_, _ string, _ []string) (string, error) { return `[]`, nil }
	out, gotStore, err := bestStoreWithWork("q", stores, stores[0], run)
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if out != `[]` {
		t.Fatalf("out = %q, want []", out)
	}
	if gotStore.dir != "" || len(gotStore.env) != 0 {
		t.Fatalf("store = %#v, want zero value when no work is found", gotStore)
	}
}

func TestBestStoreWithWorkSurfacesOwnStoreErrorWhenNoWork(t *testing.T) {
	// The agent's own store (first) timing out must be surfaced even if a
	// federated rig store returns no work — otherwise emitCityWorkQueryFailure
	// never fires and a transient timeout is silently downgraded to "no work".
	stores := []hookStore{{dir: "city"}, {dir: "riga"}}
	run := func(_, dir string, _ []string) (string, error) {
		if dir == "city" {
			return "", errTestStoreTimeout
		}
		return `[]`, nil
	}
	if _, _, err := bestStoreWithWork("q", stores, stores[0], run); !errors.Is(err, errTestStoreTimeout) {
		t.Fatalf("own-store error must be surfaced when no store has work; got %v", err)
	}
}

func TestBestStoreWithWorkIgnoresRigStoreErrorWhenOwnStoreHasNoWork(t *testing.T) {
	// A flaky federated rig store must not wedge the hook: when the agent's own
	// store is healthy (no work), a rig-store error is best-effort and dropped.
	stores := []hookStore{{dir: "city"}, {dir: "riga"}}
	run := func(_, dir string, _ []string) (string, error) {
		if dir == "city" {
			return `[]`, nil
		}
		return "", errTestStoreTimeout
	}
	out, gotStore, err := bestStoreWithWork("q", stores, stores[0], run)
	if err != nil {
		t.Fatalf("rig-store error must not surface when own store is healthy; got %v", err)
	}
	if out != `[]` {
		t.Fatalf("out = %q, want city store's no-work output", out)
	}
	if gotStore.dir != "" || len(gotStore.env) != 0 {
		t.Fatalf("store = %#v, want zero value when no work is found", gotStore)
	}
}

func TestBestStoreWithWorkSkipsStoreWithOnlyUnreadyRows(t *testing.T) {
	// A store whose only row is dep-blocked is NOT a hit; federation moves on.
	stores := []hookStore{{dir: "city"}, {dir: "riga"}}
	run := func(_, dir string, _ []string) (string, error) {
		if dir == "city" {
			return `[{"id":"x","blocked_by":[{"status":"open"}]}]`, nil
		}
		return `[{"id":"va-2"}]`, nil
	}
	out, gotStore, err := bestStoreWithWork("q", stores, stores[0], run)
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if out != `[{"id":"va-2"}]` {
		t.Fatalf("out = %q, want riga work (city row was unready)", out)
	}
	if gotStore.dir != "riga" {
		t.Fatalf("store.dir = %q, want riga", gotStore.dir)
	}
}

// TestClaimStoreWithFallbackFallsBackWhenSelectedStoreRerunsEmpty pins the
// post-merge fix for the bundled gc hook --claim change: when the
// discovery-selected store loses its claimable row before the claim, the claim
// must re-select across the federated stores instead of draining as "no work"
// while a later store still has ready routed work.
func TestClaimStoreWithFallbackFallsBackWhenSelectedStoreRerunsEmpty(t *testing.T) {
	stores := []hookStore{{dir: "city"}, {dir: "riga"}}
	selected := stores[0]
	var calls []string
	run := func(_, dir string, _ []string) (string, error) {
		calls = append(calls, dir)
		switch len(calls) {
		case 1: // claim-time re-validation of the selected store: now empty.
			return `[]`, nil
		case 2: // federated re-selection: own store still empty.
			return `[]`, nil
		case 3: // later store still has ready routed work.
			return `[{"id":"va-3"}]`, nil
		default:
			t.Fatalf("unexpected call %d to %q", len(calls), dir)
			return "", nil
		}
	}

	out, gotStore, err := claimStoreWithFallback("q", stores, selected, stores[0], "", run)
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if out != `[{"id":"va-3"}]` {
		t.Fatalf("out = %q, want later-store work", out)
	}
	if gotStore.dir != "riga" {
		t.Fatalf("store.dir = %q, want riga", gotStore.dir)
	}
	if len(calls) != 3 || calls[0] != "city" || calls[1] != "city" || calls[2] != "riga" {
		t.Fatalf("calls = %v, want [city city riga]", calls)
	}
}

// TestClaimStoreWithFallbackUsesSelectedStoreWhenStillReady covers the common
// path: when the selected store still reports ready work at claim time, the
// claim acts on that store's fresh output without a redundant federated rescan.
func TestClaimStoreWithFallbackUsesSelectedStoreWhenStillReady(t *testing.T) {
	stores := []hookStore{{dir: "city"}, {dir: "riga"}}
	selected := stores[0]
	var calls []string
	run := func(_, dir string, _ []string) (string, error) {
		calls = append(calls, dir)
		return `[{"id":"va-1"}]`, nil
	}

	out, gotStore, err := claimStoreWithFallback("q", stores, selected, stores[0], "", run)
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if out != `[{"id":"va-1"}]` {
		t.Fatalf("out = %q, want selected-store work", out)
	}
	if gotStore.dir != "city" {
		t.Fatalf("store.dir = %q, want city", gotStore.dir)
	}
	if len(calls) != 1 || calls[0] != "city" {
		t.Fatalf("calls = %v, want a single [city] re-validation", calls)
	}
}

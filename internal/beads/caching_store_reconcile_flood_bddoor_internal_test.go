package beads

// ADR-0094 A1 D8 reproduce-first guard (vc-vlyk). The D4 guards in
// caching_store_ready_projection_invalidation_test.go pass on the deployed
// build gc-main-20260902-9f27db196 while the live city store emits ~808
// bead.updated per tick, so newFloodFixture's shape omits the live condition.
// This fixture is built to the close_when spec on vc-vlyk: a BdStore-backed
// cache (fake bd runner serving BOTH the list rows and the ready-projection
// SQL door), a ~9:1 message-exempt : non-exempt population with non-exempt
// types in live proportions, depsComplete driven false, real write traffic
// through the cache, a full prime mid-window, and a >=10-tick zero-write
// assertion window with non-vacuity probes on BOTH sub-populations.
//
// DELIVERABLE CONTRACT: this test must FAIL on 9f27db196 — non-exempt rows
// re-emit bead.updated on an unwritten store — with the exempt control
// silent. The red is fatal under VC_VLYK_ENFORCE=1 (see the guard comment at
// the assertion); ungated, the fixture runs and SKIPS with the recorded
// verdict, because a permanently-failing test cannot ride the shared push
// gate or merge through PR CI. The gate is a strict expected-failure: if the
// flood stops reproducing while the gate stands, the ungated run FAILS, so the
// guard can never go quietly green. NO production code change rides on this
// bead; the repair is downstream of the red, and the repair bead deletes the
// enforcement gate.
//
// Mechanism under test (ADR-0094 A1 D10 lead, pa-1 2026-09-04). As
// certified on 9f27db196 the flood had two ledger-side causes on top of the
// decline: the verdict-nil'ing path a preservation decline routes through
// (absorbReadyProjectionLocked under the reconcile absorb's readyFromFresh
// mode) marked only readyProjectionLost, which the differ's anti-flood
// substitution never reads, and Prime rebuilt both ledgers empty. #173
// (vc-u2n6) paired every verdict-nil'ing site with the disowned-value ledger,
// so on current main both of those are closed: the returning verdict finds its
// readyProjectionInvalid mark and the nil->verdict half is silent. What
// remains is the decline half — preserveCachedReadyProjectionLocked declining
// to carry a cached verdict across a verdict-less fresh row manufactures a
// verdict->nil diff on a row nothing wrote. The guard keeps classifying every
// population (floodPopulation) so a regression in the ledger pairing shows up
// as its own named population rather than folded into the decline count.
//
// The three harness-v1 gate questions, resolved against 9f27db196..main
// (791b82ad9; reconcile paths unchanged from the deployed build — only
// bdstore.go comments, conditional-release, preflight, and tests drifted):
//
//  1. bdReadyProjectionEnabled (bdstore_ready_projection.go:206) gates on the
//     scope degrade verdict and `bd version` >= 1.0.5, checked once per store
//     (readyProjectionChecked). No config/metadata read sits on the enabled
//     path: readyProjectionBackendRefusal (:332) reads .beads/metadata.json
//     only to pick the DOOR, and a missing file is not ErrUnknownBackend, so
//     the fake needs only the version verb.
//  2. Door selection: SQL door is the default (projectViaBlockedDoor :502 is
//     reached only via unknown-backend metadata, the scope latch, or a
//     runtime embedded-mode `bd sql` refusal) — the live city scope's door.
//     Row shapes: SQL answers []bdReadyProjectionRow{{id,is_blocked}} where
//     an explicit null leaves the row OUT of the result map (:469) — IsBlocked
//     stays nil; `bd blocked --json` would answer []bdBlockedRow{{id}} where
//     absence is written false (:622-626). The doors' absence semantics are
//     NOT interchangeable, which is why harness v1's single-shape answer was
//     ambiguous. This fixture drives the SQL door and flaps its column.
//  3. extractJSON (bdstore.go:702) tolerates stderr noise; the fake serves
//     pure JSON regardless.
//
// The tick loop, per non-exempt row R with a stable `bd list` payload and a
// stable blocks-edge onto target T:
//
//	OFF tick: the SQL column is null for R -> fresh R is verdict-less;
//	          preservation (caching_store_reconcile.go:850) declines to
//	          restore because T's fresh status differs from T's cached status
//	          (:885-890) — T is held frozen by a warmup write-through (cached
//	          closed) while its listed status alternates open/in_progress, so
//	          the cached-vs-fresh mismatch the decline arm needs is permanent.
//	          R emits bead.updated (verdict -> nil); since #173 its absorb
//	          marks readyProjectionInvalid as well as readyProjectionLost.
//	ON  tick: the door answers R's verdict again. Cached nil vs fresh verdict
//	          reaches the substitution, which finds the readyProjectionInvalid
//	          mark and stays silent. (On 9f27db196 the mark sat only in
//	          readyProjectionLost and R emitted AGAIN, nil -> verdict.)
//	A mid-window prime (caching_store.go) replaces non-kept rows wholesale
//	against a null column, installing nil verdicts; since #173 the rebuilt
//	rows keep their disowned-value marks, so the following ON tick is silent.
//
// The warmup write-through (closing T) takes clearDependentReadyProjectionsLocked's
// whole-cache branch and records readyProjectionInvalid per row. Tick 1's
// fresh verdicts meet those marks and stay silent: the CONTROL for the ledger
// populations — a nil'd row whose mark is in readyProjectionInvalid, facing a
// returning verdict, does not emit.
//
// Asserted population: the ten R rows only. T and W also appear in the
// emission log — T because its frozen-vs-listed status flap is a legitimate
// differ diff whenever its recency fence lapses, W once when the mid-window
// prime replaces it verdict-less — but those are artifacts of the devices,
// not the flood, and no ledger repair would (or should) silence them. The
// repair this guard awaits targets the R rows; they have no fixture-induced
// reason to emit on a healthy differ.
//
// The R-row emissions are NOT one population. Each is classified by the row's
// cached state before the pass that emitted it (floodPopulation): the OFF-tick
// verdict->nil half comes from the preservation decline itself and touches no
// ledger; an ON-tick nil->verdict emission is classified by which ledger (if
// any) held the row's mark — lost-ledger (the pre-#173 divergence), invalid-
// ledger (the substitution failing its own precondition) or unmarked (a nil
// installed with no mark at all). On current main only the decline half
// emits; the others are kept as named populations so a ledger regression is
// reported as itself rather than blamed on, or hidden inside, the decline
// half.
//
// Classification assumes a stable verdict VALUE: the fixture's door always
// answers is_blocked:false, so every verdict-bearing emission is a presence
// flip (verdict<->nil), never a false<->true change. A fixture that varied the
// value would need a fifth population for verdict->verdict diffs.

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"sort"
	"strings"
	"sync"
	"testing"
)

// floodRecEvent is one cache-emitted notification, payload bytes included so
// consecutive-payload byte-identity can be scored (live: 99.3% identical).
type floodRecEvent struct {
	eventType string
	beadID    string
	payload   []byte
}

type floodEventLog struct {
	mu     sync.Mutex
	events []floodRecEvent
}

func (l *floodEventLog) record(eventType, beadID string, payload json.RawMessage) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.events = append(l.events, floodRecEvent{
		eventType: eventType,
		beadID:    beadID,
		payload:   append([]byte(nil), payload...),
	})
}

func (l *floodEventLog) mark() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return len(l.events)
}

func (l *floodEventLog) since(n int) []floodRecEvent {
	l.mu.Lock()
	defer l.mu.Unlock()
	return append([]floodRecEvent(nil), l.events[n:]...)
}

// floodRow is one bd row the fake serves over `bd list` / `bd show`.
type floodRow struct {
	id     string
	title  string
	typ    string
	deps   []map[string]string
	status func(tick int) string
}

// floodBdRunner serves every bd subprocess a BdStore-backed cache primes,
// reconciles, enriches, and writes through, on the SQL projection door.
type floodBdRunner struct {
	mu     sync.Mutex
	tick   int  // advanced by the test between passes
	doorOn bool // the SQL column's state for the current pass

	rows map[string]*floodRow

	// coverage probes (non-vacuity), counted per door call. The door answers
	// the whole row set; which ids the client keeps is decided client-side
	// (enrichReadyProjectionForCache's wanted set), invisible to this fake.
	sqlCalls     int // every SQL door call
	sqlSetCalls  int // calls answering is_blocked:false (verdict present)
	sqlNullCalls int // calls answering is_blocked:null (verdict absent)
}

func newFloodBdRunner(rows map[string]*floodRow) *floodBdRunner {
	return &floodBdRunner{rows: rows}
}

func (r *floodBdRunner) serve(_ string, name string, args ...string) ([]byte, error) {
	if name != "bd" || len(args) == 0 {
		return []byte("[]"), nil
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	switch args[0] {
	case "version":
		// Gate 1: the only metadata the enabled path needs (>= 1.0.5).
		return []byte("bd version 1.0.6\n"), nil
	case "list":
		return json.Marshal(r.listRows())
	case "show":
		// BdStore.Get runs `bd show --json <id>` and parses a JSON array.
		row, ok := r.rows[args[len(args)-1]]
		if !ok {
			return []byte("[]"), nil
		}
		return json.Marshal([]map[string]any{r.rowJSON(row)})
	case "sql":
		// Gate 2: the SQL door (the live Dolt city scope's door). A null
		// column drops the row from the result map entirely — the exact
		// absence shape enrichReadyProjectionForCache's `ok` miss leaves nil.
		r.sqlCalls++
		if r.doorOn {
			r.sqlSetCalls++
		} else {
			r.sqlNullCalls++
		}
		out := make([]map[string]any, 0, len(r.rows))
		for id := range r.rows {
			if r.doorOn {
				out = append(out, map[string]any{"id": id, "is_blocked": false})
			} else {
				out = append(out, map[string]any{"id": id, "is_blocked": nil})
			}
		}
		return json.Marshal(out)
	case "update":
		// BdStore.Update ignores the write's output; only the error matters.
		return []byte("{}"), nil
	default:
		return []byte("[]"), nil
	}
}

func (r *floodBdRunner) rowJSON(row *floodRow) map[string]any {
	out := map[string]any{
		"id":         row.id,
		"title":      row.title,
		"status":     row.status(r.tick),
		"issue_type": row.typ,
		"created_at": "2026-09-04T00:00:00Z",
		"updated_at": "2026-09-04T00:00:00Z",
	}
	// Gate on the real wire shape: bd list rows carry the is_blocked column
	// only through the projection door, never inline (the absorb path's own
	// doc: "beads has no is_blocked JSON tag at all").
	if len(row.deps) > 0 {
		out["dependencies"] = row.deps
	}
	return out
}

func (r *floodBdRunner) listRows() []map[string]any {
	ids := make([]string, 0, len(r.rows))
	for id := range r.rows {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	out := make([]map[string]any, 0, len(ids))
	for _, id := range ids {
		out = append(out, r.rowJSON(r.rows[id]))
	}
	return out
}

const (
	floodTargetID      = "vc-flood-target"  // the frozen blocker T
	floodSacrificialID = "vc-flood-traffic" // the write-traffic row W
	floodRows          = 10                 // non-exempt rows, 1:9 against the exempt control
	floodExemptRows    = 90
)

// newFloodBdDoorFixture builds the cache, primes it against a settled door,
// closes T through a real cache write, and drives the (iii) surface: real
// write traffic with depsComplete latched false, so every invalidation caller
// takes the whole-cache branch exactly as the live D6 latch produces.
func newFloodBdDoorFixture(t *testing.T) (*CachingStore, *floodBdRunner, *floodEventLog) {
	t.Helper()

	rows := map[string]*floodRow{}
	// Non-exempt population in live proportions (decision 36%, molecule 22%,
	// task/bug/convoy the rest per the vc-vlyk measurement), each with a
	// ready-blocking edge onto the shared target T.
	nonExemptTypes := []string{"decision", "decision", "decision", "decision", "molecule", "molecule", "task", "task", "bug", "convoy"}
	for i, typ := range nonExemptTypes {
		id := fmt.Sprintf("vc-flood-%02d", i)
		rows[id] = &floodRow{
			id:     id,
			title:  "flood row " + typ,
			typ:    typ,
			deps:   []map[string]string{{"issue_id": id, "depends_on_id": floodTargetID, "type": "blocks"}},
			status: func(int) string { return "open" },
		}
	}
	// Exempt control: 90 message rows, 9:1. They are never enriched
	// (skipBDReadyProjectionEnrichment) and must stay silent throughout.
	for i := 0; i < floodExemptRows; i++ {
		id := fmt.Sprintf("vc-flood-msg-%03d", i)
		rows[id] = &floodRow{
			id: id, title: "message " + id, typ: "message",
			status: func(int) string { return "open" },
		}
	}
	// T: cached closed by the warmup write-through; listed open/in_progress
	// alternating — ALWAYS different from cached, so the preservation guard's
	// status arm fires for every dependent row on every tick while the differ
	// merge-skips T itself (recent-local + changed) without an emission.
	parityStatus := func(tick int) string {
		if tick%2 == 0 {
			return "in_progress"
		}
		return "open"
	}
	rows[floodTargetID] = &floodRow{
		id: floodTargetID, title: "target", typ: "task",
		status: parityStatus,
	}
	// W: the sacrificial write-traffic row for the (iii) interleave. Frozen
	// mid-flight by the same recency skip, emitting nothing of its own.
	rows[floodSacrificialID] = &floodRow{
		id: floodSacrificialID, title: "traffic", typ: "task",
		status: func(int) string { return "open" },
	}

	runner := newFloodBdRunner(rows)
	// A unique dir per test isolates the process-global ready-projection
	// scope guard (readyProjectionGuards is a sync.Map keyed by dir).
	store := NewBdStore(t.TempDir(), runner.serve)
	log := &floodEventLog{}
	cache := NewCachingStoreForTest(store, log.record)

	// Prime against a settled ON door: every non-exempt row installs a
	// verdict, deps ride the rows inline (witnessed), the exempt control
	// installs nil and stays that way.
	runner.mu.Lock()
	runner.doorOn = true
	runner.mu.Unlock()
	if err := cache.Prime(context.Background()); err != nil {
		t.Fatalf("prime: %v", err)
	}

	// Warmup write-through: close T for real. This stamps T recent-local
	// (the 5s recency skip that freezes it against its flapping listed
	// status) and invalidates every dependent's verdict the D4 way —
	// readyProjectionInvalid entries carrying the disowned values, which is
	// what lets the differ substitution fire exactly once below before the
	// lost path takes over.
	closed := "closed"
	if err := cache.Update(floodTargetID, UpdateOpts{Status: &closed}); err != nil {
		t.Fatalf("warmup close of %s: %v", floodTargetID, err)
	}

	// The (iii) surface: depsComplete latched false so every invalidation
	// caller takes clearAllReadyProjectionsLocked. The production chain (the
	// coverage-unknown dep drop described on ApplyEventSnapshot, ga-yoix1 /
	// the D6 bead) ends in exactly this latch; the D4(b) guard emulates it
	// the same direct way.
	cache.mu.Lock()
	cache.depsComplete = false
	cache.mu.Unlock()

	inProgress := "in_progress"
	open := "open"
	// Real write traffic through the cache — a bd-hook-shaped event patch and
	// a write-through status update — each of which reaches
	// clearDependentReadyProjectionsLocked and, with the latch down, the
	// whole-cache branch. Payloads are verdict-less (beadslib shape: no
	// is_blocked tag), as live event payloads are.
	evolve := func(id string, status string) {
		payload, err := json.Marshal(map[string]any{"id": id, "status": status})
		if err != nil {
			t.Fatalf("marshal event: %v", err)
		}
		cache.ApplyEvent("bead.updated", payload)
	}
	evolve(floodSacrificialID, inProgress)
	if err := cache.Update(floodSacrificialID, UpdateOpts{Status: &open}); err != nil {
		t.Fatalf("write-traffic update of %s: %v", floodSacrificialID, err)
	}
	return cache, runner, log
}

// floodOccupancy is the per-pass discriminating measurement (acceptance 2):
// which ledger holds the non-exempt rows whose verdicts are gone.
type floodOccupancy struct {
	lost         int // rows marked readyProjectionLost (the absorb lost-path ledger)
	invalid      int // rows marked readyProjectionInvalid (the invalidation ledger)
	nilVerdict   int // non-exempt rows cached with IsBlocked == nil
	depsComplete bool
}

func floodOccupancyOf(c *CachingStore, isFloodRow func(string) bool) floodOccupancy {
	c.mu.RLock()
	defer c.mu.RUnlock()
	occ := floodOccupancy{depsComplete: c.depsComplete}
	for id, bead := range c.beads {
		if !isFloodRow(id) {
			continue
		}
		if _, lost := c.readyProjectionLost[id]; lost {
			occ.lost++
		}
		if _, invalid := c.readyProjectionInvalid[id]; invalid {
			occ.invalid++
		}
		if bead.IsBlocked == nil {
			occ.nilVerdict++
		}
	}
	return occ
}

// floodPopulation names the mechanism behind one flood-row emission, decided
// from the row's cached state immediately BEFORE the pass that emitted it.
// The populations need different repairs, so the guard counts and reports them
// separately: a repair aimed at one leaves the others red, and the verdict must
// say which half is still flooding instead of blaming a ledger for all of it.
type floodPopulation int

const (
	// floodDecline: the row held a verdict and the pass nil'd it — the fresh
	// row was verdict-less (door null) and preserveCachedReadyProjectionLocked
	// declined to carry the cached verdict across (T's cached-vs-fresh status
	// mismatch). No mark ledger is involved; the verdict->nil diff is
	// manufactured by the decline itself.
	floodDecline floodPopulation = iota
	// floodLostLedger: the row was nil and marked ONLY in readyProjectionLost,
	// and the pass emitted nil->verdict. The differ substitution reads
	// readyProjectionInvalid, so it cannot fire for these rows. This was the
	// ON-tick half on 9f27db196; #173 pairs every nil'ing site with the
	// invalid ledger, so a nonzero count here is a regression of that pairing.
	floodLostLedger
	// floodInvalidLedger: the row was nil and marked in readyProjectionInvalid
	// — the substitution's own precondition — and still emitted. On the
	// deployed build this population is silent (the tick-1 control).
	floodInvalidLedger
	// floodUnmarkedNil: the row was nil with NO mark in either ledger and the
	// pass emitted nil->verdict. Before #173 the mid-window prime rebuilt both
	// ledgers empty and its verdict-less installs landed here; since #173 they
	// keep their marks, so a nonzero count means some path again installs a
	// nil verdict without recording the disowned value.
	floodUnmarkedNil
	floodPopulationCount
)

func (p floodPopulation) String() string {
	switch p {
	case floodDecline:
		return "decline(verdict->nil)"
	case floodLostLedger:
		return "lost-ledger(nil->verdict)"
	case floodInvalidLedger:
		return "invalid-ledger(nil->verdict)"
	case floodUnmarkedNil:
		return "unmarked-nil(nil->verdict)"
	default:
		return fmt.Sprintf("floodPopulation(%d)", int(p))
	}
}

// floodPrePass records, per flood row, which population an emission from the
// coming pass would belong to.
func floodPrePass(c *CachingStore, isFloodRow func(string) bool) map[string]floodPopulation {
	c.mu.RLock()
	defer c.mu.RUnlock()
	out := make(map[string]floodPopulation)
	for id, bead := range c.beads {
		if !isFloodRow(id) {
			continue
		}
		_, lost := c.readyProjectionLost[id]
		_, invalid := c.readyProjectionInvalid[id]
		switch {
		case bead.IsBlocked != nil:
			out[id] = floodDecline
		case invalid:
			out[id] = floodInvalidLedger
		case lost:
			out[id] = floodLostLedger
		default:
			out[id] = floodUnmarkedNil
		}
	}
	return out
}

// TestBdStoreBackedFloodReproduction is the vc-vlyk deliverable: a guard that
// FAILS on the deployed build because non-exempt rows re-emit bead.updated
// across >= 10 zero-write ticks while the exempt control stays silent. It goes
// green only when EVERY flood population is silent — the ADR-0094 invariant is
// "an unwritten row never emits", not "one mechanism stops emitting" — and its
// verdict reports each population with its own mechanism, so a repair that
// silences one half is told exactly which half is still red.
func TestBdStoreBackedFloodReproduction(t *testing.T) {
	t.Parallel()

	cache, runner, log := newFloodBdDoorFixture(t)
	// The asserted flood population is the ten R rows (vc-flood-00..09) and
	// only them: see the header — the target/traffic devices may legitimately
	// emit their own fixture-induced diffs, and the repair must not be judged
	// by them.
	isFloodRow := func(id string) bool {
		rest, ok := strings.CutPrefix(id, "vc-flood-")
		return ok && len(rest) == 2 && rest[0] >= '0' && rest[0] <= '9' && rest[1] >= '0' && rest[1] <= '9'
	}
	isExempt := func(id string) bool { return strings.HasPrefix(id, "vc-flood-msg-") }

	// The assertion window: 12 zero-write ticks (>= 10 per the close_when),
	// the SQL column flapping null/set per tick, a full prime mid-window —
	// zero local writes, zero events; exactly the idle-store shape the live
	// flood sustains itself across.
	const windowTicks = 12
	const primeAtTick = 6
	occLog := make([]string, 0, windowTicks)
	var byPopulation [floodPopulationCount]int
	// presented counts nil flood rows that faced a returning verdict (door on)
	// per ledger state — the denominator that makes a silent population a
	// measurement rather than an absence of opportunity.
	var presented [floodPopulationCount]int

	windowStart := log.mark()
	for tick := 1; tick <= windowTicks; tick++ {
		doorOn := tick%2 == 1 // odd ticks answer, even ticks null
		runner.mu.Lock()
		runner.tick = tick
		runner.doorOn = doorOn
		runner.mu.Unlock()

		if tick == primeAtTick {
			// The per-request rebuild cadence (cmd/gc rebuilds per request;
			// the control dispatcher per controlReadyCacheTTL). A read, not a
			// write: it rebuilds rows and installs whatever the door answered
			// THIS pass (since #173 the rebuilt rows keep their marks).
			runner.mu.Lock()
			runner.doorOn = false // prime against a null column: nil installs
			runner.mu.Unlock()
			if err := cache.Prime(context.Background()); err != nil {
				t.Fatalf("mid-window prime: %v", err)
			}
			runner.mu.Lock()
			runner.doorOn = doorOn
			runner.mu.Unlock()
		}

		pre := floodPrePass(cache, isFloodRow)
		if doorOn {
			for _, pop := range pre {
				if pop != floodDecline {
					presented[pop]++
				}
			}
		}
		passStart := log.mark()
		cache.runReconciliation()
		flooded := 0
		var tickPop [floodPopulationCount]int
		for _, ev := range log.since(passStart) {
			if ev.eventType != "bead.updated" || !isFloodRow(ev.beadID) {
				continue
			}
			pop, ok := pre[ev.beadID]
			if !ok {
				t.Fatalf("tick %d: flood row %s emitted but was not cached before the pass", tick, ev.beadID)
			}
			byPopulation[pop]++
			tickPop[pop]++
			flooded++
		}

		// The whole reproduction rests on the recentLocalMutation fence keeping
		// T cached closed: that is what makes preservation decline. If the
		// fence ever lapses (a loaded box stretching the window past its 5s),
		// T is absorbed, the decline stops, and the flood rows fall silent for
		// a reason that has nothing to do with a repair. Fail loudly instead of
		// reporting that silence as the green future.
		cache.mu.RLock()
		target, ok := cache.beads[floodTargetID]
		cache.mu.RUnlock()
		if !ok || target.Status != "closed" {
			t.Fatalf("fixture precondition lost at tick %d: %s cached status=%q (present=%v), want \"closed\" — the recency fence lapsed, so preservation no longer declines and flood silence here would be vacuous", tick, floodTargetID, target.Status, ok)
		}

		occ := floodOccupancyOf(cache, isFloodRow)
		// Non-vacuity of the flood rows themselves: after every door-on pass
		// each flood row must hold a verdict. Without this, a door that never
		// answered for the flood rows (always null for them) leaves them nil
		// throughout — nothing to decline, nothing to flip — and the silence
		// would read as a repair, even under VC_VLYK_ENFORCE=1.
		if doorOn && occ.nilVerdict != 0 {
			t.Fatalf("fixture non-vacuity failed at tick %d: %d/%d flood rows hold no verdict after a door-on pass; the door never gave them a verdict to lose, so flood silence here would be vacuous", tick, occ.nilVerdict, floodRows)
		}
		occLog = append(occLog, fmt.Sprintf(
			"tick %2d: door=%-5v flood-emissions=%2d [decline=%2d lost-ledger=%2d invalid-ledger=%2d unmarked-nil=%2d] after: lost=%2d invalid=%2d nil-verdict=%2d depsComplete=%v",
			tick, doorOn, flooded,
			tickPop[floodDecline], tickPop[floodLostLedger], tickPop[floodInvalidLedger], tickPop[floodUnmarkedNil],
			occ.lost, occ.invalid, occ.nilVerdict, occ.depsComplete))
	}
	windowEvents := log.since(windowStart)

	// ---- Non-vacuity probes (fixture must prove it exercises the shape) ----
	runner.mu.Lock()
	sqlCalls, sqlSet, sqlNull := runner.sqlCalls, runner.sqlSetCalls, runner.sqlNullCalls
	runner.mu.Unlock()
	// The door must have been consulted on every reconcile pass and in BOTH
	// states: a guard whose door never flapped, or that stopped being asked,
	// would pass vacuously.
	if sqlCalls < windowTicks || sqlSet == 0 || sqlNull == 0 {
		t.Fatalf("fixture non-vacuity failed: sql door calls=%d over %d passes (verdict-set=%d, null=%d); the door was not consulted every pass in both states and this guard would pass vacuously", sqlCalls, windowTicks, sqlSet, sqlNull)
	}

	cache.mu.RLock()
	exemptStillNil := 0
	for id, bead := range cache.beads {
		if isExempt(id) && bead.IsBlocked == nil {
			exemptStillNil++
		}
	}
	cache.mu.RUnlock()
	if exemptStillNil != floodExemptRows {
		t.Fatalf("fixture non-vacuity failed: only %d/%d exempt rows stayed verdict-less; the control population was not exercised as designed", exemptStillNil, floodExemptRows)
	}

	// ---- Attribution measurement (acceptance 2) ----
	endOcc := floodOccupancyOf(cache, isFloodRow)
	perID := map[string]int{}
	deviceEmissions := map[string]int{}
	byteIdentical, bytePairs := 0, 0
	lastPayload := map[string][]byte{}
	var controlViolations []string
	for _, ev := range windowEvents {
		if ev.eventType != "bead.updated" {
			continue
		}
		if isExempt(ev.beadID) {
			controlViolations = append(controlViolations, ev.beadID)
			continue
		}
		if !isFloodRow(ev.beadID) {
			deviceEmissions[ev.beadID]++
			continue
		}
		perID[ev.beadID]++
		if prev, ok := lastPayload[ev.beadID]; ok {
			bytePairs++
			if string(prev) == string(ev.payload) {
				byteIdentical++
			}
		}
		lastPayload[ev.beadID] = ev.payload
	}

	// ---- Control: the exempt population must be silent, red or not ----
	if len(controlViolations) > 0 {
		t.Fatalf("exempt control FAILED: %d message-row emissions — the fixture leaked the flood into the exemption population, so this red is not the ADR-0094 signature (first offenders: %v)",
			len(controlViolations), controlViolations[:min(5, len(controlViolations))])
	}

	// ---- Per-bead/tick bound (fixture sanity): <= 1 emission each ----
	for id, n := range perID {
		if n > windowTicks {
			t.Fatalf("flood row %s emitted %d times in %d ticks (>1/tick) — the fixture is double-counting, not reproducing the 1/row/tick flood", id, n, windowTicks)
		}
	}
	for id, n := range deviceEmissions {
		if n > windowTicks {
			t.Fatalf("device row %s emitted %d times in %d ticks (>1/tick) — the fixture is double-counting", id, n, windowTicks)
		}
	}

	// ---- THE GUARD (expected RED on 9f27db196) ----
	//
	// Enforcement is gated: a permanently-failing test cannot ride the shared
	// gate (.githooks/pre-push execs `make test-fast-parallel` over ~190
	// packages — a deterministically red test here blocks EVERY push touching
	// a .go file, the ga-at7jv0 P0 shape) and cannot merge (PR CI runs the
	// same suite). The gate is a STRICT expected-failure, not an off switch:
	//
	//   - flood present, ungated: SKIP with the per-population verdict (the
	//     known red, recorded on every run);
	//   - flood present, VC_VLYK_ENFORCE=1: FAIL (certify the red);
	//   - flood ABSENT, ungated: FAIL — the expected red vanished while the
	//     gate still stands, so either the repair landed (delete the gate) or
	//     the fixture rotted (fix it). Silence is never accepted unreviewed;
	//   - flood absent, VC_VLYK_ENFORCE=1: PASS.
	//
	//	VC_VLYK_ENFORCE=1 go test ./internal/beads/ -run TestBdStoreBackedFloodReproduction
	//
	// The repair bead deletes this gate, making the silence assertion
	// unconditional.
	totalFlood := 0
	for _, n := range byPopulation {
		totalFlood += n
	}
	enforce := os.Getenv("VC_VLYK_ENFORCE") == "1"
	if totalFlood == 0 {
		// Reached only when every flood population is silent. Vacuity of that
		// green is ruled out above: the door flapped and was consulted every
		// pass, every flood row held a verdict after every door-on pass (so
		// each null tick had a verdict to lose), and T stayed cached closed on
		// every tick, so preservation's decline arm stayed armed. Ledger occupancy is deliberately NOT a vacuity signal: a
		// repair that preserves verdicts through the decline leaves no marks
		// behind, and demanding residual marks would false-positive it.
		if !enforce {
			t.Fatalf("expected-red XPASS: the ADR-0094 flood no longer reproduces (0 flood-row emissions over %d zero-write ticks) while the VC_VLYK_ENFORCE gate still stands. If the repair landed, delete the gate so this silence assertion is unconditional; otherwise the fixture has stopped exercising the mechanism.\n\nPer-tick instrument:\n  %s",
				windowTicks, strings.Join(occLog, "\n  "))
		}
		return
	}

	ids := make([]string, 0, len(perID))
	for id := range perID {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	devices := make([]string, 0, len(deviceEmissions))
	for id, n := range deviceEmissions {
		devices = append(devices, fmt.Sprintf("%s=%d", id, n))
	}
	sort.Strings(devices)
	identity := 0.0
	if bytePairs > 0 {
		identity = float64(byteIdentical) / float64(bytePairs)
	}
	var breakdown []string
	for pop := floodPopulation(0); pop < floodPopulationCount; pop++ {
		line := fmt.Sprintf("%-30s emissions=%3d", pop, byPopulation[pop])
		if pop != floodDecline {
			line += fmt.Sprintf(" (nil rows facing a returning verdict: %d)", presented[pop])
		}
		breakdown = append(breakdown, line)
	}
	var attribution []string
	if n := byPopulation[floodDecline]; n > 0 {
		attribution = append(attribution, fmt.Sprintf(
			"- decline (%d): on null-door ticks preserveCachedReadyProjectionLocked declines to carry the cached verdict across (T's cached status differs from its fresh one) and the absorb installs nil — a verdict->nil diff on a row nothing wrote. No mark ledger is consulted on this half; changing which ledger the substitution reads cannot silence it.", n))
	}
	if n := byPopulation[floodLostLedger]; n > 0 {
		attribution = append(attribution, fmt.Sprintf(
			"- lost-ledger (%d): rows nil'd by that decline carried a mark in readyProjectionLost but NOT in readyProjectionInvalid, the ledger the differ substitution (caching_store_reconcile.go, the cached-nil/fresh-verdict arm) reads, so the returning verdict re-emitted. #173 (vc-u2n6) paired every verdict-nil'ing site with the invalid ledger; this population means that pairing regressed. Control: rows marked in readyProjectionInvalid facing the same returning verdict emitted %d times over %d presentations.", n, byPopulation[floodInvalidLedger], presented[floodInvalidLedger]))
	}
	if n := byPopulation[floodInvalidLedger]; n > 0 {
		attribution = append(attribution, fmt.Sprintf(
			"- invalid-ledger (%d): rows marked in readyProjectionInvalid emitted nil->verdict anyway — the substitution's own precondition held and it did not fire. This is a defect in the substitution itself, not in which ledger it reads.", n))
	}
	if n := byPopulation[floodUnmarkedNil]; n > 0 {
		attribution = append(attribution, fmt.Sprintf(
			"- unmarked-nil (%d): rows installed verdict-less with NO mark in either ledger emitted nil->verdict when the door answered. Before #173 the mid-window Prime produced these by rebuilding both ledgers empty; this population means some path again installs a nil verdict without recording the disowned value.", n))
	}
	verdict := fmt.Sprintf("ADR-0094 flood REPRODUCED on this build: %d bead.updated emissions across %d unwritten flood rows (%v) in %d zero-write ticks; exempt control silent; consecutive-payload byte-identity %.1f%% over %d pairs (live measured 99.3%%; here the two flap halves alternate within each row); device rows logged, not asserted: [%s].\n\nBy population (each needs its own repair; the guard goes green only when ALL are zero):\n  %s\n\nAttribution:\n%s\n\nEnd of window among flood rows: lost=%d invalid=%d nil-verdict=%d.\n\nPer-tick instrument (acceptance 2):\n  %s",
		totalFlood, len(perID), ids, windowTicks, identity*100, bytePairs, strings.Join(devices, " "),
		strings.Join(breakdown, "\n  "),
		strings.Join(attribution, "\n"),
		endOcc.lost, endOcc.invalid, endOcc.nilVerdict,
		strings.Join(occLog, "\n  "))
	if !enforce {
		t.Skipf("%s\n\n[gated, strict expected-red] A permanently-red test cannot ride the shared push gate (ga-at7jv0 precedent); certify the red with: VC_VLYK_ENFORCE=1 go test ./internal/beads/ -run TestBdStoreBackedFloodReproduction. When the flood stops reproducing this test FAILS until the gate is deleted.", verdict)
	}
	t.Fatal(verdict)
}

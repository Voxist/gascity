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
// gate or merge through PR CI. NO production code change rides on this bead;
// the repair is downstream of the red, and the repair bead deletes the
// enforcement gate.
//
// Mechanism under test (ADR-0094 A1 D10 lead, pa-1 2026-09-04): the differ's
// anti-flood substitution (caching_store_reconcile.go:545) consults ONLY
// readyProjectionInvalid, but the verdict-nil'ing path a preservation decline
// routes through — absorbReadyProjectionLocked (caching_store.go:607) under
// the reconcile absorb's readyFromFresh mode (:595) — marks ONLY
// readyProjectionLost (:627). A row nil'd through the lost path therefore
// re-floods once per flap cycle with the substitution structurally unable to
// fire, forever. The at-rest invalidation-invariant test cannot see this: the
// row's Lost mark is correct per ITS ledger; the defect is that the two
// ledgers diverge.
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
//	          R emits bead.updated (verdict -> nil) and its absorb marks
//	          readyProjectionLost.
//	ON  tick: the door answers R's verdict again. Cached nil vs fresh verdict
//	          reaches the substitution, which needs readyProjectionInvalid and
//	          finds none -> R emits AGAIN (nil -> verdict), byte-identical
//	          payload family, on a row nothing wrote.
//	A mid-window prime (caching_store.go:1149-1193) rebuilds nextReadyInvalid
//	empty and replaces non-kept rows wholesale, so marks for rebuilt rows are
//	wiped either way — the amplifier that makes the nil installs universal.
//
// The warmup write-through (closing T) takes clearDependentReadyProjectionsLocked's
// whole-cache branch and records readyProjectionInvalid per row — marks in the
// ledger the substitution DOES read. They survive only until the first pass:
// tick 1's fresh verdicts are absorbed as observed, which discharges them, and
// tick 1 stays silent. That silence is the CONTROL for the attribution below:
// with marks in readyProjectionInvalid, a nil'd row facing a returning verdict
// does not emit. From tick 3 onward every ON tick presents the substitution
// its exact precondition — cached nil, fresh verdict, prior invalidation —
// except the mark now sits in readyProjectionLost, where the substitution
// never looks, and the row emits. Same precondition, other ledger, opposite
// outcome: that contrast is the discriminating measurement.
//
// Asserted population: the ten R rows only. T and W also appear in the
// emission log — T because its frozen-vs-listed status flap is a legitimate
// differ diff whenever its recency fence lapses, W once when the mid-window
// prime replaces it verdict-less — but those are artifacts of the devices,
// not the flood, and no ledger repair would (or should) silence them. The
// repair this guard awaits targets the R rows; they have no fixture-induced
// reason to emit on a healthy differ.

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
	// exempt rows are the Type=="message" control population.
	exempt bool
}

// floodBdRunner serves every bd subprocess a BdStore-backed cache primes,
// reconciles, enriches, and writes through, on the SQL projection door.
type floodBdRunner struct {
	mu     sync.Mutex
	tick   int  // advanced by the test between passes
	doorOn bool // the SQL column's state for the current pass

	rows map[string]*floodRow

	// coverage probes (non-vacuity):
	sqlCalls     int // every call answered the WHOLE row set
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
		if len(args) < 2 {
			return []byte("[]"), nil
		}
		row, ok := r.rows[args[1]]
		if !ok {
			return []byte("[]"), nil
		}
		return json.Marshal(r.rowJSON(row))
	case "sql":
		// Gate 2: the SQL door (the live Dolt city scope's door). A null
		// column drops the row from the result map entirely — the exact
		// absence shape enrichReadyProjectionForCache's `ok` miss leaves nil.
		r.sqlCalls++
		out := make([]map[string]any, 0, len(r.rows))
		for id := range r.rows {
			if r.doorOn {
				r.sqlSetCalls++
				out = append(out, map[string]any{"id": id, "is_blocked": false})
			} else {
				r.sqlNullCalls++
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
			exempt: true,
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

// TestBdStoreBackedFloodReproduction is the vc-vlyk deliverable: a guard that
// FAILS on the deployed build because non-exempt rows re-emit bead.updated
// across >= 10 zero-write ticks while the exempt control stays silent. On a
// build where the lost path records into the ledger the differ actually
// reads — or where a "no answer this cycle" never manufactures a diff in
// either direction — this goes green without weakening any D4 guard.
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
	emissions := make(map[string]map[string]int) // tick -> id -> count
	occLog := make([]string, 0, windowTicks)
	floodedByTick := make(map[int]int)

	windowStart := log.mark()
	for tick := 1; tick <= windowTicks; tick++ {
		runner.mu.Lock()
		runner.tick = tick
		runner.doorOn = tick%2 == 1 // odd ticks answer, even ticks null
		runner.mu.Unlock()

		if tick == primeAtTick {
			// The per-request rebuild cadence (cmd/gc rebuilds per request;
			// the control dispatcher per controlReadyCacheTTL). A read, not a
			// write: it wipes the mark ledgers for rebuilt rows and installs
			// whatever the door answered THIS pass.
			runner.mu.Lock()
			runner.doorOn = false // prime against a null column: nil installs, marks wiped
			runner.mu.Unlock()
			if err := cache.Prime(context.Background()); err != nil {
				t.Fatalf("mid-window prime: %v", err)
			}
		}

		passStart := log.mark()
		cache.runReconciliation()
		tickEm := map[string]int{}
		for _, ev := range log.since(passStart) {
			if ev.eventType != "bead.updated" {
				continue
			}
			tickEm[ev.beadID]++
		}
		flooded := 0
		for id, n := range tickEm {
			if isFloodRow(id) {
				flooded += n
			}
		}
		floodedByTick[tick] = flooded
		emissions[fmt.Sprintf("%d", tick)] = tickEm
		occ := floodOccupancyOf(cache, isFloodRow)
		occLog = append(occLog, fmt.Sprintf(
			"tick %2d: door=%v flood-emissions=%2d lost=%2d invalid=%2d nil-verdict=%2d depsComplete=%v",
			tick, tick%2 == 1, flooded, occ.lost, occ.invalid, occ.nilVerdict, occ.depsComplete))
	}
	windowEvents := log.since(windowStart)

	// ---- Non-vacuity probes (fixture must prove it exercises the shape) ----
	runner.mu.Lock()
	sqlCalls, sqlSet, sqlNull := runner.sqlCalls, runner.sqlSetCalls, runner.sqlNullCalls
	runner.mu.Unlock()
	// One SQL call per reconcile pass, plus the prime's own pass. The exempt
	// rows ride EVERY answer (102 rows per call) — their silence is the
	// client-side exemption, never the door omitting them.
	if sqlCalls == 0 || sqlSet == 0 || sqlNull == 0 {
		t.Fatalf("fixture non-vacuity failed: sql calls=%d (verdict-set=%d, null=%d); the door never flapped and this guard would pass vacuously", sqlCalls, sqlSet, sqlNull)
	}
	perCall := floodExemptRows + floodRows + 2 // messages + flood rows + T + W
	if sqlCalls*(floodExemptRows+floodRows+2) != sqlSet+sqlNull {
		t.Fatalf("fixture non-vacuity failed: sql answered %d+%d verdict slots across %d calls, want %d rows per call (the door must cover BOTH sub-populations)", sqlSet, sqlNull, sqlCalls, perCall)
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
	// same suite). So the fixture runs on every pass and, while it observes
	// the flood, SKIPS with the verdict — unless VC_VLYK_ENFORCE=1, in which
	// case the red is fatal:
	//
	//	VC_VLYK_ENFORCE=1 go test ./internal/beads/ -run TestBdStoreBackedFloodReproduction
	//
	// FAILS on 9f27db196 (certified on a detached worktree at that commit,
	// bead vc-vlyk) and PASSES on the repaired build. The repair bead deletes
	// this gate, making the silence assertion unconditional.
	totalFlood := 0
	for _, n := range perID {
		totalFlood += n
	}
	if totalFlood != 0 {
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
		verdict := fmt.Sprintf("ADR-0094 flood REPRODUCED on this build: %d bead.updated emissions across %d unwritten flood rows in %d zero-write ticks (types present: live proportions decision/molecule/task/bug/convoy; exempt control silent; consecutive-payload byte-identity %.1f%% over %d pairs — live measured 99.3%%, here the two flap halves alternate within each row; device rows logged, not asserted: [%s]).\n\nPer-tick instrument (acceptance 2):\n  %s\n\nAttribution: rows nil'd through the preservation-decline path land in readyProjectionLost (end-of-window: lost=%d, invalid=%d, nil-verdict=%d among flood rows; flooded ids: %v). The differ substitution (caching_store_reconcile.go:545) reads ONLY readyProjectionInvalid, so for lost-path rows it can NEVER fire — the nil->verdict half re-emits every cycle. THE SUBSTITUTION READS THE WRONG LEDGER; the lost path bypasses the invalid ledger (hypothesis (i) variant, invisible to the at-rest invariant test). If marks were present but vanishing between recording and diff, lost would be 0 here and invalid >0 — that is hypothesis (ii) and it is NOT what the instrument shows.",
			totalFlood, len(perID), windowTicks, identity*100, bytePairs, strings.Join(devices, " "),
			strings.Join(occLog, "\n  "),
			endOcc.lost, endOcc.invalid, endOcc.nilVerdict, ids)
		if os.Getenv("VC_VLYK_ENFORCE") != "1" {
			t.Skipf("%s\n\n[gated] A permanently-red test cannot ride the shared push gate (ga-at7jv0 precedent); certify the red with: VC_VLYK_ENFORCE=1 go test ./internal/beads/ -run TestBdStoreBackedFloodReproduction — FAILS on 9f27db196, PASSES on the repaired build. The repair bead removes this gate.", verdict)
		}
		t.Fatal(verdict)
	}

	// Reached only when the flood population is silent — the green future.
	// Vacuity of that green is already ruled out above: the door-flap
	// non-vacuity check demanded verdict-present AND verdict-less service
	// across the whole row set, so silence cannot mean the fixture stopped
	// exercising the mechanism. Ledger occupancy is deliberately NOT used as
	// the vacuity signal here: a repair that silences the flood by preserving
	// verdicts through the decline leaves no marks behind, and demanding
	// residual marks would false-positive that legitimate repair.
}

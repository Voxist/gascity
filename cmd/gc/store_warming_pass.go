package main

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/fsys"
)

// L3 of the vc-ny00 store-warming plan: the automated warming pass.
//
// WHAT IT AUTOMATES. After a managed restart the operator used to hand-run a
// warming window against each store (vc-t6xp). ADR-0064 already made exactly
// this conversion for the delivery window, and gave the reason: "each
// execution worked; each was undone by the next restart." A hand-run step is
// not a mechanism.
//
// WHAT IT IS NOT. It is not the fix for the incident. The 2026-09-06 cliff
// began +29 MINUTES after start, long after any boot-time gate had finished,
// so a one-shot post-start pass could not have caught it — L1's bound and
// breaker and L2's continuous, probe-entered state machine are what make
// that class survivable and visible. This pass only SHORTENS the degraded
// period after a restart, which is the one case it can see coming.
//
// HOW IT READS. Through the LIVE listener at the settled 30000 wall — no
// second listener and no runtime reconfiguration (ADR-0064 constraint 1: one
// process, one deadline, fixed at start; session overrides are accepted and
// ignored). Three read shapes per attempt, cheapest first, so a store that
// is merely unreachable fails fast instead of burning the budget on a scan:
// an id-keyed lookup (the "SELECT 1" analogue), the reconcile's own
// full-scan shape, and the order-tracking sweep's label query — the heavy
// read whose slowness is what actually stalls the tick.
//
// CONSTRAINT 3 IS ABSOLUTE. A failed, slow or skipped pass never blocks
// serving. It runs on its own goroutine after the server is already ready
// and published; refusing to serve because a warming pass failed would
// convert a degradation into an outage, which is strictly worse.

const (
	// storeWarmingBudgetEnv is L3's kill switch and budget knob.
	// GC_STORE_WARMING_BUDGET_S=0 disables the pass; the outcome record
	// still says so out loud (constraint 4).
	storeWarmingBudgetEnv = "GC_STORE_WARMING_BUDGET_S"
	// defaultStoreWarmingBudget bounds the whole pass. Generous, because
	// the observed degraded periods were HOURS and the pass costs nothing
	// but local queries against a listener the fleet is already paying to
	// warm; bounded, because a warm loop with no end is a background
	// watcher by another name.
	defaultStoreWarmingBudget = 30 * time.Minute
	// storeWarmingPassFileName is the durable outcome record, a sibling of
	// dolt-delivery-window-outcome.json and shaped the same way (AC5).
	storeWarmingPassFileName = "dolt-store-warming-pass.json"

	storeWarmingPassInitialBackoff = 2 * time.Second
	storeWarmingPassMaxBackoff     = 30 * time.Second

	// storeWarmingPassStateStarted marks a pass that began but whose
	// process exited before it finished — see writeStoreWarmingPassStarted.
	storeWarmingPassStateStarted = "started"
	storeWarmingPassStateRan     = "ran"
	storeWarmingPassStateSkipped = "skipped"
	storeWarmingPassStateFailed  = "failed"
)

// storeWarmingPassStoreOutcome is one store's result within a pass.
type storeWarmingPassStoreOutcome struct {
	Store string `json:"store"`
	// Warmed reports whether this store beat the wall before the budget ran
	// out. False with no Err means the budget expired while it was still
	// slow — a real outcome, not an error.
	Warmed      bool   `json:"warmed"`
	Attempts    int    `json:"attempts"`
	LastProbeMs int64  `json:"last_probe_ms"`
	Err         string `json:"err,omitempty"`
}

// storeWarmingPassOutcome is the durable AC5 record. State is the
// three-way distinction the delivery window's own record established:
// ran / skipped / failed must be machine-distinguishable, not three ways of
// printing to a stream nothing captures.
type storeWarmingPassOutcome struct {
	At       time.Time                      `json:"at"`
	State    string                         `json:"state"`
	Skipped  string                         `json:"skipped,omitempty"`
	Err      string                         `json:"err,omitempty"`
	Duration string                         `json:"duration,omitempty"`
	BudgetS  int                            `json:"budget_s"`
	WallMs   int64                          `json:"wall_ms"`
	Stores   []storeWarmingPassStoreOutcome `json:"stores,omitempty"`
}

// storeWarmingPassBudget resolves the budget. ok=false means the pass is
// switched off, which is a SKIP the caller must still record loudly.
func storeWarmingPassBudget() (time.Duration, bool) {
	raw := strings.TrimSpace(os.Getenv(storeWarmingBudgetEnv))
	if raw == "" {
		return defaultStoreWarmingBudget, true
	}
	secs, err := strconv.Atoi(raw)
	if err != nil {
		// A garbage value is not consent to disable the pass; fall back to
		// the default rather than silently skipping.
		return defaultStoreWarmingBudget, true
	}
	if secs <= 0 {
		return 0, false
	}
	return time.Duration(secs) * time.Second, true
}

// storeWarmingPassPath resolves the outcome record's path from a pack state
// dir, honoring the same overrides as the rest of the managed-dolt files.
func storeWarmingPassPath(packStateDir string) string {
	return filepath.Join(packStateDir, storeWarmingPassFileName)
}

// writeStoreWarmingPassFile persists the outcome record. Mirrors
// writeDeliveryWindowOutcomeFile exactly.
func writeStoreWarmingPassFile(packStateDir string, outcome storeWarmingPassOutcome) error {
	path := storeWarmingPassPath(packStateDir)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	data, err := json.Marshal(outcome)
	if err != nil {
		return err
	}
	data = append(data, '\n')
	return fsys.WriteFileAtomic(fsys.OSFS{}, path, data, 0o644)
}

// storeWarmingPassProbe issues one attempt's worth of reads against a store
// and returns how long they took. The error is returned rather than
// swallowed so a store that is unreachable is distinguishable from one that
// is merely slow — the same distinction the L2 state machine draws between a
// fast failure and a bound-exceeded read.
func storeWarmingPassProbe(store beads.Store) (time.Duration, error) {
	start := time.Now()
	// 1. Cheapest possible round trip: an id-keyed lookup that matches
	// nothing. IDs counts as a filter, so this needs no AllowScan and
	// touches no table scan — it measures the listener, not the data.
	if _, err := store.List(beads.ListQuery{IDs: []string{"gc-store-warming-probe"}}); err != nil {
		return time.Since(start), fmt.Errorf("liveness probe: %w", err)
	}
	// 2. The reconciler's own full-scan shape — what the once-a-minute
	// heartbeat times, and therefore what L2's state machine judges.
	if _, err := store.List(beads.ListQuery{AllowScan: true, SkipLabels: true, IncludeClosed: false}); err != nil {
		return time.Since(start), fmt.Errorf("reconcile-shaped probe: %w", err)
	}
	// 3. The order-tracking sweep's shape, including closed rows. This is
	// the heavy read whose slowness stalls the serial tick, so a store is
	// not "warm" until this one is inside the wall too.
	if _, err := store.List(beads.ListQuery{Label: labelOrderTracking, IncludeClosed: true, AllowScan: true}); err != nil {
		return time.Since(start), fmt.Errorf("sweep-shaped probe: %w", err)
	}
	return time.Since(start), nil
}

// runStoreWarmingPass warms every store until each beats the wall or the
// shared budget expires. It is the pass's testable core: every dependency
// that touches the world — the stores, the clock, the sleep — is a
// parameter.
//
// The budget is shared across stores rather than per-store on purpose: the
// resource under contention is the one listener, and N stores each granted
// the full budget would let a single wedged store hold the pass open N
// times longer than the operator asked for.
func runStoreWarmingPass(stores []beads.Store, budget, wall time.Duration, sleep func(time.Duration)) storeWarmingPassOutcome {
	started := time.Now()
	out := storeWarmingPassOutcome{
		State:   storeWarmingPassStateRan,
		BudgetS: int(budget / time.Second),
		WallMs:  wall.Milliseconds(),
	}
	deadline := started.Add(budget)

	for _, store := range stores {
		prefix, ok := sweepStorePrefix(store)
		if !ok {
			prefix = "(no-prefix)"
		}
		// The managed-start warming edge: no reconcile probe can observe a
		// server restarting underneath a live cache, so the pass announces
		// it directly.
		beads.MarkStoreWarmingByPrefix(prefix)

		result := storeWarmingPassStoreOutcome{Store: prefix}
		backoff := storeWarmingPassInitialBackoff
		for time.Now().Before(deadline) {
			result.Attempts++
			elapsed, err := storeWarmingPassProbe(store)
			result.LastProbeMs = elapsed.Milliseconds()
			if err != nil {
				result.Err = err.Error()
			} else {
				result.Err = ""
				if elapsed < wall {
					result.Warmed = true
					break
				}
			}
			remaining := time.Until(deadline)
			if remaining <= 0 {
				break
			}
			if backoff > remaining {
				backoff = remaining
			}
			sleep(backoff)
			if backoff < storeWarmingPassMaxBackoff {
				backoff *= 2
			}
		}
		out.Stores = append(out.Stores, result)
	}

	// A pass whose every store errored is a FAILED pass, not a successful
	// one that happened to warm nothing — the AC5 three-way distinction has
	// to survive contact with a store that never answered.
	if len(out.Stores) > 0 {
		allErrored := true
		for _, s := range out.Stores {
			if s.Err == "" {
				allErrored = false
				break
			}
		}
		if allErrored {
			out.State = storeWarmingPassStateFailed
			out.Err = out.Stores[0].Err
		}
	}

	out.Duration = time.Since(started).Round(time.Millisecond).String()
	out.At = time.Now().UTC()
	return out
}

// reportStoreWarmingPassOutcome emits the AC5 record to stderr. Loud on
// every path including the skips, because an unrequested skip that is quiet
// is the vp-cblo shape this plan's constraint 4 exists to forbid.
func reportStoreWarmingPassOutcome(out storeWarmingPassOutcome, stderr io.Writer) {
	if stderr == nil {
		return
	}
	switch out.State {
	case storeWarmingPassStateRan:
		warmed := 0
		for _, s := range out.Stores {
			if s.Warmed {
				warmed++
			}
		}
		fmt.Fprintf(stderr, "gc dolt: STORE WARMING PASS: warmed %d/%d store(s) in %s (budget %ds, wall %dms)\n",
			warmed, len(out.Stores), out.Duration, out.BudgetS, out.WallMs) //nolint:errcheck
	case storeWarmingPassStateFailed:
		fmt.Fprintf(stderr, "gc dolt: STORE WARMING PASS FAILED (ran %s): %s\n", out.Duration, out.Err) //nolint:errcheck
	case storeWarmingPassStateStarted:
		fmt.Fprintf(stderr, "gc dolt: STORE WARMING PASS STARTED (budget %ds, wall %dms)\n", out.BudgetS, out.WallMs) //nolint:errcheck
	default:
		fmt.Fprintf(stderr, "gc dolt: STORE WARMING PASS SKIPPED: %s\n", out.Skipped) //nolint:errcheck
	}
}

// storeWarmingPassStoresFn is the seam over store resolution, so the pass is
// testable without a live city on disk. Production resolves the same
// per-scope order stores the tracking sweep walks — the reads whose latency
// is what stalls the tick.
var storeWarmingPassStoresFn = defaultStoreWarmingPassStores

func defaultStoreWarmingPassStores(cityPath string) ([]beads.Store, func(), error) {
	cfg, err := loadCityConfig(cityPath)
	if err != nil {
		return nil, func() {}, err
	}
	stores, _, err := orderTrackingSweepStoresForConfigTargets(cityPath, cfg, nil)
	if err != nil {
		return stores, func() { closeStoreWarmingPassStores(stores) }, err
	}
	return stores, func() { closeStoreWarmingPassStores(stores) }, nil
}

func closeStoreWarmingPassStores(stores []beads.Store) {
	for _, s := range stores {
		_ = closeBeadStoreHandle(s) //nolint:errcheck // best-effort
	}
}

// startStoreWarmingPass launches the warming pass for a city that has just
// finished a managed start.
//
// It returns IMMEDIATELY (constraint 3). Before launching it writes a
// `started` record synchronously, so a short-lived process that exits before
// the goroutine finishes leaves an honest trace rather than nothing at all —
// the pass never silently didn't happen.
func startStoreWarmingPass(cityPath, packStateDir string, stderr io.Writer) {
	wall := beads.StoreWarmingWall()
	budget, enabled := storeWarmingPassBudget()
	if !enabled {
		out := storeWarmingPassOutcome{
			At:      time.Now().UTC(),
			State:   storeWarmingPassStateSkipped,
			Skipped: storeWarmingBudgetEnv + "=0 (warming pass disabled by operator)",
			WallMs:  wall.Milliseconds(),
		}
		reportStoreWarmingPassOutcome(out, stderr)
		persistStoreWarmingPassOutcome(packStateDir, out, stderr)
		return
	}

	started := storeWarmingPassOutcome{
		At:      time.Now().UTC(),
		State:   storeWarmingPassStateStarted,
		BudgetS: int(budget / time.Second),
		WallMs:  wall.Milliseconds(),
	}
	reportStoreWarmingPassOutcome(started, stderr)
	persistStoreWarmingPassOutcome(packStateDir, started, stderr)

	go func() {
		stores, closeStores, err := storeWarmingPassStoresFn(cityPath)
		defer closeStores()
		if err != nil && len(stores) == 0 {
			out := storeWarmingPassOutcome{
				At:      time.Now().UTC(),
				State:   storeWarmingPassStateFailed,
				Err:     fmt.Sprintf("resolving stores: %v", err),
				BudgetS: int(budget / time.Second),
				WallMs:  wall.Milliseconds(),
			}
			reportStoreWarmingPassOutcome(out, stderr)
			persistStoreWarmingPassOutcome(packStateDir, out, stderr)
			return
		}
		out := runStoreWarmingPass(stores, budget, wall, time.Sleep)
		reportStoreWarmingPassOutcome(out, stderr)
		persistStoreWarmingPassOutcome(packStateDir, out, stderr)
	}()
}

func persistStoreWarmingPassOutcome(packStateDir string, out storeWarmingPassOutcome, stderr io.Writer) {
	if packStateDir == "" {
		return
	}
	if err := writeStoreWarmingPassFile(packStateDir, out); err != nil && stderr != nil {
		// Constraint 2/3 apply to this write too: it is best-effort
		// observability, never a start precondition.
		fmt.Fprintf(stderr, "gc dolt: failed to persist store warming pass record: %v\n", err) //nolint:errcheck
	}
}

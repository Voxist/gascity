package main

import (
	"errors"
	"fmt"

	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/beads"
)

// orderGateFlatScanLimit bounds the single whole-scope open-bead List the
// flat-membership open-work gate issues per evaluation. When the scan
// returns this many beads the snapshot may be truncated, and the gate falls
// back to the authoritative per-root walk rather than declare a root idle
// from partial data. Package-level var so it is tunable and overridable in
// tests (mirrors orderGateTimeout).
var orderGateFlatScanLimit = 5000

// orderGateFlatAncestorGetBudget bounds the per-evaluation number of Get
// round-trips the flat gate spends resolving open beads whose parent chain
// passes through closed UNSTAMPED intermediates (each distinct ancestor is
// fetched at most once per evaluation). Stamped molecule data and open
// parent chains resolve entirely in memory and spend none of it. When the
// budget is exhausted the gate falls back to the authoritative walk instead
// of guessing. Package-level var so it is tunable and overridable in tests.
var orderGateFlatAncestorGetBudget = 256

// errFlatGateBudgetExhausted reports that the flat evaluation could not
// complete within its bounded store-call budget; callers fall back to the
// authoritative walk.
var errFlatGateBudgetExhausted = errors.New("flat gate ancestor budget exhausted")

// hasOpenOrderWorkFlat is the flat-membership open-work gate (incident 12 /
// vc-6qh1 #1'). It answers "does this order still have in-flight work?" in a
// bounded number of store round-trips — 1 order-run List + 1 membership List
// per open wisp root + 1 whole-scope open scan, plus a budget-bounded number
// of memoized ancestor Gets — with all descendant reasoning done by
// in-memory set operations. It never issues the historical O(tree) per-node
// ParentID/DepList walk, whose per-bead subprocess calls grew with wisp-tree
// size, blew past the per-order gate bound under Dolt write contention, and
// turned every timeout into a silent fail-open (#2893).
//
// Blocking shapes, in evaluation order:
//
//  1. An open tracking bead (labelOrderTracking): a dispatchOne goroutine is
//     in flight.
//  2. A root-only wisp candidate (gc.kind == "wisp", non-molecule): the wisp
//     itself is the work.
//  3. An open stamped member of an open wisp root: every descendant created
//     by any molecule growth path carries gc.root_bead_id == rootID (an
//     invariant enforced in internal/molecule), so one metadata-filtered
//     List per root returns the whole membership set.
//  4. An open bead whose ParentID chain reaches a root: resolved in memory
//     against the whole-scope open snapshot and the stamped membership sets
//     (closed stamped members act as adjacency carriers). Only a chain that
//     passes through a closed UNSTAMPED intermediate costs a Get, memoized
//     per evaluation and bounded by orderGateFlatAncestorGetBudget.
//
// Graph-v2 dependency edges need no DepList: the walk only ever counted a
// graph dependent when it carried the gc.root_bead_id stamp
// (orderWispGraphDependentOwnedByRoot), so the membership List in step 3 is
// a superset of that check.
//
// The idle-confirmation direction — the incident-12 hot path, where a
// leftover root's tree is entirely closed and the historical walk paid
// O(tree) subprocess calls per tick just to confirm "no open work" — costs
// exactly three Lists and zero Gets, independent of tree size.
//
// When the open scan is truncated at orderGateFlatScanLimit, or the ancestor
// budget runs out, the gate falls back to storeHasOpenDescendantsByWalk —
// bounded by the caller's per-order gate timeout — so single-flight is never
// weakened by the flat path's bounds.
//
// When skip is non-nil, an open bead for which skip returns true does not
// block, but it still extends the membership chain for its own descendants —
// the same contract the walk honors for transient nudge/mail chores
// (#2893 #3).
func hasOpenOrderWorkFlat(store beads.Store, scopedName string, skip func(beads.Bead) bool) (bool, error) {
	reader := beads.HandlesFor(store).Live
	results, err := reader.List(beads.ListQuery{
		Label: "order-run:" + scopedName,
		Sort:  beads.SortCreatedDesc,
		// Tracking beads are ephemeral while wisp roots are issue-tier, so
		// the authoritative single-flight gate must union both tiers.
		TierMode: beads.TierBoth,
	})
	if err != nil {
		return false, fmt.Errorf("listing order work beads: %w", err)
	}
	var roots []beads.Bead
	for _, b := range results {
		if b.Status == "closed" {
			continue
		}
		if beadLabelsContain(b.Labels, labelOrderTracking) {
			return true, nil
		}
		if !isOrderWispRootCandidate(b) {
			continue
		}
		if isOrderRootOnlyWispCandidate(b) {
			return true, nil
		}
		roots = append(roots, b)
	}
	if len(roots) == 0 {
		return false, nil
	}

	// Membership pass: one metadata-filtered List per open root. Any open
	// non-skipped member blocks immediately; closed and skipped members are
	// kept as membership carriers for the ancestry resolution below.
	member := make(map[string]struct{}, len(roots))
	for _, r := range roots {
		member[r.ID] = struct{}{}
	}
	for _, r := range roots {
		members, err := reader.List(beads.ListQuery{
			Metadata:      map[string]string{beadmeta.RootBeadIDMetadataKey: r.ID},
			IncludeClosed: true,
			TierMode:      beads.TierBoth,
		})
		if err != nil {
			return false, fmt.Errorf("checking open descendants of wisp %s: %w", r.ID, err)
		}
		for _, b := range members {
			if b.ID == r.ID {
				continue
			}
			if b.Status != "closed" && (skip == nil || !skip(b)) {
				return true, nil
			}
			member[b.ID] = struct{}{}
		}
	}

	// Whole-scope open snapshot: one bounded List covering every non-closed
	// bead in the store scope, in both tiers.
	open, err := reader.List(beads.ListQuery{
		AllowScan: true,
		Limit:     orderGateFlatScanLimit,
		TierMode:  beads.TierBoth,
	})
	if err != nil {
		return false, fmt.Errorf("listing open beads for order gate: %w", err)
	}
	if orderGateFlatScanLimit > 0 && len(open) >= orderGateFlatScanLimit {
		// Truncated snapshot: absence of open descendants cannot be proven
		// from an incomplete set. Fall back to the authoritative walk
		// (bounded by the caller's gate timeout) instead of weakening
		// single-flight.
		return hasOpenDescendantsByWalkAcrossRoots(store, roots, skip)
	}

	resolver := &orderGateAncestry{
		reader:    reader,
		member:    member,
		nonMember: make(map[string]struct{}),
		openByID:  make(map[string]beads.Bead, len(open)),
		ancestors: make(map[string]beads.Bead),
		budget:    orderGateFlatAncestorGetBudget,
	}
	for _, b := range open {
		resolver.openByID[b.ID] = b
	}
	for _, b := range open {
		if _, ok := member[b.ID]; ok {
			// Already-proven members in the open snapshot are the roots
			// themselves (an orphan root is leftover state, not work —
			// ga-jra/ga-lo8c) and skipped stamped members; every open
			// stamped member that blocks returned above.
			continue
		}
		if skip != nil && skip(b) {
			// Skipped beads never block; their membership is resolved
			// lazily if a blocking descendant chases up through them.
			continue
		}
		isMember, err := resolver.resolve(b)
		if errors.Is(err, errFlatGateBudgetExhausted) {
			return hasOpenDescendantsByWalkAcrossRoots(store, roots, skip)
		}
		if err != nil {
			return false, err
		}
		if isMember {
			return true, nil
		}
	}
	return false, nil
}

// hasOpenDescendantsByWalkAcrossRoots runs the authoritative O(tree) walk
// for each open root. It is the flat gate's conservative fallback for
// truncated snapshots and exhausted ancestor budgets; the caller's per-order
// gate timeout bounds its runtime.
func hasOpenDescendantsByWalkAcrossRoots(store beads.Store, roots []beads.Bead, skip func(beads.Bead) bool) (bool, error) {
	for _, r := range roots {
		has, err := storeHasOpenDescendantsByWalk(store, r.ID, skip)
		if err != nil {
			return false, fmt.Errorf("checking open descendants of wisp %s: %w", r.ID, err)
		}
		if has {
			return true, nil
		}
	}
	return false, nil
}

// orderGateAncestry resolves whether a bead's ParentID chain reaches one of
// an order's wisp roots, memoizing every verdict and every fetched ancestor
// so each distinct bead costs at most one store round-trip per evaluation.
type orderGateAncestry struct {
	reader beads.LiveReader
	// member holds IDs proven to belong to a root's wisp (the roots
	// themselves, stamped members, and beads resolved through them).
	member map[string]struct{}
	// nonMember holds IDs proven to NOT reach any root.
	nonMember map[string]struct{}
	// openByID indexes the whole-scope open snapshot.
	openByID map[string]beads.Bead
	// ancestors memoizes closed/missing parents fetched via Get.
	ancestors map[string]beads.Bead
	// budget is the remaining number of Get round-trips.
	budget int
}

// resolve reports whether b's ParentID chain reaches a wisp root. It walks
// upward in memory through open beads and stamped members, fetching a closed
// unstamped ancestor at most once, and memoizes the verdict for every bead
// on the visited chain.
func (a *orderGateAncestry) resolve(b beads.Bead) (bool, error) {
	var chain []string
	cur := b
	visiting := map[string]struct{}{}
	for {
		if _, ok := a.member[cur.ID]; ok {
			a.markChain(chain, true)
			return true, nil
		}
		if _, ok := a.nonMember[cur.ID]; ok {
			a.markChain(chain, false)
			return false, nil
		}
		if _, ok := visiting[cur.ID]; ok {
			// Parent cycle disconnected from every root.
			a.markChain(chain, false)
			return false, nil
		}
		visiting[cur.ID] = struct{}{}
		chain = append(chain, cur.ID)
		if cur.ParentID == "" {
			a.markChain(chain, false)
			return false, nil
		}
		if parent, ok := a.openByID[cur.ParentID]; ok {
			cur = parent
			continue
		}
		if parent, ok := a.ancestors[cur.ParentID]; ok {
			cur = parent
			continue
		}
		if a.budget <= 0 {
			return false, errFlatGateBudgetExhausted
		}
		a.budget--
		parent, err := a.reader.Get(cur.ParentID)
		if errors.Is(err, beads.ErrNotFound) {
			a.markChain(chain, false)
			return false, nil
		}
		if err != nil {
			return false, fmt.Errorf("resolving wisp ancestry of %s: %w", b.ID, err)
		}
		a.ancestors[parent.ID] = parent
		cur = parent
	}
}

func (a *orderGateAncestry) markChain(chain []string, isMember bool) {
	for _, id := range chain {
		if isMember {
			a.member[id] = struct{}{}
		} else {
			a.nonMember[id] = struct{}{}
		}
	}
}

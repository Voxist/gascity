package config

import (
	"encoding/json"
	"os/exec"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/beadmeta"
)

// Regression coverage for vp-4dj7, the windowing sibling of the gas-kg6 hold
// gate and the dep-blocked gate pinned in workquery_inprogress_blocked_test.go.
//
// Those gates made the crash-recovery tier SKIP a held or blocked candidate.
// But the tier still reads that candidate with `--limit=1`: the one sampled row
// is spent on the gate's verdict, and a second, genuinely actionable assigned
// in_progress bead behind it is never read at all — invisible to this tier
// because the window closed on row one, and invisible to the ready tiers below
// because they exclude in_progress rows. A seat holding [held, actionable] got
// its actionable bead served by nobody.
//
// The fix widens the read to a bounded, priority-sorted window and drains it
// through the UNCHANGED per-row gates, so exclusion semantics are untouched and
// an all-held (or all-blocked) window still falls through to the ready tier.
// Like the gate tests, these EXECUTE the generated shell against a fake `bd`
// on PATH so they pin observable behavior rather than the script's spelling.

// twoAssignedInProgressRows is a crash-recovery window whose first row is
// parked on a canonical dispatch hold and whose second row is plain. The list
// order puts the held bead FIRST — today's single-row window samples exactly
// that row and abandons the tier.
func twoAssignedInProgressRows(label string) string {
	return `[{"id":"wk-1","status":"in_progress","assignee":"sess-1","title":"held","labels":["` +
		label + `","mysql-cutover"]},` +
		`{"id":"wk-2","status":"in_progress","assignee":"sess-1","title":"actionable","labels":["routine"]}]`
}

// fakeBdHeldThenActionable returns a fake bd whose `bd list` reports the
// held-first window above (ignoring --sort/--limit, as the shadowing defect
// requires) and whose `bd show` resolves empty dependencies for either id, so
// only the hold gate can be what suppresses a candidate.
func fakeBdHeldThenActionable(label string) string {
	return `#!/bin/sh
case "$1" in
  list) printf '%s' '` + twoAssignedInProgressRows(label) + `' ;;
  show) case "$2" in
          wk-1) printf '%s' '[{"id":"wk-1","status":"in_progress","dependencies":[]}]' ;;
          wk-2) printf '%s' '[{"id":"wk-2","status":"in_progress","dependencies":[]}]' ;;
          *) printf '[]' ;;
        esac ;;
  *) printf '[]' ;;
esac
`
}

// TestInProgressTierServesSecondCandidateWhenFirstIsHeld is the vp-4dj7
// acceptance red test: a seat holding [held, actionable] must receive the
// ACTIONABLE bead from the crash-recovery tier, not a silent fall-through.
func TestInProgressTierServesSecondCandidateWhenFirstIsHeld(t *testing.T) {
	for _, label := range beadmeta.DispatchHoldLabels {
		t.Run(label, func(t *testing.T) {
			if _, err := exec.LookPath("jq"); err != nil {
				t.Skip("jq not available; the work-query shell requires it")
			}
			script := standardAssignedInProgressWorkQueryScript(QueryTopology{}) + `printf "[]"`
			out := runShellWithFakeBd(t, script, map[string]string{"GC_SESSION_ID": "sess-1"}, fakeBdHeldThenActionable(label))

			var rows []map[string]any
			if err := json.Unmarshal([]byte(strings.TrimSpace(out)), &rows); err != nil {
				t.Fatalf("tier output is not a JSON array: %v (output %q)", err, out)
			}
			if len(rows) != 1 || rows[0]["id"] != "wk-2" {
				t.Fatalf("actionable second candidate was shadowed by the held first candidate; got %q", out)
			}
		})
	}
}

// TestLegacyControlInProgressTierServesSecondCandidateWhenFirstIsHeld pins the
// same windowing fix on the control-dispatcher variant — it shares the tier
// reader and the per-row gate, so a fix applied to only the standard shape
// would leave the legacy shape sampling one row and giving up.
func TestLegacyControlInProgressTierServesSecondCandidateWhenFirstIsHeld(t *testing.T) {
	if _, err := exec.LookPath("jq"); err != nil {
		t.Skip("jq not available; the work-query shell requires it")
	}
	script := legacyControlAssignedInProgressWorkQueryScript(QueryTopology{}) + `printf "[]"`
	out := runShellWithFakeBd(t, script, map[string]string{"GC_SESSION_ID": "sess-1"}, fakeBdHeldThenActionable(beadmeta.HoldExternalLabel))

	var rows []map[string]any
	if err := json.Unmarshal([]byte(strings.TrimSpace(out)), &rows); err != nil {
		t.Fatalf("tier output is not a JSON array: %v (output %q)", err, out)
	}
	if len(rows) != 1 || rows[0]["id"] != "wk-2" {
		t.Fatalf("legacy-control tier shadowed the actionable second candidate; got %q", out)
	}
}

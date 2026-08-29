package config

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// TestRoutedReadyTierCommandUnionsPriorityAndWindow pins the ADR-0076 D4
// shape (vp-d1kjk): the routed tier's served set is the union of a small
// best-by-priority read and the unchanged oldest-first lookahead, merged
// through a compact jq filter so the tier's existing "$r" != "[]" fallthrough
// check keeps working.
func TestRoutedReadyTierCommandUnionsPriorityAndWindow(t *testing.T) {
	got := routedReadyTierCommand(QueryTopology{})
	for _, want := range []string{
		"--sort priority --limit=" + strconv.Itoa(routedReadyPriorityWindowLimit),
		"--sort oldest --limit=20",
		`--metadata-field "gc.routed_to=$target"`,
		"jq -nc",
		"--argjson p",
		"--argjson h",
		"bd ready",
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("routed ready tier command = %q, want it to contain %q", got, want)
		}
	}
	// Exactly two reads: one priority probe, one oldest-first lookahead. A
	// third "bd ready" would silently reintroduce a per-store cost regression
	// (constraint C1).
	if n := strings.Count(got, "bd ready"); n != 2 {
		t.Fatalf("routed ready tier command runs %d bd ready reads, want exactly 2: %q", n, got)
	}
}

// TestRoutedReadyPriorityRepresentativeMergeJQDedupesPreservingPriorityOrder
// exercises the ADR-0076 D4 merge filter directly against a real jq binary
// (skipped when jq is not on PATH) so the hand-written jq expression is
// proven, not just believed: priority rows first, a duplicate id from the
// window dropped in favor of the priority copy, and an id-less row on either
// side never treated as a duplicate of another id-less row (mirrors
// hookTiedIDSeen's rule on the Go side of ADR-0076).
func TestRoutedReadyPriorityRepresentativeMergeJQDedupesPreservingPriorityOrder(t *testing.T) {
	jqPath, err := exec.LookPath("jq")
	if err != nil {
		t.Skip("jq not on PATH")
	}
	p := `[{"id":"vp-p0","priority":0},{"id":"vp-dup","priority":1}]`
	h := `[{"id":"vp-dup","priority":1},{"id":"vp-h1","priority":3},{"foo":"no-id-1"},{"foo":"no-id-2"}]`
	cmd := exec.Command(jqPath, "-nc", "--argjson", "p", p, "--argjson", "h", h, routedReadyPriorityRepresentativeMergeJQ())
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("jq failed: %v\n%s", err, out)
	}
	var rows []map[string]any
	if err := json.Unmarshal(out, &rows); err != nil {
		t.Fatalf("merged output is not a JSON array: %v\n%s", err, out)
	}
	var ids []string
	for _, r := range rows {
		id, _ := r["id"].(string)
		ids = append(ids, id)
	}
	want := []string{"vp-p0", "vp-dup", "vp-h1", "", ""}
	if len(ids) != len(want) {
		t.Fatalf("merged ids = %v, want %v (len mismatch, got %d rows: %s)", ids, want, len(rows), out)
	}
	for i, id := range want {
		if ids[i] != id {
			t.Fatalf("merged ids = %v, want %v (mismatch at %d)", ids, want, i)
		}
	}
}

// TestRoutedReadyTierCommandServesTopPriorityBehindDeepBacklog is the D4
// acceptance regression, symmetric to D1's (ADR-0076, vp-d1kjk): "a pool
// holding >=1 ready P0 must SERVE that P0 regardless of how many aged
// P1/P2/P3 rows precede it under --sort oldest." It runs the ACTUAL generated
// shell command against a fake `bd` on PATH that reproduces exactly the
// measured 2026-08-28 shape (20 aged rows on --sort oldest, zero of them P0;
// the P0 only reachable via --sort priority) and asserts the P0 row survives
// into the served output — proving the runtime behavior, not just the string
// shape.
func TestRoutedReadyTierCommandServesTopPriorityBehindDeepBacklog(t *testing.T) {
	if _, err := exec.LookPath("jq"); err != nil {
		t.Skip("jq not on PATH")
	}
	shPath, err := exec.LookPath("sh")
	if err != nil {
		t.Skip("sh not on PATH")
	}

	// 20 aged rows, all P2/P3, none P0 — the exact 08-28 measurement shape
	// ("20 rows: 14x P1, 5x P2, 1x P3. ZERO P0.", collapsed here to a single
	// non-P0 priority for a minimal fixture). The window read (--sort oldest)
	// returns this set; the priority read (--sort priority) returns the P0
	// that the window cannot reach.
	var agedRows []map[string]any
	for i := 0; i < 20; i++ {
		agedRows = append(agedRows, map[string]any{
			"id": "vp-aged" + strconv.Itoa(i), "priority": 3, "status": "open",
		})
	}
	agedJSON, err := json.Marshal(agedRows)
	if err != nil {
		t.Fatal(err)
	}
	p0Row := `[{"id":"vp-o52ia","priority":0,"status":"open"}]`

	binDir := t.TempDir()
	fakeBd := strings.Join([]string{
		"#!/bin/sh",
		`case "$*" in`,
		`  *"--sort priority"*) printf '%s' ` + shellQuoteForScript(p0Row) + `;;`,
		`  *"--sort oldest"*) printf '%s' ` + shellQuoteForScript(string(agedJSON)) + `;;`,
		`  *) echo "unexpected bd invocation: $*" >&2; exit 1;;`,
		`esac`,
		"",
	}, "\n")
	if err := os.WriteFile(filepath.Join(binDir, "bd"), []byte(fakeBd), 0o755); err != nil {
		t.Fatal(err)
	}

	// Mirror poolDemandFirstRowFunctionScript's own wrapping exactly (target
	// set as a shell var, $r captured from the tier) so this test runs the
	// tier the way its only caller does.
	script := `target="rig/worker"; r=$(` + routedReadyTierCommand(QueryTopology{}) + `); printf '%s' "$r"`

	cmd := exec.Command(shPath, "-c", script)
	cmd.Env = append(os.Environ(), "PATH="+binDir+string(os.PathListSeparator)+os.Getenv("PATH"))
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("generated routed ready tier command failed: %v\n%s", err, out)
	}

	var rows []map[string]any
	if err := json.Unmarshal(out, &rows); err != nil {
		t.Fatalf("served output is not a JSON array: %v\n%s", err, out)
	}

	foundP0 := false
	agedSeen := 0
	for _, r := range rows {
		id, _ := r["id"].(string)
		if id == "vp-o52ia" {
			foundP0 = true
		}
		if strings.HasPrefix(id, "vp-aged") {
			agedSeen++
		}
	}
	if !foundP0 {
		t.Fatalf("D4 regression: ready P0 vp-o52ia was NOT served behind the 20-row aged window: %s", out)
	}
	if agedSeen != 20 {
		t.Fatalf("D4 must not drop the oldest-first lookahead (ADR-0035 anti-starvation, constraint C2): got %d of 20 aged rows: %s", agedSeen, out)
	}
	// The P0 row must be servED first — claimFirstReadyHookAssignment walks
	// the array in order and tries the first row first.
	if len(rows) == 0 || rows[0]["id"] != "vp-o52ia" {
		t.Fatalf("D4 must serve the priority row FIRST so the claim path tries it before the aged tail, got order: %v", rowIDs(rows))
	}
}

func rowIDs(rows []map[string]any) []string {
	ids := make([]string, len(rows))
	for i, r := range rows {
		id, _ := r["id"].(string)
		ids[i] = id
	}
	return ids
}

// shellQuoteForScript single-quotes a JSON blob for embedding in the
// generated fake-bd shell script literal above. Test-only; production
// shell-quoting goes through internal/shellquote.
func shellQuoteForScript(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

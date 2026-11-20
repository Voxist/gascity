//go:build unix

package core

import (
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// crossRigDepsScriptPath is the on-disk copy of the cross-rig-deps order's
// script. The test runs it, so it needs a real path.
const crossRigDepsScriptPath = "assets/scripts/cross-rig-deps.sh"

// crossRigDepsEnvKeys are the settings each test supplies itself.
var crossRigDepsEnvKeys = map[string]struct{}{
	"GC_CITY":               {},
	"CROSS_RIG_LOOKBACK":    {},
	"GC_BD_TRACE_JSON":      {},
	"GC_ORDER_OUTCOME_FILE": {},
}

func runCrossRigDeps(t *testing.T, binDir string) (string, error) {
	t.Helper()
	return runPackScript(t, crossRigDepsScriptPath, binDir, crossRigDepsEnvKeys, nil)
}

// TestCrossRigDepsUsesQueryNotClosedAfter pins the HTTP-store fix: the script
// must ask for recently-closed issues with `bd query`, not
// `bd list --closed-after`. maintainer-city's HTTP-backed city store refuses
// the ClosedAfter listIssues filter outright (no wire member for it), so a
// script still calling --closed-after would always hit the error path and,
// before this fix, silently convert zero cross-rig deps on every run.
//
// It also pins the rest of the query's contract: the closed-after time
// filter, --json (the script parses the array), and --limit 0. bd before
// v1.3.1 caps an unflagged query at 50 rows even when piped, so without
// --limit 0 any recent close past the cap would be dropped without a word.
func TestCrossRigDepsUsesQueryNotClosedAfter(t *testing.T) {
	binDir, logPath := fakeGCBin(t, `case "$*" in
bd\ query\ status=closed*) printf '[]\n' ;;
rig\ list*) printf '{"rigs":[]}\n' ;;
esac
exit 0
`)
	out, err := runCrossRigDeps(t, binDir)
	if err != nil {
		t.Fatalf("cross-rig-deps.sh failed: %v\n%s", err, out)
	}
	logged, readErr := os.ReadFile(logPath)
	if readErr != nil {
		t.Fatalf("read gc log: %v", readErr)
	}
	if strings.Contains(string(logged), "closed-after") {
		t.Fatalf("script must not call the refused --closed-after flag, got:\n%s", logged)
	}
	query := regexp.MustCompile(`(?m)^gc bd query .*$`).FindString(string(logged))
	for _, want := range []string{"status=closed AND closed>\"", " --json", " --limit 0"} {
		if !strings.Contains(query, want) {
			t.Fatalf("script must call `bd query` with %q, got:\n%s", want, logged)
		}
	}
}

// TestCrossRigDepsConvertsExternalBlockOnQueryHit pins the happy path end to
// end with the new query-based lookup: a closed issue with an external
// (cross-rig) blocker dependent gets that dependency converted from blocks to
// related.
func TestCrossRigDepsConvertsExternalBlockOnQueryHit(t *testing.T) {
	binDir, logPath := fakeGCBin(t, `case "$*" in
bd\ query\ status=closed*) printf '[{"id":"ga-blocker","status":"closed"}]\n' ;;
rig\ list*) printf '{"rigs":[]}\n' ;;
bd\ dep\ list\ ga-blocker*) printf '[{"id":"external:dep-1"}]\n' ;;
bd\ dep\ remove*) ;;
bd\ dep\ add*) ;;
esac
exit 0
`)
	out, err := runCrossRigDeps(t, binDir)
	if err != nil {
		t.Fatalf("cross-rig-deps.sh failed: %v\n%s", err, out)
	}
	if !strings.Contains(out, "resolved 1 cross-rig dependencies") {
		t.Fatalf("expected one resolved dependency, got:\n%s", out)
	}
	logged, readErr := os.ReadFile(logPath)
	if readErr != nil {
		t.Fatalf("read gc log: %v", readErr)
	}
	if !strings.Contains(string(logged), "gc bd dep remove external:dep-1 external:ga-blocker") {
		t.Fatalf("expected the blocks dep to be removed, got:\n%s", logged)
	}
	if !strings.Contains(string(logged), "gc bd dep add external:dep-1 external:ga-blocker --type=related") {
		t.Fatalf("expected the related dep to be added, got:\n%s", logged)
	}
}

// TestCrossRigDepsRefusalFailsLoudly pins the other half of the HTTP-store
// fix: a real bd refusal (not just "nothing closed recently") must not be
// swallowed. Before this fix the lookup redirected stderr to /dev/null and
// then exited 0 on any failure, so an HTTP store's refusal of the whole
// query shape looked identical, in the order log, to an empty result and the
// order went green while doing nothing. Now the script must exit non-zero
// (so the order fails visibly) and the refusal text must still reach the
// combined output so an operator can see why.
func TestCrossRigDepsRefusalFailsLoudly(t *testing.T) {
	binDir, _ := fakeGCBin(t, `case "$*" in
bd\ query\ status=closed*)
    echo "Error: bd query is not supported against HTTP workspace: part of this command is not served over HTTP" >&2
    exit 1
    ;;
esac
exit 0
`)
	out, err := runCrossRigDeps(t, binDir)
	if err == nil {
		t.Fatalf("a real bd refusal must fail the order (non-zero exit), got success:\n%s", out)
	}
	if !strings.Contains(out, "not supported against HTTP workspace") {
		t.Fatalf("a real bd refusal must reach the order's log, not be swallowed; got:\n%s", out)
	}
}

// TestCrossRigDepsSkipsNonBdStore pins the one refusal that is not a failure:
// on a city whose beads provider is not bd (file- or exec-backed) gc refuses
// every bd verb, and there is nothing for this order to do. The script must
// exit 0, print a line saying why, and declare the run skipped, as the reaper
// and jsonl-export orders do, instead of failing the order every cooldown.
func TestCrossRigDepsSkipsNonBdStore(t *testing.T) {
	binDir, logPath := fakeGCBin(t, `case "$*" in
bd\ query\ status=closed*)
    echo 'gc bd: only supported for bd-backed beads providers (resolved "file" for /city)' >&2
    exit 1
    ;;
esac
exit 0
`)
	outcomePath := filepath.Join(t.TempDir(), "outcome.json")
	out, err := runPackScript(t, crossRigDepsScriptPath, binDir, crossRigDepsEnvKeys,
		[]string{"GC_ORDER_OUTCOME_FILE=" + outcomePath})
	if err != nil {
		t.Fatalf("a city without a bd bead store must not fail the order: %v\n%s", err, out)
	}
	if !strings.Contains(out, "cross-rig-deps: city is not a bd bead store; nothing to resolve there") {
		t.Fatalf("a skipped run must say why in its output, got:\n%s", out)
	}
	data, readErr := os.ReadFile(outcomePath)
	if readErr != nil {
		t.Fatalf("read outcome declaration: %v", readErr)
	}
	type scopeSkip struct {
		Scope  string `json:"scope"`
		Reason string `json:"reason"`
	}
	var declared struct {
		Outcome string      `json:"outcome"`
		Reason  string      `json:"reason"`
		Scopes  []scopeSkip `json:"scopes"`
	}
	if err := json.Unmarshal(data, &declared); err != nil {
		t.Fatalf("decode outcome declaration %q: %v", data, err)
	}
	wantScope := scopeSkip{Scope: "city", Reason: "not a bd bead store"}
	if declared.Outcome != "skipped" || declared.Reason != "not a bd bead store" ||
		len(declared.Scopes) != 1 || declared.Scopes[0] != wantScope {
		t.Fatalf("outcome = %+v, want skipped: not a bd bead store, scope %+v", declared, wantScope)
	}
	logged, readErr := os.ReadFile(logPath)
	if readErr != nil {
		t.Fatalf("read gc log: %v", readErr)
	}
	if strings.Contains(string(logged), "dep list") {
		t.Fatalf("a skipped run must not walk dependencies, got:\n%s", logged)
	}
}

// TestCrossRigDepsNoRecentClosesIsQuiet pins that an empty query result (the
// ordinary idle case) exits 0 with no "resolved" line and no attempt to walk
// dependencies.
func TestCrossRigDepsNoRecentClosesIsQuiet(t *testing.T) {
	binDir, logPath := fakeGCBin(t, `case "$*" in
bd\ query\ status=closed*) printf '[]\n' ;;
esac
exit 0
`)
	out, err := runCrossRigDeps(t, binDir)
	if err != nil {
		t.Fatalf("cross-rig-deps.sh failed: %v\n%s", err, out)
	}
	if strings.Contains(out, "resolved") {
		t.Fatalf("an empty result must not report anything resolved, got:\n%s", out)
	}
	logged, readErr := os.ReadFile(logPath)
	if readErr != nil {
		t.Fatalf("read gc log: %v", readErr)
	}
	if strings.Contains(string(logged), "dep list") {
		t.Fatalf("an empty result must not attempt to walk dependencies, got:\n%s", logged)
	}
}

// gcCalls returns the fake gc's logged invocations that start with prefix.
func gcCalls(t *testing.T, logPath, prefix string) []string {
	t.Helper()
	logged, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatalf("read gc log: %v", err)
	}
	var calls []string
	for _, line := range strings.Split(string(logged), "\n") {
		if strings.HasPrefix(line, prefix) {
			calls = append(calls, line)
		}
	}
	return calls
}

// TestCrossRigDepsSkipsClosesNothingDependsOn pins the N+1 fix: order-tracking
// beads, session and other infra rows, ephemeral rows and any row bd reports
// with dependent_count 0 are never looked up. A recent-close window is mostly
// order bookkeeping (about 300 rows per run on a busy city), and the script
// used to run one `gc bd dep list` per row and `gc rig list` on every run.
func TestCrossRigDepsSkipsClosesNothingDependsOn(t *testing.T) {
	binDir, logPath := fakeGCBin(t, `case "$*" in
bd\ query\ status=closed*) printf '%s\n' '[
  {"id":"ga-t1","labels":["exec","order-run:gate-sweep","order-tracking"],"dependent_count":0},
  {"id":"ga-t2","labels":["order-tracking"]},
  {"id":"ga-wisp-s1","issue_type":"session","labels":["gc:session"],"dependent_count":0},
  {"id":"ga-op1","labels":["gc:op"]},
  {"id":"ga-e1","ephemeral":true},
  {"id":"ga-work","labels":["bug"],"dependent_count":0}
]' ;;
rig\ list*) printf '{"rigs":[]}\n' ;;
esac
exit 0
`)
	out, err := runCrossRigDeps(t, binDir)
	if err != nil {
		t.Fatalf("cross-rig-deps.sh failed: %v\n%s", err, out)
	}
	if calls := gcCalls(t, logPath, "gc bd dep "); len(calls) != 0 {
		t.Fatalf("closes nothing depends on must not be looked up, got:\n%s", strings.Join(calls, "\n"))
	}
	if calls := gcCalls(t, logPath, "gc rig list"); len(calls) != 0 {
		t.Fatalf("with no candidate the rig list must not be read, got:\n%s", strings.Join(calls, "\n"))
	}
}

// TestCrossRigDepsBatchesLookupPerPrefix pins the batched lookup: the
// candidates that remain are looked up with one `gc bd dep list` per bead
// prefix (gc bd routes by prefix), carrying every candidate id of that prefix.
// A batched answer with no external dependent ends the prefix there.
func TestCrossRigDepsBatchesLookupPerPrefix(t *testing.T) {
	binDir, logPath := fakeGCBin(t, `case "$*" in
bd\ query\ status=closed*) printf '%s\n' '[
  {"id":"ga-1","dependent_count":2},
  {"id":"gb-1","dependent_count":1},
  {"id":"ga-2"},
  {"id":"ga-3","dependent_count":1}
]' ;;
rig\ list*) printf '{"rigs":[]}\n' ;;
bd\ dep\ list\ ga-1\ ga-2\ ga-3\ *) printf '[{"id":"ga-local-1"},{"id":"ga-local-2"}]\n' ;;
bd\ dep\ list\ gb-1\ *) printf '[{"id":"gb-local"}]\n' ;;
bd\ dep\ list*) echo "unexpected dep list: $*" >&2; exit 2 ;;
esac
exit 0
`)
	out, err := runCrossRigDeps(t, binDir)
	if err != nil {
		t.Fatalf("cross-rig-deps.sh failed: %v\n%s", err, out)
	}
	want := []string{
		"gc bd dep list ga-1 ga-2 ga-3 --direction=up --type=blocks --json",
		"gc bd dep list gb-1 --direction=up --type=blocks --json",
	}
	got := gcCalls(t, logPath, "gc bd dep ")
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Fatalf("dep calls =\n%s\nwant\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
	if strings.Contains(out, "resolved") {
		t.Fatalf("no external dependent means nothing resolved, got:\n%s", out)
	}
}

// TestCrossRigDepsBatchFallsBackPerBead pins that the batch never changes what
// gets converted: a batched answer holding an external dependent does not say
// which closed issue it hangs off, so each id of that prefix is looked up on
// its own and only its own external dependents are converted. A failed batch
// call falls back the same way.
func TestCrossRigDepsBatchFallsBackPerBead(t *testing.T) {
	for _, tc := range []struct {
		name  string
		batch string
	}{
		{name: "external dependent in batch", batch: `printf '[{"id":"ga-local"},{"id":"external:rig-b/gb-9"}]\n'`},
		{name: "batch call fails", batch: `echo "batch refused" >&2; exit 1`},
		{name: "batch answer not an array", batch: `printf '{"error":{"id":"x"}}\n'`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			binDir, logPath := fakeGCBin(t, `case "$*" in
bd\ query\ status=closed*) printf '[{"id":"ga-1"},{"id":"ga-2"}]\n' ;;
rig\ list*) printf '{"rigs":[]}\n' ;;
bd\ dep\ list\ ga-1\ ga-2\ *) `+tc.batch+` ;;
bd\ dep\ list\ ga-1\ *) printf '[{"id":"ga-local"}]\n' ;;
bd\ dep\ list\ ga-2\ *) printf '[{"id":"external:rig-b/gb-9"}]\n' ;;
bd\ dep\ remove*|bd\ dep\ add*) ;;
esac
exit 0
`)
			out, err := runCrossRigDeps(t, binDir)
			if err != nil {
				t.Fatalf("cross-rig-deps.sh failed: %v\n%s", err, out)
			}
			if !strings.Contains(out, "resolved 1 cross-rig dependencies") {
				t.Fatalf("expected one resolved dependency, got:\n%s", out)
			}
			want := []string{
				"gc bd dep list ga-1 ga-2 --direction=up --type=blocks --json",
				"gc bd dep list ga-1 --direction=up --type=blocks --json",
				"gc bd dep list ga-2 --direction=up --type=blocks --json",
				"gc bd dep remove external:rig-b/gb-9 external:ga-2",
				"gc bd dep add external:rig-b/gb-9 external:ga-2 --type=related",
			}
			got := gcCalls(t, logPath, "gc bd dep ")
			if strings.Join(got, "\n") != strings.Join(want, "\n") {
				t.Fatalf("dep calls =\n%s\nwant\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
			}
		})
	}
}

// TestCrossRigDepsSkipsSuspendedRigs pins that a closed issue whose prefix
// belongs to a suspended rig is not looked up: any bd read against a suspended
// rig restarts its retired proxy. The HQ entry is never treated as suspended.
func TestCrossRigDepsSkipsSuspendedRigs(t *testing.T) {
	binDir, logPath := fakeGCBin(t, `case "$*" in
bd\ query\ status=closed*) printf '[{"id":"gs-1"},{"id":"ga-1"}]\n' ;;
rig\ list*) printf '{"rigs":[{"name":"hq","prefix":"ga","hq":true,"suspended":true},{"name":"cold","prefix":"gs","suspended":true}]}\n' ;;
bd\ dep\ list\ ga-1\ *) printf '[{"id":"external:gx-3"}]\n' ;;
bd\ dep\ list*) echo "unexpected dep list: $*" >&2; exit 2 ;;
bd\ dep\ remove*|bd\ dep\ add*) ;;
esac
exit 0
`)
	out, err := runCrossRigDeps(t, binDir)
	if err != nil {
		t.Fatalf("cross-rig-deps.sh failed: %v\n%s", err, out)
	}
	want := []string{
		"gc bd dep list ga-1 --direction=up --type=blocks --json",
		"gc bd dep remove external:gx-3 external:ga-1",
		"gc bd dep add external:gx-3 external:ga-1 --type=related",
	}
	got := gcCalls(t, logPath, "gc bd dep ")
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Fatalf("dep calls =\n%s\nwant\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
}

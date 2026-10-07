package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// hookOriginGateCity stands up a one-agent city plus a fake bd that holds real
// routed pool work for "worker", so a hook that reaches the pool tier finds
// something to return.
func hookOriginGateCity(t *testing.T) {
	t.Helper()
	disableManagedDoltRecoveryForTest(t)
	clearInheritedCityRoutingEnv(t)
	cityDir := t.TempDir()
	fakeBin := t.TempDir()
	if err := os.MkdirAll(filepath.Join(cityDir, ".gc"), 0o755); err != nil {
		t.Fatal(err)
	}
	cityToml := `[workspace]
name = "test-city"

[[agent]]
name = "worker"
`
	if err := os.WriteFile(filepath.Join(cityDir, "city.toml"), []byte(cityToml), 0o644); err != nil {
		t.Fatal(err)
	}
	fakeBD := filepath.Join(fakeBin, "bd")
	script := `#!/bin/sh
case "$*" in
  *"--metadata-field gc.routed_to=worker"*) printf '[{"id":"hw-1","title":"routed work"}]' ;;
  *) printf '[]' ;;
esac
`
	if err := os.WriteFile(fakeBD, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", fakeBin+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("GC_CITY", cityDir)
}

// This is bead vc-ozanp5's acceptance criterion executed at the level the
// criterion names: "from a named session with N>0 beads routed to its own
// alias, gc hook either surfaces them or reports a reason that distinguishes
// 'gated' from 'no work' — asserted by running the hook, not by reading
// config."
//
// The work_query script emits that reason on stderr, but the script is not the
// instrument an operator runs. gc hook is. This test pins the whole path.
func TestCmdHookNamedOriginReportsGatedReasonRatherThanSilentEmpty(t *testing.T) {
	hookOriginGateCity(t)
	// GC_ALIAS deliberately does NOT match the routed target. Upstream #6180
	// added namedSelfTargetAdmit to the gate script --
	// `[ -n "$GC_ALIAS" ] && [ "$1" = "$GC_ALIAS" ]` -- which runs FIRST and
	// admits a named session outright when the target it is probing IS its own
	// alias. The fixture routes hw-1 to gc.routed_to=worker, so with
	// GC_ALIAS="worker" the seat is admitted and takes work addressed TO
	// ITSELF. That is not the pool-poaching this test guards against, and
	// upstream is right that such work "stays exactly the work the claim path
	// already accepts" -- but it does mean the old GC_ALIAS="worker" no longer
	// exercises a refusal at all.
	//
	// The property under test is unchanged: a named seat must not take routed
	// work addressed to SOMEONE ELSE, and when the gate refuses it must say so
	// rather than reporting a silent empty (vc-ozanp5 / ADR-0043). A
	// non-matching alias is what puts the gate in that state now.
	t.Setenv("GC_ALIAS", "other-seat")
	t.Setenv("GC_AGENT", "worker")
	t.Setenv("GC_SESSION_ID", "worker-session-id")
	t.Setenv("GC_SESSION_NAME", "worker-session")
	t.Setenv("GC_TEMPLATE", "worker")
	t.Setenv("GC_SESSION_ORIGIN", "named")

	var stdout, stderr bytes.Buffer
	code := cmdHookWithFormat(nil, false, "", &stdout, &stderr)

	// The gate must still refuse: a named seat does not poach the pool.
	if strings.Contains(stdout.String(), "hw-1") {
		t.Fatalf("named origin poached routed pool work; the gate did not refuse: stdout=%q", stdout.String())
	}
	if code == 0 {
		t.Fatalf("cmdHook() = 0 for a gated named origin, want non-zero; stdout=%q", stdout.String())
	}
	// ...and the refusal must reach the operator. Without this, `gc hook`
	// reporting empty is indistinguishable from a drained queue, which is the
	// entire defect vc-ozanp5 exists to remove.
	if !strings.Contains(stderr.String(), "work_query pool tier not probed") {
		t.Fatalf("gc hook swallowed the origin-gate reason — an empty hook is still "+
			"indistinguishable from a drained queue:\nstderr=%q", stderr.String())
	}
}

// Control: the same city and the same routed work, from an origin the gate
// permits. Without this the test above could pass merely because no work
// existed, and the gated signal must not fire when the tier was really probed.
func TestCmdHookEphemeralOriginFindsRoutedWorkAndStaysSilent(t *testing.T) {
	hookOriginGateCity(t)
	t.Setenv("GC_ALIAS", "worker")
	t.Setenv("GC_AGENT", "worker")
	t.Setenv("GC_SESSION_ID", "worker-session-id")
	t.Setenv("GC_SESSION_NAME", "worker-session")
	t.Setenv("GC_TEMPLATE", "worker")
	t.Setenv("GC_SESSION_ORIGIN", "ephemeral")

	var stdout, stderr bytes.Buffer
	code := cmdHookWithFormat(nil, false, "", &stdout, &stderr)
	if code != 0 {
		t.Fatalf("cmdHook() = %d for a permitted origin with work available, want 0; stderr=%s", code, stderr.String())
	}
	if !strings.Contains(stdout.String(), "hw-1") {
		t.Fatalf("permitted origin did not surface routed pool work: stdout=%q", stdout.String())
	}
	if strings.Contains(stderr.String(), "work_query pool tier not probed") {
		t.Fatalf("permitted origin emitted the gated reason despite probing: stderr=%q", stderr.String())
	}
}

// The signal is wired into BOTH hook paths, and they are mutually exclusive:
// cmdHookWithOptions returns through claimHookWork when Claim is set and
// through doHook otherwise, each with its own runner. The tests above cover the
// read path; without this one the claim path — the form the startup wrapper
// actually runs — would ship unproven.
func TestCmdHookClaimNamedOriginReportsGatedReason(t *testing.T) {
	hookOriginGateCity(t)
	// GC_ALIAS deliberately does NOT match the probe target. Upstream #6180
	// added namedSelfTargetAdmit to the gate script --
	// `[ -n "$GC_ALIAS" ] && [ "$1" = "$GC_ALIAS" ]` -- which runs FIRST and
	// admits a named session outright when its alias IS the target it is
	// probing, on the reasoning that such work is exactly what the claim path
	// already accepts. An admitted session is never refused, so it prints no
	// refusal line.
	//
	// This test predates that and used GC_ALIAS="worker" against target
	// "worker", so after the 2026-09-13 resync it was silently exercising the
	// ADMITTED path while still asserting the refusal, and failed. The read
	// path above keeps GC_ALIAS="worker" because its probe targets differ and
	// the admit does not fire there.
	//
	// The subject here is unchanged and is NOT about which sessions are
	// admitted: it is that the CLAIM path must not SWALLOW a refusal the gate
	// did emit. Using a non-matching alias keeps this a named seat with a real
	// identity while making the gate actually refuse, which is the only state
	// in which "was the reason surfaced?" is a meaningful question.
	t.Setenv("GC_ALIAS", "other-seat")
	t.Setenv("GC_AGENT", "worker")
	t.Setenv("GC_SESSION_ID", "worker-session-id")
	t.Setenv("GC_SESSION_NAME", "worker-session")
	t.Setenv("GC_TEMPLATE", "worker")
	t.Setenv("GC_SESSION_ORIGIN", "named")

	var stdout, stderr bytes.Buffer
	cmdHookWithOptions(nil, hookCommandOptions{Claim: true, JSON: true}, &stdout, &stderr)

	if strings.Contains(stdout.String(), "hw-1") {
		t.Fatalf("named origin claimed routed pool work; the gate did not refuse: stdout=%q", stdout.String())
	}
	if !strings.Contains(stderr.String(), "work_query pool tier not probed") {
		t.Fatalf("gc hook --claim swallowed the origin-gate reason:\nstdout=%q\nstderr=%q",
			stdout.String(), stderr.String())
	}
}

// vc-0sub S1: on a stderr-discarding caller the gated answer must survive
// somewhere, and the only channel left is the exit code. The role templates'
// own startup idiom is `gc hook 2>/dev/null` (vc-0sub S2 is fixing the idiom,
// not the fleet of wrappers around it), so until every caller is retrained the
// discovery door reports the gated state on a distinct exit code while stdout
// stays the parseable empty array vc-ozanp5 pinned.
func TestCmdHookNamedOriginGatedEmptyExitsDistinctCode(t *testing.T) {
	hookOriginGateCity(t)
	// Same seat shape as the audible-refusal test above: a named origin whose
	// alias does not match the routed target, so the gate actually refuses and
	// the pool tier is never probed.
	t.Setenv("GC_ALIAS", "other-seat")
	t.Setenv("GC_AGENT", "worker")
	t.Setenv("GC_SESSION_ID", "worker-session-id")
	t.Setenv("GC_SESSION_NAME", "worker-session")
	t.Setenv("GC_TEMPLATE", "worker")
	t.Setenv("GC_SESSION_ORIGIN", "named")

	var stdout, stderr bytes.Buffer
	code := cmdHookWithFormat(nil, false, "", &stdout, &stderr)

	if code != hookExitPoolTierGated {
		t.Fatalf("cmdHook() = %d for a gated named origin with routed work available, want %d — "+
			"a caller that discards stderr cannot tell gated from drained; stdout=%q stderr=%q",
			code, hookExitPoolTierGated, stdout.String(), stderr.String())
	}
	// The stdout contract is unchanged (vc-ozanp5): still the empty JSON array
	// every consumer parses. The new signal rides the exit code only.
	if trimmed := strings.TrimSpace(stdout.String()); trimmed != "[]" {
		t.Fatalf("stdout = %q, want [] — the gated signal must not corrupt the JSON array contract", trimmed)
	}
	// The audible reason is unchanged too: exit 3 is an ADDITIONAL channel, not
	// a replacement for the stderr refusal.
	if !strings.Contains(stderr.String(), "work_query pool tier not probed") {
		t.Fatalf("gated exit lost the audible refusal:\nstderr=%q", stderr.String())
	}
}

// Control for the exit-code rewrite: a permitted origin that finds the routed
// work must still exit 0. The rewrite keys on exit 1 + refusal, so this is the
// case most likely to break by accident if the refusal flag leaks across
// invocations.
func TestCmdHookEphemeralOriginGatedCodeUnaffected(t *testing.T) {
	hookOriginGateCity(t)
	t.Setenv("GC_ALIAS", "worker")
	t.Setenv("GC_AGENT", "worker")
	t.Setenv("GC_SESSION_ID", "worker-session-id")
	t.Setenv("GC_SESSION_NAME", "worker-session")
	t.Setenv("GC_TEMPLATE", "worker")
	t.Setenv("GC_SESSION_ORIGIN", "ephemeral")

	var stdout, stderr bytes.Buffer
	code := cmdHookWithFormat(nil, false, "", &stdout, &stderr)
	if code != 0 {
		t.Fatalf("cmdHook() = %d for a permitted origin with work available, want 0; stderr=%s", code, stderr.String())
	}
}

// hookDiscoveryExitCode is a pure rewrite: exit 1 (empty) becomes the gated
// exit code ONLY when the origin gate actually refused this invocation; every
// other exit — work found, store unavailable, a failed read, a timeout —
// passes through untouched. Pinned as a table so the precedence stays explicit.
func TestHookDiscoveryExitCodeRewritesOnlyGatedEmpty(t *testing.T) {
	cases := []struct {
		name        string
		code        int
		gateRefused bool
		want        int
	}{
		{"gated empty becomes gated code", 1, true, hookExitPoolTierGated},
		{"plain empty stays exit 1", 1, false, 1},
		{"work found wins over the refusal flag", 0, true, 0},
		{"store unavailable wins", 2, true, 2},
		{"failed read stays exit 1 shape", 1, true, hookExitPoolTierGated},
		{"arbitrary failure passes through", 124, false, 124},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := hookDiscoveryExitCode(tc.code, tc.gateRefused); got != tc.want {
				t.Fatalf("hookDiscoveryExitCode(%d, %v) = %d, want %d", tc.code, tc.gateRefused, got, tc.want)
			}
		})
	}
}

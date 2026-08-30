package config

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// The work_query pool tier is guarded by an origin gate: only an ephemeral
// origin (or an unset one) may probe pool demand, because a named session is a
// reserved coordinator identity rather than a generic-demand worker.
//
// vc-ozanp5 changed the gate from `*) exit 0` to a flag plus an audible tail.
// The refusal itself is unchanged; what changed is that it is no longer
// indistinguishable from a drained queue. These tests pin the part that
// actually matters at runtime — what the emitted shell DOES — which neither
// the parity oracle nor the goldens can show, since both only compare strings.

// runGate composes the real gate, a stubbed probe, the real gated probe call
// and the real tail, then executes it. probe_pool_demand is stubbed rather than built from
// poolDemandFirstRowFunctionScript so the test needs no bd/jq and stays
// hermetic; the stub reports that it ran and then misses, which is the path
// that reaches the tail.
func runGate(t *testing.T, origin, shellPrefix string) (stdout, stderr string, err error) {
	t.Helper()
	script := shellPrefix +
		poolDemandOriginGateScript() +
		`probe_pool_demand() { printf "PROBED(%s)" "$1" >&2; return 1; }; ` +
		poolDemandProbeCallScript(`"$1"`) +
		poolDemandGatedTailScript()
	cmd := exec.Command("sh", "-c", script, "--", "worker-pool")
	// An unset GC_SESSION_ORIGIN and an empty one are different inputs to the
	// gate's case statement, so set it explicitly either way.
	cmd.Env = append(cmd.Environ(), "GC_SESSION_ORIGIN="+origin)
	var out, errb strings.Builder
	cmd.Stdout = &out
	cmd.Stderr = &errb
	runErr := cmd.Run()
	return out.String(), errb.String(), runErr
}

const gateSignalMarker = "work_query pool tier not probed"

func TestPoolDemandOriginGateRefusesNamedOriginAudibly(t *testing.T) {
	stdout, stderr, err := runGate(t, "named", "")
	if err != nil {
		t.Fatalf("script exited non-zero: %v (stderr=%q)", err, stderr)
	}
	// The gate must still refuse: the probe must not have run at all.
	if strings.Contains(stderr, "PROBED(") {
		t.Errorf("origin=named probed pool demand; the gate did not refuse\nstderr=%q", stderr)
	}
	// ...and the refusal must be audible, which is the whole point of vc-ozanp5.
	if !strings.Contains(stderr, gateSignalMarker) {
		t.Errorf("origin=named produced no gated signal on stderr\nstderr=%q", stderr)
	}
	if !strings.Contains(stderr, "origin=named") {
		t.Errorf("gated signal does not name the offending origin\nstderr=%q", stderr)
	}
	// The signal is a diagnostic; it must not corrupt the JSON on stdout that
	// the caller parses. This also pins a second change the gate made: the old
	// `*) exit 0` form left stdout EMPTY for a named origin, so a caller
	// parsing it as JSON saw "" rather than an empty result. The gated path now
	// falls through to the shared `printf "[]"`.
	if stdout != "[]" {
		t.Errorf("stdout = %q, want %q — the signal must go to stderr only", stdout, "[]")
	}
}

func TestPoolDemandOriginGateAllowsProbingOrigins(t *testing.T) {
	for _, origin := range []string{"ephemeral", ""} {
		name := origin
		if name == "" {
			name = "unset"
		}
		t.Run(name, func(t *testing.T) {
			stdout, stderr, err := runGate(t, origin, "")
			if err != nil {
				t.Fatalf("script exited non-zero: %v (stderr=%q)", err, stderr)
			}
			if !strings.Contains(stderr, "PROBED(worker-pool)") {
				t.Errorf("origin=%q did not probe pool demand\nstderr=%q", origin, stderr)
			}
			// A permitted origin that simply found nothing must stay silent —
			// otherwise the signal would be noise rather than a discriminator.
			if strings.Contains(stderr, gateSignalMarker) {
				t.Errorf("origin=%q emitted the gated signal despite probing\nstderr=%q", origin, stderr)
			}
			if stdout != "[]" {
				t.Errorf("stdout = %q, want %q", stdout, "[]")
			}
		})
	}
}

// The gated path no longer ends in `exit 0`, so it now runs test commands that
// can fail. Under `set -e` the refusal must still reach the empty fallthrough:
// a caller that got no stdout at all would read a shell abort as a malformed
// result rather than as "nothing to do".
//
// Scope note: only the GATED path is asserted here. A PERMITTED origin whose
// probe misses does abort under `set -e` before printing `[]` — but that is
// pre-existing and unchanged by this commit (the old form's bare
// `probe_pool_demand "$1"` returns 1 in exactly the same place), and gc invokes
// work_query as a plain `sh -c` with no errexit. Asserting it here would pin a
// property the script has never had.
func TestPoolDemandOriginGateGatedPathSurvivesErrexit(t *testing.T) {
	stdout, stderr, err := runGate(t, "named", "set -e; ")
	if err != nil {
		t.Fatalf("gated path exited non-zero under set -e: %v (stderr=%q)", err, stderr)
	}
	if stdout != "[]" {
		t.Errorf("stdout = %q under set -e, want %q", stdout, "[]")
	}
	if !strings.Contains(stderr, gateSignalMarker) {
		t.Errorf("gated signal lost under set -e\nstderr=%q", stderr)
	}
}

// Regression guard for the form the gate replaced. A bare `probe_pool_demand
// "$N"` is exactly what would let a named origin silently re-acquire
// pool-poaching behavior, and it is easy to reintroduce when adding a target.
func TestGeneratedWorkQueriesHaveNoUngatedProbeCall(t *testing.T) {
	agents := map[string]*Agent{
		"plain":      {Name: "worker"},
		"pool":       {Name: "worker", PoolName: "worker-pool"},
		"legacyBare": {Name: ControlDispatcherAgentName},
		"legacyDir":  {Name: ControlDispatcherAgentName, Dir: "rig"},
	}
	queries := map[string]func(*Agent) string{
		"Work":       (*Agent).EffectiveWorkQuery,
		"RoutedPool": (*Agent).EffectiveRoutedPoolQuery,
		"PoolDemand": (*Agent).EffectivePoolDemandQuery,
	}
	for agentName, agent := range agents {
		for queryName, q := range queries {
			got := q(agent)
			if !strings.Contains(got, "probe_pool_demand") {
				continue // this query has no pool tier
			}
			for _, arg := range []string{`"$1"`, `"$2"`} {
				ungated := `; probe_pool_demand ` + arg
				if strings.Contains(got, ungated) {
					t.Errorf("%s/%s contains an UNGATED probe call %q — a named origin would poach the pool",
						agentName, queryName, strings.TrimPrefix(ungated, "; "))
				}
			}
			// A gate with no tail would exit silently again, which is the
			// defect vc-ozanp5 exists to remove.
			if !strings.Contains(got, gateSignalMarker) {
				t.Errorf("%s/%s has a pool tier but no gated-signal tail", agentName, queryName)
			}
		}
	}
}

// runFullQuery executes a COMPLETE generated work_query against a fake bd, the
// way gc runs it, and captures stderr as well as stdout. It mirrors
// runShellWithFakeBd in config_test.go, which uses cmd.Output() and therefore
// cannot observe the gated signal at all.
func runFullQuery(t *testing.T, query, origin, bdScript string) (stdout, stderr string) {
	t.Helper()
	tmp := t.TempDir()
	if err := os.WriteFile(filepath.Join(tmp, "bd"), []byte(bdScript), 0o755); err != nil {
		t.Fatalf("write fake bd: %v", err)
	}
	cmd := exec.Command("sh", "-c", query)
	cmd.Env = []string{
		"PATH=" + tmp + ":" + os.Getenv("PATH"),
		"GC_SESSION_ORIGIN=" + origin,
	}
	var out, errb strings.Builder
	cmd.Stdout = &out
	cmd.Stderr = &errb
	if err := cmd.Run(); err != nil {
		t.Fatalf("run work_query: %v (stderr=%q)", err, errb.String())
	}
	return out.String(), errb.String()
}

// End-to-end statement of the bead this change exists for (vc-ozanp5): a named
// seat with routed pool work waiting must not be told "[]" and nothing else.
// The fragment tests above pin the gate's pieces; this one pins the artifact gc
// actually executes, with pool work genuinely available to be found.
func TestWorkQueryNamedOriginReportsGatedRatherThanEmpty(t *testing.T) {
	a := &Agent{Name: "worker", Dir: "hello-world"}
	query := a.EffectiveWorkQuery()
	const bdScript = `#!/bin/sh
set -eu
case "$*" in
  ready*"--metadata-field gc.routed_to=hello-world/worker"*) printf '[{"id":"routed-pool"}]' ;;
  *) printf '[]' ;;
esac
`
	// Control: the gate permits an ephemeral origin, which finds the work.
	// Without this the test below could pass simply because no work existed.
	stdout, stderr := runFullQuery(t, query, "ephemeral", bdScript)
	if !strings.Contains(stdout, "routed-pool") {
		t.Fatalf("ephemeral origin did not find the available pool work: stdout=%q stderr=%q", stdout, stderr)
	}
	if strings.Contains(stderr, gateSignalMarker) {
		t.Errorf("ephemeral origin emitted the gated signal: stderr=%q", stderr)
	}

	// The case that matters: same query, same available work, named origin.
	stdout, stderr = runFullQuery(t, query, "named", bdScript)
	if strings.Contains(stdout, "routed-pool") {
		t.Fatalf("named origin poached pool work; the gate did not refuse: stdout=%q", stdout)
	}
	if stdout != "[]" {
		t.Errorf("stdout = %q, want %q", stdout, "[]")
	}
	if !strings.Contains(stderr, gateSignalMarker) {
		t.Fatalf("named origin was refused SILENTLY — indistinguishable from a drained queue, "+
			"which is the defect this change removes: stderr=%q", stderr)
	}
}

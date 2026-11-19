package config

import (
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

// The work_query pool tier is guarded by an origin gate: only an ephemeral
// origin (or an unset one) may probe pool demand, because a named session is a
// reserved coordinator identity rather than a generic-demand worker.
//
// vc-ozanp5 changed the gate from a silent `*) exit 0` to an audible refusal.
// The refusal itself is unchanged; what changed is that it is no longer
// indistinguishable from a drained queue. These tests pin the part that
// actually matters at runtime — what the emitted shell DOES — which neither
// the parity oracle nor the goldens can show, since both only compare strings.

// runGate composes the real gate, a stubbed probe, one probe call and the
// empty tail, then executes it. probe_pool_demand is stubbed rather than built
// from poolDemandFirstRowFunctionScript so the test needs no bd/jq and stays
// hermetic; the stub reports that it ran and then misses, which is the path
// that reaches the tail.
func runGate(t *testing.T, origin, shellPrefix string) (stdout, stderr string, err error) {
	t.Helper()
	script := shellPrefix +
		poolDemandOriginGateScript() +
		`probe_pool_demand() { printf "PROBED(%s)" "$1" >&2; return 1; }; ` +
		`probe_pool_demand "$1"; ` +
		`printf "[]"`
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

const gateSignalMarker = PoolDemandOriginGateRefusalPrefix

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

// Regression guard: every probe_pool_demand call in every generated query must
// sit AFTER the audible origin gate, whose non-ephemeral arm exits the script
// before any probe runs. A probe emitted ahead of the gate (easy to
// reintroduce when adding a target) is exactly what would let a named origin
// silently re-acquire pool-poaching behavior; a gate without the refusal line
// would exit silently again, the defect vc-ozanp5 exists to remove.
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
	gateArm := gateSignalMarker
	for agentName, agent := range agents {
		for queryName, q := range queries {
			got := q(agent)
			firstProbe := strings.Index(got, `probe_pool_demand "$`)
			if firstProbe < 0 {
				continue // this query has no pool tier
			}
			gate := strings.Index(got, gateArm)
			if gate < 0 {
				t.Errorf("%s/%s has a pool tier but no audible origin gate", agentName, queryName)
				continue
			}
			if gate > firstProbe {
				t.Errorf("%s/%s emits a probe call before the origin gate — a named origin would poach the pool", agentName, queryName)
			}
			// The refusing arm must exit, or the probes after it would still run.
			arm := got[gate:]
			if end := strings.Index(arm, ";;"); end >= 0 {
				arm = arm[:end]
			}
			if !strings.Contains(arm, "exit 0") {
				t.Errorf("%s/%s: the origin gate's refusing arm does not exit before the probes:\n%s", agentName, queryName, arm)
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

// runGateSessionEnv is runGate generalized for the identity the gate consults
// beyond the origin: the caller sets session env (GC_ALIAS / GC_AGENT), the
// probe target, and which gate emitter executes. Everything else matches
// runGate: probe_pool_demand is stubbed to report-and-miss, so a probe that
// ran is visible as PROBED(<target>) on stderr without needing bd/jq.
func runGateSessionEnv(t *testing.T, origin, target string, sessionEnv map[string]string, gate string) (stdout, stderr string, err error) {
	t.Helper()
	script := gate +
		`probe_pool_demand() { printf "PROBED(%s)" "$1" >&2; return 1; }; ` +
		`probe_pool_demand "$1"; ` +
		`printf "[]"`
	cmd := exec.Command("sh", "-c", script, "--", target)
	cmd.Env = append(cmd.Environ(), "GC_SESSION_ORIGIN="+origin)
	keys := make([]string, 0, len(sessionEnv))
	for k := range sessionEnv {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		cmd.Env = append(cmd.Env, k+"="+sessionEnv[k])
	}
	var out, errb strings.Builder
	cmd.Stdout = &out
	cmd.Stderr = &errb
	runErr := cmd.Run()
	return out.String(), errb.String(), runErr
}

// allGates runs f against both emitters so any admission admitted by one is
// admitted by the other; the combined query must never diverge from the plain
// one on who may probe.
func allGates(t *testing.T, f func(t *testing.T, gate string)) {
	t.Helper()
	t.Run("plain", func(t *testing.T) { f(t, poolDemandOriginGateScript()) })
	t.Run("graphAnchor", func(t *testing.T) { f(t, poolDemandOriginGateScriptWithGraphAnchorFallback()) })
}

// vp-xxnt9: `gc session new <pool agent>` with no --alias mints a pool
// instance named "<agent>-adhoc-<token>" and stamps origin=manual. The gate
// refused that session the pool tier, so every adhoc spawn drained no_work
// while ready routed work sat unclaimed — the spawn->drain loop the witness
// filed this bead for. The minted shape must admit.
func TestPoolDemandOriginGateAdmitsAdhocPoolInstanceShape(t *testing.T) {
	cases := []struct {
		name    string
		origin  string
		gcAgent string
		target  string
	}{
		{"bare instance, bare target", "manual", "worker-adhoc-ab12cd34ef", "worker"},
		{"qualified instance, qualified target", "manual", "rig/worker-adhoc-ab12cd34ef", "rig/worker"},
		{"qualified instance, bare target", "manual", "rig/worker-adhoc-ab12cd34ef", "worker"},
		{"pack template leaf with dot (the vp-xxnt9 fleet shape)", "manual", "gastown.polecat-adhoc-25ba5e1df4", "voxist-platform/gastown.polecat"},
		{"base containing -adhoc- splits at the last one", "manual", "x-adhoc-y-adhoc-ab12cd34ef", "x-adhoc-y"},
		{"shape admission ignores origin, like namedSelfTargetAdmit", "named", "worker-adhoc-ab12cd34ef", "worker"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			allGates(t, func(t *testing.T, gate string) {
				stdout, stderr, err := runGateSessionEnv(t, tc.origin, tc.target, map[string]string{"GC_AGENT": tc.gcAgent}, gate)
				if err != nil {
					t.Fatalf("script exited non-zero: %v (stderr=%q)", err, stderr)
				}
				if !strings.Contains(stderr, "PROBED("+tc.target+")") {
					t.Errorf("adhoc pool instance %q was refused the pool tier for target %q\nstderr=%q", tc.gcAgent, tc.target, stderr)
				}
				if strings.Contains(stderr, gateSignalMarker) {
					t.Errorf("admitted session still emitted the gated signal\nstderr=%q", stderr)
				}
				if stdout != "[]" {
					t.Errorf("stdout = %q, want %q (the stub probe missed)", stdout, "[]")
				}
			})
		})
	}
}

// The mint is the ONLY thing that produces the shape, so anything else that
// carries it — a plain identity, another pool's instance, a truncated token —
// must keep failing closed into the audible refusal.
func TestPoolDemandOriginGateRefusesNonInstances(t *testing.T) {
	cases := []struct {
		name    string
		gcAgent string
		gcAlias string
		target  string
	}{
		{"plain identity carries no shape", "worker", "", "worker"},
		{"another pool's instance", "other-adhoc-ab12cd34ef", "", "worker"},
		{"empty token is not a mint", "worker-adhoc-", "", "worker"},
		{"suffix without trailing dash is not a mint", "worker-adhoc", "", "worker"},
		{"no GC_AGENT at all", "", "", "worker"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			allGates(t, func(t *testing.T, gate string) {
				env := map[string]string{}
				if tc.gcAgent != "" {
					env["GC_AGENT"] = tc.gcAgent
				}
				if tc.gcAlias != "" {
					env["GC_ALIAS"] = tc.gcAlias
				}
				stdout, stderr, err := runGateSessionEnv(t, "manual", tc.target, env, gate)
				if err != nil {
					t.Fatalf("script exited non-zero: %v (stderr=%q)", err, stderr)
				}
				if strings.Contains(stderr, "PROBED(") {
					t.Errorf("non-instance %q probed pool demand; the gate did not refuse\nstderr=%q", tc.gcAgent, stderr)
				}
				if !strings.Contains(stderr, gateSignalMarker) {
					t.Errorf("refusal was silent for %q\nstderr=%q", tc.gcAgent, stderr)
				}
				if stdout != "[]" {
					t.Errorf("stdout = %q, want %q", stdout, "[]")
				}
			})
		})
	}
}

// Precedence pin for the brace grouping: shell && and || are left-associative
// with equal precedence, so the OR of the two admit conditions must be braced
// per arm. Unbraced, `A && B || C && D` parses as `((A && B) || C) && D` and
// alias admission (namedSelfTargetAdmit, upstream #6180) would wrongly depend
// on the shape arm also matching. Each test here admits through exactly ONE
// condition while the other cannot match.
func TestPoolDemandOriginGateAdmitConditionsAreIndependent(t *testing.T) {
	t.Run("alias admits without any shape", func(t *testing.T) {
		allGates(t, func(t *testing.T, gate string) {
			stdout, stderr, err := runGateSessionEnv(t, "manual", "worker", map[string]string{"GC_ALIAS": "worker"}, gate)
			if err != nil {
				t.Fatalf("script exited non-zero: %v (stderr=%q)", err, stderr)
			}
			if !strings.Contains(stderr, "PROBED(worker)") {
				t.Errorf("deliberately-aliased manual session was refused; namedSelfTargetAdmit regressed\nstderr=%q", stderr)
			}
			if strings.Contains(stderr, gateSignalMarker) {
				t.Errorf("admitted session emitted the gated signal\nstderr=%q", stderr)
			}
			if stdout != "[]" {
				t.Errorf("stdout = %q, want %q", stdout, "[]")
			}
		})
	})
	t.Run("alias admits despite a non-matching shape arm", func(t *testing.T) {
		// The exact shape of the precedence bug: alias matches, GC_AGENT names
		// ANOTHER pool's instance. Left-associative parsing would evaluate the
		// shape arm as the group's tail and refuse.
		allGates(t, func(t *testing.T, gate string) {
			stdout, stderr, err := runGateSessionEnv(t, "manual", "worker", map[string]string{
				"GC_ALIAS": "worker",
				"GC_AGENT": "other-adhoc-ab12cd34ef",
			}, gate)
			if err != nil {
				t.Fatalf("script exited non-zero: %v (stderr=%q)", err, stderr)
			}
			if !strings.Contains(stderr, "PROBED(worker)") {
				t.Errorf("alias admission was overridden by the non-matching shape arm — the admit OR lost precedence\nstderr=%q", stderr)
			}
			if stdout != "[]" {
				t.Errorf("stdout = %q, want %q", stdout, "[]")
			}
		})
	})
	t.Run("shape admits despite an empty alias", func(t *testing.T) {
		allGates(t, func(t *testing.T, gate string) {
			stdout, stderr, err := runGateSessionEnv(t, "manual", "worker", map[string]string{
				"GC_AGENT": "worker-adhoc-ab12cd34ef",
			}, gate)
			if err != nil {
				t.Fatalf("script exited non-zero: %v (stderr=%q)", err, stderr)
			}
			if !strings.Contains(stderr, "PROBED(worker)") {
				t.Errorf("adhoc instance with empty alias was refused\nstderr=%q", stderr)
			}
			if stdout != "[]" {
				t.Errorf("stdout = %q, want %q", stdout, "[]")
			}
		})
	})
}

// runFullQueryEnv is runFullQuery with the session identity env the gate now
// consults (GC_AGENT), so the artifact gc actually executes can be tested for
// a manual adhoc spawn end to end.
func runFullQueryEnv(t *testing.T, query, origin, gcAgent, bdScript string) (stdout, stderr string) {
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
	if gcAgent != "" {
		cmd.Env = append(cmd.Env, "GC_AGENT="+gcAgent)
	}
	var out, errb strings.Builder
	cmd.Stdout = &out
	cmd.Stderr = &errb
	if err := cmd.Run(); err != nil {
		t.Fatalf("run work_query: %v (stderr=%q)", err, errb.String())
	}
	return out.String(), errb.String()
}

// End-to-end statement of vp-xxnt9, mirroring the vc-ozanp5 test above: the
// artifact gc actually executes, with pool work genuinely available, must
// serve that work to a manual adhoc pool instance — and keep refusing one
// that is an instance of a DIFFERENT pool.
func TestWorkQueryManualAdhocOriginFindsPoolWorkEndToEnd(t *testing.T) {
	a := &Agent{Name: "worker", Dir: "hello-world"}
	query := a.EffectiveWorkQuery()
	const bdScript = `#!/bin/sh
set -eu
case "$*" in
  ready*"--metadata-field gc.routed_to=hello-world/worker"*) printf '[{"id":"routed-pool"}]' ;;
  *) printf '[]' ;;
esac
`
	// The case this change exists for: origin=manual, GC_AGENT is the minted
	// instance shape of THIS pool's agent.
	stdout, stderr := runFullQueryEnv(t, query, "manual", "hello-world/worker-adhoc-abc123de", bdScript)
	if !strings.Contains(stdout, "routed-pool") {
		t.Fatalf("manual adhoc pool instance did not find the available pool work: stdout=%q stderr=%q", stdout, stderr)
	}
	if strings.Contains(stderr, gateSignalMarker) {
		t.Errorf("admitted adhoc instance emitted the gated signal: stderr=%q", stderr)
	}

	// Same query, same available work, but the session is an instance of a
	// DIFFERENT pool: still refused, still audibly.
	stdout, stderr = runFullQueryEnv(t, query, "manual", "hello-world/other-adhoc-abc123de", bdScript)
	if strings.Contains(stdout, "routed-pool") {
		t.Fatalf("another pool's manual instance poached pool work; the gate did not refuse: stdout=%q", stdout)
	}
	if stdout != "[]" {
		t.Errorf("stdout = %q, want %q", stdout, "[]")
	}
	if !strings.Contains(stderr, gateSignalMarker) {
		t.Fatalf("non-instance manual origin was refused SILENTLY: stderr=%q", stderr)
	}
}

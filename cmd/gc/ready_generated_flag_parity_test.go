package main

import (
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/spf13/cobra"
)

// The generated-flag parity test. workquery.go's reader-swap comment claims
// `gc ready` "accepts every flag the queries in this file generate, so the
// swap is the command word and nothing else". That claim was prose, and it
// broke silently: the routed tier started emitting `--sort priority` and
// `--sort hybrid` (ADR-0076 D4's union legs over ADR-0035's lookahead), and
// the federated reader kept rejecting both — so flipping a city to the
// federated reader would have served the error envelope as the work payload
// on every pool, on every spawn (vp-bzeg).
//
// This test makes the claim structural. It sweeps the query generator over
// the query kinds that carry a ready read × the agent shapes × all four
// topologies, extracts EVERY ready-reader invocation from the generated
// commands, and requires the real `gc ready` surface to accept each argv
// verbatim: the same flag registration, then the reader core
// (readyBeadsForOpts) over a live leg. A flag the generator grows without a
// reader to accept it fails here, over the generator's own output, instead of
// on a city's dispatch.

// parityRoutedTarget stands in for the generator's $target. It is a plain
// token with no shell metacharacters: what the reader parses must be the
// generated argv, not a quoting stress test.
const parityRoutedTarget = "vp-bzeg-parity-target"

// readyReaderSweepKind is one generated-query kind whose script carries ready
// reads — the same five TestEffectiveQueryParity marks federates=true. The
// other kinds (OnDeath, OnBoot) are bd list/update scripts and stay
// topology-blind by contract (TestQueryKindsWithoutAReadyReadAreTopologyBlind).
type readyReaderSweepKind struct {
	name  string
	build func(*config.Agent, config.QueryTopology) string
}

func readyReaderSweepKinds() []readyReaderSweepKind {
	return []readyReaderSweepKind{
		{"work", (*config.Agent).EffectiveWorkQueryFor},
		{"assigned_in_progress", (*config.Agent).EffectiveAssignedInProgressQueryFor},
		{"assigned_ready", (*config.Agent).EffectiveAssignedReadyQueryFor},
		{"routed_pool", (*config.Agent).EffectiveRoutedPoolQueryFor},
		{"pool_demand", (*config.Agent).EffectivePoolDemandQueryFor},
	}
}

// generatedReadyRead is one extracted reader invocation: the reader word the
// generator emitted (`bd ready` single-store, `gc ready` federated) plus the
// argv that follows it — the part that must parse identically on either.
type generatedReadyRead struct {
	reader string
	args   []string
}

// extractReadyReaderInvocations returns every `bd ready`/`gc ready` invocation
// in a generated command, in order, with the reader word it was emitted under.
//
// The generator builds each read inside `$( ... )` with its flags up to the
// closing paren (or the next shell separator), double-quotes only metadata and
// label values, and — single-store form — appends the `2>/dev/null` sink
// INSIDE the substitution, so that artifact is stripped before scanning.
//
// The capture runs to the first unquoted shell terminator rather than to
// ` --json` on purpose: the pool-demand builder puts --sort and --limit AFTER
// --json, and that half of the argv is exactly what the reader kept rejecting
// while every capture-based check stayed green (vp-bzeg).
func extractReadyReaderInvocations(command string) []generatedReadyRead {
	// readyReaderStderrSink's exact artifact. The generator appends it inside
	// the command substitution; left in place it would terminate the scan one
	// token early and hide every flag that follows it.
	command = strings.ReplaceAll(command, " 2>/dev/null", " ")
	var out []generatedReadyRead
	for _, reader := range []string{"bd ready", "gc ready"} {
		for from := 0; ; {
			at := strings.Index(command[from:], reader)
			if at < 0 {
				break
			}
			start := from + at + len(reader)
			quoted := false
			end := len(command)
		scan:
			for p := start; p < len(command); p++ {
				switch c := command[p]; {
				case c == '"':
					quoted = !quoted
				case !quoted && (c == ')' || c == ';' || c == '|' || c == '&' || c == '`' || c == '\n'):
					end = p
					break scan
				}
			}
			segment := strings.TrimSpace(command[start:end])
			// Every variable the generator embeds inside a ready read is bound
			// at runtime — $target by the script's own assignment, $1/$2 by the
			// caller, $id/$cand by the sweep loops (worker and control-
			// dispatcher spell their loop variable differently),
			// $graph_anchor_id by the crash-recovery tier's jq extraction, the
			// GC_* trio by the session environment. Binding them here makes
			// the sweep exercise the argv shape the real shell produces; any
			// variable left over is one NEITHER this sweep nor a runtime
			// binding covers, and the guard below fails loud on it rather
			// than parse a value no real shell would have sent.
			segment = strings.NewReplacer(
				"$target", parityRoutedTarget,
				"$1", parityRoutedTarget,
				"$2", parityRoutedTarget,
				"$id", parityRoutedTarget,
				"$cand", parityRoutedTarget,
				"$graph_anchor_id", parityRoutedTarget,
				"$GC_SESSION_ID", parityRoutedTarget,
				"$GC_SESSION_NAME", parityRoutedTarget,
				"$GC_ALIAS", parityRoutedTarget,
			).Replace(segment)
			if segment != "" {
				out = append(out, generatedReadyRead{reader: reader, args: splitShellWords(segment)})
			}
			from = end
		}
	}
	return out
}

// mustAcceptGeneratedReadyArgs requires the generated argv to clear the reader's
// two acceptance layers: the cobra flag surface (unknown flags, bad values) and
// the reader core (readySortOrder and friends, which validate --sort at READ
// time — flag parsing alone accepted `--sort hybrid` all along, which is how
// the rejection stayed invisible to parse-only checks).
func mustAcceptGeneratedReadyArgs(t *testing.T, label string, args []string) {
	t.Helper()
	var opts readyOpts
	var includeEphemeral, jsonOut bool
	cmd := &cobra.Command{Use: "ready"}
	registerReadyFlags(cmd, &opts, &includeEphemeral, &jsonOut)
	if err := cmd.Flags().Parse(args); err != nil {
		t.Errorf("%s: the generated argv %v does not parse on gc ready's flag surface: %v", label, args, err)
		return
	}
	if _, err := readyBeadsForOpts([]readyLeg{readyTestLeg("city", beads.NewMemStore())}, opts); err != nil {
		t.Errorf("%s: the generated argv %v parses but the reader rejects it: %v", label, args, err)
	}
}

// TestGeneratedReadyReaderArgvIsAcceptedByGcReady is the structural form of the
// swap's "the swap is the command word and nothing else" claim (vp-bzeg).
//
// The vacuous-pass guards at the bottom are load-bearing: a scanner that
// stopped finding reads, or a generator that stopped emitting the sorts that
// broke, must fail this test rather than green it.
func TestGeneratedReadyReaderArgvIsAcceptedByGcReady(t *testing.T) {
	topos := []struct {
		name string
		topo config.QueryTopology
	}{
		{"bd104_single", config.QueryTopology{}},
		{"bd105_single", config.QueryTopology{Beads: config.BeadsConfig{BDCompatibility: config.BeadsBDCompatibility105}}},
		{"bd104_federated", config.QueryTopology{FederatedReady: true}},
		{"bd105_federated", config.QueryTopology{Beads: config.BeadsConfig{BDCompatibility: config.BeadsBDCompatibility105}, FederatedReady: true}},
	}
	shapes := []*config.Agent{
		{Name: "worker"},
		{Name: "worker", PoolName: "worker-pool"},
		{Name: config.ControlDispatcherAgentName, Dir: "rig"},
	}

	invocations := 0
	uniqueArgv := map[string]bool{}
	readerSeen := map[string]bool{}
	sortSeen := map[string]bool{}
	statusReadSeen := false

	for _, kind := range readyReaderSweepKinds() {
		for _, shape := range shapes {
			for _, tt := range topos {
				command := kind.build(shape, tt.topo)
				for _, read := range extractReadyReaderInvocations(command) {
					invocations++
					label := kind.name + "/" + shape.Name + "/" + tt.name
					readerSeen[read.reader] = true
					for _, arg := range read.args {
						switch arg {
						case readySortPriority, readySortHybrid, readySortOldest, readySortNewest:
							sortSeen[arg] = true
						case readyStatusInProgress:
							statusReadSeen = true
						}
					}
					// A leftover positional means the generator grew a variable
					// this sweep does not bind — fail loud instead of feeding the
					// reader an argv the real shell would have expanded.
					for _, arg := range read.args {
						if strings.Contains(arg, "$") {
							t.Errorf("%s: the generated argv %v still carries an unbound shell variable", label, read.args)
						}
					}
					mustAcceptGeneratedReadyArgs(t, label, read.args)
					uniqueArgv[strings.Join(read.args, "\x00")] = true
				}
			}
		}
	}

	// The reader words. Both sides of the swap must appear: the single-store
	// topologies emit `bd ready`, the federated ones `gc ready`, and the claim
	// under test is that the SAME argv parses on the replacement reader.
	if !readerSeen["bd ready"] || !readerSeen["gc ready"] {
		t.Errorf("sweep saw readers %v; both `bd ready` and `gc ready` must appear or the swap's parity claim is not being exercised", readerSeen)
	}
	// The sorts. hybrid and priority are the exact argv that broke (vp-bzeg);
	// if the sweep stops observing them, the generator changed shape and this
	// test is no longer covering the regression.
	for _, policy := range []string{readySortHybrid, readySortPriority, readySortOldest} {
		if !sortSeen[policy] {
			t.Errorf("sweep never observed --sort %s; the generator or the scanner changed and the parity claim no longer covers the argv that broke", policy)
		}
	}
	// The crash-recovery tier's status read, the fifth swap site.
	if !statusReadSeen {
		t.Errorf("sweep never observed a %q read; the crash-recovery tier left the generated surface", readyStatusInProgress)
	}
	// The sweep itself. A generator or scanner regression that finds nothing
	// must red here, not green the acceptance loop above.
	if invocations < 30 {
		t.Fatalf("sweep extracted only %d ready-reader invocations (%d unique argv); the generator or the extractor regressed", invocations, len(uniqueArgv))
	}
	if len(uniqueArgv) < 10 {
		t.Fatalf("sweep extracted %d invocations but only %d unique argv; the matrix collapsed onto one shape", invocations, len(uniqueArgv))
	}
}

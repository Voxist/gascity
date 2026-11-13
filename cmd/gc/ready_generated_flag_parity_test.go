package main

import (
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/readywork"
	"github.com/spf13/cobra"
)

// The generated-flag parity test. workquery.go claims `gc ready` "accepts
// every flag the queries in this file generate, so the swap is the command
// word and nothing else". That claim was prose, and it broke silently: the
// routed tier emits `--sort priority` and `--sort hybrid`, and the federated
// reader rejected both, so every federated read that asked for them failed
// (vp-bzeg).
//
// This test makes the claim structural. It sweeps the query generator over
// the query kinds that carry a ready read, the agent shapes and all four
// topologies, extracts EVERY ready-reader invocation from the generated
// commands, and requires the real `gc ready` surface to accept each argv
// verbatim: the same flag registration, then the reader core
// (readywork.Read) over a live leg. A flag the generator grows without a
// reader to accept it fails here, over the generator's own output, instead of
// on a city's dispatch.

// parityRoutedTarget stands in for the generator's runtime variables. It is a
// plain token with no shell metacharacters: what the reader parses must be the
// generated argv, not a quoting stress test.
const parityRoutedTarget = "vp-bzeg-parity-target"

// readyReaderSweepKind is one generated-query kind whose script carries ready
// reads.
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
		{"routed_rank_probe", (*config.Agent).EffectiveRoutedRankProbeQueryFor},
	}
}

// generatedReadyRead is one extracted reader invocation: the reader word the
// generator emitted (`bd ready` single-store, `gc ready` federated) and the
// argv that follows it, the part that must parse identically on either.
type generatedReadyRead struct {
	reader string
	args   []string
}

// extractReadyReaderInvocations returns every `bd ready`/`gc ready` invocation
// in a generated command, with the reader word it was emitted under.
//
// The capture runs to the first unquoted shell terminator rather than to
// ` --json`: the pool-demand builder puts --sort and --limit AFTER --json, and
// that half of the argv is exactly what the reader rejected while captures
// that stopped at --json stayed green (vp-bzeg).
func extractReadyReaderInvocations(command string) []generatedReadyRead {
	// A stderr sink inside the substitution would end the scan one token early
	// and hide every flag after it.
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
				case !quoted && (c == ')' || c == ';' || c == '|' || c == '&' || c == '`' || c == '\n' || c == '\''):
					end = p
					break scan
				}
			}
			// Every variable a generated ready read embeds is bound at runtime:
			// $target by the script's own assignment, $1/$2 by the caller, the
			// sweep loops' variables, the crash-recovery tier's anchor, and the
			// session environment. Binding them makes the sweep parse the argv
			// the real shell produces; a variable left over fails the guard in
			// the test rather than reach the reader unexpanded.
			segment := strings.NewReplacer(
				"$target", parityRoutedTarget,
				"$1", parityRoutedTarget,
				"$2", parityRoutedTarget,
				"$id", parityRoutedTarget,
				"$cand", parityRoutedTarget,
				"$graph_anchor_id", parityRoutedTarget,
				"$GC_SESSION_ID", parityRoutedTarget,
				"$GC_SESSION_NAME", parityRoutedTarget,
				"$GC_ALIAS", parityRoutedTarget,
			).Replace(strings.TrimSpace(command[start:end]))
			if segment != "" {
				out = append(out, generatedReadyRead{reader: reader, args: splitShellWords(segment)})
			}
			from = end
		}
	}
	return out
}

// acceptGeneratedReadyArgs reports why the generated argv fails either of the
// reader's acceptance layers, or "" when it clears both: the cobra flag
// surface (unknown flags, bad values) and the reader itself, which validates
// --sort and --status. Flag parsing alone accepted `--sort hybrid` all along,
// which is how the refusal stayed invisible to parse-only checks.
func acceptGeneratedReadyArgs(args []string) string {
	var flags readyFlags
	var includeEphemeral, jsonOut bool
	cmd := &cobra.Command{Use: "ready"}
	registerReadyFlags(cmd, &flags, &includeEphemeral, &jsonOut)
	if err := cmd.Flags().Parse(args); err != nil {
		return "does not parse on gc ready's flag surface: " + err.Error()
	}
	if _, err := readywork.Read([]readywork.Leg{readyTestLeg("city", beads.NewMemStore())}, flags.query); err != nil {
		return "parses but the reader refuses it: " + err.Error()
	}
	return ""
}

// TestGeneratedReadyReaderArgvIsAcceptedByGcReady is the structural form of the
// swap's "the swap is the command word and nothing else" claim (vp-bzeg).
//
// The guards at the bottom are load-bearing: a scanner that stopped finding
// reads, or a generator that stopped emitting the sorts that broke, must fail
// this test rather than pass it vacuously.
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
				for _, read := range extractReadyReaderInvocations(kind.build(shape, tt.topo)) {
					invocations++
					label := kind.name + "/" + shape.Name + "/" + tt.name + "/" + read.reader
					readerSeen[read.reader] = true
					for i, arg := range read.args {
						if strings.Contains(arg, "$") {
							t.Errorf("%s: the generated argv %v carries an unbound shell variable", label, read.args)
						}
						if i > 0 && read.args[i-1] == "--sort" {
							sortSeen[arg] = true
						}
						if arg == "--status" || arg == "--status="+readywork.StatusInProgress {
							statusReadSeen = true
						}
					}
					if why := acceptGeneratedReadyArgs(read.args); why != "" {
						t.Errorf("%s: the generated argv %v %s", label, read.args, why)
					}
					uniqueArgv[strings.Join(read.args, "\x00")] = true
				}
			}
		}
	}

	// Both sides of the swap must appear: the single-store topologies emit
	// `bd ready`, the federated ones `gc ready`, and the claim under test is that
	// the SAME argv parses on the replacement reader.
	if !readerSeen["bd ready"] || !readerSeen["gc ready"] {
		t.Errorf("sweep saw readers %v; both `bd ready` and `gc ready` must appear or the swap's parity claim is not exercised", readerSeen)
	}
	// hybrid and priority are the argv that broke; if the sweep stops observing
	// them, the generator changed shape and this test no longer covers the
	// regression.
	for _, order := range []string{readywork.SortHybrid, readywork.SortPriority, readywork.SortOldest} {
		if !sortSeen[order] {
			t.Errorf("sweep never observed --sort %s (saw %v); the generator or the scanner changed", order, sortSeen)
		}
	}
	if !statusReadSeen {
		t.Errorf("sweep never observed a --status read; the crash-recovery tier left the generated surface")
	}
	if invocations < 30 || len(uniqueArgv) < 10 {
		t.Fatalf("sweep extracted %d ready-reader invocations (%d unique argv); the generator or the extractor regressed", invocations, len(uniqueArgv))
	}
}

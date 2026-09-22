package main

import (
	"bytes"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads/contract"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/runtime"
)

// scaleCheckProbeEnv is the connection-coordinate env the controller threads
// into every scale_check probe. The checks below read GC_DOLT_PORT.
func scaleCheckProbeEnv() map[string]string {
	return map[string]string{
		"GC_DOLT_HOST": "127.0.0.1",
		"GC_DOLT_PORT": "9876",
	}
}

type scaleCheckProbeResult struct {
	count   int
	partial bool
	stderr  string
	command string
}

// runScaleCheckProbe drives one pool through the real evaluatePendingPools —
// and therefore the real shellScaleCheck — so the assertions below describe
// what a reconciler tick actually does to an operator's scale_check.
func runScaleCheckProbe(t *testing.T, check string, env map[string]string) scaleCheckProbeResult {
	t.Helper()
	cfg := &config.City{Agents: []config.Agent{{Name: "worker", Dir: "workers"}}}
	template := cfg.Agents[0].QualifiedName()
	trace := newPoolDesiredStateTestTrace(template)
	var stderr bytes.Buffer
	pending := []poolEvalWork{{
		agentIdx: 0,
		sp:       scaleParams{Min: 0, Max: -1, Check: check},
		poolDir:  t.TempDir(),
		env:      env,
	}}
	counts, partials := evaluatePendingPools(cfg, pending, &stderr, trace)
	if len(counts) != 1 || len(partials) != 1 {
		t.Fatalf("evaluatePendingPools returned %d counts / %d partials, want 1 each", len(counts), len(partials))
	}
	return scaleCheckProbeResult{
		count:   counts[0],
		partial: partials[0],
		stderr:  stderr.String(),
		command: scaleCheckTracedCommand(t, trace),
	}
}

func scaleCheckTracedCommand(t *testing.T, trace *sessionReconcilerTraceCycle) string {
	t.Helper()
	for _, rec := range trace.records {
		if rec.RecordType != TraceRecordOperation || rec.SiteCode != TraceSiteScaleCheckExec {
			continue
		}
		command, ok := rec.Fields["command"].(string)
		if !ok {
			t.Fatalf("scale_check trace command field = %#v, want string", rec.Fields["command"])
		}
		return command
	}
	t.Fatalf("missing scale_check exec trace record; records=%#v", trace.records)
	return ""
}

// TestShellScaleCheckStructuralEnvReachesChild is the ground truth the rest of
// this file rests on: the probe env reaches the scale_check subprocess through
// runShellCommand's cmd.Env, with no textual assignment prefix on the command.
// If this ever fails, a textual prefix is load-bearing and removing it is not
// the right fix.
func TestShellScaleCheckStructuralEnvReachesChild(t *testing.T) {
	t.Setenv("GC_DOLT_PORT", "")
	out, err := shellScaleCheck(`printf '%s' "${GC_DOLT_PORT:-unset}"`, "", scaleCheckProbeEnv())
	if err != nil {
		t.Fatalf("shellScaleCheck: %v", err)
	}
	if got := strings.TrimSpace(out); got != "9876" {
		t.Fatalf("GC_DOLT_PORT in scale_check subprocess = %q, want %q (structural env must reach the child)", got, "9876")
	}
}

// TestEvaluatePendingPoolsKeywordInitialScaleCheck covers a scale_check that
// starts with a shell keyword. A POSIX assignment prefix cannot precede one, so
// a textual prefix turns the whole check into a syntax error — the pool then
// collapses to min_active_sessions on every tick.
func TestEvaluatePendingPoolsKeywordInitialScaleCheck(t *testing.T) {
	t.Setenv("GC_DOLT_PORT", "")
	got := runScaleCheckProbe(t, `if [ "$GC_DOLT_PORT" = "9876" ]; then printf 7; else printf 0; fi`, scaleCheckProbeEnv())
	if got.partial {
		t.Fatalf("keyword-initial scale_check failed: stderr=%q command=%q", got.stderr, got.command)
	}
	if got.count != 7 {
		t.Fatalf("keyword-initial scale_check desired = %d, want 7 (probe env must reach the check)", got.count)
	}
}

// TestEvaluatePendingPoolsCompoundScaleCheck covers "cd x && probe". A textual
// assignment prefix binds only to the `cd`, so the probe silently queries the
// wrong store and returns a plausible-looking number.
func TestEvaluatePendingPoolsCompoundScaleCheck(t *testing.T) {
	t.Setenv("GC_DOLT_PORT", "")
	got := runScaleCheckProbe(t, `cd / && printf '%s' "${GC_DOLT_PORT:-0}"`, scaleCheckProbeEnv())
	if got.partial {
		t.Fatalf("compound scale_check failed: stderr=%q command=%q", got.stderr, got.command)
	}
	if got.count != 9876 {
		t.Fatalf("compound scale_check saw GC_DOLT_PORT = %d, want 9876", got.count)
	}
}

// TestEvaluatePendingPoolsAssignmentFirstScaleCheck covers a check whose first
// word is itself an assignment. A textual prefix then merges into an
// assignment-only command, which exports nothing to anything downstream.
func TestEvaluatePendingPoolsAssignmentFirstScaleCheck(t *testing.T) {
	t.Setenv("GC_DOLT_PORT", "")
	got := runScaleCheckProbe(t, `limit=0; printf '%s' "${GC_DOLT_PORT:-0}"`, scaleCheckProbeEnv())
	if got.partial {
		t.Fatalf("assignment-first scale_check failed: stderr=%q command=%q", got.stderr, got.command)
	}
	if got.count != 9876 {
		t.Fatalf("assignment-first scale_check saw GC_DOLT_PORT = %d, want 9876", got.count)
	}
}

// TestEvaluatePendingPoolsScaleCheckCommandIsVerbatim pins both halves of the
// contract: the operator's command reaches the shell unmodified, and no
// credential (nor any other probe env value) is serialized into a command
// string that a process listing would expose.
func TestEvaluatePendingPoolsScaleCheckCommandIsVerbatim(t *testing.T) {
	t.Setenv("GC_DOLT_PORT", "")
	env := scaleCheckProbeEnv()
	env["GC_DOLT_PASSWORD"] = "probe-password"
	const check = `printf 3`
	got := runScaleCheckProbe(t, check, env)
	if got.partial || got.count != 3 {
		t.Fatalf("simple scale_check desired = %d (partial=%v), want 3: stderr=%q", got.count, got.partial, got.stderr)
	}
	if got.command != check {
		t.Fatalf("scale_check command = %q, want the configured command %q verbatim", got.command, check)
	}
	for _, secret := range []string{"probe-password", "GC_DOLT_PASSWORD", "GC_DOLT_HOST=", "GC_DOLT_PORT="} {
		if strings.Contains(got.command, secret) {
			t.Fatalf("scale_check command %q leaked probe env %q into the command string", got.command, secret)
		}
	}
}

// TestShellScaleCheckNilEnvHasNoStoreCoordinates pins why a poolEvalWork must
// never be built without a probe env. mergeRuntimeEnv strips the inherited
// GC_DOLT_*/BEADS_* keys before applying overrides, so a nil env does not mean
// "fall back to the controller's ambient coordinates" — it means the probe runs
// with no coordinates at all.
func TestShellScaleCheckNilEnvHasNoStoreCoordinates(t *testing.T) {
	t.Setenv("GC_DOLT_HOST", "ambient.example.com")
	t.Setenv("GC_DOLT_PORT", "1234")
	t.Setenv("BEADS_DOLT_SERVER_PORT", "5678")
	out, err := shellScaleCheck(`printf '%s/%s/%s' "${GC_DOLT_HOST:-unset}" "${GC_DOLT_PORT:-unset}" "${BEADS_DOLT_SERVER_PORT:-unset}"`, "", nil)
	if err != nil {
		t.Fatalf("shellScaleCheck: %v", err)
	}
	if got := strings.TrimSpace(out); got != "unset/unset/unset" {
		t.Fatalf("nil-env probe coordinates = %q, want %q", got, "unset/unset/unset")
	}
}

// TestBuildDesiredStateNamedBackingPoolProbeCarriesRigEnv covers the pool
// producer that used to append a poolEvalWork with no env: a rig-scoped
// template that backs a named session and carries a custom scale_check. Its
// probe must see the rig's own Dolt host and port, not the city's and not
// nothing at all.
func TestBuildDesiredStateNamedBackingPoolProbeCarriesRigEnv(t *testing.T) {
	t.Setenv("GC_BEADS", "bd")
	t.Setenv("GC_DOLT_HOST", "city-db.example.com")
	t.Setenv("GC_DOLT_PORT", "3306")
	t.Setenv("GC_DOLT_USER", "")
	t.Setenv("GC_DOLT_PASSWORD", "")
	t.Setenv("BEADS_CREDENTIALS_FILE", "")

	cityPath := t.TempDir()
	rigPath := filepath.Join(cityPath, "demo")
	if err := os.MkdirAll(filepath.Join(cityPath, ".beads"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(rigPath, 0o755); err != nil {
		t.Fatal(err)
	}
	writeRigEndpointCanonicalConfig(t, rigPath, contract.ConfigState{
		IssuePrefix:    "dm",
		EndpointOrigin: contract.EndpointOriginExplicit,
		EndpointStatus: contract.EndpointStatusVerified,
		DoltHost:       "rig-db.example.com",
		DoltPort:       "3308",
		DoltUser:       "rig-user",
	})

	// Counts 2 only when the probe holds the RIG's coordinates. The ambient
	// process env above holds the city's, so a stripped or inherited env both
	// score 0 and the assertion cannot pass by accident.
	checkCmd := `sh -c 'test "$GC_DOLT_HOST" = "rig-db.example.com" && test "$GC_DOLT_PORT" = "3308" && printf 2 || printf 0'`
	cfg := &config.City{
		Rigs: []config.Rig{{Name: "demo", Path: rigPath}},
		NamedSessions: []config.NamedSession{{
			Name:     "demo-lead",
			Template: "worker",
			Dir:      "demo",
			Mode:     "on_demand",
		}},
		Agents: []config.Agent{{
			Name:              "worker",
			Dir:               "demo",
			StartCommand:      "true",
			MinActiveSessions: intPtr(0),
			MaxActiveSessions: intPtr(5),
			ScaleCheck:        checkCmd,
		}},
	}

	desired := buildDesiredState("test-city", cityPath, time.Now().UTC(), cfg, runtime.NewFake(), nil, io.Discard)
	slots := 0
	for _, tp := range desired.State {
		if tp.TemplateName == "demo/worker" {
			slots++
		}
	}
	if slots != 2 {
		t.Fatalf("named-backing pool desired slots = %d, want 2 (probe must carry the rig's Dolt host and port)", slots)
	}
}

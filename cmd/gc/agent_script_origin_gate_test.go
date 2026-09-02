package main

import (
	"bytes"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// The vc-ozanp5 origin-gate refusal, byte-for-byte as poolDemandGatedTailScript
// (internal/config/workquery.go) emits it for a named session's empty poll.
const originGateRefusalSample = "gc: work_query pool tier not probed: origin=named is not ephemeral; routed pool work (if any) was NOT considered"

// TestAgentScriptTreatsOriginGateRefusalAsNoWork pins the interaction between
// two deliberate designs: the origin-gate refusal is AUDIBLE on stderr on
// every named session's empty poll (vc-ozanp5 / ga-bea6p — an empty hook must
// be distinguishable from a drained queue), and script-mode's exit-1
// classifier treats any non-"warning" stderr line as a hard hook failure.
// Together they made EVERY idle named script-mode agent exit 1 with
// "gc hook failed" instead of taking the graceful empty turn. The refusal is a
// policy disclosure, not a failure: it stays on stderr, and only its
// classification changes.
func TestAgentScriptTreatsOriginGateRefusalAsNoWork(t *testing.T) {
	t.Parallel()
	if !agentScriptHookExitIsNoWork("", originGateRefusalSample+"\n") {
		t.Fatal("origin-gate refusal classified as a hook FAILURE; every idle named script-mode agent exits 1 instead of the graceful no-work turn")
	}
	// The classifier must not become a blanket allowlist: a real error line
	// alongside the refusal still fails.
	if agentScriptHookExitIsNoWork("", originGateRefusalSample+"\ngc hook: running work query: exit status 1\n") {
		t.Fatal("a genuine hook error was swallowed once the gate-refusal line became benign")
	}
}

// TestOriginGateRefusalPrefixMatchesEmitter locks the benign-line prefix in
// cmd_agent_script.go to the actual emitter in internal/config/workquery.go
// (poolDemandGatedTailScript), in the repo's source-lock idiom: if the emitted
// text changes shape, this fails here instead of silently re-breaking every
// idle named script agent.
func TestOriginGateRefusalPrefixMatchesEmitter(t *testing.T) {
	t.Parallel()
	_, currentFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller failed")
	}
	emitter := filepath.Join(filepath.Dir(currentFile), "..", "..", "internal", "config", "workquery.go")
	data, err := os.ReadFile(emitter)
	if err != nil {
		t.Fatalf("ReadFile(%q): %v", emitter, err)
	}
	if !strings.Contains(string(data), originGateRefusalPrefix) {
		t.Fatalf("internal/config/workquery.go no longer emits a line starting %q; update originGateRefusalPrefix (cmd_agent_script.go) to the emitter's new shape or idle named script agents will fail again", originGateRefusalPrefix)
	}
}

// mysqlDriverChatterSample is the shape of benign stderr a federated city's bd
// legs emit on a successful poll: go-sql-driver logging a dropped connection
// before its retry succeeds. The query exits 0 with "[]"; only stderr is noisy.
const mysqlDriverChatterSample = "[mysql] 2026/08/31 10:00:00 packets.go:37: unexpected EOF"

// TestAgentScriptTreatsStderrOfSuccessfulWorkQueryAsBenign pins that stderr
// text from a work query that EXITED 0 is never a failure signal. On a
// federated city the bd legs of the generated query run with stderr attached
// (readyReaderStderrSink is "" when topo.FederatedReady), so driver chatter like
// mysqlDriverChatterSample reaches gc hook's stderr on every idle poll. Script
// mode only sees gc hook's exit code, which is 1 for BOTH no-work and a failed
// read, so the runner has to carry "the query exited 0" into the forwarded
// line itself — otherwise the classifier turned every idle poll on such a city
// into "gc hook failed".
func TestAgentScriptTreatsStderrOfSuccessfulWorkQueryAsBenign(t *testing.T) {
	t.Parallel()
	var hookStderr bytes.Buffer
	run := hookWorkQueryRunner(&hookStderr)
	out, err := run(`printf '%s\n' "`+mysqlDriverChatterSample+`" >&2; printf '[]'`, "", nil)
	if err != nil {
		t.Fatalf("work query exited non-zero: %v", err)
	}
	if strings.TrimSpace(out) != "[]" {
		t.Fatalf("work query stdout = %q, want []", out)
	}
	// The chatter must still be audible: forwarding is the whole point of the
	// diag path (an empty hook must be distinguishable from a drained queue).
	if !strings.Contains(hookStderr.String(), mysqlDriverChatterSample) {
		t.Fatalf("runner dropped the query's stderr: %q", hookStderr.String())
	}
	if !agentScriptHookExitIsNoWork(out, hookStderr.String()) {
		t.Fatalf("stderr from a work query that exited 0 was classified as a hook FAILURE; every idle poll on a federated city exits 1 with \"gc hook failed\":\nstderr=%q", hookStderr.String())
	}

	// Through the script-mode entrypoint: a hook that polled empty (exit 1)
	// after forwarding that chatter takes the graceful no-work turn.
	var scriptStderr bytes.Buffer
	bead, ok, err := agentScriptHookBeadWithRunner(&scriptStderr, func(_ []string, _ bool, _ string, stdout, stderr io.Writer) int {
		_, _ = stderr.Write(hookStderr.Bytes())
		_, _ = io.WriteString(stdout, out)
		return 1
	})
	if err != nil {
		t.Fatalf("agentScriptHookBeadWithRunner: %v (stderr=%q)", err, scriptStderr.String())
	}
	if ok || bead.ID != "" {
		t.Fatalf("empty poll returned a bead: %+v", bead)
	}
	if !strings.Contains(scriptStderr.String(), mysqlDriverChatterSample) {
		t.Fatalf("script mode dropped the forwarded chatter: %q", scriptStderr.String())
	}
}

// The failure path is untouched: a work query that exits non-zero is still a
// hook error, with its stderr folded into the error line, whatever it said.
// (An application error, not a transport marker: a transport-class message on
// a FAILED query is the store-unavailable exit-2 path, pinned elsewhere.)
func TestAgentScriptStillFailsWhenWorkQueryExitsNonZero(t *testing.T) {
	t.Parallel()
	var hookStderr bytes.Buffer
	run := hookWorkQueryRunner(&hookStderr)
	_, err := run(`printf 'Error: unknown flag: --nope\n' >&2; exit 1`, "", nil)
	if err == nil {
		t.Fatal("work query exit 1 returned nil error")
	}
	if hookStderr.Len() != 0 {
		t.Fatalf("a failed query's stderr must ride the error, not the diag stream: %q", hookStderr.String())
	}
	// doHook renders that error the way gc hook does; the classifier must
	// still call it a failure.
	var stdout, stderr bytes.Buffer
	code := doHook("q", "", false, func(string, string) (string, error) { return "", err }, &stdout, &stderr, hookVisibility{})
	if code != 1 {
		t.Fatalf("doHook = %d, want 1", code)
	}
	if agentScriptHookExitIsNoWork(stdout.String(), stderr.String()) {
		t.Fatalf("a failed work query was classified as no-work:\nstderr=%q", stderr.String())
	}
}

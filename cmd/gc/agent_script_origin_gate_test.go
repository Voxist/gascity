package main

import (
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

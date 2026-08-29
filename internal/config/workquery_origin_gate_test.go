package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The vc-ozanp5 regression set: the Tier 3 origin gate must let a NAMED
// session probe pool demand for its own identity (exact alias or one of its
// numbered seats) while still refusing a foreign pool's target — and when it
// refuses, it must say so on stderr so "gated" stops reading as "no work".
// Before the fix the gate was `*) exit 0`: every named origin exited before
// probe_pool_demand ran, and gc.routed_to=<named agent> work was invisible to
// gc hook for every named session fleet-wide.
func runWorkQueryWithFakeBd(t *testing.T, a Agent, env map[string]string) (stdout, stderr string, probeLog string) {
	t.Helper()
	tmp := t.TempDir()
	logPath := filepath.Join(tmp, "probes.log")
	// The fake bd logs every routed_to target it is asked about and serves an
	// empty array, so the script runs to the tail instead of exiting on a row.
	bdScript := "#!/bin/sh\nfor a in \"$@\"; do case \"$a\" in gc.routed_to=*) printf '%s\\n' \"$a\" >> " + logPath + " ;; esac; done\nprintf '[]'\n"
	bdPath := filepath.Join(tmp, "bd")
	if err := os.WriteFile(bdPath, []byte(bdScript), 0o755); err != nil {
		t.Fatalf("write fake bd: %v", err)
	}
	commandEnv := []string{"PATH=" + tmp + ":" + os.Getenv("PATH")}
	for k, v := range env {
		commandEnv = append(commandEnv, k+"="+v)
	}
	stdout, stderr, exit := runShellCommandCapture(t, a.EffectiveWorkQueryFor(QueryTopology{}), commandEnv)
	if exit != 0 {
		t.Fatalf("work query exited %d: %s", exit, stderr)
	}
	log, _ := os.ReadFile(logPath)
	return stdout, stderr, string(log)
}

func TestOriginGateNamedSessionProbesSelfAddressedAlias(t *testing.T) {
	a := Agent{Name: "mayor", Dir: "gastown"}
	stdout, stderr, log := runWorkQueryWithFakeBd(t, a, map[string]string{
		"GC_SESSION_ORIGIN": "named",
		"GC_ALIAS":          "gastown.mayor",
		"GC_AGENT":          "gastown.mayor",
		"GC_SESSION_ID":     "",
		"GC_SESSION_NAME":   "",
	})
	if !strings.Contains(log, "gc.routed_to=gastown/mayor") {
		t.Fatalf("named singleton did not probe its own routed queue: log=%q stdout=%q stderr=%q", log, stdout, stderr)
	}
	if strings.Contains(stderr, "gated") {
		t.Fatalf("self-addressed probe must not report gated: %q", stderr)
	}
}

func TestOriginGateNamedSeatProbesItsUnsuffixedTarget(t *testing.T) {
	// voxist-city.platform-architect-1 is a numbered seat of the agent whose
	// qualified name (the gc.routed_to stamp) is voxist.platform-architect.
	a := Agent{Name: "platform-architect", Dir: "voxist-city"}
	stdout, stderr, log := runWorkQueryWithFakeBd(t, a, map[string]string{
		"GC_SESSION_ORIGIN": "named",
		"GC_ALIAS":          "voxist-city.platform-architect-1",
		"GC_AGENT":          "voxist-city.platform-architect-1",
		"GC_SESSION_ID":     "",
		"GC_SESSION_NAME":   "",
	})
	if !strings.Contains(log, "gc.routed_to=voxist-city/platform-architect") {
		t.Fatalf("numbered seat did not probe its agent's routed queue: log=%q stdout=%q stderr=%q", log, stdout, stderr)
	}
	if strings.Contains(stderr, "gated") {
		t.Fatalf("self-addressed seat probe must not report gated: %q", stderr)
	}
}

func TestOriginGateNamedSessionSkipsForeignTargetWithSignal(t *testing.T) {
	// A named polecat seat (alias rig/polecat-adhoc-<hash>) must NOT match the
	// polecat pool target rig/polecat: "-adhoc-..." is not a numbered seat.
	a := Agent{Name: "polecat", Dir: "rig"}
	stdout, stderr, log := runWorkQueryWithFakeBd(t, a, map[string]string{
		"GC_SESSION_ORIGIN": "named",
		"GC_ALIAS":          "rig/polecat-adhoc-deadbeef",
		"GC_AGENT":          "rig/polecat-adhoc-deadbeef",
		"GC_SESSION_ID":     "",
		"GC_SESSION_NAME":   "",
	})
	if strings.Contains(log, "gc.routed_to=rig/polecat") {
		t.Fatalf("named adhoc seat poached the pool queue: log=%q", log)
	}
	if strings.TrimSpace(stdout) != "[]" {
		t.Fatalf("gated query must still print the empty fallthrough, got %q", stdout)
	}
	if !strings.Contains(stderr, "gated") {
		t.Fatalf("gated skip must be distinguishable from no work on stderr, got %q", stderr)
	}
}

func TestOriginGateEphemeralOriginStillProbes(t *testing.T) {
	a := Agent{Name: "polecat", Dir: "rig"}
	stdout, _, log := runWorkQueryWithFakeBd(t, a, map[string]string{
		"GC_SESSION_ORIGIN": "ephemeral",
		"GC_ALIAS":          "rig/polecat-7",
		"GC_AGENT":          "rig/polecat-7",
		"GC_SESSION_ID":     "",
		"GC_SESSION_NAME":   "",
	})
	if !strings.Contains(log, "gc.routed_to=rig/polecat") {
		t.Fatalf("ephemeral worker did not probe its pool queue: log=%q stdout=%q", log, stdout)
	}
}

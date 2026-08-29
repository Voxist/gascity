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
	t.Setenv("GC_ALIAS", "worker")
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

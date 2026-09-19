package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	bdpack "github.com/gastownhall/gascity/examples/bd"
)

// killImposterHarness runs gc-beads-bd.sh's real kill_imposter against a port
// holder whose ownership verify_our_server reports as ours or not, with kill
// and sleep replaced by tracing stubs so nothing is signaled. It returns the
// trace and kill_imposter's exit status.
func killImposterHarness(t *testing.T, owned bool) (trace string, status string) {
	t.Helper()
	scriptData, err := bdpack.PackFS.ReadFile("assets/scripts/gc-beads-bd.sh")
	if err != nil {
		t.Fatalf("read embedded gc-beads-bd.sh: %v", err)
	}
	prelude, _, ok := strings.Cut(string(scriptData), "# --- Main ---")
	if !ok {
		t.Fatal("gc-beads-bd script missing main marker")
	}
	dir := t.TempDir()
	traceFile := filepath.Join(dir, "trace")
	ownedStatus := "1"
	if owned {
		ownedStatus = "0"
	}
	harness := prelude + `
TRACE_FILE="$GC_TEST_TRACE_FILE"
DOLT_PORT=15000
DATA_DIR=/city/.beads/dolt
trace() { printf '%s\n' "$1" >> "$TRACE_FILE"; }
verify_our_server() { trace "verify:$1"; return ` + ownedStatus + `; }
kill() { trace "kill:$*"; return 1; }
sleep() { return 0; }

if kill_imposter 4242; then
    trace "status:0"
else
    trace "status:1"
fi
`
	harnessPath := filepath.Join(dir, "harness.sh")
	if err := os.WriteFile(harnessPath, []byte(harness), 0o644); err != nil {
		t.Fatalf("write harness: %v", err)
	}
	runShHarness(t, harnessPath, "kill_imposter", sanitizedBaseEnv("GC_TEST_TRACE_FILE="+traceFile))
	data, err := os.ReadFile(traceFile)
	if err != nil {
		t.Fatalf("read trace: %v", err)
	}
	trace = string(data)
	switch {
	case strings.Contains(trace, "status:0"):
		status = "0"
	case strings.Contains(trace, "status:1"):
		status = "1"
	}
	return trace, status
}

// TestGcBeadsBdKillImposterRefusesForeignPortHolder is the ga-cflrh guard for
// the script's port-holder kill. A process on DOLT_PORT that verify_our_server
// cannot identify as this city's dolt server — another city's live server, or
// anything else — must never be signaled; kill_imposter refuses and reports
// failure so the start aborts instead.
func TestGcBeadsBdKillImposterRefusesForeignPortHolder(t *testing.T) {
	trace, status := killImposterHarness(t, false)

	if !strings.Contains(trace, "verify:4242") {
		t.Fatalf("kill_imposter did not verify ownership before acting; trace:\n%s", trace)
	}
	if strings.Contains(trace, "kill:") {
		t.Fatalf("kill_imposter signaled a port holder that is not this city's server; trace:\n%s", trace)
	}
	if status != "1" {
		t.Fatalf("kill_imposter status = %q, want 1 so the caller fails the start; trace:\n%s", status, trace)
	}
}

// TestGcBeadsBdKillImposterStopsOwnStaleServer is the control: a stale server
// verify_our_server identifies as ours is still stopped.
func TestGcBeadsBdKillImposterStopsOwnStaleServer(t *testing.T) {
	trace, status := killImposterHarness(t, true)

	if !strings.Contains(trace, "kill:4242") {
		t.Fatalf("kill_imposter did not stop this city's own stale server; trace:\n%s", trace)
	}
	if status != "0" {
		t.Fatalf("kill_imposter status = %q, want 0; trace:\n%s", status, trace)
	}
}

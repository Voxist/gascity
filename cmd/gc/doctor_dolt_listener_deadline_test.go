package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/doctor"
)

// vp-9ex9z: the doctor check that would have caught the leak while it was
// live. read_timeout_millis is fixed at server start and no SET GLOBAL moves
// it, so city.toml, the process env and `gc config` can all agree and all be
// wrong about the running listener. The check reads the two artifacts that
// can disagree: the rendered YAML and dolt.log's `Starting server` record.

func writeDoltLogWithStartingServer(t *testing.T, path string, timeouts ...string) {
	t.Helper()
	var b strings.Builder
	for _, tmo := range timeouts {
		b.WriteString("INFO[0000] Starting server with Config HP=\"127.0.0.1:48770\"|T=" + tmo + "|R=\"false\"|L=\"warning\"\n")
		b.WriteString("INFO[0001] Server ready. Accepting connections.\n")
	}
	if err := os.WriteFile(path, []byte(b.String()), 0o644); err != nil {
		t.Fatalf("write dolt.log: %v", err)
	}
}

func TestReadDoltLogStartingServerTimeoutMillisTakesTheLastStart(t *testing.T) {
	logPath := filepath.Join(t.TempDir(), "dolt.log")
	writeDoltLogWithStartingServer(t, logPath, `"30000"`, `"600000"`, `"30000"`)
	got, ok := readDoltLogStartingServerTimeoutMillis(logPath)
	if !ok {
		t.Fatal("no Starting server record parsed")
	}
	if got != 30000 {
		t.Errorf("timeout = %d, want the LAST start's 30000", got)
	}
}

func TestReadDoltLogStartingServerTimeoutMillisAcceptsUnquoted(t *testing.T) {
	logPath := filepath.Join(t.TempDir(), "dolt.log")
	writeDoltLogWithStartingServer(t, logPath, "600000")
	got, ok := readDoltLogStartingServerTimeoutMillis(logPath)
	if !ok || got != 600000 {
		t.Errorf("timeout = %d (ok=%v), want 600000", got, ok)
	}
}

// The live city's dolt.log was 225 MB on 2026-09-05. The check must read the
// tail only, and must still find the most recent start record.
func TestReadDoltLogStartingServerTimeoutMillisReadsOnlyTheTail(t *testing.T) {
	logPath := filepath.Join(t.TempDir(), "dolt.log")
	f, err := os.Create(logPath)
	if err != nil {
		t.Fatalf("create log: %v", err)
	}
	// A stale start record far outside the tail window, then noise, then the
	// current one.
	if _, err := fmt.Fprintf(f, "INFO Starting server with Config HP=\"x\"|T=\"600000\"\n"); err != nil {
		t.Fatalf("write stale record: %v", err)
	}
	noise := strings.Repeat("INFO[0000] query executed in 1ms\n", 40000)
	if _, err := f.WriteString(noise); err != nil {
		t.Fatalf("write noise: %v", err)
	}
	if _, err := fmt.Fprintf(f, "INFO Starting server with Config HP=\"x\"|T=\"30000\"\n"); err != nil {
		t.Fatalf("write current record: %v", err)
	}
	if err := f.Close(); err != nil {
		t.Fatalf("close log: %v", err)
	}
	info, err := os.Stat(logPath)
	if err != nil {
		t.Fatalf("stat log: %v", err)
	}
	if info.Size() <= doltListenerDeadlineLogTailBytes {
		t.Fatalf("log is %d bytes, needs to exceed the %d-byte tail window", info.Size(), doltListenerDeadlineLogTailBytes)
	}
	got, ok := readDoltLogStartingServerTimeoutMillis(logPath)
	if !ok || got != 30000 {
		t.Errorf("timeout = %d (ok=%v), want the current start's 30000", got, ok)
	}
}

func TestReadDoltConfigReadTimeoutMillis(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "dolt-config.yaml")
	if err := writeManagedDoltConfigFile(path, "127.0.0.1", "48770", dir, "warning",
		config.DoltConfig{ReadTimeoutMillis: 30000}); err != nil {
		t.Fatalf("render config: %v", err)
	}
	got, ok := readDoltConfigReadTimeoutMillis(path)
	if !ok || got != 30000 {
		t.Errorf("read_timeout_millis = %d (ok=%v), want 30000", got, ok)
	}
	if _, ok := readDoltConfigReadTimeoutMillis(filepath.Join(dir, "absent.yaml")); ok {
		t.Error("a missing config file must report no value")
	}
}

// listenerDeadlineTestConfigured is the deadline city.toml resolved to on
// the leaking host.
const listenerDeadlineTestConfigured = 30000

// installListenerDeadlineCity builds a city whose managed-Dolt layout lives
// under a tmpdir, with the configured deadline supplied through the same env
// fallback production used on the leaking host.
func installListenerDeadlineCity(t *testing.T) (string, managedDoltRuntimeLayout) {
	t.Helper()
	cityPath := t.TempDir()
	packStateDir := filepath.Join(cityPath, "pack-state")
	if err := os.MkdirAll(packStateDir, 0o755); err != nil {
		t.Fatalf("mkdir pack state dir: %v", err)
	}
	t.Setenv("GC_PACK_STATE_DIR", packStateDir)
	t.Setenv("GC_DOLT_READ_TIMEOUT_MILLIS", fmt.Sprintf("%d", listenerDeadlineTestConfigured))
	layout, err := resolveManagedDoltRuntimeLayout(cityPath)
	if err != nil {
		t.Fatalf("resolve layout: %v", err)
	}
	// The publish guard arm needs no live server for these cases.
	prevPort := managedDoltResolvablePortFn
	t.Cleanup(func() { managedDoltResolvablePortFn = prevPort })
	managedDoltResolvablePortFn = func(string) string { return "" }
	return cityPath, layout
}

func runListenerDeadlineCheck(t *testing.T, cityPath string) *doctor.CheckResult {
	t.Helper()
	return newDoltListenerDeadlineCheck(cityPath, &config.City{}).Run(nil)
}

// The 2026-09-05 shape: the running server bound 600000 while city.toml
// resolved to 30000. The check must FAIL and name both numbers.
func TestDoltListenerDeadlineCheckFailsOnALiveServerAboveTheConfiguredValue(t *testing.T) {
	cityPath, layout := installListenerDeadlineCity(t)
	writeDoltLogWithStartingServer(t, layout.LogFile, `"600000"`)
	if err := writeManagedDoltConfigFile(layout.ConfigFile, "127.0.0.1", "48770", layout.DataDir, "warning",
		config.DoltConfig{ReadTimeoutMillis: 600000}); err != nil {
		t.Fatalf("render config: %v", err)
	}

	got := runListenerDeadlineCheck(t, cityPath)
	if got.Status != doctor.StatusError {
		t.Fatalf("status = %v, want error (result %+v)", got.Status, got)
	}
	joined := got.Message + " " + strings.Join(got.Details, " ")
	for _, want := range []string{"600000", "30000"} {
		if !strings.Contains(joined, want) {
			t.Errorf("result %q does not name %s", joined, want)
		}
	}
}

// The rendered config can drift on its own: the running server is fine but
// the next start would bind the wrong deadline.
func TestDoltListenerDeadlineCheckFailsOnRenderedConfigDriftAlone(t *testing.T) {
	cityPath, layout := installListenerDeadlineCity(t)
	writeDoltLogWithStartingServer(t, layout.LogFile, `"30000"`)
	if err := writeManagedDoltConfigFile(layout.ConfigFile, "127.0.0.1", "48770", layout.DataDir, "warning",
		config.DoltConfig{ReadTimeoutMillis: 600000}); err != nil {
		t.Fatalf("render config: %v", err)
	}

	got := runListenerDeadlineCheck(t, cityPath)
	if got.Status != doctor.StatusError {
		t.Fatalf("status = %v, want error (result %+v)", got.Status, got)
	}
	if !strings.Contains(strings.Join(got.Details, " "), "rendered read_timeout_millis 600000") {
		t.Errorf("details %v do not name the rendered drift", got.Details)
	}
}

func TestDoltListenerDeadlineCheckPassesWhenEverythingAgrees(t *testing.T) {
	cityPath, layout := installListenerDeadlineCity(t)
	writeDoltLogWithStartingServer(t, layout.LogFile, `"30000"`)
	if err := writeManagedDoltConfigFile(layout.ConfigFile, "127.0.0.1", "48770", layout.DataDir, "warning",
		config.DoltConfig{ReadTimeoutMillis: 30000}); err != nil {
		t.Fatalf("render config: %v", err)
	}

	got := runListenerDeadlineCheck(t, cityPath)
	if got.Status != doctor.StatusOK {
		t.Fatalf("status = %v, want ok (result %+v)", got.Status, got)
	}
}

// A window server holding the managed port is its own failure: by
// construction it runs at the raised deadline and the swarm must never
// reach it.
func TestDoltListenerDeadlineCheckFailsWhenAWindowServerHoldsThePort(t *testing.T) {
	cityPath, layout := installListenerDeadlineCity(t)
	writeDoltLogWithStartingServer(t, layout.LogFile, `"30000"`)
	if err := writeManagedDoltConfigFile(layout.ConfigFile, "127.0.0.1", "48770", layout.DataDir, "warning",
		config.DoltConfig{ReadTimeoutMillis: 30000}); err != nil {
		t.Fatalf("render config: %v", err)
	}
	managedDoltResolvablePortFn = func(string) string { return "48770" }
	prevPID := deliveryWindowServerPIDFn
	t.Cleanup(func() { deliveryWindowServerPIDFn = prevPID })
	deliveryWindowServerPIDFn = func(string, string) int { return 9911 }

	got := runListenerDeadlineCheck(t, cityPath)
	if got.Status != doctor.StatusError {
		t.Fatalf("status = %v, want error (result %+v)", got.Status, got)
	}
	if !strings.Contains(strings.Join(got.Details, " "), "9911") {
		t.Errorf("details %v do not name the window server pid", got.Details)
	}
}

func TestDoltListenerDeadlineCheckIsQuietWithoutManagedDolt(t *testing.T) {
	cityPath, _ := installListenerDeadlineCity(t)
	// A non-bd provider takes the city out of managed-Dolt topology.
	t.Setenv("GC_BEADS", "exec:/bin/true")
	got := runListenerDeadlineCheck(t, cityPath)
	if got.Status != doctor.StatusOK {
		t.Fatalf("status = %v, want ok (result %+v)", got.Status, got)
	}
	if !strings.Contains(got.Message, "not using managed Dolt topology") {
		t.Errorf("message = %q, want the not-applicable record", got.Message)
	}
}

func TestDoltListenerDeadlineCheckMetadata(t *testing.T) {
	c := newDoltListenerDeadlineCheck("/city", nil)
	if c.Name() != "dolt-listener-deadline" {
		t.Errorf("name = %q", c.Name())
	}
	if c.CanFix() {
		t.Error("CanFix must be false: repair needs a supervised Dolt restart")
	}
	if c.WarmupEligible() {
		t.Error("WarmupEligible must be false: the check tails a very large log")
	}
	if err := c.Fix(nil); err != nil {
		t.Errorf("Fix = %v, want nil", err)
	}
}

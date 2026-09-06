package main

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

// vp-9ex9z regression suite. The ADR-0064 delivery window raises
// read_timeout_millis for its OWN nested server only; the swarm-facing
// server must always bind at the value city.toml (or the ambient
// GC_DOLT_READ_TIMEOUT_MILLIS) resolves to. On 2026-09-05 both servers
// rendered to the same file, so the published config — and the server that
// stayed up on 48770 — carried the window's 600000 while the configured
// value was 30000.
//
// These tests drive the REAL start path (startManagedDoltProcessWithConfig
// with the window armed) through the existing loop stubs, so the assertions
// are about bytes on disk and the config each start was handed, not about
// the window helper in isolation.

const (
	leakTestConfiguredReadTimeout = 30000
	leakTestWindowReadTimeout     = 600000
)

// deliveryWindowStartRecord is one observed dolt start: the config file it
// was handed, plus what the PUBLISHED config file held at that instant.
// The second field is the leak detector — on the shipped 2026-09-04 build
// the published file already carried the window's raised deadline by the
// time the window server started, and every later reader (an outer start
// that never re-renders, a watchdog, an operator) saw that value.
type deliveryWindowStartRecord struct {
	ConfigFile          string
	PublishedReadTimout int
	PublishedExists     bool
}

type deliveryWindowStartRecorder struct {
	mu           sync.Mutex
	publishedCfg string
	records      []deliveryWindowStartRecord
}

func (r *deliveryWindowStartRecorder) record(configFile string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	rec := deliveryWindowStartRecord{ConfigFile: configFile}
	rec.PublishedReadTimout, rec.PublishedExists = readDoltConfigReadTimeoutMillis(r.publishedCfg)
	r.records = append(r.records, rec)
}

func (r *deliveryWindowStartRecorder) snapshot() []deliveryWindowStartRecord {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]deliveryWindowStartRecord(nil), r.records...)
}

func (r *deliveryWindowStartRecorder) configFiles() []string {
	out := []string{}
	for _, rec := range r.snapshot() {
		out = append(out, rec.ConfigFile)
	}
	return out
}

// installDeliveryWindowLeakHarness wires a publishing start whose window is
// armed, whose drain and stop are stubbed, and whose dolt subprocess is
// faked. It returns the city path and the recorder of every config file a
// start was handed.
//
// The window's START seam is deliberately left at production
// (defaultDeliveryWindowStart) — the leak lives in the nested start's render,
// so stubbing it would test nothing.
func installDeliveryWindowLeakHarness(t *testing.T, drainErr error, stopErr error) (string, *deliveryWindowStartRecorder) {
	t.Helper()
	// The configured value arrives via the env fallback that production used
	// on the leaking host (GC_DOLT_READ_TIMEOUT_MILLIS=30000 was set on both
	// the watchdog and the server), which also keeps the tmpdir city free of
	// a city.toml so the publish step is a clean no-op.
	t.Setenv("GC_DOLT_READ_TIMEOUT_MILLIS", strconv.Itoa(leakTestConfiguredReadTimeout))
	t.Setenv("GC_DOLT_DELIVERY_WINDOW_READ_TIMEOUT_MILLIS", strconv.Itoa(leakTestWindowReadTimeout))
	t.Setenv("GC_DOLT_BOOT_DRAIN", "0")
	// The tmpdir city has no city.toml and no bead store, so the publish
	// step at the end of a publishing start has nothing to publish. Point
	// the beads provider away from the bd contract so
	// managedDoltLifecycleOwned answers "not owned" and publishing is a
	// clean no-op — the window arming this test drives is gated on the
	// publish FLAG, not on publication succeeding.
	t.Setenv("GC_BEADS", "exec:/bin/true")

	recorder := &deliveryWindowStartRecorder{}
	cityPath := installStartManagedDoltLoopStubs(t, startManagedDoltLoopStubs{
		startFn: func(cityPath, configFile, _ string, _ *os.File) (managedDoltStartedProcess, error) {
			recorder.record(configFile)
			return managedDoltStartedProcess{CityPath: cityPath, PID: 0}, nil
		},
		waitReadyFn: func(_, _, _, _ string, _ int, _ time.Duration, _ bool) (managedDoltWaitReadyReport, error) {
			return managedDoltWaitReadyReport{Ready: true, PIDAlive: true}, nil
		},
		logSuffixFn:     func(_ string, _ int64) (string, error) { return "", nil },
		portAvailableFn: func(_ string, _ int) bool { return true },
		retryWindow:     10 * time.Millisecond,
	})

	layout, err := resolveManagedDoltRuntimeLayout(cityPath)
	if err != nil {
		t.Fatalf("resolve layout: %v", err)
	}
	recorder.publishedCfg = layout.ConfigFile

	prevDrain, prevStop := deliveryWindowDrainFn, deliveryWindowStopFn
	t.Cleanup(func() { deliveryWindowDrainFn, deliveryWindowStopFn = prevDrain, prevStop })
	deliveryWindowDrainFn = func(_, _, _ string, _ time.Duration) error { return drainErr }
	deliveryWindowStopFn = func(_, _ string) error { return stopErr }

	return cityPath, recorder
}

func readRenderedReadTimeout(t *testing.T, path string) int {
	t.Helper()
	value, ok := readDoltConfigReadTimeoutMillis(path)
	if !ok {
		t.Fatalf("no read_timeout_millis in rendered config %s", path)
	}
	return value
}

// The published config file must carry the CONFIGURED deadline after a
// windowed start, and the window's raised deadline must live in its own
// sibling file. This is the exact artifact that was wrong on the live host:
// .gc/runtime/packs/dolt/dolt-config.yaml held 600000 with city.toml at
// 30000.
func TestDeliveryWindowDoesNotWriteItsDeadlineIntoThePublishedConfig(t *testing.T) {
	cityPath, recorder := installDeliveryWindowLeakHarness(t, nil, nil)
	layout, err := resolveManagedDoltRuntimeLayout(cityPath)
	if err != nil {
		t.Fatalf("resolve layout: %v", err)
	}

	report, err := startManagedDoltProcessWithConfig(cityPath, "127.0.0.1", "17781", "root", "warning", -1, time.Second, true, nil, false)
	if err != nil {
		t.Fatalf("publishing start failed: %v", err)
	}
	if !report.Ready {
		t.Fatalf("report.Ready = false, want true")
	}
	if !report.DeliveryWindow.Ran || report.DeliveryWindow.Err != "" {
		t.Fatalf("delivery window outcome = %+v, want a clean run", report.DeliveryWindow)
	}

	if got := readRenderedReadTimeout(t, layout.ConfigFile); got != leakTestConfiguredReadTimeout {
		t.Errorf("published config read_timeout_millis = %d, want %d — the window leaked its deadline into %s",
			got, leakTestConfiguredReadTimeout, layout.ConfigFile)
	}
	windowConfig := managedDoltDeliveryWindowConfigFile(layout)
	if got := readRenderedReadTimeout(t, windowConfig); got != leakTestWindowReadTimeout {
		t.Errorf("window config read_timeout_millis = %d, want %d (%s)", got, leakTestWindowReadTimeout, windowConfig)
	}
	if windowConfig == layout.ConfigFile {
		t.Fatalf("window config path %s must differ from the published one", windowConfig)
	}

	// The leak was never only about the END state: on the shipped build the
	// published config ALREADY held 600000 while the window server ran, so
	// an outer start that never got to re-render (its lock gate refuses
	// while the window server still owns the data dir) left that value in
	// place. Assert the published file is untouched at the instant the
	// window server starts.
	starts := recorder.snapshot()
	if len(starts) != 2 {
		t.Fatalf("dolt starts = %d, want 2", len(starts))
	}
	if starts[0].PublishedExists && starts[0].PublishedReadTimout == leakTestWindowReadTimeout {
		t.Errorf("published config held the window deadline %d while the window server started — the vp-9ex9z leak",
			starts[0].PublishedReadTimout)
	}
	if !starts[1].PublishedExists || starts[1].PublishedReadTimout != leakTestConfiguredReadTimeout {
		t.Errorf("published server started with published config read_timeout_millis = %d (present=%v), want %d",
			starts[1].PublishedReadTimout, starts[1].PublishedExists, leakTestConfiguredReadTimeout)
	}
}

// The PUBLISHED server is started from the published config, and the window
// server from its own. Two starts, two distinct --config paths, in that
// order — on the live host there was exactly ONE `Starting server` line per
// restart and it carried the window's deadline.
func TestDeliveryWindowPublishedServerStartsFromThePublishedConfig(t *testing.T) {
	cityPath, recorder := installDeliveryWindowLeakHarness(t, nil, nil)
	layout, err := resolveManagedDoltRuntimeLayout(cityPath)
	if err != nil {
		t.Fatalf("resolve layout: %v", err)
	}

	if _, err := startManagedDoltProcessWithConfig(cityPath, "127.0.0.1", "17782", "root", "warning", -1, time.Second, true, nil, false); err != nil {
		t.Fatalf("publishing start failed: %v", err)
	}

	starts := recorder.configFiles()
	if len(starts) != 2 {
		t.Fatalf("dolt starts = %v, want 2 (the window server then the published server)", starts)
	}
	if starts[0] != managedDoltDeliveryWindowConfigFile(layout) {
		t.Errorf("first start used %s, want the window config %s", starts[0], managedDoltDeliveryWindowConfigFile(layout))
	}
	if starts[1] != layout.ConfigFile {
		t.Errorf("published start used %s, want %s", starts[1], layout.ConfigFile)
	}
}

// ADR-0064 constraint 2: a window that FAILS must not block the boot, and
// the server that boots anyway must still bind at the configured deadline.
func TestDeliveryWindowFailedDrainStillPublishesAtConfiguredDeadline(t *testing.T) {
	cityPath, recorder := installDeliveryWindowLeakHarness(t, errors.New("BACKLOG NOT DRAINED: hq"), nil)
	layout, err := resolveManagedDoltRuntimeLayout(cityPath)
	if err != nil {
		t.Fatalf("resolve layout: %v", err)
	}

	report, err := startManagedDoltProcessWithConfig(cityPath, "127.0.0.1", "17783", "root", "warning", -1, time.Second, true, nil, false)
	if err != nil {
		t.Fatalf("a failed window must not block the boot; got %v", err)
	}
	if !report.Ready {
		t.Fatalf("report.Ready = false, want true")
	}
	if !strings.Contains(report.DeliveryWindow.Err, "BACKLOG NOT DRAINED") {
		t.Errorf("delivery window Err = %q, want the drain's terminal record", report.DeliveryWindow.Err)
	}
	if got := readRenderedReadTimeout(t, layout.ConfigFile); got != leakTestConfiguredReadTimeout {
		t.Errorf("published config read_timeout_millis = %d, want %d after a failed window", got, leakTestConfiguredReadTimeout)
	}
	starts := recorder.snapshot()
	if len(starts) != 2 || starts[1].ConfigFile != layout.ConfigFile {
		t.Errorf("dolt starts = %v, want the published start from %s after a failed drain", starts, layout.ConfigFile)
	}
	if starts[0].PublishedExists && starts[0].PublishedReadTimout == leakTestWindowReadTimeout {
		t.Errorf("a failing window still leaked %d into the published config", starts[0].PublishedReadTimout)
	}
}

// Same constraint on the budget-expiry path: the window never reaches its
// drain, and the published server still comes up at the configured deadline.
func TestDeliveryWindowBudgetExpiryStillPublishesAtConfiguredDeadline(t *testing.T) {
	cityPath, recorder := installDeliveryWindowLeakHarness(t, nil, nil)
	layout, err := resolveManagedDoltRuntimeLayout(cityPath)
	if err != nil {
		t.Fatalf("resolve layout: %v", err)
	}
	// A clock that jumps a full hour after the window's start stamp, so the
	// pre-drain deadline check is already past. No sleeping.
	prevNow := deliveryWindowNowFn
	t.Cleanup(func() { deliveryWindowNowFn = prevNow })
	calls := 0
	base := time.Unix(1_700_000_000, 0)
	deliveryWindowNowFn = func() time.Time {
		calls++
		if calls <= 1 {
			return base
		}
		return base.Add(time.Hour)
	}
	t.Setenv("GC_DOLT_DELIVERY_WINDOW_BUDGET", "60")

	report, err := startManagedDoltProcessWithConfig(cityPath, "127.0.0.1", "17784", "root", "warning", -1, time.Second, true, nil, false)
	if err != nil {
		t.Fatalf("an exhausted window must not block the boot; got %v", err)
	}
	if !strings.Contains(report.DeliveryWindow.Err, "budget") {
		t.Errorf("delivery window Err = %q, want the budget-exhausted record", report.DeliveryWindow.Err)
	}
	if got := readRenderedReadTimeout(t, layout.ConfigFile); got != leakTestConfiguredReadTimeout {
		t.Errorf("published config read_timeout_millis = %d, want %d after an exhausted window", got, leakTestConfiguredReadTimeout)
	}
	if starts := recorder.configFiles(); len(starts) != 2 || starts[1] != layout.ConfigFile {
		t.Errorf("dolt starts = %v, want the published start from %s", starts, layout.ConfigFile)
	}
}

// A window whose STOP fails is a window failure (the raised-deadline server
// is left bound), and it must be recorded loudly rather than swallowed —
// AC3. This harness's data dir lock is always free, so the outer start
// proceeds here where production's lock gate would refuse; what the test
// pins is that the failure reaches the outcome record and that nothing
// writes the window's deadline into the published config on the way.
func TestDeliveryWindowFailedStopIsRecordedAndDoesNotLeak(t *testing.T) {
	cityPath, recorder := installDeliveryWindowLeakHarness(t, nil, errors.New("pid 4242 still alive after forced stop"))
	layout, err := resolveManagedDoltRuntimeLayout(cityPath)
	if err != nil {
		t.Fatalf("resolve layout: %v", err)
	}

	report, err := startManagedDoltProcessWithConfig(cityPath, "127.0.0.1", "17786", "root", "warning", -1, time.Second, true, nil, false)
	if err != nil {
		t.Fatalf("start failed: %v", err)
	}
	if !strings.Contains(report.DeliveryWindow.Err, "window server stop failed") {
		t.Errorf("delivery window Err = %q, want the stop failure recorded", report.DeliveryWindow.Err)
	}
	if got := readRenderedReadTimeout(t, layout.ConfigFile); got != leakTestConfiguredReadTimeout {
		t.Errorf("published config read_timeout_millis = %d, want %d after a failed window stop", got, leakTestConfiguredReadTimeout)
	}
	starts := recorder.snapshot()
	if len(starts) != 2 {
		t.Fatalf("dolt starts = %d, want 2", len(starts))
	}
	if starts[0].PublishedExists && starts[0].PublishedReadTimout == leakTestWindowReadTimeout {
		t.Errorf("a window with a failing stop leaked %d into the published config", starts[0].PublishedReadTimout)
	}
}

// A start with the window DISABLED never creates the window config at all,
// and still renders the configured deadline.
func TestDeliveryWindowDisabledLeavesOnlyThePublishedConfig(t *testing.T) {
	cityPath, recorder := installDeliveryWindowLeakHarness(t, nil, nil)
	t.Setenv("GC_DOLT_DELIVERY_WINDOW", "0")
	layout, err := resolveManagedDoltRuntimeLayout(cityPath)
	if err != nil {
		t.Fatalf("resolve layout: %v", err)
	}

	if _, err := startManagedDoltProcessWithConfig(cityPath, "127.0.0.1", "17785", "root", "warning", -1, time.Second, true, nil, false); err != nil {
		t.Fatalf("start failed: %v", err)
	}
	if got := readRenderedReadTimeout(t, layout.ConfigFile); got != leakTestConfiguredReadTimeout {
		t.Errorf("published config read_timeout_millis = %d, want %d", got, leakTestConfiguredReadTimeout)
	}
	if _, err := os.Stat(managedDoltDeliveryWindowConfigFile(layout)); !os.IsNotExist(err) {
		t.Errorf("window config exists with the window disabled: %v", err)
	}
	if starts := recorder.configFiles(); len(starts) != 1 {
		t.Errorf("dolt starts = %v, want exactly the published start", starts)
	}
}

func TestManagedDoltDeliveryWindowConfigFileIsAPublishedSibling(t *testing.T) {
	layout := managedDoltRuntimeLayout{ConfigFile: filepath.Join("/city", ".gc", "runtime", "packs", "dolt", "dolt-config.yaml")}
	want := filepath.Join("/city", ".gc", "runtime", "packs", "dolt", "dolt-config.window.yaml")
	if got := managedDoltDeliveryWindowConfigFile(layout); got != want {
		t.Errorf("window config = %s, want %s", got, want)
	}
}

// The window server keeps a DIFFERENT --config, so the ownership check that
// gates every stop and reap must still claim it. Without this the window
// server reads as "another project's dolt" and nothing can stop it.
func TestWindowConfigProcessStaysOwned(t *testing.T) {
	layout := managedDoltRuntimeLayout{
		ConfigFile: "/city/.gc/runtime/packs/dolt/dolt-config.yaml",
		DataDir:    "/city/.beads/dolt",
	}
	windowConfig := managedDoltDeliveryWindowConfigFile(layout)
	cases := []struct {
		name string
		args string
		want bool
	}{
		{"published-config-space", "dolt sql-server --config " + layout.ConfigFile, true},
		{"published-config-equals", "dolt sql-server --config=" + layout.ConfigFile, true},
		{"window-config-space", "dolt sql-server --config " + windowConfig, true},
		{"window-config-equals", "dolt sql-server --config=" + windowConfig, true},
		{"foreign-config", "dolt sql-server --config /other/project/dolt-config.yaml", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := managedDoltProcessArgsNameOwnedConfig(tc.args, layout); got != tc.want {
				t.Errorf("owned(%q) = %v, want %v", tc.args, got, tc.want)
			}
		})
	}
}

// The reclaim guard: ensureBeadsProvider's "the start errored but the health
// probe passes, so publish what is live" recovery must not publish a window
// server. ADR-0064 constraint 2 forbids simply refusing, so the stranded
// window server is stopped instead and the caller starts again. This is the
// path CI exercised for real: a provider start that hit its context deadline
// while the window server still held the managed port.
func TestReclaimStrandedDeliveryWindowServer(t *testing.T) {
	prevPort, prevPID, prevStop := managedDoltResolvablePortFn, deliveryWindowServerPIDFn, deliveryWindowReclaimStopFn
	t.Cleanup(func() {
		managedDoltResolvablePortFn, deliveryWindowServerPIDFn, deliveryWindowReclaimStopFn = prevPort, prevPID, prevStop
	})

	var stopped []string
	managedDoltResolvablePortFn = func(string) string { return "48770" }
	deliveryWindowServerPIDFn = func(string, string) int { return 4242 }
	deliveryWindowReclaimStopFn = func(_, port string) error {
		stopped = append(stopped, port)
		return nil
	}
	var stderr bytes.Buffer
	reclaimed, err := reclaimStrandedDeliveryWindowServer("/city", &stderr)
	if err != nil {
		t.Fatalf("reclaim = %v, want nil", err)
	}
	if !reclaimed {
		t.Error("a live window server on the managed port must be reclaimed")
	}
	if len(stopped) != 1 || stopped[0] != "48770" {
		t.Errorf("stop calls = %v, want one on 48770", stopped)
	}
	for _, want := range []string{"DELIVERY WINDOW STRANDED", "4242", "48770"} {
		if !strings.Contains(stderr.String(), want) {
			t.Errorf("stderr %q missing %q", stderr.String(), want)
		}
	}

	// An ordinary published server is left alone.
	stopped = nil
	deliveryWindowServerPIDFn = func(string, string) int { return 0 }
	reclaimed, err = reclaimStrandedDeliveryWindowServer("/city", &stderr)
	if err != nil || reclaimed {
		t.Errorf("reclaim = (%v, %v), want (false, nil) for an ordinary server", reclaimed, err)
	}
	if len(stopped) != 0 {
		t.Errorf("stop calls = %v, want none", stopped)
	}

	// No resolvable managed port: nothing to reclaim.
	managedDoltResolvablePortFn = func(string) string { return "" }
	deliveryWindowServerPIDFn = func(string, string) int { return 4242 }
	if reclaimed, err := reclaimStrandedDeliveryWindowServer("/city", &stderr); reclaimed || err != nil {
		t.Errorf("reclaim = (%v, %v), want (false, nil) with no resolvable port", reclaimed, err)
	}

	// A stop that fails is reported, and still counts as "there was one".
	managedDoltResolvablePortFn = func(string) string { return "48770" }
	deliveryWindowReclaimStopFn = func(string, string) error { return errors.New("pid 4242 still alive after forced stop") }
	reclaimed, err = reclaimStrandedDeliveryWindowServer("/city", &stderr)
	if !reclaimed || err == nil {
		t.Fatalf("reclaim = (%v, %v), want (true, error) when the stop fails", reclaimed, err)
	}
	if !strings.Contains(err.Error(), "stranded delivery window server") {
		t.Errorf("reclaim error = %q, want it to name the stranded window server", err.Error())
	}
}

// managedDoltDeliveryWindowServerPID identifies the window server by the
// config its argv names, and only that.
func TestManagedDoltDeliveryWindowServerPIDMatchesOnWindowConfig(t *testing.T) {
	cityPath := t.TempDir()
	packStateDir := filepath.Join(cityPath, "pack-state")
	if err := os.MkdirAll(packStateDir, 0o755); err != nil {
		t.Fatalf("mkdir pack state dir: %v", err)
	}
	t.Setenv("GC_PACK_STATE_DIR", packStateDir)
	layout, err := resolveManagedDoltRuntimeLayout(cityPath)
	if err != nil {
		t.Fatalf("resolve layout: %v", err)
	}

	prevInspect, prevArgs := deliveryWindowInspectFn, deliveryWindowProcessArgsFn
	t.Cleanup(func() { deliveryWindowInspectFn, deliveryWindowProcessArgsFn = prevInspect, prevArgs })
	deliveryWindowInspectFn = func(string, string) (managedDoltProcessInspection, error) {
		return managedDoltProcessInspection{ManagedPID: 0, PortHolderPID: 5150}, nil
	}

	deliveryWindowProcessArgsFn = func(int) (string, error) {
		return "dolt sql-server --config " + managedDoltDeliveryWindowConfigFile(layout), nil
	}
	if got := managedDoltDeliveryWindowServerPID(cityPath, "48770"); got != 5150 {
		t.Errorf("window server pid = %d, want 5150", got)
	}

	deliveryWindowProcessArgsFn = func(int) (string, error) {
		return "dolt sql-server --config " + layout.ConfigFile, nil
	}
	if got := managedDoltDeliveryWindowServerPID(cityPath, "48770"); got != 0 {
		t.Errorf("published server reported as a window server (pid %d)", got)
	}
}

// The caller wiring for the reclaim, end to end through ensureBeadsProvider.
// This is the shape CI hit on da73e034: the managed start burned its context
// deadline on a cold drain, the window server was still holding the managed
// port, and the health probe therefore passed. The publish guard turned that
// into a hard boot failure, which ADR-0064 constraint 2 forbids. What must
// happen instead is stop the window server, start again, and publish the
// server that second start produced.
func TestEnsureBeadsProviderReclaimsStrandedWindowAndStartsAgain(t *testing.T) {
	dir := t.TempDir()
	script := gcBeadsBdScriptPath(dir)
	callLog := filepath.Join(dir, "provider.log")
	marker := filepath.Join(dir, "started")
	attempts := filepath.Join(dir, "attempts")
	port := reserveRandomTCPPort(t)
	listener := startTCPListenerProcess(t, port)
	if err := writeDoltRuntimeStateFile(providerManagedDoltStatePath(dir), doltRuntimeState{
		Running:   true,
		PID:       listener.Process.Pid,
		Port:      port,
		DataDir:   filepath.Join(dir, ".beads", "dolt"),
		StartedAt: time.Now().UTC().Format(time.RFC3339),
	}); err != nil {
		t.Fatalf("write provider state: %v", err)
	}
	if err := os.MkdirAll(filepath.Dir(script), 0o755); err != nil {
		t.Fatal(err)
	}
	// The first start fails the way the provider does when its context
	// deadline expires under a window server; the second one succeeds.
	content := "#!/bin/sh\n" +
		"set -eu\n" +
		"echo \"$1\" >> \"" + callLog + "\"\n" +
		"case \"${1:-}\" in\n" +
		"  start)\n" +
		"    : > \"" + marker + "\"\n" +
		"    echo x >> \"" + attempts + "\"\n" +
		"    if [ \"$(wc -l < \"" + attempts + "\")\" -le 1 ]; then\n" +
		"      echo 'context deadline exceeded' >&2\n" +
		"      exit 1\n" +
		"    fi\n" +
		"    exit 0\n" +
		"    ;;\n" +
		"  health)\n" +
		"    [ -f \"" + marker + "\" ]\n" +
		"    ;;\n" +
		"  *)\n" +
		"    exit 0\n" +
		"    ;;\n" +
		"esac\n"
	if err := os.WriteFile(script, []byte(content), 0o755); err != nil {
		t.Fatal(err)
	}
	setScopedBeadsProviderForTest(t, dir, "bd")

	prevPort, prevPID, prevStop := managedDoltResolvablePortFn, deliveryWindowServerPIDFn, deliveryWindowReclaimStopFn
	t.Cleanup(func() {
		managedDoltResolvablePortFn, deliveryWindowServerPIDFn, deliveryWindowReclaimStopFn = prevPort, prevPID, prevStop
	})
	var stopped int
	managedDoltResolvablePortFn = func(string) string { return strconv.Itoa(port) }
	deliveryWindowServerPIDFn = func(string, string) int { return 4242 }
	deliveryWindowReclaimStopFn = func(string, string) error {
		stopped++
		return nil
	}

	if err := ensureBeadsProvider(dir); err != nil {
		t.Fatalf("ensureBeadsProvider = %v, want nil — a stranded window must not block the boot (ADR-0064 constraint 2)", err)
	}
	if stopped != 1 {
		t.Errorf("reclaim stops = %d, want 1", stopped)
	}
	data, err := os.ReadFile(callLog)
	if err != nil {
		t.Fatalf("read call log: %v", err)
	}
	got := strings.Join(strings.Fields(string(data)), ",")
	if want := "start,health,start"; got != want {
		t.Errorf("provider calls = %s, want %s", got, want)
	}
}

// The ordinary racing start is untouched: no window server on the port means
// no reclaim, no second start, and the live server is published as before.
func TestEnsureBeadsProviderLeavesAnOrdinaryRacingStartAlone(t *testing.T) {
	dir := t.TempDir()
	script := gcBeadsBdScriptPath(dir)
	callLog := filepath.Join(dir, "provider.log")
	marker := filepath.Join(dir, "started")
	port := reserveRandomTCPPort(t)
	listener := startTCPListenerProcess(t, port)
	if err := writeDoltRuntimeStateFile(providerManagedDoltStatePath(dir), doltRuntimeState{
		Running:   true,
		PID:       listener.Process.Pid,
		Port:      port,
		DataDir:   filepath.Join(dir, ".beads", "dolt"),
		StartedAt: time.Now().UTC().Format(time.RFC3339),
	}); err != nil {
		t.Fatalf("write provider state: %v", err)
	}
	if err := os.MkdirAll(filepath.Dir(script), 0o755); err != nil {
		t.Fatal(err)
	}
	content := "#!/bin/sh\n" +
		"set -eu\n" +
		"echo \"$1\" >> \"" + callLog + "\"\n" +
		"case \"${1:-}\" in\n" +
		"  start)\n" +
		"    : > \"" + marker + "\"\n" +
		"    echo 'signal: terminated' >&2\n" +
		"    exit 1\n" +
		"    ;;\n" +
		"  health)\n" +
		"    [ -f \"" + marker + "\" ]\n" +
		"    ;;\n" +
		"  *)\n" +
		"    exit 0\n" +
		"    ;;\n" +
		"esac\n"
	if err := os.WriteFile(script, []byte(content), 0o755); err != nil {
		t.Fatal(err)
	}
	setScopedBeadsProviderForTest(t, dir, "bd")

	prevPort, prevPID, prevStop := managedDoltResolvablePortFn, deliveryWindowServerPIDFn, deliveryWindowReclaimStopFn
	t.Cleanup(func() {
		managedDoltResolvablePortFn, deliveryWindowServerPIDFn, deliveryWindowReclaimStopFn = prevPort, prevPID, prevStop
	})
	var stopped int
	managedDoltResolvablePortFn = func(string) string { return strconv.Itoa(port) }
	deliveryWindowServerPIDFn = func(string, string) int { return 0 }
	deliveryWindowReclaimStopFn = func(string, string) error {
		stopped++
		return nil
	}

	if err := ensureBeadsProvider(dir); err != nil {
		t.Fatalf("ensureBeadsProvider = %v, want nil", err)
	}
	if stopped != 0 {
		t.Errorf("reclaim stops = %d, want none for an ordinary racing start", stopped)
	}
	data, err := os.ReadFile(callLog)
	if err != nil {
		t.Fatalf("read call log: %v", err)
	}
	got := strings.Join(strings.Fields(string(data)), ",")
	if want := "start,health"; got != want {
		t.Errorf("provider calls = %s, want %s", got, want)
	}
}

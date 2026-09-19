package main

import (
	"errors"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"
)

// ownManagedDoltLifecycleForTest sets whether this test process owns the
// managed Dolt lifecycle, restoring the previous mark on cleanup. The mark is
// process-global, so a test that depends on it must set it explicitly rather
// than inherit whatever an earlier test left behind.
func ownManagedDoltLifecycleForTest(t *testing.T, owner bool) {
	t.Helper()
	prev := managedDoltLifecycleOwner.Load()
	managedDoltLifecycleOwner.Store(owner)
	t.Cleanup(func() { managedDoltLifecycleOwner.Store(prev) })
}

// TestNonOwnerBdRunnerNeverRecoversManagedDolt is the ga-fjr5f regression on
// the bd runner: a process that does not own the managed Dolt lifecycle — an
// agent's `gc prime --hook` above all — must report a lost server, never
// restart it. The restart would run in the agent session and die with it.
func TestNonOwnerBdRunnerNeverRecoversManagedDolt(t *testing.T) {
	for _, tc := range []struct {
		name         string
		owner        bool
		wantRecovers int
	}{
		{"non-owner reports unavailable", false, 0},
		{"owner recovers", true, 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ownManagedDoltLifecycleForTest(t, tc.owner)
			t.Setenv("GC_BEADS", "bd")
			cityPath := writeBreakerTestCity(t, "")
			installFakeBdExec(t, func(_, _ string, _ ...string) ([]byte, error) {
				return nil, errors.New("dial tcp 127.0.0.1:3307: connection refused")
			})
			recovers := 0
			recoverManagedBDCommand = func(string) error { recovers++; return nil }

			_, err := breakerTestRunner(cityPath)(t.TempDir(), "bd", "list", "--json")
			if err == nil {
				t.Fatal("err = nil, want the transport failure")
			}
			if recovers != tc.wantRecovers {
				t.Fatalf("recover ran %d time(s), want %d", recovers, tc.wantRecovers)
			}
			if got := errors.Is(err, errManagedDoltLifecycleNotOwned); got != !tc.owner {
				t.Fatalf("errors.Is(err, errManagedDoltLifecycleNotOwned) = %v, want %v (err: %v)", got, !tc.owner, err)
			}
		})
	}
}

// TestNonOwnerHealthCheckNeverRecoversManagedDolt is the ga-fjr5f regression
// on the other implicit route: the bd env resolution's health check, which
// every gc process runs before a bd call and which ran the provider "recover"
// op on an unhealthy managed server.
func TestNonOwnerHealthCheckNeverRecoversManagedDolt(t *testing.T) {
	for _, tc := range []struct {
		name         string
		owner        bool
		wantRecovers int
	}{
		{"non-owner reports unavailable", false, 0},
		{"owner recovers", true, 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ownManagedDoltLifecycleForTest(t, tc.owner)
			cityPath := t.TempDir()
			writeMinimalCityToml(t, cityPath)
			opsFile := writeBreakerAwarePreflightFakes(t, cityPath, "unhealthy")
			cityKey := normalizePathForCompare(cityPath)
			t.Cleanup(func() { lastBeadsProviderRecover.Delete(cityKey) })

			err := healthBeadsProvider(cityPath)

			ops, readErr := os.ReadFile(opsFile)
			if readErr != nil {
				t.Fatalf("read provider ops: %v", readErr)
			}
			got := strings.Fields(strings.TrimSpace(string(ops)))
			if _, r := countOps(got, "health", "recover"); r != tc.wantRecovers {
				t.Fatalf("provider ops = %v; want recover == %d", got, tc.wantRecovers)
			}
			if !tc.owner && !errors.Is(err, errManagedDoltLifecycleNotOwned) {
				t.Fatalf("non-owner err = %v, want errManagedDoltLifecycleNotOwned", err)
			}
		})
	}
}

// TestControllerEntryPointsClaimTheManagedDoltLifecycle pins that the
// processes that must keep recovering the server — the supervisor and a
// controller's city runtime — claim ownership before they read any store.
// Without the claim, the gate above would leave a down server down for good.
func TestControllerEntryPointsClaimTheManagedDoltLifecycle(t *testing.T) {
	for _, tc := range []struct{ file, fn string }{
		{"cmd_supervisor.go", "func runSupervisor(stdout, stderr io.Writer) int {"},
		{"city_runtime.go", "func (cr *CityRuntime) run(ctx context.Context) {"},
	} {
		raw, err := os.ReadFile(tc.file)
		if err != nil {
			t.Fatal(err)
		}
		src := string(raw)
		i := strings.Index(src, tc.fn)
		if i < 0 {
			t.Fatalf("%s: %q not found; update this test if the entry point moved", tc.file, tc.fn)
		}
		body := src[i+len(tc.fn):]
		if first := strings.TrimSpace(strings.SplitN(body, "\n", 3)[1]); first != "claimManagedDoltLifecycle()" {
			t.Fatalf("%s: first statement of %q is %q, want claimManagedDoltLifecycle()", tc.file, tc.fn, first)
		}
	}
}

// sustainManagedDoltUnresponsiveForTest records that the city's live managed
// server has already been unresponsive for longer than the grace, so the next
// recovery decision escalates. For tests about what happens once recovery
// runs, not about when it may.
func sustainManagedDoltUnresponsiveForTest(t *testing.T, cityPath string) {
	t.Helper()
	key := normalizePathForCompare(cityPath)
	now := managedDoltRecoveryNow()
	managedDoltUnresponsiveMu.Lock()
	managedDoltUnresponsiveEpisodes[key] = managedDoltUnresponsiveEpisode{
		first: now.Add(-managedDoltLiveUnresponsiveGrace - time.Second),
		last:  now.Add(-time.Second),
	}
	managedDoltUnresponsiveMu.Unlock()
	t.Cleanup(func() {
		managedDoltUnresponsiveMu.Lock()
		delete(managedDoltUnresponsiveEpisodes, key)
		managedDoltUnresponsiveMu.Unlock()
	})
}

// fakeManagedDoltClockForTest pins the recovery decision's clock and reports
// the city's managed server as alive (or not), returning a function that
// advances the clock.
func fakeManagedDoltClockForTest(t *testing.T, alive *bool) func(time.Duration) {
	t.Helper()
	now := time.Date(2026, 9, 19, 2, 0, 0, 0, time.UTC)
	prevNow, prevAlive := managedDoltRecoveryNow, managedDoltServerAliveFn
	managedDoltRecoveryNow = func() time.Time { return now }
	managedDoltServerAliveFn = func(string) bool { return *alive }
	t.Cleanup(func() { managedDoltRecoveryNow, managedDoltServerAliveFn = prevNow, prevAlive })
	return func(d time.Duration) { now = now.Add(d) }
}

// TestOwnerNeverReplacesALiveButSlowManagedDolt pins the ga-fjr5f trigger:
// under host saturation a live managed server (process alive, port open)
// answered slowly, a caller decided it was down, and a new one replaced it.
// Even the lifecycle owner waits out managedDoltLiveUnresponsiveGrace before
// replacing a live server, and replaces a dead one at once.
func TestOwnerNeverReplacesALiveButSlowManagedDolt(t *testing.T) {
	ownManagedDoltLifecycleForTest(t, true)
	cityPath := t.TempDir()
	t.Cleanup(func() {
		managedDoltUnresponsiveMu.Lock()
		delete(managedDoltUnresponsiveEpisodes, normalizePathForCompare(cityPath))
		managedDoltUnresponsiveMu.Unlock()
	})
	alive := true
	advance := fakeManagedDoltClockForTest(t, &alive)

	step := managedDoltLiveUnresponsiveGrace / 4
	for i := 0; i < 3; i++ {
		if err := managedDoltImplicitRecoveryDecision(cityPath); !errors.Is(err, errManagedDoltAliveButUnresponsive) {
			t.Fatalf("observation %d inside the grace: err = %v, want errManagedDoltAliveButUnresponsive", i+1, err)
		}
		advance(step)
	}
	advance(step)
	if err := managedDoltImplicitRecoveryDecision(cityPath); err != nil {
		t.Fatalf("after a sustained %s the owner must escalate: err = %v", managedDoltLiveUnresponsiveGrace, err)
	}
	if err := managedDoltImplicitRecoveryDecision(cityPath); err != nil {
		t.Fatalf("a sustained episode must stay escalated for the recovery's own follow-up reads: err = %v", err)
	}

	// A lapse longer than the grace means the server answered in between:
	// the next slow answer starts a new episode rather than a replacement.
	advance(managedDoltLiveUnresponsiveGrace + time.Second)
	if err := managedDoltImplicitRecoveryDecision(cityPath); !errors.Is(err, errManagedDoltAliveButUnresponsive) {
		t.Fatalf("after a lapse the episode must restart: err = %v", err)
	}

	// A server that is gone is replaced at once.
	alive = false
	if err := managedDoltImplicitRecoveryDecision(cityPath); err != nil {
		t.Fatalf("a dead server must be recoverable at once: err = %v", err)
	}
}

// TestNonOwnerNeverReplacesManagedDoltAliveOrNot pins that the grace is no
// back door: a process that does not own the lifecycle never recovers.
func TestNonOwnerNeverReplacesManagedDoltAliveOrNot(t *testing.T) {
	ownManagedDoltLifecycleForTest(t, false)
	for _, isAlive := range []bool{true, false} {
		alive := isAlive
		advance := fakeManagedDoltClockForTest(t, &alive)
		advance(10 * managedDoltLiveUnresponsiveGrace)
		if err := managedDoltImplicitRecoveryDecision(t.TempDir()); !errors.Is(err, errManagedDoltLifecycleNotOwned) {
			t.Fatalf("alive=%v: err = %v, want errManagedDoltLifecycleNotOwned", isAlive, err)
		}
	}
}

// TestHealthCheckDoesNotReplaceALiveButSlowManagedDolt drives the same rule
// through the controller's health path: an unhealthy report about a server
// that is alive does not run the provider "recover" op inside the grace.
func TestHealthCheckDoesNotReplaceALiveButSlowManagedDolt(t *testing.T) {
	ownManagedDoltLifecycleForTest(t, true)
	cityPath := t.TempDir()
	writeMinimalCityToml(t, cityPath)
	opsFile := writeBreakerAwarePreflightFakes(t, cityPath, "unhealthy")
	t.Cleanup(func() {
		lastBeadsProviderRecover.Delete(normalizePathForCompare(cityPath))
		managedDoltUnresponsiveMu.Lock()
		delete(managedDoltUnresponsiveEpisodes, normalizePathForCompare(cityPath))
		managedDoltUnresponsiveMu.Unlock()
	})
	alive := true
	fakeManagedDoltClockForTest(t, &alive)

	err := healthBeadsProvider(cityPath)

	ops, readErr := os.ReadFile(opsFile)
	if readErr != nil {
		t.Fatalf("read provider ops: %v", readErr)
	}
	if _, r := countOps(strings.Fields(strings.TrimSpace(string(ops))), "health", "recover"); r != 0 {
		t.Fatalf("provider ops = %q; a live server was replaced on its first unhealthy report", ops)
	}
	if !errors.Is(err, errManagedDoltAliveButUnresponsive) {
		t.Fatalf("err = %v, want errManagedDoltAliveButUnresponsive", err)
	}
}

// TestManagedDoltNotOwnedErrorIsActionable pins that the refusal tells an
// operator what to do rather than only what went wrong.
func TestManagedDoltNotOwnedErrorIsActionable(t *testing.T) {
	msg := errManagedDoltLifecycleNotOwned.Error()
	for _, want := range []string{"`gc start`", "controller"} {
		if !strings.Contains(msg, want) {
			t.Fatalf("errManagedDoltLifecycleNotOwned = %q, want it to mention %s", msg, want)
		}
	}
}

// managedDoltIdentityStopCall matches every call that can stop or replace the
// city's managed Dolt server by identity (pid file, port, config): the stop
// and shutdown helpers, the recovery helpers (recovery stops the running
// server before starting a new one), and the provider stop/shutdown/recover
// ops.
var managedDoltIdentityStopCall = regexp.MustCompile(
	`\b(stopManagedDoltProcess|stopManagedDoltProcessWithOptions|shutdownBeadsProvider|shutdownBeadsProviderForStop|recoverManagedDoltProcess|recoverManagedBDCommand)\(|runProviderOp\w*\([^)]*"(stop|shutdown|recover)"`)

// managedDoltIdentityStopDefinition matches the definition line of one of the
// helpers above, which is not a call site. Only the definition is excluded: a
// one-line function whose body calls a helper is still counted.
var managedDoltIdentityStopDefinition = regexp.MustCompile(
	`^func (stopManagedDoltProcess|stopManagedDoltProcessWithOptions|shutdownBeadsProvider|shutdownBeadsProviderForStop|recoverManagedDoltProcess)\(`)

// TestManagedDoltIdentityStopCallSitesAreAnEnumeratedSet is the ga-fjr5f
// guarantee that no session lifecycle event — the tmux or worker stop, a
// drain-ack, a SessionEnd/Stop hook, `gc prime` — can stop the city's managed
// Dolt by identity. Every such call site is listed here with why it may. A new
// one fails this test until it is reviewed: a session path must not be added.
func TestManagedDoltIdentityStopCallSitesAreAnEnumeratedSet(t *testing.T) {
	want := map[string]int{
		"bd_env.go":                   2, // recoverManagedBDCommand: its definition, and the runner call gated by managedDoltImplicitRecoveryDecision
		"beads_provider_lifecycle.go": 2, // shutdownBeadsProvider's stop op; the health path's recover op, gated by managedDoltImplicitRecoveryDecision
		"cmd_beads_city.go":           1, // `gc beads city` endpoint change: explicit operator command
		"cmd_dolt_state.go":           2, // `gc dolt-state stop-managed` / `recover-managed`: explicit, run by the provider script
		"cmd_stop.go":                 3, // `gc stop`: explicit city shutdown
		"cmd_supervisor.go":           3, // the supervisor stopping a city, or cleaning up a failed start
		"dolt_delivery_window.go":     2, // the controller's own start-up delivery window
		"dolt_recover_managed.go":     1, // recoverManagedDoltProcess's stop step, reached only through the gated paths above
		"dolt_stop_managed.go":        1, // stopManagedDoltProcess itself
	}
	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]int{}
	for _, name := range files {
		if strings.HasSuffix(name, "_test.go") {
			continue
		}
		raw, err := os.ReadFile(name)
		if err != nil {
			t.Fatal(err)
		}
		for _, line := range strings.Split(string(raw), "\n") {
			trimmed := strings.TrimSpace(line)
			if strings.HasPrefix(trimmed, "//") || managedDoltIdentityStopDefinition.MatchString(trimmed) {
				continue
			}
			if managedDoltIdentityStopCall.MatchString(line) {
				got[name]++
			}
		}
	}
	for name, n := range got {
		if want[name] != n {
			t.Errorf("%s: %d managed-dolt stop/recover call site(s), enumerated %d. A new call site "+
				"must not be reachable from a session lifecycle event (ga-fjr5f); review it and update this list.", name, n, want[name])
		}
	}
	for name, n := range want {
		if got[name] == 0 {
			t.Errorf("%s: enumerated %d call site(s) but found none; update this list", name, n)
		}
	}
}

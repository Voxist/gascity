package main

import (
	"errors"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/session"
)

// ownManagedDoltLifecycleForTest claims one city's managed Dolt lifecycle for
// this test process and releases it on cleanup. Claims are per city, so a test
// whose city is a fresh directory is unaffected by every other test; a test
// that wants the non-owner path simply does not call this.
func ownManagedDoltLifecycleForTest(t *testing.T, cityPath string) {
	t.Helper()
	claimManagedDoltLifecycle(cityPath)
	t.Cleanup(func() { managedDoltLifecycleClaims.Delete(normalizePathForCompare(cityPath)) })
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
			t.Setenv("GC_BEADS", "bd")
			cityPath := writeBreakerTestCity(t, "")
			if tc.owner {
				ownManagedDoltLifecycleForTest(t, cityPath)
			}
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
			cityPath := t.TempDir()
			if tc.owner {
				ownManagedDoltLifecycleForTest(t, cityPath)
			}
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

// TestControllerEntryPointsClaimTheirCitysManagedDoltLifecycle pins where a
// city's lifecycle is claimed (ga-fjr5f). The claim is per city, so each site
// must pass the city it brought up, and CityRuntime.run must claim only AFTER
// ownedCity: a runtime that failed init owns nothing, and in a multi-city
// supervisor the process must not become able to restart the server of a city
// it never brought up.
//
// This reads source text, so it is a tripwire on the three known sites rather
// than a proof that no other site claims: `gc start --dry-run` not claiming is
// pinned behaviourally in cmd_start_lifecycle_owner_test.go.
func TestControllerEntryPointsClaimTheirCitysManagedDoltLifecycle(t *testing.T) {
	for _, tc := range []struct{ file, claim string }{
		// The supervisor's per-city boot, which starts that city's bead store.
		{"cmd_supervisor.go", "claimManagedDoltLifecycle(cityPath)"},
		// The city's own runtime, after it takes ownership of the city.
		{"city_runtime.go", "claimManagedDoltLifecycle(cr.cityPath)"},
		// The explicit lifecycle command.
		{"cmd_start.go", "claimManagedDoltLifecycle(cityPath)"},
	} {
		raw, err := os.ReadFile(tc.file)
		if err != nil {
			t.Fatal(err)
		}
		src := string(raw)
		if !strings.Contains(src, tc.claim) {
			t.Errorf("%s: no %s; this city's managed Dolt could never be recovered by the "+
				"process that brought it up (ga-fjr5f)", tc.file, tc.claim)
		}
		if strings.Contains(src, "claimManagedDoltLifecycle()") {
			t.Errorf("%s: claims the lifecycle process-wide; the claim is per city", tc.file)
		}
	}

	raw, err := os.ReadFile("city_runtime.go")
	if err != nil {
		t.Fatal(err)
	}
	src := string(raw)
	owned := strings.Index(src, "cr.ownedCity.Store(true)")
	claim := strings.Index(src, "claimManagedDoltLifecycle(cr.cityPath)")
	if owned < 0 || claim < 0 || claim < owned {
		t.Fatalf("city_runtime.go: claim at %d must come after cr.ownedCity.Store(true) at %d; a runtime "+
			"that fails init must leave this process unable to restart that city's server", claim, owned)
	}
}

// TestManagedDoltNotOwnedErrorIsActionable pins that the refusal tells an
// operator what to do rather than only what went wrong.
func TestManagedDoltNotOwnedErrorIsActionable(t *testing.T) {
	msg := errManagedDoltLifecycleNotOwned.Error()
	for _, want := range []string{"`gc start`", "`gc supervisor status`", "controller"} {
		if !strings.Contains(msg, want) {
			t.Fatalf("errManagedDoltLifecycleNotOwned = %q, want it to mention %s", msg, want)
		}
	}
}

// managedDoltIdentityStopCall matches every call that can stop or replace the
// city's managed Dolt server by identity (pid file, port, config): the stop
// and shutdown helpers, the recovery helpers (recovery stops the running
// server before starting a new one), the PID terminators the watchdog and the
// start/cleanup paths use, and EVERY provider-op call.
//
// Provider ops are matched whatever their operation argument, not only a
// "stop"/"shutdown"/"recover" literal, because an op held in a variable
// (`op := "stop"; runProviderOp(..., op)`) defeats a literal match. The cost
// is that unrelated ops ("health", "probe", "ensure-ready") are counted too,
// so the expected counts move when those change; that is the price of closing
// the variable-op hole.
var managedDoltIdentityStopCall = regexp.MustCompile(
	`\b(stopManagedDoltProcess|stopManagedDoltProcessWithOptions|shutdownBeadsProvider|shutdownBeadsProviderForStop|recoverManagedDoltProcess|recoverManagedBDCommand|terminateManagedDoltPID|terminateManagedDoltPIDGuarded|terminateManagedDoltStartedProcess|terminateManagedDoltScopeWatchdogChild)\(|runProviderOp\w*\(`)

// managedDoltIdentityStopDefinition matches the definition line of one of the
// helpers above, which is not a call site. Only the definition is excluded: a
// one-line function whose body calls a helper is still counted.
var managedDoltIdentityStopDefinition = regexp.MustCompile(
	`^func (\([^)]*\) )?(stopManagedDoltProcess|stopManagedDoltProcessWithOptions|shutdownBeadsProvider|shutdownBeadsProviderForStop|recoverManagedDoltProcess|terminateManagedDoltPID|terminateManagedDoltPIDGuarded|terminateManagedDoltStartedProcess|terminateManagedDoltScopeWatchdogChild|runProviderOp\w*)\(`)

// TestManagedDoltStopCallSiteTripwire is a REVIEW TRIPWIRE, not a guarantee.
//
// What it does guarantee: no file outside this list gains a call that can stop
// or replace the city's managed Dolt server by identity. That is the ga-fjr5f
// property that matters — no session lifecycle event (the tmux or worker stop,
// a drain-ack, a SessionEnd/Stop hook, `gc prime`) may reach one. `gc doctor`
// is absent on purpose: its checks only open a store and Ping, so it reaches
// managed Dolt through the gated env-resolution route and calls none of these
// helpers itself. The session path that DID reach managed Dolt was not a call
// at all — it was the runtime's orphan sweep matching a session's
// GC_SESSION_ID in the server's environment, which doltServerEnv now strips.
//
// What it cannot catch, because it counts per file rather than resolving the
// call graph:
//   - substituting one enumerated call for another INSIDE an already-listed
//     file (the count is unchanged), and
//   - a listed file growing a call in a newly session-reachable function.
//
// Both are review's job; this test only makes the diff impossible to miss.
func TestManagedDoltStopCallSiteTripwire(t *testing.T) {
	want := map[string]int{
		"bd_env.go":                   2,  // recoverManagedBDCommand: its definition's provider op, and the runner call gated by managedDoltImplicitRecoveryDecision
		"beads_provider_lifecycle.go": 9,  // the provider lifecycle itself: start/health/ensure-ready/stop/shutdown, plus the health path's recover op, gated
		"cmd_beads_city.go":           1,  // `gc beads city` endpoint change: explicit operator command
		"cmd_dolt_state.go":           2,  // `gc dolt-state stop-managed` / `recover-managed`: explicit, run by the provider script
		"cmd_stop.go":                 3,  // `gc stop`: explicit city shutdown
		"cmd_supervisor.go":           3,  // the supervisor stopping a city, or cleaning up a failed start
		"dolt_delivery_window.go":     2,  // the controller's own start-up delivery window
		"dolt_recover_managed.go":     2,  // recoverManagedDoltProcess's stop step and its failed-recovery cleanup, reached only through the gated paths above
		"dolt_scope_watchdog.go":      4,  // the scope watchdog reaping the server it supervises
		"dolt_start_managed.go":       13, // start-path cleanup, the test watchdog and the stop helpers
		"dolt_stop_managed.go":        1,  // stopManagedDoltProcess itself
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
			t.Errorf("%s: %d managed-dolt stop/recover/terminate call site(s), enumerated %d. A new call site "+
				"must not be reachable from a session lifecycle event (ga-fjr5f); review it and update this list.", name, n, want[name])
		}
	}
	for name, n := range want {
		if got[name] == 0 {
			t.Errorf("%s: enumerated %d call site(s) but found none; update this list", name, n)
		}
	}
}

// TestManagedDoltEnvCarriesNoSessionIdentity pins the source half of the
// ga-fjr5f teardown fix: whatever session environment the spawner runs in,
// the managed server's environment carries none of its per-incarnation keys,
// so the runtime's orphan sweep can never mistake the server for a session's
// leftover process.
func TestManagedDoltEnvCarriesNoSessionIdentity(t *testing.T) {
	parent := []string{"PATH=/usr/bin", "HOME=/home/x"}
	for key, value := range session.RuntimeEnvWithAlias("gc-deacon", "gastown__deacon", "deacon", 3, 1, "token") {
		parent = append(parent, key+"="+value)
	}
	env := doltServerEnv("", parent)
	for key := range session.RuntimeEnvWithAlias("", "", "", 0, 0, "") {
		for _, entry := range env {
			if strings.HasPrefix(entry, key+"=") {
				t.Fatalf("doltServerEnv kept the session key %q: %v", key, env)
			}
		}
	}
	for _, keep := range []string{"PATH=/usr/bin", "HOME=/home/x"} {
		found := false
		for _, entry := range env {
			found = found || entry == keep
		}
		if !found {
			t.Fatalf("doltServerEnv dropped %q, which is not session identity: %v", keep, env)
		}
	}
}

// TestNonOwnerBdFailureWithoutATargetCarriesTheLifecycleHint pins review
// finding F5: a bd call that a non-owner makes with no managed Dolt target —
// the env resolution could not restart the server — fails with the
// actionable refusal rather than bd's bare "no server" message. An owner, or
// a call that had a target, is left alone.
func TestNonOwnerBdFailureWithoutATargetCarriesTheLifecycleHint(t *testing.T) {
	cityPath := t.TempDir()
	writeMinimalCityToml(t, cityPath)
	bdErr := errors.New("bd list: dolt server unreachable")
	noTarget := map[string]string{"GC_DOLT_PORT": "", "BEADS_DOLT_SERVER_PORT": ""}
	withTarget := map[string]string{"GC_DOLT_PORT": "3307"}

	if owned, err := managedDoltLifecycleOwned(cityPath); err != nil || !owned {
		t.Fatalf("precondition: fixture city's dolt lifecycle is not gc-managed (owned=%v err=%v)", owned, err)
	}
	if err := withManagedDoltNotOwnedHint(cityPath, cityPath, noTarget, bdErr); !errors.Is(err, errManagedDoltLifecycleNotOwned) || !errors.Is(err, bdErr) {
		t.Fatalf("non-owner, no target: err = %v, want bd's error wrapped with errManagedDoltLifecycleNotOwned", err)
	}
	if err := withManagedDoltNotOwnedHint(cityPath, cityPath, withTarget, bdErr); errors.Is(err, errManagedDoltLifecycleNotOwned) {
		t.Fatalf("non-owner with a target: err = %v, want bd's error unchanged", err)
	}
	if err := withManagedDoltNotOwnedHint(cityPath, cityPath, noTarget, nil); err != nil {
		t.Fatalf("success: err = %v, want nil", err)
	}

	ownManagedDoltLifecycleForTest(t, cityPath)
	if err := withManagedDoltNotOwnedHint(cityPath, cityPath, noTarget, bdErr); errors.Is(err, errManagedDoltLifecycleNotOwned) {
		t.Fatalf("owner: err = %v, want bd's error unchanged", err)
	}
}

// TestNonOwnerBdRunnerWithoutATargetReportsTheLifecycleHint drives F5 through
// the real bd runner: env resolution left no target, bd failed, and the
// caller sees who restarts the server and what to run.
func TestNonOwnerBdRunnerWithoutATargetReportsTheLifecycleHint(t *testing.T) {
	t.Setenv("GC_BEADS", "bd")
	cityPath := writeBreakerTestCity(t, "")
	installFakeBdExec(t, func(_, _ string, _ ...string) ([]byte, error) {
		return nil, errors.New("Error: no dolt server is running for this database")
	})
	runner := bdCommandRunnerWithManagedRetry(cityPath, func(string) map[string]string { return map[string]string{} })

	_, err := runner(cityPath, "bd", "list", "--json")
	if !errors.Is(err, errManagedDoltLifecycleNotOwned) {
		t.Fatalf("err = %v, want the bd failure to carry errManagedDoltLifecycleNotOwned", err)
	}
}

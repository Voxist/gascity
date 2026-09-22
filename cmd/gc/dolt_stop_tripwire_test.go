package main

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/session"
)

// Managed-Dolt teardown safety (ga-fjr5f).
//
// The 2026-09-19 restart loop was not caused by WHO started the server. The
// runtime's orphan sweep (proctable.ScanBySessionID, via killExistingOrphans)
// kills every process whose ENVIRONMENT carries a session's GC_SESSION_ID and
// whose parent is outside that session, when the session is torn down or
// replaced. A managed dolt started from inside an agent session — a
// SessionStart hook, a `gc doctor` whose checks open a store, or a shell with
// the session's env — inherited that identity, so the sweep reaped it and the
// scope watchdog took dolt down with it.
//
// So the fix is about the server's environment and session, not its caller:
// doltServerEnv strips every per-incarnation key, and managed dolt and its
// watchdog start in their own session. Replacing a live-but-slow server is a
// different defect with a different owner (ga-amol9's choke-point guard).

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
// managed Dolt through the ordinary store path and calls none of these helpers
// itself — it was such a `gc doctor`, running inside the deacon's session at
// 01:54 on 2026-09-19, whose implicit recovery produced the watchdog the sweep
// killed at 02:01. The session path that DID reach managed Dolt was not a call
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
		"bd_env.go":                   2,  // recoverManagedBDCommand: the provider op inside its definition, and the bd runner's transport-recovery call
		"beads_provider_lifecycle.go": 9,  // the provider lifecycle itself: start/health/ensure-ready/stop/shutdown, plus the health path's recover op
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

// managedDoltServerSpawn matches an exec.Command that starts a managed Dolt
// server or the watchdog process that supervises one.
var managedDoltServerSpawn = regexp.MustCompile(`exec\.Command\((watchdogExecutable|"dolt", "sql-server")`)

// TestEveryManagedDoltSpawnStripsSessionIdentity is the structural half of the
// ga-fjr5f env proof: EVERY path that starts a managed Dolt server or its
// watchdog takes its environment from doltServerEnv, which is where the
// session keys are stripped.
//
// It exists because the end-to-end tests can only drive the production
// scope-watchdog path. The other spawns — the direct spawn when the scope
// watchdog is switched off, the test watchdog and its child — are proven here
// by construction instead: the spawn site and its `cmd.Env = doltServerEnv(...)`
// must sit in the same function. A new spawn that builds its own environment
// fails this test.
//
// Note what this does NOT cover: `gc dolt sync --drain`
// (dolt_delivery_window.go) passes os.Environ() through, deliberately. It is a
// short-lived drain command, not a server, so no orphan sweep outlives it.
func TestEveryManagedDoltSpawnStripsSessionIdentity(t *testing.T) {
	t.Parallel()

	// Every spawn site, and the function it must draw its environment in.
	want := map[string][]string{
		"dolt_start_managed.go": {
			"startManagedDoltSQLServer",                 // direct spawn (GC_DOLT_SCOPE_WATCHDOG=0)
			"startManagedDoltSQLServerWithTestWatchdog", // test watchdog re-exec
			"runManagedDoltTestWatchdog",                // that watchdog's own dolt child
		},
		"dolt_scope_watchdog.go": {
			"startManagedDoltSQLServerWithScopeWatchdog", // production watchdog re-exec
			"runManagedDoltScopeWatchdog",                // the watchdog's own dolt child
		},
	}
	fset := token.NewFileSet()
	found := 0
	for file, fns := range want {
		parsed, err := parser.ParseFile(fset, file, nil, 0)
		if err != nil {
			t.Fatalf("parse %s: %v", file, err)
		}
		bodies := map[string]string{}
		raw, err := os.ReadFile(file)
		if err != nil {
			t.Fatal(err)
		}
		src := string(raw)
		for _, decl := range parsed.Decls {
			fn, ok := decl.(*ast.FuncDecl)
			if !ok || fn.Body == nil {
				continue
			}
			bodies[fn.Name.Name] = src[fset.Position(fn.Body.Pos()).Offset:fset.Position(fn.Body.End()).Offset]
		}
		for _, name := range fns {
			body, ok := bodies[name]
			if !ok {
				t.Errorf("%s: %s not found; update this list if the spawn moved", file, name)
				continue
			}
			if !managedDoltServerSpawn.MatchString(body) {
				t.Errorf("%s: %s no longer spawns a managed Dolt server or watchdog; update this list", file, name)
				continue
			}
			found++
			if !strings.Contains(body, "cmd.Env = doltServerEnv(") {
				t.Errorf("%s: %s spawns a managed Dolt server or watchdog without doltServerEnv; it would "+
					"inherit the caller's session identity and a session teardown would reap it (ga-fjr5f)", file, name)
			}
		}
	}

	// Any spawn site outside the list is unreviewed.
	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	spawns := 0
	for _, name := range files {
		if strings.HasSuffix(name, "_test.go") {
			continue
		}
		raw, err := os.ReadFile(name)
		if err != nil {
			t.Fatal(err)
		}
		for _, line := range strings.Split(string(raw), "\n") {
			if !strings.HasPrefix(strings.TrimSpace(line), "//") && managedDoltServerSpawn.MatchString(line) {
				spawns++
			}
		}
	}
	if spawns != found {
		t.Errorf("found %d managed Dolt spawn site(s) in cmd/gc but %d are enumerated; a new spawn must "+
			"draw its environment from doltServerEnv (ga-fjr5f)", spawns, found)
	}
}

package main

import (
	"errors"
	"go/ast"
	"go/parser"
	"go/token"
	"net"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

type gateSeams struct {
	liveness   controllerLiveness
	frozen     bool
	registered bool
	supervisor bool
	logs       int
}

func installGateSeams(t *testing.T, s *gateSeams) {
	t.Helper()
	resetLifecycleIntentForTest(t)
	oa, of, or, prevSupervisorAlive, ol := implicitRecoveryControllerLiveness, implicitRecoveryMigrationFrozen, implicitRecoveryRegistered, implicitRecoverySupervisorAlive, logImplicitRecoveryDenial
	implicitRecoveryControllerLiveness = func(string) controllerLiveness { return s.liveness }
	implicitRecoverySupervisorAlive = func() bool { return s.supervisor }
	implicitRecoveryMigrationFrozen = func(string) bool { return s.frozen }
	implicitRecoveryRegistered = func(string) bool { return s.registered }
	logImplicitRecoveryDenial = func(_, _, _ string) { s.logs++ }
	implicitRecoveryDenialMu.Lock()
	implicitRecoveryDenialLogged = map[string]time.Time{}
	implicitRecoveryDenialMu.Unlock()
	t.Cleanup(func() {
		implicitRecoveryControllerLiveness, implicitRecoveryMigrationFrozen, implicitRecoveryRegistered, implicitRecoverySupervisorAlive, logImplicitRecoveryDenial = oa, of, or, prevSupervisorAlive, ol
	})
}

func TestImplicitManagedDoltRecoveryCheck(t *testing.T) {
	tests := []struct {
		name       string
		seams      gateSeams
		intent     bool
		wantDenied bool
		wantText   string
	}{
		{name: "controller stopped denied unregistered", seams: gateSeams{liveness: controllerLivenessStopped}, wantDenied: true, wantText: "gc register /city && gc start"},
		{name: "controller stopped denied registered", seams: gateSeams{liveness: controllerLivenessStopped, registered: true}, wantDenied: true, wantText: "run gc start"},
		{name: "controller running permitted", seams: gateSeams{liveness: controllerLivenessRunning}},
		{name: "controller unknown permitted", seams: gateSeams{liveness: controllerLivenessUnknown}},
		{name: "stopped but registered with live supervisor permitted", seams: gateSeams{liveness: controllerLivenessStopped, registered: true, supervisor: true}},
		{name: "stopped unregistered with live supervisor denied", seams: gateSeams{liveness: controllerLivenessStopped, supervisor: true}, wantDenied: true, wantText: "city is stopped"},
		{name: "lifecycle intent permitted", seams: gateSeams{liveness: controllerLivenessStopped}, intent: true},
		{name: "frozen denied even with controller", seams: gateSeams{liveness: controllerLivenessRunning, frozen: true}, wantDenied: true, wantText: "MIGRATION-FREEZE"},
		{name: "frozen denied even with intent", seams: gateSeams{liveness: controllerLivenessStopped, frozen: true}, intent: true, wantDenied: true, wantText: "MIGRATION-FREEZE"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			s := tc.seams
			installGateSeams(t, &s)
			if tc.intent {
				declareLifecycleIntent()
			}
			err := implicitManagedDoltRecoveryCheck("/city")
			if !tc.wantDenied {
				if err != nil {
					t.Fatalf("err = %v, want nil", err)
				}
				return
			}
			var denied *implicitRecoveryDeniedError
			if !errors.As(err, &denied) {
				t.Fatalf("err = %v, want *implicitRecoveryDeniedError", err)
			}
			if !strings.Contains(err.Error(), tc.wantText) {
				t.Fatalf("err = %q, want it to contain %q", err, tc.wantText)
			}
		})
	}
}

func TestImplicitRecoveryDenialLoggedOncePerCityAndReason(t *testing.T) {
	s := &gateSeams{liveness: controllerLivenessStopped}
	installGateSeams(t, s)
	for i := 0; i < 5; i++ {
		_ = implicitManagedDoltRecoveryCheck("/city")
	}
	if s.logs != 1 {
		t.Fatalf("logs = %d, want 1", s.logs)
	}
	_ = implicitManagedDoltRecoveryCheck("/other")
	if s.logs != 2 {
		t.Fatalf("logs = %d after a second city, want 2", s.logs)
	}
}

// The denied recover must not reach the provider script: a spy script is
// installed where the runner would execute it.
func TestRecoverManagedBDCommandDeniedDoesNotRunProviderScript(t *testing.T) {
	s := &gateSeams{liveness: controllerLivenessStopped}
	installGateSeams(t, s)
	city := t.TempDir()
	marker := filepath.Join(t.TempDir(), "ran")
	script := gcBeadsBdScriptPath(city)
	if err := os.MkdirAll(filepath.Dir(script), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(script, []byte("#!/bin/sh\ntouch "+marker+"\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	err := recoverManagedBDCommand(city)
	if !errors.Is(err, errManagedDoltRecoverDeclined) {
		t.Fatalf("err = %v, want errManagedDoltRecoverDeclined", err)
	}
	var denied *implicitRecoveryDeniedError
	if !errors.As(err, &denied) {
		t.Fatalf("err = %v, want it to wrap the denial", err)
	}
	if _, statErr := os.Stat(marker); statErr == nil {
		t.Fatal("provider script was invoked on a denied recovery")
	}
}

func TestResolvedRuntimeCityDoltTargetDeniedForStoppedCity(t *testing.T) {
	s := &gateSeams{liveness: controllerLivenessStopped}
	installGateSeams(t, s)
	configureIsolatedRuntimeEnv(t)
	cityPath := t.TempDir()
	if err := os.WriteFile(filepath.Join(cityPath, "city.toml"), []byte("[workspace]\nname = \"demo\"\n\n[beads]\nprovider = \"file\"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(cityPath, ".beads"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cityPath, ".beads", "config.yaml"), []byte("issue_prefix: demo\ngc.endpoint_origin: managed_city\ngc.endpoint_status: verified\ndolt.auto-start: false\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	writeReachableProviderManagedDoltState(t, cityPath)

	_, ok, err := resolvedRuntimeCityDoltTarget(cityPath, true)
	var denied *implicitRecoveryDeniedError
	if ok || !errors.As(err, &denied) {
		t.Fatalf("resolvedRuntimeCityDoltTarget() ok=%v err=%v, want the stopped-city denial", ok, err)
	}

	declareLifecycleIntent()
	_, _, err = resolvedRuntimeCityDoltTarget(cityPath, true)
	if errors.As(err, &denied) {
		t.Fatalf("with lifecycle intent the denial must not apply, got %v", err)
	}
}

func TestExplicitLifecycleEntryPointsDeclareIntent(t *testing.T) {
	city := t.TempDir()
	resetLifecycleIntentForTest(t)
	if lifecycleIntent.Load() {
		t.Fatal("intent set before any lifecycle call")
	}
	_ = healthBeadsProvider(city)
	if !lifecycleIntent.Load() {
		t.Fatal("healthBeadsProvider (gc beads health, gc start) did not declare lifecycle intent")
	}
}

func TestProbeControllerLivenessWithNoSocketIsStopped(t *testing.T) {
	if got := probeControllerLiveness(shortSocketTempDir(t, "gc-lv-")); got != controllerLivenessStopped {
		t.Fatalf("liveness = %v, want stopped", got)
	}
}

type timeoutNetError struct{}

func (timeoutNetError) Error() string   { return "i/o timeout" }
func (timeoutNetError) Timeout() bool   { return true }
func (timeoutNetError) Temporary() bool { return true }

// Only a refused connection or a missing socket confirms a stop: a slow or
// otherwise failing controller must read as unknown so a starved host is never
// mistaken for a stopped city.
func TestClassifyControllerPingErrorIsThreeValued(t *testing.T) {
	unavailable := func(err error) error {
		return controllerCommandError{op: "connecting to controller", err: err, unavailable: true}
	}
	tests := []struct {
		name string
		err  error
		want controllerLiveness
	}{
		{"answered", nil, controllerLivenessRunning},
		{"connection refused (stale socket)", unavailable(&net.OpError{Op: "dial", Err: os.NewSyscallError("connect", syscall.ECONNREFUSED)}), controllerLivenessStopped},
		{"missing socket", unavailable(&net.OpError{Op: "dial", Err: os.NewSyscallError("connect", syscall.ENOENT)}), controllerLivenessStopped},
		{"dial timeout", unavailable(&net.OpError{Op: "dial", Err: timeoutNetError{}}), controllerLivenessUnknown},
		{"read timeout", controllerCommandError{op: "reading response", err: timeoutNetError{}, unresponsive: true}, controllerLivenessUnknown},
		{"write failure", errors.New("sending command: broken pipe"), controllerLivenessUnknown},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := classifyControllerPingError(tc.err); got != tc.want {
				t.Fatalf("classify = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestImplicitRecoveryDenialIsLoggedAgainAfterInterval(t *testing.T) {
	s := &gateSeams{liveness: controllerLivenessStopped}
	installGateSeams(t, s)
	now := time.Now()
	orig := implicitRecoveryNow
	implicitRecoveryNow = func() time.Time { return now }
	t.Cleanup(func() { implicitRecoveryNow = orig })
	_ = implicitManagedDoltRecoveryCheck("/city")
	now = now.Add(implicitRecoveryDenialLogInterval - time.Second)
	_ = implicitManagedDoltRecoveryCheck("/city")
	if s.logs != 1 {
		t.Fatalf("logs = %d inside the interval, want 1", s.logs)
	}
	now = now.Add(2 * time.Second)
	_ = implicitManagedDoltRecoveryCheck("/city")
	if s.logs != 2 {
		t.Fatalf("logs = %d after the interval, want 2", s.logs)
	}
}

func TestRecoverManagedBDCommandForScopeDeniesFrozenRigScope(t *testing.T) {
	s := &gateSeams{liveness: controllerLivenessRunning}
	installGateSeams(t, s)
	frozenScope := "/rig/outside/city"
	implicitRecoveryMigrationFrozen = func(dir string) bool { return dir == frozenScope }
	err := recoverManagedBDCommandForScope(t.TempDir(), frozenScope)
	if !errors.Is(err, errManagedDoltRecoverDeclined) || !strings.Contains(err.Error(), "MIGRATION-FREEZE") {
		t.Fatalf("err = %v, want a declined MIGRATION-FREEZE denial", err)
	}
}

// The non-test callers of the lifecycle-intent surface are pinned. A new caller
// of any of these on a store-open or doctor path would silently grant lifecycle
// intent to every ordinary command that reaches it.
func TestLifecycleIntentCallSiteSetIsPinned(t *testing.T) {
	watched := map[string]bool{
		"ensureBeadsProvider":       true,
		"healthBeadsProvider":       true,
		"startManagedDoltProcess":   true,
		"recoverManagedDoltProcess": true,
		"declareLifecycleIntent":    true,
	}
	want := map[string]bool{
		"beads_provider_lifecycle.go:var initDirIfReadyEnsureBeadsProvider": true,
		"beads_provider_lifecycle.go:var startBeadsLifecycleEnsureProvider": true,
		"beads_provider_lifecycle.go:startBeadsLifecycle":                   true,
		"beads_provider_lifecycle.go:ensureBeadsProvider":                   true,
		"beads_provider_lifecycle.go:healthBeadsProvider":                   true,
		"cmd_beads.go:doBeadsHealth":                                        true,
		"cmd_start.go:doStartStandalone":                                    true,
		"cmd_supervisor.go:runSupervisor":                                   true,
		"city_runtime.go:newCityRuntime":                                    true,
		"city_runtime.go:ensureManagedDoltPublishedForTick":                 true,
		"cmd_supervisor.go:prepareCityForSupervisor":                        true,
		"cmd_dolt_state.go:newDoltStateCmd":                                 true,
		"controller.go:runController":                                       true,
		"dolt_recover_managed.go:recoverManagedDoltProcess":                 true,
		"dolt_start_managed.go:startManagedDoltProcess":                     true,
	}
	fset := token.NewFileSet()
	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]bool{}
	for _, name := range files {
		if strings.HasSuffix(name, "_test.go") || name == "managed_dolt_implicit_recovery_gate.go" {
			continue
		}
		f, err := parser.ParseFile(fset, name, nil, 0)
		if err != nil {
			t.Fatalf("parse %s: %v", name, err)
		}
		scan := func(owner string, node ast.Node, self *ast.Ident) {
			ast.Inspect(node, func(n ast.Node) bool {
				if id, ok := n.(*ast.Ident); ok && watched[id.Name] && id != self {
					got[name+":"+owner] = true
				}
				return true
			})
		}
		for _, decl := range f.Decls {
			switch d := decl.(type) {
			case *ast.FuncDecl:
				scan(d.Name.Name, d, d.Name)
			case *ast.GenDecl:
				for _, spec := range d.Specs {
					if vs, ok := spec.(*ast.ValueSpec); ok {
						scan("var "+vs.Names[0].Name, vs, nil)
					}
				}
			}
		}
	}
	for k := range got {
		if !want[k] {
			t.Errorf("unexpected caller of the lifecycle-intent surface: %s (if it is a genuine lifecycle entry point add it to this allowlist; otherwise route it away from ensureBeadsProvider/healthBeadsProvider/startManagedDoltProcess/recoverManagedDoltProcess)", k)
		}
	}
	for k := range want {
		if !got[k] {
			t.Errorf("allowlisted caller no longer present: %s", k)
		}
	}
}

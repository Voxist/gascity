package main

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

type gateSeams struct {
	alive      int
	frozen     bool
	registered bool
	logs       int
}

func installGateSeams(t *testing.T, s *gateSeams) {
	t.Helper()
	resetLifecycleIntentForTest(t)
	oa, of, or, ol := implicitRecoveryControllerAlive, implicitRecoveryMigrationFrozen, implicitRecoveryRegistered, logImplicitRecoveryDenial
	implicitRecoveryControllerAlive = func(string) int { return s.alive }
	implicitRecoveryMigrationFrozen = func(string) bool { return s.frozen }
	implicitRecoveryRegistered = func(string) bool { return s.registered }
	logImplicitRecoveryDenial = func(_, _, _ string) { s.logs++ }
	implicitRecoveryDenialLogged.Range(func(k, _ any) bool { implicitRecoveryDenialLogged.Delete(k); return true })
	t.Cleanup(func() {
		implicitRecoveryControllerAlive, implicitRecoveryMigrationFrozen, implicitRecoveryRegistered, logImplicitRecoveryDenial = oa, of, or, ol
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
		{name: "controller down denied unregistered", wantDenied: true, wantText: "gc register /city && gc start"},
		{name: "controller down denied registered", seams: gateSeams{registered: true}, wantDenied: true, wantText: "run gc start"},
		{name: "controller up permitted", seams: gateSeams{alive: 42}},
		{name: "lifecycle intent permitted", intent: true},
		{name: "frozen denied even with controller", seams: gateSeams{alive: 42, frozen: true}, wantDenied: true, wantText: "MIGRATION-FREEZE"},
		{name: "frozen denied even with intent", seams: gateSeams{frozen: true}, intent: true, wantDenied: true, wantText: "MIGRATION-FREEZE"},
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
	s := &gateSeams{}
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
	s := &gateSeams{}
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
	s := &gateSeams{}
	installGateSeams(t, s)
	t.Setenv("GC_BEADS", "file")
	t.Setenv("GC_DOLT", "skip")
	_ = os.Unsetenv("GC_DOLT_HOST")
	_ = os.Unsetenv("GC_DOLT_PORT")
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
	t.Setenv("GC_BEADS", "file")
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

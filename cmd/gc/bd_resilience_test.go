package main

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
)

// TestBdTransportRetryableMarkersArePinned pins the bd stderr string table
// gc uses to classify transport-class failures. This table is an explicit
// compatibility surface with the bd CLI (city-scale plan item 1.2): the
// circuit breaker, the managed-retry path, and gc hook's exit-2 store-
// unavailable signal all classify through it until bd ships a typed
// machine-readable error envelope (reserved exit-code contract). If bd's
// wording changes, this test fails and the markers must be updated
// deliberately, in one place.
func TestBdTransportRetryableMarkersArePinned(t *testing.T) {
	wantRetryable := []string{
		"server unreachable",
		"dial tcp",
		"connection refused",
		"broken pipe",
		"unexpected eof",
		"bad connection",
		"use of closed network connection",
		"auto-importing",
		"into empty database",
	}
	if !reflect.DeepEqual(bdTransportRetryableMarkers, wantRetryable) {
		t.Fatalf("bdTransportRetryableMarkers = %q, want %q — this string table is a tested bd compatibility surface; change both deliberately", bdTransportRetryableMarkers, wantRetryable)
	}
	wantRecoverable := []string{
		"server unreachable",
		"dial tcp",
		"connection refused",
		"auto-importing",
		"into empty database",
	}
	if !reflect.DeepEqual(bdTransportRecoverableMarkers, wantRecoverable) {
		t.Fatalf("bdTransportRecoverableMarkers = %q, want %q", bdTransportRecoverableMarkers, wantRecoverable)
	}
}

// writeBreakerTestCity writes a city.toml with deterministic breaker
// settings (long backoff so an open breaker stays open for the whole
// test) and returns the city path.
func writeBreakerTestCity(t *testing.T, extra string) string {
	t.Helper()
	cityPath := t.TempDir()
	content := `[workspace]
name = "breaker-test"

[beads.resilience]
consecutive_failures = 3
open_base = "1h"
open_max = "1h"
` + extra
	if err := os.WriteFile(filepath.Join(cityPath, "city.toml"), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	return cityPath
}

// installFakeBdExec replaces the exec layer with fn and disables managed
// recovery, restoring both on cleanup.
func installFakeBdExec(t *testing.T, fn func(dir, name string, args ...string) ([]byte, error)) *int {
	t.Helper()
	origRunner := beadsExecCommandRunnerWithEnv
	origRecover := recoverManagedBDCommand
	t.Cleanup(func() {
		beadsExecCommandRunnerWithEnv = origRunner
		recoverManagedBDCommand = origRecover
	})
	calls := new(int)
	beadsExecCommandRunnerWithEnv = func(_ map[string]string) beads.CommandRunner {
		return func(dir, name string, args ...string) ([]byte, error) {
			*calls++
			return fn(dir, name, args...)
		}
	}
	recoverManagedBDCommand = func(_ string) error { return nil }
	return calls
}

func breakerTestRunner(cityPath string) beads.CommandRunner {
	return bdCommandRunnerWithManagedRetry(cityPath, func(_ string) map[string]string {
		return map[string]string{"GC_DOLT_PORT": "3307"}
	})
}

func TestBdRunnerTransportFailuresTripBreakerToErrStoreUnavailable(t *testing.T) {
	t.Setenv("GC_BEADS", "bd")
	cityPath := writeBreakerTestCity(t, "")
	bdResilienceRegistryForCity(cityPath).SetJitterForTest(func(capDur time.Duration) time.Duration { return capDur })
	calls := installFakeBdExec(t, func(_, _ string, _ ...string) ([]byte, error) {
		return nil, fmt.Errorf("dial tcp 127.0.0.1:3307: connection refused")
	})
	runner := breakerTestRunner(cityPath)
	scope := t.TempDir()

	// Three calls; each runs the initial attempt + one managed retry, and
	// each counts as one consecutive transport failure.
	for i := 0; i < 3; i++ {
		if _, err := runner(scope, "bd", "list", "--json"); err == nil {
			t.Fatalf("call %d: err = nil, want transport error", i)
		} else if errors.Is(err, beads.ErrStoreUnavailable) {
			t.Fatalf("call %d: err = %v before the trip threshold, want raw transport error", i, err)
		}
	}
	callsBefore := *calls
	out, err := runner(scope, "bd", "list", "--json")
	if !errors.Is(err, beads.ErrStoreUnavailable) {
		t.Fatalf("call after trip: err = %v, want errors.Is(_, beads.ErrStoreUnavailable)", err)
	}
	if out != nil {
		t.Fatalf("call after trip: out = %q, want nil", out)
	}
	if *calls != callsBefore {
		t.Fatalf("breaker-open call spawned %d subprocess attempts, want 0", *calls-callsBefore)
	}
}

func TestBdRunnerScopesBreakerIndependently(t *testing.T) {
	t.Setenv("GC_BEADS", "bd")
	cityPath := writeBreakerTestCity(t, "")
	bdResilienceRegistryForCity(cityPath).SetJitterForTest(func(capDur time.Duration) time.Duration { return capDur })
	calls := installFakeBdExec(t, func(_, _ string, _ ...string) ([]byte, error) {
		return nil, fmt.Errorf("dial tcp 127.0.0.1:3307: connection refused")
	})
	runner := breakerTestRunner(cityPath)
	scopeA := t.TempDir()
	scopeB := t.TempDir()
	for i := 0; i < 3; i++ {
		_, _ = runner(scopeA, "bd", "list", "--json")
	}
	if _, err := runner(scopeA, "bd", "list", "--json"); !errors.Is(err, beads.ErrStoreUnavailable) {
		t.Fatalf("scope A after trip: err = %v, want ErrStoreUnavailable", err)
	}
	callsBefore := *calls
	if _, err := runner(scopeB, "bd", "list", "--json"); errors.Is(err, beads.ErrStoreUnavailable) {
		t.Fatalf("scope B: err = %v — one scope's open breaker must not quarantine another scope", err)
	}
	if *calls == callsBefore {
		t.Fatal("scope B call spawned no subprocess; expected it to run")
	}
}

func TestBdRunnerApplicationErrorsDoNotTripBreaker(t *testing.T) {
	t.Setenv("GC_BEADS", "bd")
	cityPath := writeBreakerTestCity(t, "")
	installFakeBdExec(t, func(_, _ string, _ ...string) ([]byte, error) {
		return nil, fmt.Errorf("exit status 1: unknown flag --bogus")
	})
	runner := breakerTestRunner(cityPath)
	scope := t.TempDir()
	for i := 0; i < 6; i++ {
		_, _ = runner(scope, "bd", "list", "--bogus")
	}
	if _, err := runner(scope, "bd", "list", "--bogus"); errors.Is(err, beads.ErrStoreUnavailable) {
		t.Fatalf("err = %v — application-class failures must never trip the transport breaker", err)
	}
}

func TestBdRunnerSuccessResetsConsecutiveFailures(t *testing.T) {
	t.Setenv("GC_BEADS", "bd")
	cityPath := writeBreakerTestCity(t, "")
	bdResilienceRegistryForCity(cityPath).SetJitterForTest(func(capDur time.Duration) time.Duration { return capDur })
	fail := true
	failures := 0
	installFakeBdExec(t, func(_, _ string, _ ...string) ([]byte, error) {
		if fail {
			failures++
			return nil, fmt.Errorf("dial tcp 127.0.0.1:3307: connection refused")
		}
		return []byte("[]"), nil
	})
	runner := breakerTestRunner(cityPath)
	scope := t.TempDir()

	_, _ = runner(scope, "bd", "list", "--json")
	_, _ = runner(scope, "bd", "list", "--json")
	fail = false
	if _, err := runner(scope, "bd", "list", "--json"); err != nil {
		t.Fatalf("successful call: %v", err)
	}
	fail = true
	_, _ = runner(scope, "bd", "list", "--json")
	_, _ = runner(scope, "bd", "list", "--json")
	// Only 2 consecutive failures since the success — breaker stays closed.
	if _, err := runner(scope, "bd", "list", "--json"); errors.Is(err, beads.ErrStoreUnavailable) {
		t.Fatalf("err = %v — success must reset the consecutive failure count", err)
	}
}

func TestBdRunnerBreakerDisabledByConfig(t *testing.T) {
	t.Setenv("GC_BEADS", "bd")
	cityPath := writeBreakerTestCity(t, "enabled = false\n")
	calls := installFakeBdExec(t, func(_, _ string, _ ...string) ([]byte, error) {
		return nil, fmt.Errorf("dial tcp 127.0.0.1:3307: connection refused")
	})
	runner := breakerTestRunner(cityPath)
	scope := t.TempDir()
	for i := 0; i < 6; i++ {
		_, _ = runner(scope, "bd", "list", "--json")
	}
	callsBefore := *calls
	if _, err := runner(scope, "bd", "list", "--json"); errors.Is(err, beads.ErrStoreUnavailable) {
		t.Fatalf("err = %v — [beads.resilience] enabled=false must restore pre-breaker behavior", err)
	}
	if *calls == callsBefore {
		t.Fatal("disabled breaker still blocked the subprocess")
	}
}

func TestBdRunnerNonBdCommandsBypassBreaker(t *testing.T) {
	t.Setenv("GC_BEADS", "bd")
	cityPath := writeBreakerTestCity(t, "")
	bdResilienceRegistryForCity(cityPath).SetJitterForTest(func(capDur time.Duration) time.Duration { return capDur })
	calls := installFakeBdExec(t, func(_, name string, _ ...string) ([]byte, error) {
		if name == "bd" {
			return nil, fmt.Errorf("dial tcp 127.0.0.1:3307: connection refused")
		}
		return []byte("ok"), nil
	})
	runner := breakerTestRunner(cityPath)
	scope := t.TempDir()
	for i := 0; i < 4; i++ {
		_, _ = runner(scope, "bd", "list", "--json")
	}
	callsBefore := *calls
	out, err := runner(scope, "git", "status")
	if err != nil || string(out) != "ok" {
		t.Fatalf("non-bd command under open breaker: out=%q err=%v, want ok/nil", out, err)
	}
	if *calls == callsBefore {
		t.Fatal("non-bd command did not run")
	}
}

func TestBdScopeBreakerSharedWithCachingStoreGate(t *testing.T) {
	// The breaker handed to CachingStore.SetAvailabilityGate must be the
	// same instance the bd runner records into, so caching reads degrade
	// the moment the runner trips the scope.
	cityPath := writeBreakerTestCity(t, "")
	scope := filepath.Join(cityPath, "rigs", "vr")
	a := bdScopeBreaker(cityPath, scope)
	b := bdScopeBreaker(cityPath, scope)
	if a != b {
		t.Fatal("bdScopeBreaker returned distinct breakers for the same scope")
	}
	var gate beads.AvailabilityGate = a
	if !gate.Available() {
		t.Fatal("fresh breaker gate reports unavailable")
	}
}

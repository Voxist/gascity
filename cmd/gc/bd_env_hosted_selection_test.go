package main

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
)

// countHostedBeadsSelectionLoads wraps the config loader behind the hosted
// Beads selection so a test can count how many full config loads one bd
// invocation performs.
func countHostedBeadsSelectionLoads(t *testing.T) *int {
	t.Helper()
	orig := loadHostedBeadsSelectionConfig
	t.Cleanup(func() { loadHostedBeadsSelectionConfig = orig })
	loads := new(int)
	loadHostedBeadsSelectionConfig = func(cityConfigPath string) (*config.City, error) {
		*loads++
		return orig(cityConfigPath)
	}
	return loads
}

// installFakeHostedBdExec stubs the hermetic (ambient-withholding) exec layer
// the hosted-city runner selects, counting subprocess attempts the same way
// installFakeBdExec does for the plain layer.
func installFakeHostedBdExec(t *testing.T, fn func(dir, name string, args ...string) ([]byte, error)) *int {
	t.Helper()
	orig := beadsExecCommandRunnerWithEnvWithoutAmbientBeads
	t.Cleanup(func() { beadsExecCommandRunnerWithEnvWithoutAmbientBeads = orig })
	calls := new(int)
	beadsExecCommandRunnerWithEnvWithoutAmbientBeads = func(_ map[string]string) beads.CommandRunner {
		return func(dir, name string, args ...string) ([]byte, error) {
			*calls++
			return fn(dir, name, args...)
		}
	}
	return calls
}

// TestBdRunnersLoadConfigOncePerInvocation pins the config-load budget of one
// bd subprocess: the hosted-Beads selection is the only config-derived
// answer the runner needs, and answering it costs a full city load (pack
// expansion under the machine-wide repo-cache lock). The 2026-08-31 resync
// repair asked the question at every site independently — the credential
// env, the runner choice, again on the managed retry — so a single ready
// read loaded the city config two to four times.
func TestBdRunnersLoadConfigOncePerInvocation(t *testing.T) {
	t.Run("managed runner", func(t *testing.T) {
		t.Setenv("GC_BEADS", "bd")
		disableManagedDoltRecoveryForTest(t)
		clearInheritedCityRoutingEnv(t)
		cityPath := writeBreakerTestCity(t, "")
		installFakeBdExec(t, func(_, _ string, _ ...string) ([]byte, error) { return []byte("[]"), nil })
		loads := countHostedBeadsSelectionLoads(t)

		if _, err := bdCommandRunnerForCity(cityPath)(t.TempDir(), "bd", "list", "--json"); err != nil {
			t.Fatalf("runner: %v", err)
		}
		if *loads != 1 {
			t.Fatalf("managed bd invocation loaded the city config %d times, want 1", *loads)
		}
	})

	t.Run("managed runner with retry", func(t *testing.T) {
		t.Setenv("GC_BEADS", "bd")
		disableManagedDoltRecoveryForTest(t)
		clearInheritedCityRoutingEnv(t)
		cityPath := writeBreakerTestCity(t, "")
		origSleep := bdCommandRetrySleep
		t.Cleanup(func() { bdCommandRetrySleep = origSleep })
		bdCommandRetrySleep = func(time.Duration) {}
		attempts := 0
		calls := installFakeBdExec(t, func(_, _ string, _ ...string) ([]byte, error) {
			attempts++
			if attempts == 1 {
				return nil, fmt.Errorf("dial tcp 127.0.0.1:3307: connection refused")
			}
			return []byte("[]"), nil
		})
		loads := countHostedBeadsSelectionLoads(t)

		if _, err := bdCommandRunnerForCity(cityPath)(t.TempDir(), "bd", "list", "--json"); err != nil {
			t.Fatalf("runner: %v", err)
		}
		if *calls != 2 {
			t.Fatalf("subprocess attempts = %d, want 2 (initial + managed retry)", *calls)
		}
		if *loads != 1 {
			t.Fatalf("managed bd invocation with retry loaded the city config %d times, want 1", *loads)
		}
	})

	t.Run("rig runner", func(t *testing.T) {
		t.Setenv("GC_BEADS", "bd")
		disableManagedDoltRecoveryForTest(t)
		clearInheritedCityRoutingEnv(t)
		cityPath := writeBreakerTestCity(t, "")
		rigDir := filepath.Join(cityPath, "rigs", "alpha")
		if err := os.MkdirAll(rigDir, 0o755); err != nil {
			t.Fatal(err)
		}
		installFakeBdExec(t, func(_, _ string, _ ...string) ([]byte, error) { return []byte("[]"), nil })
		loads := countHostedBeadsSelectionLoads(t)

		if _, err := bdCommandRunnerForRig(cityPath, nil, rigDir)(rigDir, "bd", "list", "--json"); err != nil {
			t.Fatalf("runner: %v", err)
		}
		if *loads != 1 {
			t.Fatalf("rig bd invocation loaded the city config %d times, want 1", *loads)
		}
	})

	t.Run("external-binding runner on a hosted city", func(t *testing.T) {
		disableManagedDoltRecoveryForTest(t)
		clearInheritedCityRoutingEnv(t)
		stubHostedBeadsCredentialExecutable(t, "/opt/current gc/bin/gc")
		t.Setenv(registryCredentialProviderEnv, `["/opt/gasworks","credential-provider"]`)
		cityPath := writeHostedBeadsCity(t, "https://beads.example/workspaces/infra", "gasworks", false)
		writeCompleteStorageBinding(t, cityPath)
		hostedCalls := installFakeHostedBdExec(t, func(_, _ string, _ ...string) ([]byte, error) { return []byte("[]"), nil })
		loads := countHostedBeadsSelectionLoads(t)

		if _, err := bdCommandRunnerForCity(cityPath)(t.TempDir(), "bd", "list", "--json"); err != nil {
			t.Fatalf("runner: %v", err)
		}
		if *hostedCalls != 1 {
			t.Fatalf("hosted runner spawned %d subprocesses, want 1 through the ambient-withholding exec layer", *hostedCalls)
		}
		if *loads != 1 {
			t.Fatalf("external-binding bd invocation loaded the city config %d times, want 1", *loads)
		}
	})
}

// appendBreakerTestResilience gives a city the same deterministic breaker
// settings writeBreakerTestCity uses, so an open breaker stays open for the
// rest of the test.
func appendBreakerTestResilience(t *testing.T, cityPath string) {
	t.Helper()
	f, err := os.OpenFile(filepath.Join(cityPath, "city.toml"), os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = f.Close() }()
	if _, err := f.WriteString("\n[beads.resilience]\nconsecutive_failures = 3\nopen_base = \"1h\"\nopen_max = \"1h\"\n"); err != nil {
		t.Fatal(err)
	}
}

// TestBdRunnersOpenBreakerLoadsNoConfig pins what an open scope breaker
// costs: zero subprocesses, and no config load for the runner choice. Before
// the choice moved below the breaker, every call against a wedged scope paid
// a full city load (and the repo-cache lock) just to be refused.
//
// The managed path still projects its env above the gate — upstream's order,
// kept because the projection carries the managed-Dolt recovery probe — so
// that one load remains; the runner choice must not add a second.
func TestBdRunnersOpenBreakerLoadsNoConfig(t *testing.T) {
	t.Run("managed runner", func(t *testing.T) {
		t.Setenv("GC_BEADS", "bd")
		disableManagedDoltRecoveryForTest(t)
		clearInheritedCityRoutingEnv(t)
		cityPath := writeBreakerTestCity(t, "")
		bdResilienceRegistryForCity(cityPath).SetJitterForTest(func(capDur time.Duration) time.Duration { return capDur })
		origSleep := bdCommandRetrySleep
		t.Cleanup(func() { bdCommandRetrySleep = origSleep })
		bdCommandRetrySleep = func(time.Duration) {}
		calls := installFakeBdExec(t, func(_, _ string, _ ...string) ([]byte, error) {
			return nil, fmt.Errorf("dial tcp 127.0.0.1:3307: connection refused")
		})
		runner := bdCommandRunnerForCity(cityPath)
		scope := t.TempDir()
		for i := 0; i < 3; i++ {
			if _, err := runner(scope, "bd", "list", "--json"); err == nil {
				t.Fatalf("call %d: err = nil, want transport error", i)
			}
		}

		loads := countHostedBeadsSelectionLoads(t)
		callsBefore := *calls
		_, err := runner(scope, "bd", "list", "--json")
		if !errors.Is(err, beads.ErrStoreUnavailable) {
			t.Fatalf("call after trip: err = %v, want errors.Is(_, beads.ErrStoreUnavailable)", err)
		}
		if *calls != callsBefore {
			t.Fatalf("breaker-open call spawned %d subprocesses, want 0", *calls-callsBefore)
		}
		if *loads != 1 {
			t.Fatalf("breaker-open call loaded the city config %d times, want 1 (the env projection's own; the runner choice must not add one)", *loads)
		}
	})

	t.Run("external-binding runner", func(t *testing.T) {
		disableManagedDoltRecoveryForTest(t)
		clearInheritedCityRoutingEnv(t)
		stubHostedBeadsCredentialExecutable(t, "/opt/current gc/bin/gc")
		t.Setenv(registryCredentialProviderEnv, `["/opt/gasworks","credential-provider"]`)
		cityPath := writeHostedBeadsCity(t, "https://beads.example/workspaces/infra", "gasworks", false)
		writeCompleteStorageBinding(t, cityPath)
		appendBreakerTestResilience(t, cityPath)
		bdResilienceRegistryForCity(cityPath).SetJitterForTest(func(capDur time.Duration) time.Duration { return capDur })
		calls := installFakeHostedBdExec(t, func(_, _ string, _ ...string) ([]byte, error) {
			return nil, fmt.Errorf("dial tcp 127.0.0.1:3307: connection refused")
		})
		runner := bdCommandRunnerForCity(cityPath)
		scope := t.TempDir()
		// An external binding has no managed recovery, so the classifier only
		// counts per-command deadline kills; trip the scope breaker directly.
		breaker := bdScopeBreaker(cityPath, scope)
		for i := 0; i < 3; i++ {
			recordBdBreakerOutcome(breaker, true)
		}
		if breaker.Allow() {
			t.Fatal("breaker did not trip after three recorded transport failures")
		}

		loads := countHostedBeadsSelectionLoads(t)
		callsBefore := *calls
		_, err := runner(scope, "bd", "list", "--json")
		if !errors.Is(err, beads.ErrStoreUnavailable) {
			t.Fatalf("call after trip: err = %v, want errors.Is(_, beads.ErrStoreUnavailable)", err)
		}
		if *calls != callsBefore {
			t.Fatalf("breaker-open call spawned %d subprocesses, want 0", *calls-callsBefore)
		}
		if *loads != 0 {
			t.Fatalf("breaker-open call loaded the city config %d times, want 0", *loads)
		}
	})
}

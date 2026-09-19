package main

import (
	"errors"
	"os"
	"strings"
	"testing"
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

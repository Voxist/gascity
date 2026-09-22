package main

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
)

// stubRecoverGateClock pins providerRecoverNow to a movable instant,
// restoring it on cleanup, and returns the advance function so a test can
// step the clock across the cooldown window. providerRecoverCooldown is
// deliberately left at its production value: these tests pin the real
// window, not a test-only one.
func stubRecoverGateClock(t *testing.T) func(time.Duration) {
	t.Helper()
	origNow := providerRecoverNow
	t.Cleanup(func() { providerRecoverNow = origNow })
	var mu sync.Mutex
	now := time.Date(2026, 9, 22, 16, 13, 0, 0, time.UTC)
	providerRecoverNow = func() time.Time {
		mu.Lock()
		defer mu.Unlock()
		return now
	}
	return func(d time.Duration) {
		mu.Lock()
		defer mu.Unlock()
		now = now.Add(d)
	}
}

// stubRecoverLiveness pins the guard's liveness verdict.
func stubRecoverLiveness(t *testing.T, liveness managedDoltLiveness) {
	t.Helper()
	orig := managedDoltReplaceLiveness
	t.Cleanup(func() { managedDoltReplaceLiveness = orig })
	managedDoltReplaceLiveness = func(string) (managedDoltLiveness, string) {
		return liveness, "stubbed for test"
	}
}

// countRecoverRuns replaces the provider recover runner with a counter,
// so a test observes whether the guard let the op through without ever
// executing a provider script.
//
// Load-bearing, do not remove: the bd-runner tests below drive the REAL
// recoverManagedBDCommand, whose script path resolves to the bundled
// provider pack. Without this stub they start actual dolt sql-servers —
// observed while mutation-checking the funnel, where bypassing the guard
// leaked two of them and tripped the suite's dolt leak guard.
func countRecoverRuns(t *testing.T) *int {
	t.Helper()
	orig := managedDoltRecoverRunner
	t.Cleanup(func() { managedDoltRecoverRunner = orig })
	runs := new(int)
	managedDoltRecoverRunner = func(context.Context, string, []string) error {
		*runs++
		return nil
	}
	return runs
}

// ---------------------------------------------------------------------
// The cooldown
// ---------------------------------------------------------------------

func TestAdmitManagedDoltRecoverAdmitsOncePerCooldownWindow(t *testing.T) {
	cityPath := t.TempDir()
	advance := stubRecoverGateClock(t)

	admitted := 0
	for i := 0; i < 8; i++ {
		if admitManagedDoltRecover(cityPath) {
			admitted++
		}
	}
	if admitted != 1 {
		t.Fatalf("admitted = %d across 8 attempts inside the window, want 1", admitted)
	}

	window := providerRecoverCooldown()
	if window != 30*time.Second {
		t.Fatalf("providerRecoverCooldown() = %s, want the documented 30s window", window)
	}
	advance(window - time.Second)
	if admitManagedDoltRecover(cityPath) {
		t.Fatalf("admitted a recover one second short of the %s cooldown window", window)
	}

	// The window is elapsed, not canceled: the next attempt is admitted.
	advance(2 * time.Second)
	if !admitManagedDoltRecover(cityPath) {
		t.Fatal("refused a recover after the cooldown window elapsed")
	}
	if admitManagedDoltRecover(cityPath) {
		t.Fatal("admitted a second recover immediately after the window reset")
	}
}

func TestAdmitManagedDoltRecoverWindowIsPerCity(t *testing.T) {
	cityA := t.TempDir()
	cityB := t.TempDir()
	stubRecoverGateClock(t)

	if !admitManagedDoltRecover(cityA) {
		t.Fatal("first recover on city A refused")
	}
	if !admitManagedDoltRecover(cityB) {
		t.Fatal("city B refused because city A consumed its window: the throttle must be per-city")
	}
	if admitManagedDoltRecover(cityA) {
		t.Fatal("city A admitted a second recover inside its window")
	}
}

func TestAdmitManagedDoltRecoverAdmitsOnceUnderConcurrentCallers(t *testing.T) {
	cityPath := t.TempDir()
	stubRecoverGateClock(t)

	const callers = 64
	var wg sync.WaitGroup
	var mu sync.Mutex
	admitted := 0
	start := make(chan struct{})
	for i := 0; i < callers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			if admitManagedDoltRecover(cityPath) {
				mu.Lock()
				admitted++
				mu.Unlock()
			}
		}()
	}
	close(start)
	wg.Wait()

	if admitted != 1 {
		t.Fatalf("admitted = %d across %d concurrent callers, want 1", admitted, callers)
	}
}

// ---------------------------------------------------------------------
// Three-valued liveness
//
// The contract under test: a failure to OBSERVE is never sufficient
// evidence to destroy a running server. Only managedDoltLivenessConfirmedDead
// authorizes a replacement.
// ---------------------------------------------------------------------

// livenessCity writes a canonical managed-dolt runtime state for a fresh
// temp city and returns the city path. The city is left owned (no
// external storage binding), so the ownership probe answers true.
func livenessCity(t *testing.T, state doltRuntimeState) string {
	t.Helper()
	cityPath := t.TempDir()
	if err := os.MkdirAll(filepath.Join(cityPath, ".gc", "runtime", "packs", "dolt"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(cityPath, ".beads"), 0o700); err != nil {
		t.Fatal(err)
	}
	if state.DataDir == "" {
		state.DataDir = filepath.Join(cityPath, ".beads", "dolt")
	}
	if state.StartedAt == "" {
		state.StartedAt = time.Now().UTC().Format(time.RFC3339)
	}
	if err := writeDoltState(cityPath, state); err != nil {
		t.Fatal(err)
	}
	return cityPath
}

// ownedProbes is the default probe set with ownership forced true, so a
// liveness case exercises the branch under test rather than the city's
// storage binding.
func ownedProbes() managedDoltLivenessProbes {
	probes := defaultManagedDoltLivenessProbes()
	probes.lifecycleOwned = func(string) (bool, error) { return true, nil }
	return probes
}

func requireLiveness(t *testing.T, want managedDoltLiveness, got managedDoltLiveness, reason string) {
	t.Helper()
	if got != want {
		t.Fatalf("liveness = %s (%s), want %s", got, reason, want)
	}
}

func TestManagedDoltReplaceLivenessConfirmsDeathWhenPidIsGone(t *testing.T) {
	ln := listenOnRandomPort(t)
	t.Cleanup(func() { _ = ln.Close() })
	cityPath := livenessCity(t, doltRuntimeState{
		Running: true,
		PID:     deadPID(t),
		Port:    ln.Addr().(*net.TCPAddr).Port,
	})

	got, reason := managedDoltReplaceLivenessWith(cityPath, ownedProbes())
	requireLiveness(t, managedDoltLivenessConfirmedDead, got, reason)
}

func TestManagedDoltReplaceLivenessConfirmsDeathWhenPortIsHeldByAnotherPid(t *testing.T) {
	cityPath := livenessCity(t, doltRuntimeState{Running: true, PID: os.Getpid(), Port: 34567})

	probes := ownedProbes()
	probes.pidAlive = func(int) bool { return true }
	probes.portHeldByPID = func(string, int) (bool, bool) { return false, true }
	probes.portHolders = func(string) ([]int, bool) { return []int{os.Getpid() + 1}, true }

	got, reason := managedDoltReplaceLivenessWith(cityPath, probes)
	requireLiveness(t, managedDoltLivenessConfirmedDead, got, reason)
}

func TestManagedDoltReplaceLivenessIsAliveWhenPidHoldsItsPort(t *testing.T) {
	cityPath := livenessCity(t, doltRuntimeState{Running: true, PID: os.Getpid(), Port: 34567})

	probes := ownedProbes()
	probes.pidAlive = func(int) bool { return true }
	probes.portHeldByPID = func(string, int) (bool, bool) { return true, true }

	got, reason := managedDoltReplaceLivenessWith(cityPath, probes)
	requireLiveness(t, managedDoltLivenessAlive, got, reason)
}

// A live managed dolt, observed through the REAL process-table and
// network probes rather than injected ones.
func TestManagedDoltReplaceLivenessIsAliveForARealListenerWithRealProbes(t *testing.T) {
	cityPath := t.TempDir()
	_ = writeReachableManagedDoltState(t, cityPath)

	got, reason := managedDoltReplaceLiveness(cityPath)
	requireLiveness(t, managedDoltLivenessAlive, got, reason)
}

func TestManagedDoltReplaceLivenessIsUnknownWhenStateFileIsUnreadable(t *testing.T) {
	cityPath := livenessCity(t, doltRuntimeState{Running: true, PID: os.Getpid(), Port: 34567})
	statePath := managedDoltStatePath(cityPath)
	if err := os.Chmod(statePath, 0o000); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(statePath, 0o644) })
	if _, err := os.ReadFile(statePath); err == nil {
		t.Skip("runtime state is still readable (running as root?); cannot drive the unreadable branch")
	}

	got, reason := managedDoltReplaceLivenessWith(cityPath, ownedProbes())
	requireLiveness(t, managedDoltLivenessUnknown, got, reason)
}

func TestManagedDoltReplaceLivenessIsUnknownWhenStateFileIsMalformed(t *testing.T) {
	cityPath := livenessCity(t, doltRuntimeState{Running: true, PID: os.Getpid(), Port: 34567})
	if err := os.WriteFile(managedDoltStatePath(cityPath), []byte("{not json"), 0o644); err != nil {
		t.Fatal(err)
	}

	got, reason := managedDoltReplaceLivenessWith(cityPath, ownedProbes())
	requireLiveness(t, managedDoltLivenessUnknown, got, reason)
}

// The ga-kozi8 shape: the supervisor's own probe children are being
// killed, so the port-holder answer is "I could not look". A dial that
// then times out is an absence of evidence, not a death certificate.
func TestManagedDoltReplaceLivenessIsUnknownWhenProbesCannotCompleteAndDialTimesOut(t *testing.T) {
	cityPath := livenessCity(t, doltRuntimeState{Running: true, PID: os.Getpid(), Port: 34567})

	probes := ownedProbes()
	probes.pidAlive = func(int) bool { return true }
	probes.portHeldByPID = func(string, int) (bool, bool) { return false, false }
	probes.portReachable = func(string, time.Duration) bool { return false }

	got, reason := managedDoltReplaceLivenessWith(cityPath, probes)
	requireLiveness(t, managedDoltLivenessUnknown, got, reason)
}

func TestManagedDoltReplaceLivenessIsAliveWhenPortHolderProbeFailsButPortAnswers(t *testing.T) {
	cityPath := livenessCity(t, doltRuntimeState{Running: true, PID: os.Getpid(), Port: 34567})

	var gotBudget time.Duration
	probes := ownedProbes()
	probes.pidAlive = func(int) bool { return true }
	probes.portHeldByPID = func(string, int) (bool, bool) { return false, false }
	probes.portReachable = func(_ string, budget time.Duration) bool {
		gotBudget = budget
		return true
	}

	got, reason := managedDoltReplaceLivenessWith(cityPath, probes)
	requireLiveness(t, managedDoltLivenessAlive, got, reason)
	if gotBudget != managedDoltReplaceDialBudget {
		t.Fatalf("dial budget = %s, want the replace-path budget %s", gotBudget, managedDoltReplaceDialBudget)
	}
	if managedDoltReplaceDialBudget <= 250*time.Millisecond {
		t.Fatalf("replace dial budget = %s, want longer than the 250ms health-reporting dial", managedDoltReplaceDialBudget)
	}
}

// Genuine recovery must keep working: an empty city has nothing to
// protect.
func TestManagedDoltReplaceLivenessConfirmsDeathWhenNothingIsRunning(t *testing.T) {
	cityPath := t.TempDir()
	if err := os.MkdirAll(filepath.Join(cityPath, ".beads"), 0o700); err != nil {
		t.Fatal(err)
	}

	got, reason := managedDoltReplaceLivenessWith(cityPath, ownedProbes())
	requireLiveness(t, managedDoltLivenessConfirmedDead, got, reason)
}

// Absent runtime state with something still listening on the recorded
// port is a publication that has not caught up, not an empty city.
func TestManagedDoltReplaceLivenessIsUnknownWhenStateIsAbsentButRecordedPortIsHeld(t *testing.T) {
	cityPath := t.TempDir()
	if err := os.MkdirAll(filepath.Join(cityPath, ".beads"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cityPath, ".beads", "dolt-server.port"), []byte("34567\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	probes := ownedProbes()
	probes.portHolders = func(string) ([]int, bool) { return []int{os.Getpid()}, true }

	got, reason := managedDoltReplaceLivenessWith(cityPath, probes)
	requireLiveness(t, managedDoltLivenessUnknown, got, reason)

	// The compatibility port file must survive being read: a liveness
	// probe may not mutate the state it is inspecting.
	if _, err := os.Stat(filepath.Join(cityPath, ".beads", "dolt-server.port")); err != nil {
		t.Fatalf("liveness probe removed the compatibility port file: %v", err)
	}
}

func TestManagedDoltReplaceLivenessIsUnknownWhenOwnershipProbeFails(t *testing.T) {
	cityPath := t.TempDir()
	probes := ownedProbes()
	probes.lifecycleOwned = func(string) (bool, error) { return false, errors.New("probe failed") }

	got, reason := managedDoltReplaceLivenessWith(cityPath, probes)
	requireLiveness(t, managedDoltLivenessUnknown, got, reason)
}

// ---------------------------------------------------------------------
// What a failed health op proves
// ---------------------------------------------------------------------

func TestManagedDoltHealthOpEvidence(t *testing.T) {
	expired, cancel := context.WithDeadline(context.Background(), time.Now().Add(-time.Second))
	defer cancel()

	cases := []struct {
		name string
		ctx  context.Context
		err  error
		want managedDoltRecoverEvidence
	}{
		{
			name: "query probe answered and failed",
			ctx:  context.Background(),
			err:  errors.New("exec beads health: dolt query probe failed (information_schema.SCHEMATA)"),
			want: recoverEvidenceHealthOpAnswered,
		},
		{
			name: "read-only server answered",
			ctx:  context.Background(),
			err:  errors.New("exec beads health: dolt server is in read-only mode"),
			want: recoverEvidenceHealthOpAnswered,
		},
		{
			name: "health op killed at its deadline",
			ctx:  context.Background(),
			err:  fmt.Errorf("exec beads health: %w", context.DeadlineExceeded),
			want: recoverEvidenceCallFailed,
		},
		{
			name: "caller's context already expired",
			ctx:  expired,
			err:  errors.New("exec beads health: something"),
			want: recoverEvidenceCallFailed,
		},
		{
			name: "health op killed by signal",
			ctx:  context.Background(),
			err:  errors.New("exec beads health: signal: killed"),
			want: recoverEvidenceCallFailed,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := managedDoltHealthOpEvidence(tc.ctx, tc.err); got != tc.want {
				t.Fatalf("evidence = %d, want %d", got, tc.want)
			}
		})
	}
}

// ---------------------------------------------------------------------
// The guard
// ---------------------------------------------------------------------

func TestGuardedRecoverDeclinesAgainstALiveServer(t *testing.T) {
	cityPath := t.TempDir()
	stubRecoverGateClock(t)
	stubRecoverLiveness(t, managedDoltLivenessAlive)
	runs := countRecoverRuns(t)

	err := runGuardedManagedDoltRecover(context.Background(), cityPath, "script", nil, recoverEvidenceCallFailed)
	if !errors.Is(err, errManagedDoltRecoverDeclined) {
		t.Fatalf("err = %v, want a declined recover", err)
	}
	if *runs != 0 {
		t.Fatalf("recover runs = %d, want 0: a live server must not be replaced on a failed call alone", *runs)
	}
	// Declining on liveness must not burn the cooldown window.
	if !admitManagedDoltRecover(cityPath) {
		t.Fatal("a liveness refusal consumed the cooldown window; a later genuine recover must not be delayed by it")
	}
}

func TestGuardedRecoverDeclinesWhenLivenessIsUnknown(t *testing.T) {
	cityPath := t.TempDir()
	stubRecoverGateClock(t)
	stubRecoverLiveness(t, managedDoltLivenessUnknown)
	runs := countRecoverRuns(t)

	err := runGuardedManagedDoltRecover(context.Background(), cityPath, "script", nil, recoverEvidenceCallFailed)
	if !errors.Is(err, errManagedDoltRecoverDeclined) {
		t.Fatalf("err = %v, want a declined recover", err)
	}
	if *runs != 0 {
		t.Fatalf("recover runs = %d, want 0: a failure to observe is not evidence of death", *runs)
	}
}

func TestGuardedRecoverRunsWhenTheServerIsConfirmedDead(t *testing.T) {
	cityPath := t.TempDir()
	stubRecoverGateClock(t)
	stubRecoverLiveness(t, managedDoltLivenessConfirmedDead)
	runs := countRecoverRuns(t)

	if err := runGuardedManagedDoltRecover(context.Background(), cityPath, "script", nil, recoverEvidenceCallFailed); err != nil {
		t.Fatalf("err = %v, want the recover to run against a confirmed-dead server", err)
	}
	if *runs != 1 {
		t.Fatalf("recover runs = %d, want 1: the guard must not break genuine recovery", *runs)
	}
}

func TestGuardedRecoverThrottlesASecondAttemptInsideTheWindow(t *testing.T) {
	cityPath := t.TempDir()
	advance := stubRecoverGateClock(t)
	stubRecoverLiveness(t, managedDoltLivenessConfirmedDead)
	runs := countRecoverRuns(t)

	for i := 0; i < 5; i++ {
		_ = runGuardedManagedDoltRecover(context.Background(), cityPath, "script", nil, recoverEvidenceCallFailed)
	}
	if *runs != 1 {
		t.Fatalf("recover runs = %d across 5 attempts inside the window, want 1", *runs)
	}

	advance(providerRecoverCooldown() + time.Second)
	if err := runGuardedManagedDoltRecover(context.Background(), cityPath, "script", nil, recoverEvidenceCallFailed); err != nil {
		t.Fatalf("err = %v, want the recover to run after the window elapsed", err)
	}
	if *runs != 2 {
		t.Fatalf("recover runs = %d, want 2 after the window elapsed", *runs)
	}
}

// The ga-fkidk carve-out: a health op that RAN and found the server
// unhealthy may still replace it, live port or not. Without this, a
// wedged-but-listening Dolt would be unrecoverable forever.
func TestGuardedRecoverReplacesALiveServerWhenTheHealthOpAnswered(t *testing.T) {
	cityPath := t.TempDir()
	stubRecoverGateClock(t)
	stubRecoverLiveness(t, managedDoltLivenessAlive)
	runs := countRecoverRuns(t)

	if err := runGuardedManagedDoltRecover(context.Background(), cityPath, "script", nil, recoverEvidenceHealthOpAnswered); err != nil {
		t.Fatalf("err = %v, want the recover to run on a completed health op's verdict", err)
	}
	if *runs != 1 {
		t.Fatalf("recover runs = %d, want 1: op_health is the only observer that can see a wedged-but-listening dolt", *runs)
	}
}

// ...but the cooldown still applies to it.
func TestGuardedRecoverThrottlesHealthOpEvidenceToo(t *testing.T) {
	cityPath := t.TempDir()
	stubRecoverGateClock(t)
	stubRecoverLiveness(t, managedDoltLivenessAlive)
	runs := countRecoverRuns(t)

	for i := 0; i < 4; i++ {
		_ = runGuardedManagedDoltRecover(context.Background(), cityPath, "script", nil, recoverEvidenceHealthOpAnswered)
	}
	if *runs != 1 {
		t.Fatalf("recover runs = %d across 4 health-evidence attempts inside the window, want 1", *runs)
	}
}

// The health path and the bd-runner path share ONE window.
func TestGuardedRecoverWindowIsSharedAcrossEvidenceKinds(t *testing.T) {
	cityPath := t.TempDir()
	stubRecoverGateClock(t)
	stubRecoverLiveness(t, managedDoltLivenessConfirmedDead)
	runs := countRecoverRuns(t)

	if err := runGuardedManagedDoltRecover(context.Background(), cityPath, "script", nil, recoverEvidenceHealthOpAnswered); err != nil {
		t.Fatalf("health-evidence recover: %v", err)
	}
	err := runGuardedManagedDoltRecover(context.Background(), cityPath, "script", nil, recoverEvidenceCallFailed)
	if !errors.Is(err, errManagedDoltRecoverDeclined) {
		t.Fatalf("err = %v, want the transport path to see the window the health path just took", err)
	}
	if *runs != 1 {
		t.Fatalf("recover runs = %d, want 1: the two paths must share one window", *runs)
	}
}

// ---------------------------------------------------------------------
// The bd runner's route
// ---------------------------------------------------------------------

// installFailingBdTransport makes every bd invocation fail with a
// recoverable transport error. The retry backoff is stubbed out so the
// test does not sleep. Only the PROVIDER RUNNER is faked, so the real
// guard still decides whether a recover happens.
func installFailingBdTransport(t *testing.T) (attempts, recoverRuns *int) {
	t.Helper()
	origRunner := beadsExecCommandRunnerWithEnv
	origSleep := bdCommandRetrySleep
	t.Cleanup(func() {
		beadsExecCommandRunnerWithEnv = origRunner
		bdCommandRetrySleep = origSleep
	})
	bdCommandRetrySleep = func(time.Duration) {}
	attempts = new(int)
	beadsExecCommandRunnerWithEnv = func(_ map[string]string) beads.CommandRunner {
		return func(_ string, _ string, _ ...string) ([]byte, error) {
			*attempts++
			return nil, fmt.Errorf("server unreachable at 127.0.0.1:3307")
		}
	}
	return attempts, countRecoverRuns(t)
}

// failingBdScope returns a distinct bd scope root under cityPath, so a
// sequence of failing reads spreads across per-scope transport breakers
// instead of tripping one of them and masking the recover throttle.
func failingBdScope(t *testing.T, cityPath string, i int) string {
	t.Helper()
	scope := filepath.Join(cityPath, fmt.Sprintf("scope%d", i))
	if err := os.MkdirAll(filepath.Join(scope, ".beads"), 0o755); err != nil {
		t.Fatal(err)
	}
	return scope
}

// Regression for ga-amol9: the bd-runner transport path used to reach
// the recover op on EVERY recoverable transport error, so a city whose
// bd reads kept timing out restarted its managed dolt once per failed
// call.
func TestBdTransportRecoverIsThrottledAcrossConsecutiveFailingReads(t *testing.T) {
	t.Setenv("GC_BEADS", "bd")
	cityPath := t.TempDir()
	stubRecoverGateClock(t)
	stubRecoverLiveness(t, managedDoltLivenessConfirmedDead)
	attempts, recoverRuns := installFailingBdTransport(t)

	runner := bdCommandRunnerWithManagedRetry(cityPath, func(_ string) map[string]string {
		return map[string]string{"GC_DOLT_PORT": "3307"}
	})

	// One scope per read, as in the incident: the ten orders that failed
	// in those three minutes were spread across the city's scopes, so the
	// per-scope transport breaker never fired. The cooldown is per city,
	// and has to hold across them all.
	const reads = 6
	for i := 0; i < reads; i++ {
		if _, err := runner(failingBdScope(t, cityPath, i), "bd", "list", "--json"); err == nil {
			t.Fatalf("read %d succeeded, want the stubbed transport failure", i+1)
		}
	}

	if *recoverRuns != 1 {
		t.Fatalf("recover runs = %d across %d consecutive failing reads inside the cooldown window, want 1", *recoverRuns, reads)
	}
	// The guard gates the recover, not the retry: every read still gets
	// its two attempts.
	if want := reads * 2; *attempts != want {
		t.Fatalf("bd attempts = %d, want %d (each read retried once)", *attempts, want)
	}
}

// A slow-but-alive dolt produces the same transport timeouts as a dead
// one. The bd runner must leave it running.
func TestBdTransportRecoverSkippedWhileManagedDoltIsLive(t *testing.T) {
	t.Setenv("GC_BEADS", "bd")
	cityPath := t.TempDir()
	stubRecoverGateClock(t)
	stubRecoverLiveness(t, managedDoltLivenessAlive)
	attempts, recoverRuns := installFailingBdTransport(t)

	runner := bdCommandRunnerWithManagedRetry(cityPath, func(_ string) map[string]string {
		return map[string]string{"GC_DOLT_PORT": "3307"}
	})

	const reads = 3
	for i := 0; i < reads; i++ {
		if _, err := runner(failingBdScope(t, cityPath, i), "bd", "list", "--json"); err == nil {
			t.Fatalf("read %d succeeded, want the stubbed transport failure", i+1)
		}
	}

	if *recoverRuns != 0 {
		t.Fatalf("recover runs = %d, want 0: a live managed dolt holding its port must not be replaced", *recoverRuns)
	}
	// A declined recover is not a transport fault and must still fall
	// through to the retry.
	if want := reads * 2; *attempts != want {
		t.Fatalf("bd attempts = %d, want %d (each read still retried once)", *attempts, want)
	}
}

package main

import (
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

	// Still inside the window one second short of its end.
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

// The health patrol and the bd-runner transport path must share ONE
// window, not two overlapping throttles: a recover taken by either path
// has to be visible to the other, or the pair can still restart the
// managed dolt twice in a row (ga-amol9).
func TestAdmitManagedDoltRecoverWindowIsSharedByHealthAndTransportPaths(t *testing.T) {
	cityPath := t.TempDir()
	advance := stubRecoverGateClock(t)

	// Health path takes the window; the transport path must see it.
	if !admitManagedDoltRecover(cityPath) {
		t.Fatal("health-path recover refused on a fresh city")
	}
	if admitTransportManagedDoltRecover(cityPath) {
		t.Fatal("transport path admitted a recover inside the window the health path just took")
	}

	advance(providerRecoverCooldown() + time.Second)

	// Transport path takes the next window; the health path must see it.
	if !admitTransportManagedDoltRecover(cityPath) {
		t.Fatal("transport-path recover refused after the window elapsed")
	}
	if admitManagedDoltRecover(cityPath) {
		t.Fatal("health path admitted a recover inside the window the transport path just took")
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

// A transport timeout is a client-side symptom, so it must never be
// allowed to replace a managed dolt that is alive and holding its port.
func TestAdmitTransportManagedDoltRecoverRefusesLiveManagedDolt(t *testing.T) {
	cityPath := t.TempDir()
	stubRecoverGateClock(t)
	_ = writeReachableManagedDoltState(t, cityPath)

	for i := 0; i < 3; i++ {
		if admitTransportManagedDoltRecover(cityPath) {
			t.Fatalf("attempt %d admitted a recover against a live, port-holding managed dolt", i+1)
		}
	}

	// Refusing on liveness must not burn the cooldown window: the health
	// patrol, which has real evidence of unhealthiness, still gets its
	// recover.
	if !admitManagedDoltRecover(cityPath) {
		t.Fatal("liveness refusals consumed the cooldown window; a genuine recover must not be delayed by them")
	}
}

func TestAdmitTransportManagedDoltRecoverAdmitsWhenNoManagedDoltRunning(t *testing.T) {
	cityPath := t.TempDir()
	stubRecoverGateClock(t)
	if err := os.MkdirAll(filepath.Join(cityPath, ".beads"), 0o700); err != nil {
		t.Fatal(err)
	}

	if !admitTransportManagedDoltRecover(cityPath) {
		t.Fatal("refused to start a managed dolt when none is running: the guard must not break genuine recovery")
	}
}

func TestAdmitTransportManagedDoltRecoverAdmitsWhenPublishedPidIsDead(t *testing.T) {
	cityPath := t.TempDir()
	stubRecoverGateClock(t)
	if err := os.MkdirAll(filepath.Join(cityPath, ".beads"), 0o700); err != nil {
		t.Fatal(err)
	}
	ln := listenOnRandomPort(t)
	t.Cleanup(func() { _ = ln.Close() })
	if err := writeDoltState(cityPath, doltRuntimeState{
		Running:   true,
		PID:       deadPID(t),
		Port:      ln.Addr().(*net.TCPAddr).Port,
		DataDir:   filepath.Join(cityPath, ".beads", "dolt"),
		StartedAt: time.Now().UTC().Format(time.RFC3339),
	}); err != nil {
		t.Fatal(err)
	}

	if !admitTransportManagedDoltRecover(cityPath) {
		t.Fatal("refused to replace a dead managed-dolt pid")
	}
}

func TestAdmitTransportManagedDoltRecoverAdmitsWhenPortIsUnreachable(t *testing.T) {
	cityPath := t.TempDir()
	stubRecoverGateClock(t)
	if err := os.MkdirAll(filepath.Join(cityPath, ".beads"), 0o700); err != nil {
		t.Fatal(err)
	}
	ln := listenOnRandomPort(t)
	port := ln.Addr().(*net.TCPAddr).Port
	if err := ln.Close(); err != nil {
		t.Fatal(err)
	}
	if err := writeDoltState(cityPath, doltRuntimeState{
		Running:   true,
		PID:       os.Getpid(),
		Port:      port,
		DataDir:   filepath.Join(cityPath, ".beads", "dolt"),
		StartedAt: time.Now().UTC().Format(time.RFC3339),
	}); err != nil {
		t.Fatal(err)
	}

	if !admitTransportManagedDoltRecover(cityPath) {
		t.Fatal("refused to replace a managed dolt that no longer holds its port")
	}
}

// installFailingBdTransport makes every bd invocation fail with a
// recoverable transport error and counts recover calls. The retry
// backoff is stubbed out so the test does not sleep.
func installFailingBdTransport(t *testing.T) (attempts, recoverCalls *int) {
	t.Helper()
	origRunner := beadsExecCommandRunnerWithEnv
	origRecover := recoverManagedBDCommand
	origSleep := bdCommandRetrySleep
	t.Cleanup(func() {
		beadsExecCommandRunnerWithEnv = origRunner
		recoverManagedBDCommand = origRecover
		bdCommandRetrySleep = origSleep
	})
	bdCommandRetrySleep = func(time.Duration) {}
	attempts = new(int)
	recoverCalls = new(int)
	beadsExecCommandRunnerWithEnv = func(_ map[string]string) beads.CommandRunner {
		return func(_ string, _ string, _ ...string) ([]byte, error) {
			*attempts++
			return nil, fmt.Errorf("server unreachable at 127.0.0.1:3307")
		}
	}
	recoverManagedBDCommand = func(_ string) error {
		*recoverCalls++
		return nil
	}
	return attempts, recoverCalls
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

// Regression for ga-amol9: the bd-runner transport path used to call
// recoverManagedBDCommand on EVERY recoverable transport error, so a
// city whose bd reads kept timing out restarted its managed dolt once
// per failed call.
func TestBdTransportRecoverIsThrottledAcrossConsecutiveFailingReads(t *testing.T) {
	t.Setenv("GC_BEADS", "bd")
	cityPath := t.TempDir()
	stubRecoverGateClock(t)
	attempts, recoverCalls := installFailingBdTransport(t)

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

	if *recoverCalls != 1 {
		t.Fatalf("recoverCalls = %d across %d consecutive failing reads inside the cooldown window, want 1", *recoverCalls, reads)
	}
	// The throttle gates the recover, not the retry: every read still
	// gets its two attempts.
	if want := reads * 2; *attempts != want {
		t.Fatalf("bd attempts = %d, want %d (each read retried once)", *attempts, want)
	}
}

// A slow-but-alive dolt produces the same transport timeouts as a dead
// one. The transport path must leave it running.
func TestBdTransportRecoverSkippedWhileManagedDoltIsLive(t *testing.T) {
	t.Setenv("GC_BEADS", "bd")
	cityPath := t.TempDir()
	stubRecoverGateClock(t)
	_ = writeReachableManagedDoltState(t, cityPath)
	attempts, recoverCalls := installFailingBdTransport(t)

	runner := bdCommandRunnerWithManagedRetry(cityPath, func(_ string) map[string]string {
		return map[string]string{"GC_DOLT_PORT": "3307"}
	})

	const reads = 3
	for i := 0; i < reads; i++ {
		if _, err := runner(failingBdScope(t, cityPath, i), "bd", "list", "--json"); err == nil {
			t.Fatalf("read %d succeeded, want the stubbed transport failure", i+1)
		}
	}

	if *recoverCalls != 0 {
		t.Fatalf("recoverCalls = %d, want 0: a live managed dolt holding its port must not be replaced", *recoverCalls)
	}
	if want := reads * 2; *attempts != want {
		t.Fatalf("bd attempts = %d, want %d (each read still retried once)", *attempts, want)
	}
}

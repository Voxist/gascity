package doctor

import (
	"testing"
	"time"
)

// fixedCeiling returns an injectable client idle-ceiling function for tests.
func fixedCeiling(d time.Duration) func() time.Duration {
	return func() time.Duration { return d }
}

func TestDoltTimeoutRaceCheck_OKWhenClientBelowServer(t *testing.T) {
	dir := setupManagedDoltCity(t)
	writeDoctorManagedDoltConfig(t, dir, map[string]any{
		"listener.read_timeout_millis": 30000, // live city value
	})
	c := NewDoltTimeoutRaceCheck(dir, false)
	c.clientIdleCeiling = fixedCeiling(10 * time.Second)
	r := c.Run(&CheckContext{})
	if r.Status != StatusOK {
		t.Fatalf("status = %d, want OK (10s < 30s); msg = %s", r.Status, r.Message)
	}
}

// The canonical vc-wz5 defect: client and server read timeouts EXACTLY EQUAL.
// Equality is a death match (server can kill an idle conn the client still
// trusts on the same tick), so the check must fail.
func TestDoltTimeoutRaceCheck_ErrorWhenEqual(t *testing.T) {
	dir := setupManagedDoltCity(t)
	writeDoctorManagedDoltConfig(t, dir, map[string]any{
		"listener.read_timeout_millis": 10000,
	})
	c := NewDoltTimeoutRaceCheck(dir, false)
	c.clientIdleCeiling = fixedCeiling(10 * time.Second)
	r := c.Run(&CheckContext{})
	if r.Status != StatusError {
		t.Fatalf("status = %d, want Error (10s >= 10s is the death match); msg = %s", r.Status, r.Message)
	}
	if r.Severity != SeverityBlocking {
		t.Fatalf("severity = %d, want SeverityBlocking (a death match must gate)", r.Severity)
	}
}

func TestDoltTimeoutRaceCheck_ErrorWhenClientAboveServer(t *testing.T) {
	dir := setupManagedDoltCity(t)
	writeDoctorManagedDoltConfig(t, dir, map[string]any{
		"listener.read_timeout_millis": 5000,
	})
	c := NewDoltTimeoutRaceCheck(dir, false)
	c.clientIdleCeiling = fixedCeiling(10 * time.Second)
	r := c.Run(&CheckContext{})
	if r.Status != StatusError {
		t.Fatalf("status = %d, want Error (10s >= 5s); msg = %s", r.Status, r.Message)
	}
}

// A pool with no client-side idle reaping (the pre-vc-wz5 configuration:
// connMaxLifetime=1h, no connMaxIdleTime) reports a zero ceiling and must fail
// regardless of the server timeout.
func TestDoltTimeoutRaceCheck_ErrorWhenNoIdleReaping(t *testing.T) {
	dir := setupManagedDoltCity(t)
	writeDoctorManagedDoltConfig(t, dir, map[string]any{
		"listener.read_timeout_millis": 30000,
	})
	c := NewDoltTimeoutRaceCheck(dir, false)
	c.clientIdleCeiling = fixedCeiling(0)
	r := c.Run(&CheckContext{})
	if r.Status != StatusError {
		t.Fatalf("status = %d, want Error (no idle reaping); msg = %s", r.Status, r.Message)
	}
}

func TestDoltTimeoutRaceCheck_SkippedWhenSkip(t *testing.T) {
	dir := setupManagedDoltCity(t)
	writeDoctorManagedDoltConfig(t, dir, map[string]any{
		"listener.read_timeout_millis": 5000, // would fail if not skipped
	})
	c := NewDoltTimeoutRaceCheck(dir, true)
	c.clientIdleCeiling = fixedCeiling(10 * time.Second)
	r := c.Run(&CheckContext{})
	if r.Status != StatusOK {
		t.Fatalf("status = %d, want OK when skipped; msg = %s", r.Status, r.Message)
	}
}

// End-to-end guard tying both halves of vc-wz5 together: the REAL shipped
// doltpool.IdleConnCeiling() must pass the check against both the live city
// server timeout (30s) and the managed default (15s). If a future change
// weakens the pool constants, this fails.
func TestDoltTimeoutRaceCheck_RealPoolCeilingPassesLiveAndDefault(t *testing.T) {
	for _, serverMillis := range []int{30000, 15000} {
		dir := setupManagedDoltCity(t)
		writeDoctorManagedDoltConfig(t, dir, map[string]any{
			"listener.read_timeout_millis": serverMillis,
		})
		c := NewDoltTimeoutRaceCheck(dir, false) // no injection: real IdleConnCeiling()
		r := c.Run(&CheckContext{})
		if r.Status != StatusOK {
			t.Fatalf("server=%dms: status = %d, want OK for shipped pool constants; msg = %s", serverMillis, r.Status, r.Message)
		}
	}
}

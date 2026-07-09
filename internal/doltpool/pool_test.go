package doltpool

import (
	"sync"
	"testing"
	"time"
)

// managedDoltDefaultReadTimeout mirrors config.DefaultDoltReadTimeoutMillis
// (15000ms). Duplicated as a local constant to keep this leaf package free of
// a dependency on internal/config; the dolt-timeout-race doctor check performs
// the authoritative comparison against the live/configured server value.
const managedDoltDefaultReadTimeout = 15 * time.Second

// TestIdleConnCeilingBreaksDeathMatch is the vc-wz5 regression guard: the pool
// must reap idle connections client-side strictly before the managed Dolt
// server's read_timeout would kill them. A zero ceiling (no idle reaping) or a
// ceiling >= the server read_timeout reintroduces the death match.
func TestIdleConnCeilingBreaksDeathMatch(t *testing.T) {
	got := IdleConnCeiling()
	if got <= 0 {
		t.Fatalf("IdleConnCeiling() = %v, want > 0 (idle reaping must be enabled; a 0 ceiling is the pre-vc-wz5 death match)", got)
	}
	if got >= managedDoltDefaultReadTimeout {
		t.Fatalf("IdleConnCeiling() = %v, want < %v (managed dolt read_timeout default); an idle conn held >= the server timeout is killed under the client", got, managedDoltDefaultReadTimeout)
	}
}

// TestPoolIdleReapingConstants pins the constants so an accidental revert to
// the death-match configuration (connMaxLifetime=1h, no connMaxIdleTime) fails
// loudly. connMaxIdleTime is the tighter bound and therefore the ceiling.
func TestPoolIdleReapingConstants(t *testing.T) {
	if connMaxIdleTime <= 0 {
		t.Fatalf("connMaxIdleTime = %v, want > 0 (idle reaping guard)", connMaxIdleTime)
	}
	if connMaxIdleTime >= managedDoltDefaultReadTimeout {
		t.Fatalf("connMaxIdleTime = %v, want < %v (managed dolt read_timeout default)", connMaxIdleTime, managedDoltDefaultReadTimeout)
	}
	// connMaxLifetime need not be below the server timeout for correctness (a
	// busy conn is never idle long enough to be killed; connMaxIdleTime is the
	// guard). But the vc-wz5 fix lowered it from time.Hour to short-lived
	// recycling — assert it stays well under a minute so a regression back to
	// the 1h death-match lifetime fails loudly.
	if connMaxLifetime > time.Minute {
		t.Fatalf("connMaxLifetime = %v, want <= 1m post-vc-wz5 (was time.Hour)", connMaxLifetime)
	}
	if connMaxIdleTime > connMaxLifetime {
		t.Fatalf("connMaxIdleTime (%v) > connMaxLifetime (%v): idle conns should be reaped no later than total-lifetime recycling", connMaxIdleTime, connMaxLifetime)
	}
	if want := connMaxIdleTime; IdleConnCeiling() != want {
		t.Fatalf("IdleConnCeiling() = %v, want %v (the tighter of connMaxIdleTime/connMaxLifetime)", IdleConnCeiling(), want)
	}
}

// resetForTest empties the registry so tests are order-independent.
func resetForTest(t *testing.T) {
	t.Helper()
	Shutdown()
	t.Cleanup(Shutdown)
}

func TestOpenReturnsSharedHandleForSameEndpoint(t *testing.T) {
	resetForTest(t)
	a, err := Open("127.0.0.1", "3307", "root", "pw", "hq")
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	b, err := Open("127.0.0.1", "3307", "root", "pw", "hq")
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if a != b {
		t.Fatal("Open returned distinct *sql.DB handles for the same endpoint")
	}
	if got := Len(); got != 1 {
		t.Fatalf("Len() = %d, want 1", got)
	}
}

func TestOpenSeparatesDistinctEndpoints(t *testing.T) {
	resetForTest(t)
	base, err := Open("127.0.0.1", "3307", "root", "pw", "hq")
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	cases := []struct {
		name                                 string
		host, port, user, password, database string
	}{
		{"different database", "127.0.0.1", "3307", "root", "pw", "vr"},
		{"different port", "127.0.0.1", "3308", "root", "pw", "hq"},
		{"different user", "127.0.0.1", "3307", "ops", "pw", "hq"},
		{"different password", "127.0.0.1", "3307", "root", "rotated", "hq"},
		{"empty database (server-level)", "127.0.0.1", "3307", "root", "pw", ""},
	}
	for _, tc := range cases {
		got, err := Open(tc.host, tc.port, tc.user, tc.password, tc.database)
		if err != nil {
			t.Fatalf("%s: Open: %v", tc.name, err)
		}
		if got == base {
			t.Errorf("%s: Open returned the base endpoint's handle, want a distinct pool", tc.name)
		}
	}
	if got := Len(); got != 1+len(cases) {
		t.Fatalf("Len() = %d, want %d", got, 1+len(cases))
	}
}

func TestPoolCapsConfigured(t *testing.T) {
	resetForTest(t)
	db, err := Open("127.0.0.1", "3307", "root", "pw", "hq")
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if got := db.Stats().MaxOpenConnections; got != maxOpenConns {
		t.Fatalf("MaxOpenConnections = %d, want %d", got, maxOpenConns)
	}
}

func TestShutdownEmptiesRegistryAndAllowsReopen(t *testing.T) {
	resetForTest(t)
	if _, err := Open("127.0.0.1", "3307", "root", "pw", "hq"); err != nil {
		t.Fatalf("Open: %v", err)
	}
	Shutdown()
	if got := Len(); got != 0 {
		t.Fatalf("Len() after Shutdown = %d, want 0", got)
	}
	if _, err := Open("127.0.0.1", "3307", "root", "pw", "hq"); err != nil {
		t.Fatalf("Open after Shutdown: %v", err)
	}
	if got := Len(); got != 1 {
		t.Fatalf("Len() = %d, want 1", got)
	}
}

func TestTotalOpenConnsZeroWithoutDials(t *testing.T) {
	resetForTest(t)
	// sql.Open is lazy: no server, no dial, zero open connections.
	if _, err := Open("127.0.0.1", "3307", "root", "pw", "hq"); err != nil {
		t.Fatalf("Open: %v", err)
	}
	if got := TotalOpenConns(); got != 0 {
		t.Fatalf("TotalOpenConns() = %d, want 0 before any query", got)
	}
}

func TestOpenConcurrentSameEndpoint(t *testing.T) {
	resetForTest(t)
	var wg sync.WaitGroup
	for i := 0; i < 16; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := Open("127.0.0.1", "3307", "root", "pw", "hq"); err != nil {
				t.Errorf("Open: %v", err)
			}
		}()
	}
	wg.Wait()
	if got := Len(); got != 1 {
		t.Fatalf("Len() = %d, want 1 after concurrent opens of one endpoint", got)
	}
}

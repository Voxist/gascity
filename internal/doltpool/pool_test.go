package doltpool

import (
	"sync"
	"testing"
)

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

package doltpool_test

import (
	"testing"

	"github.com/gastownhall/gascity/internal/doltpool"
)

func TestOpenReturnsSameDBForSameKey(t *testing.T) {
	t.Cleanup(func() { doltpool.Shutdown() })

	cfg := doltpool.Config{
		User:     "root",
		Password: "",
		Host:     "127.0.0.1",
		Port:     9999,
		Database: "testdb",
	}

	db1, err := doltpool.Open(cfg)
	if err != nil {
		t.Fatalf("first Open: %v", err)
	}
	db2, err := doltpool.Open(cfg)
	if err != nil {
		t.Fatalf("second Open: %v", err)
	}
	if db1 != db2 {
		t.Errorf("expected same *sql.DB for identical config, got two distinct values")
	}
}

func TestOpenReturnsDifferentDBForDifferentKey(t *testing.T) {
	t.Cleanup(func() { doltpool.Shutdown() })

	cfgA := doltpool.Config{User: "root", Host: "127.0.0.1", Port: 9001, Database: "a"}
	cfgB := doltpool.Config{User: "root", Host: "127.0.0.1", Port: 9001, Database: "b"}

	dbA, _ := doltpool.Open(cfgA)
	dbB, _ := doltpool.Open(cfgB)
	if dbA == dbB {
		t.Errorf("expected distinct *sql.DB for different databases")
	}
}

func TestShutdownClearsRegistry(t *testing.T) {
	cfg := doltpool.Config{User: "root", Host: "127.0.0.1", Port: 9999, Database: "testdb"}

	db1, _ := doltpool.Open(cfg)
	if db1 == nil {
		t.Fatal("expected non-nil *sql.DB")
	}
	doltpool.Shutdown()

	// After shutdown, Open should return a fresh (different) *sql.DB.
	db2, _ := doltpool.Open(cfg)
	t.Cleanup(func() { doltpool.Shutdown() })
	if db2 == nil {
		t.Fatal("expected non-nil *sql.DB after re-open")
	}
	if db1 == db2 {
		t.Errorf("expected fresh *sql.DB after Shutdown, got the same pointer")
	}
}

func TestTotalOpenConnsIsZeroWithNoConnections(t *testing.T) {
	t.Cleanup(func() { doltpool.Shutdown() })

	cfg := doltpool.Config{User: "root", Host: "127.0.0.1", Port: 9999, Database: "testdb"}
	if _, err := doltpool.Open(cfg); err != nil {
		t.Fatalf("Open: %v", err)
	}
	// sql.Open does not establish connections; Stats().OpenConnections == 0.
	if got := doltpool.TotalOpenConns(); got != 0 {
		t.Errorf("TotalOpenConns() = %d, want 0 (no live connections)", got)
	}
}

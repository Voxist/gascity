package main

import (
	"sync"
	"sync/atomic"
	"testing"
)

// TestBdAdmissionGlobalCapBoundsConcurrency asserts the global semaphore
// never admits more than its cap concurrently, even under a flood of
// goroutines across many scopes. Run with -race.
func TestBdAdmissionGlobalCapBoundsConcurrency(t *testing.T) {
	const global = 4
	a := newBdAdmission("/city", 0 /* per-scope disabled */, global)

	var concurrent atomic.Int64
	var peak atomic.Int64
	var wg sync.WaitGroup
	for i := 0; i < 200; i++ {
		wg.Add(1)
		go func(n int) {
			defer wg.Done()
			scope := "/city/rig" + string(rune('a'+n%8))
			release := a.acquire(scope)
			cur := concurrent.Add(1)
			for {
				old := peak.Load()
				if cur <= old || peak.CompareAndSwap(old, cur) {
					break
				}
			}
			concurrent.Add(-1)
			release()
		}(i)
	}
	wg.Wait()

	if got := peak.Load(); got > global {
		t.Fatalf("peak concurrent admissions = %d, exceeds global cap %d", got, global)
	}
	if got := a.inflightCount(); got != 0 {
		t.Fatalf("inflightCount() = %d after all releases, want 0", got)
	}
}

// TestBdAdmissionPerScopeCapBoundsConcurrency asserts each scope's
// semaphore independently bounds concurrency to the per-scope cap.
func TestBdAdmissionPerScopeCapBoundsConcurrency(t *testing.T) {
	const perScope = 2
	a := newBdAdmission("/city", perScope, 0 /* global disabled */)

	peaks := make(map[string]*atomic.Int64)
	curs := make(map[string]*atomic.Int64)
	scopes := []string{"/city/rig-a", "/city/rig-b", "/city/rig-c"}
	for _, s := range scopes {
		peaks[s] = &atomic.Int64{}
		curs[s] = &atomic.Int64{}
	}

	var wg sync.WaitGroup
	for i := 0; i < 300; i++ {
		scope := scopes[i%len(scopes)]
		wg.Add(1)
		go func(scope string) {
			defer wg.Done()
			release := a.acquire(scope)
			cur := curs[scope].Add(1)
			for {
				old := peaks[scope].Load()
				if cur <= old || peaks[scope].CompareAndSwap(old, cur) {
					break
				}
			}
			curs[scope].Add(-1)
			release()
		}(scope)
	}
	wg.Wait()

	for _, s := range scopes {
		if got := peaks[s].Load(); got > perScope {
			t.Fatalf("scope %s peak = %d, exceeds per-scope cap %d", s, got, perScope)
		}
	}
}

// TestBdAdmissionUnboundedWhenCapsDisabled asserts that non-positive caps
// admit everything without blocking (the breaker-disabled equivalent).
func TestBdAdmissionUnboundedWhenCapsDisabled(t *testing.T) {
	a := newBdAdmission("/city", 0, 0)
	releases := make([]func(), 0, 50)
	for i := 0; i < 50; i++ {
		releases = append(releases, a.acquire("/city/rig"))
	}
	if got := a.inflightCount(); got != 50 {
		t.Fatalf("inflightCount() = %d with caps disabled, want 50 (all admitted)", got)
	}
	for _, r := range releases {
		r()
	}
	if got := a.inflightCount(); got != 0 {
		t.Fatalf("inflightCount() = %d after releases, want 0", got)
	}
}

// TestBdInflightForCityUnknownCityIsZero asserts the gauge accessor is
// safe before any admission controller has been created for a city.
func TestBdInflightForCityUnknownCityIsZero(t *testing.T) {
	if got := bdInflightForCity("/no/such/city/ever"); got != 0 {
		t.Fatalf("bdInflightForCity(unknown) = %d, want 0", got)
	}
}

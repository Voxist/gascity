package main

import (
	"io"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
)

// bdAdmissionScope resolves the admission-semaphore scope key for a bd call,
// mirroring bdScopeBreaker: an empty dir snaps to the city scope so the
// per-scope and breaker keys agree.
func bdAdmissionScope(cityPath, dir string) string {
	scope := strings.TrimSpace(dir)
	if scope == "" {
		scope = cityPath
	}
	return scope
}

// bdAdmission is the process-wide subprocess admission controller for bd
// CLI calls (city-scale architecture plan item 1.9). It bounds the number
// of concurrent bd subprocesses two ways: a per-scope cap and a city-wide
// global cap. Together they put a hard ceiling on the subprocess amplifier
// (≥108 spawns/min at idle, 800/tick at 100 idle sessions) so a wedged
// backend or a fan-out burst cannot pile up unbounded processes.
//
// The semaphores are buffered channels: acquiring sends a token, releasing
// receives it. A zero-capacity cap means "unbounded" (the gate is skipped).
// gc_bd_inflight is a live gauge of currently-admitted bd calls, surfaced
// through BeadsDiagnostic for status observability.
type bdAdmission struct {
	mu       sync.Mutex
	cityPath string
	perScope int
	global   int
	globalCh chan struct{}
	scopeChs map[string]chan struct{}
	inflight atomic.Int64
}

// bdAdmissionRegistry holds one admission controller per city, created on
// first use with that city's [beads.resilience] caps. The caps are read
// once per process; changing them requires a controller restart (matching
// the breaker-registry lifetime).
var bdAdmissionRegistry = struct {
	mu          sync.Mutex
	controllers map[string]*bdAdmission
}{controllers: make(map[string]*bdAdmission)}

// bdAdmissionForCity returns the city's admission controller, creating it
// from the city's configured caps on first use.
func bdAdmissionForCity(cityPath string) *bdAdmission {
	key := filepath.Clean(cityPath)
	bdAdmissionRegistry.mu.Lock()
	if a, ok := bdAdmissionRegistry.controllers[key]; ok {
		bdAdmissionRegistry.mu.Unlock()
		return a
	}
	bdAdmissionRegistry.mu.Unlock()

	perScope, global := bdAdmissionCapsForCity(key)
	bdAdmissionRegistry.mu.Lock()
	defer bdAdmissionRegistry.mu.Unlock()
	if a, ok := bdAdmissionRegistry.controllers[key]; ok {
		return a
	}
	a := newBdAdmission(key, perScope, global)
	bdAdmissionRegistry.controllers[key] = a
	return a
}

// bdAdmissionCapsForCity resolves the per-scope and global bd inflight caps
// from the city's [beads.resilience] config, falling back to the defaults
// (4 and 16) when the config cannot be loaded.
func bdAdmissionCapsForCity(cityPath string) (perScope, global int) {
	cfg, err := loadCityConfig(cityPath, io.Discard)
	if err != nil || cfg == nil {
		return 4, 16
	}
	r := cfg.Beads.Resilience
	return r.MaxInflightPerScopeOrDefault(), r.MaxInflightGlobalOrDefault()
}

// newBdAdmission constructs an admission controller. A non-positive cap
// leaves the corresponding semaphore nil (unbounded).
func newBdAdmission(cityPath string, perScope, global int) *bdAdmission {
	a := &bdAdmission{
		cityPath: cityPath,
		perScope: perScope,
		global:   global,
		scopeChs: make(map[string]chan struct{}),
	}
	if global > 0 {
		a.globalCh = make(chan struct{}, global)
	}
	return a
}

// scopeChannel returns the per-scope semaphore channel for a scope root,
// creating it on first use. Returns nil when the per-scope cap is disabled.
func (a *bdAdmission) scopeChannel(scope string) chan struct{} {
	if a.perScope <= 0 {
		return nil
	}
	scope = filepath.Clean(scope)
	a.mu.Lock()
	defer a.mu.Unlock()
	ch, ok := a.scopeChs[scope]
	if !ok {
		ch = make(chan struct{}, a.perScope)
		a.scopeChs[scope] = ch
	}
	return ch
}

// acquire admits one bd call for a scope, blocking until both the global
// and the per-scope semaphore have a free slot. It returns a release func
// the caller MUST invoke (typically via defer) when the call returns.
// Acquisition order is global-then-scope; release is scope-then-global,
// the reverse order, so the two semaphores cannot deadlock against each
// other. The inflight gauge is incremented between the two acquisitions so
// it reflects admitted-and-running calls.
func (a *bdAdmission) acquire(scope string) func() {
	globalCh := a.globalCh
	scopeCh := a.scopeChannel(scope)
	if globalCh != nil {
		globalCh <- struct{}{}
	}
	if scopeCh != nil {
		scopeCh <- struct{}{}
	}
	a.inflight.Add(1)
	return func() {
		a.inflight.Add(-1)
		if scopeCh != nil {
			<-scopeCh
		}
		if globalCh != nil {
			<-globalCh
		}
	}
}

// inflightCount returns the number of currently-admitted bd calls. Used by
// the gc_bd_inflight gauge surfaced through BeadsDiagnostic.
func (a *bdAdmission) inflightCount() int {
	return int(a.inflight.Load())
}

// bdInflightForCity returns the city's current admitted bd-call count for
// the gc_bd_inflight gauge. Safe to call before any admission controller
// has been created (returns 0).
func bdInflightForCity(cityPath string) int {
	key := filepath.Clean(cityPath)
	bdAdmissionRegistry.mu.Lock()
	a, ok := bdAdmissionRegistry.controllers[key]
	bdAdmissionRegistry.mu.Unlock()
	if !ok {
		return 0
	}
	return a.inflightCount()
}

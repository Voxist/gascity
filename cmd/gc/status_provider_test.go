package main

import (
	"sync/atomic"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/runtime"
)

type statusProbeProvider struct {
	runtime.Provider
	delay       atomic.Int64
	running     atomic.Bool
	liveness    atomic.Value
	observeCall atomic.Int32
}

func newStatusProbeProvider() *statusProbeProvider {
	p := &statusProbeProvider{Provider: runtime.NewFake()}
	p.liveness.Store(runtime.Liveness{})
	return p
}

func (p *statusProbeProvider) IsRunning(string) bool {
	time.Sleep(time.Duration(p.delay.Load()))
	return p.running.Load()
}

func (p *statusProbeProvider) ObserveLiveness(string, []string) runtime.Liveness {
	p.observeCall.Add(1)
	return p.liveness.Load().(runtime.Liveness)
}

func TestStatusProviderTimeoutDoesNotStickAcrossCalls(t *testing.T) {
	origTimeout := statusProviderCallTimeout
	origWarn := statusProviderTimeoutWarning
	t.Cleanup(func() {
		statusProviderCallTimeout = origTimeout
		statusProviderTimeoutWarning = origWarn
	})
	statusProviderCallTimeout = 10 * time.Millisecond
	var warnings atomic.Int32
	statusProviderTimeoutWarning = func() {
		warnings.Add(1)
	}

	base := newStatusProbeProvider()
	base.running.Store(true)
	base.delay.Store(int64(100 * time.Millisecond))
	wrapped := newBoundedStatusProvider(base)

	if wrapped.IsRunning("worker") {
		t.Fatal("first IsRunning returned true, want timeout fallback false")
	}
	base.delay.Store(0)
	if !wrapped.IsRunning("worker") {
		t.Fatal("second IsRunning returned false, want fresh provider result after timeout")
	}
	if got := warnings.Load(); got != 1 {
		t.Fatalf("timeout warnings = %d, want 1", got)
	}
}

func TestStatusProbeTimeoutSelectsProxiedBound(t *testing.T) {
	proxied := true
	embedded := false
	cases := []struct {
		name string
		cfg  *config.City
		want time.Duration
	}{
		{"proxied", &config.City{Beads: config.BeadsConfig{Proxied: &proxied}}, statusProviderProxiedCallTimeout},
		{"embedded-false", &config.City{Beads: config.BeadsConfig{Proxied: &embedded}}, statusProviderCallTimeout},
		{"embedded-nil", &config.City{Beads: config.BeadsConfig{}}, statusProviderCallTimeout},
		{"nil-cfg", nil, statusProviderCallTimeout},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := statusProbeTimeout(tc.cfg); got != tc.want {
				t.Fatalf("statusProbeTimeout(%s) = %v, want %v", tc.name, got, tc.want)
			}
		})
	}
}

func TestStatusProviderPreservesNativeLivenessObservation(t *testing.T) {
	base := newStatusProbeProvider()
	base.liveness.Store(runtime.Liveness{Running: true, Alive: true})
	wrapped := newBoundedStatusProvider(base)

	got := runtime.ObserveLiveness(wrapped, "worker", []string{"agent"})
	if !got.Running || !got.Alive {
		t.Fatalf("ObserveLiveness = %#v, want running+alive from native observer", got)
	}
	if calls := base.observeCall.Load(); calls != 1 {
		t.Fatalf("ObserveLiveness calls = %d, want 1", calls)
	}
}

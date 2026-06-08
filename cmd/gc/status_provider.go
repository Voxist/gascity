package main

import (
	"context"
	"fmt"
	"os"
	"sync"
	"time"

	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/runtime"
)

var (
	statusProviderCallTimeout = 50 * time.Millisecond
	// statusProviderProxiedCallTimeout is the bounded deadline used when the
	// bd runtime is proxied-server. Status probes then traverse the
	// bd-subprocess / dbproxy round-trip (plus per-session/rig probing) and
	// routinely exceed the 50ms embedded deadline; 1s is large enough to
	// avoid false "probe timed out" warnings yet still bounds gc status so it
	// never hangs on a genuinely wedged runtime. Design range: 500ms–1s.
	statusProviderProxiedCallTimeout = 1 * time.Second
	statusProviderTimeoutWarning     = func() {
		fmt.Fprintln(os.Stderr, "gc status: runtime status probe timed out; using partial status")
	}
)

// statusProbeTimeout selects the bounded-status-call deadline for a city:
// the larger proxied bound when the bd runtime is proxied-server, otherwise
// the tight embedded default. A nil cfg (or embedded/unset Proxied) keeps the
// 50ms default so embedded latency is unchanged.
func statusProbeTimeout(cfg *config.City) time.Duration {
	if cfg != nil && cfg.Beads.ProxiedEnabled() {
		return statusProviderProxiedCallTimeout
	}
	return statusProviderCallTimeout
}

type statusProvider struct {
	base     runtime.Provider
	timeout  time.Duration
	warnOnce sync.Once
}

// newBoundedStatusProvider wraps base with the default embedded deadline,
// snapshotting the current statusProviderCallTimeout global at construction so
// the per-provider bound is fixed for the provider's lifetime.
func newBoundedStatusProvider(base runtime.Provider) runtime.Provider {
	return newBoundedStatusProviderWithTimeout(base, statusProviderCallTimeout)
}

// newBoundedStatusProviderWithTimeout wraps base so each status probe is
// bounded by d (d <= 0 means unbounded). An already-wrapped provider is
// returned unchanged so re-wrapping stays idempotent.
func newBoundedStatusProviderWithTimeout(base runtime.Provider, d time.Duration) runtime.Provider {
	if sp, ok := base.(*statusProvider); ok {
		return sp
	}
	return &statusProvider{base: base, timeout: d}
}

func boundedStatusCall[T any](p *statusProvider, fallback T, fn func() T) T {
	if p.timeout <= 0 {
		return fn()
	}
	resultCh := make(chan T, 1)
	go func() {
		resultCh <- fn()
	}()
	select {
	case result := <-resultCh:
		return result
	case <-time.After(p.timeout):
		p.warnOnce.Do(statusProviderTimeoutWarning)
		return fallback
	}
}

func (p *statusProvider) Start(ctx context.Context, name string, cfg runtime.Config) error {
	return p.base.Start(ctx, name, cfg)
}

func (p *statusProvider) Stop(name string) error {
	return p.base.Stop(name)
}

func (p *statusProvider) Interrupt(name string) error {
	return p.base.Interrupt(name)
}

func (p *statusProvider) IsRunning(name string) bool {
	return boundedStatusCall(p, false, func() bool {
		return p.base.IsRunning(name)
	})
}

func (p *statusProvider) IsAttached(name string) bool {
	return boundedStatusCall(p, false, func() bool {
		return p.base.IsAttached(name)
	})
}

func (p *statusProvider) Attach(name string) error {
	return p.base.Attach(name)
}

func (p *statusProvider) ProcessAlive(name string, processNames []string) bool {
	return boundedStatusCall(p, false, func() bool {
		return p.base.ProcessAlive(name, processNames)
	})
}

func (p *statusProvider) ObserveLiveness(name string, processNames []string) runtime.Liveness {
	return boundedStatusCall(p, runtime.Liveness{}, func() runtime.Liveness {
		return runtime.ObserveLiveness(p.base, name, processNames)
	})
}

func (p *statusProvider) Nudge(name string, content []runtime.ContentBlock) error {
	return p.base.Nudge(name, content)
}

func (p *statusProvider) SetMeta(name, key, value string) error {
	return p.base.SetMeta(name, key, value)
}

func (p *statusProvider) GetMeta(name, key string) (string, error) {
	result := boundedStatusCall(p, struct {
		value string
		err   error
	}{}, func() struct {
		value string
		err   error
	} {
		value, err := p.base.GetMeta(name, key)
		return struct {
			value string
			err   error
		}{value: value, err: err}
	})
	return result.value, result.err
}

func (p *statusProvider) RemoveMeta(name, key string) error {
	return p.base.RemoveMeta(name, key)
}

func (p *statusProvider) Peek(name string, lines int) (string, error) {
	result := boundedStatusCall(p, struct {
		value string
		err   error
	}{}, func() struct {
		value string
		err   error
	} {
		value, err := p.base.Peek(name, lines)
		return struct {
			value string
			err   error
		}{value: value, err: err}
	})
	return result.value, result.err
}

func (p *statusProvider) ListRunning(prefix string) ([]string, error) {
	result := boundedStatusCall(p, struct {
		value []string
		err   error
	}{}, func() struct {
		value []string
		err   error
	} {
		value, err := p.base.ListRunning(prefix)
		return struct {
			value []string
			err   error
		}{value: value, err: err}
	})
	return result.value, result.err
}

func (p *statusProvider) RouteACP(name string) {
	if router, ok := p.base.(interface{ RouteACP(string) }); ok {
		router.RouteACP(name)
	}
}

func (p *statusProvider) GetLastActivity(name string) (time.Time, error) {
	result := boundedStatusCall(p, struct {
		value time.Time
		err   error
	}{}, func() struct {
		value time.Time
		err   error
	} {
		value, err := p.base.GetLastActivity(name)
		return struct {
			value time.Time
			err   error
		}{value: value, err: err}
	})
	return result.value, result.err
}

func (p *statusProvider) ClearScrollback(name string) error {
	return p.base.ClearScrollback(name)
}

func (p *statusProvider) CopyTo(name, src, relDst string) error {
	return p.base.CopyTo(name, src, relDst)
}

func (p *statusProvider) SendKeys(name string, keys ...string) error {
	return p.base.SendKeys(name, keys...)
}

func (p *statusProvider) RunLive(name string, cfg runtime.Config) error {
	return p.base.RunLive(name, cfg)
}

func (p *statusProvider) Capabilities() runtime.ProviderCapabilities {
	return p.base.Capabilities()
}

func (p *statusProvider) Pending(name string) (*runtime.PendingInteraction, error) {
	ip, ok := p.base.(runtime.InteractionProvider)
	if !ok {
		return nil, nil
	}
	result := boundedStatusCall(p, struct {
		value *runtime.PendingInteraction
		err   error
	}{}, func() struct {
		value *runtime.PendingInteraction
		err   error
	} {
		value, err := ip.Pending(name)
		return struct {
			value *runtime.PendingInteraction
			err   error
		}{value: value, err: err}
	})
	return result.value, result.err
}

func (p *statusProvider) Respond(name string, response runtime.InteractionResponse) error {
	ip, ok := p.base.(runtime.InteractionProvider)
	if !ok {
		return runtime.ErrInteractionUnsupported
	}
	return ip.Respond(name, response)
}

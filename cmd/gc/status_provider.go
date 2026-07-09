package main

import (
	"context"
	"fmt"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/gastownhall/gascity/internal/runtime"
)

var (
	statusProviderCallTimeout    = 50 * time.Millisecond
	statusProviderTimeoutWarning = func() {
		fmt.Fprintln(os.Stderr, "gc status: runtime status probe timed out; using partial status")
	}
)

type statusProvider struct {
	base     runtime.Provider
	warnOnce sync.Once

	// Last-known-good liveness signals, served on a probe timeout so a slow
	// (but not hung) runtime renders its last observed state instead of a false
	// "dead". This is the status-CLI-only boundary (see newBoundedStatusProvider
	// callers: only newStatusSessionProviderForCity[WithSnapshot], used by
	// `gc status`/`gc city status`) — the reconciler/control plane uses the
	// UNBOUNDED provider, so stale values here can never drive a control decision.
	mu            sync.Mutex
	lastRunning   map[string]bool
	lastProcAlive map[string]bool
	lastLiveness  map[string]runtime.Liveness
}

// livenessKey namespaces a per-session last-good entry by the process-name set
// the probe was asked about (ProcessAlive/ObserveLiveness vary on it).
func livenessKey(name string, processNames []string) string {
	return name + "\x00" + strings.Join(processNames, "\x00")
}

var _ runtime.RelaunchProvider = (*statusProvider)(nil)

func newBoundedStatusProvider(base runtime.Provider) runtime.Provider {
	if sp, ok := base.(*statusProvider); ok {
		return sp
	}
	return &statusProvider{base: base}
}

func boundedStatusCall[T any](p *statusProvider, fallback T, fn func() T) T {
	if statusProviderCallTimeout <= 0 {
		return fn()
	}
	resultCh := make(chan T, 1)
	go func() {
		resultCh <- fn()
	}()
	select {
	case result := <-resultCh:
		return result
	case <-time.After(statusProviderCallTimeout):
		p.warnOnce.Do(statusProviderTimeoutWarning)
		return fallback
	}
}

// boundedStatusCallSWR is boundedStatusCall with stale-while-revalidate. fn runs
// under the status timeout and ALWAYS records its result as last-known-good via
// store — even when the bound has already elapsed, the goroutine keeps running,
// lands the value, and (as a side effect) refreshes the base StateCache. On a
// timeout we serve the last-known-good via load instead of the zero fallback, so
// a slow-but-live runtime shows its last observed state rather than a false
// "dead". Cold start (no last-good recorded yet) still returns zero, preserving
// the original timeout-fallback contract. The stored value converges to the
// truth within one probe completion, so a genuinely dead session self-corrects
// on the next render. Safe only because this wrapper is status-CLI-only.
func boundedStatusCallSWR[T any](p *statusProvider, zero T, load func() (T, bool), store func(T), fn func() T) T {
	if statusProviderCallTimeout <= 0 {
		r := fn()
		store(r)
		return r
	}
	resultCh := make(chan T, 1)
	go func() {
		r := fn()
		store(r)
		resultCh <- r
	}()
	select {
	case result := <-resultCh:
		return result
	case <-time.After(statusProviderCallTimeout):
		p.warnOnce.Do(statusProviderTimeoutWarning)
		if last, ok := load(); ok {
			return last
		}
		return zero
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
	return boundedStatusCallSWR(p, false,
		func() (bool, bool) {
			p.mu.Lock()
			defer p.mu.Unlock()
			v, ok := p.lastRunning[name]
			return v, ok
		},
		func(v bool) {
			p.mu.Lock()
			defer p.mu.Unlock()
			if p.lastRunning == nil {
				p.lastRunning = map[string]bool{}
			}
			p.lastRunning[name] = v
		},
		func() bool { return p.base.IsRunning(name) },
	)
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
	key := livenessKey(name, processNames)
	return boundedStatusCallSWR(p, false,
		func() (bool, bool) {
			p.mu.Lock()
			defer p.mu.Unlock()
			v, ok := p.lastProcAlive[key]
			return v, ok
		},
		func(v bool) {
			p.mu.Lock()
			defer p.mu.Unlock()
			if p.lastProcAlive == nil {
				p.lastProcAlive = map[string]bool{}
			}
			p.lastProcAlive[key] = v
		},
		func() bool { return p.base.ProcessAlive(name, processNames) },
	)
}

func (p *statusProvider) ObserveLiveness(name string, processNames []string) runtime.Liveness {
	key := livenessKey(name, processNames)
	return boundedStatusCallSWR(p, runtime.Liveness{},
		func() (runtime.Liveness, bool) {
			p.mu.Lock()
			defer p.mu.Unlock()
			v, ok := p.lastLiveness[key]
			return v, ok
		},
		func(v runtime.Liveness) {
			p.mu.Lock()
			defer p.mu.Unlock()
			if p.lastLiveness == nil {
				p.lastLiveness = map[string]runtime.Liveness{}
			}
			p.lastLiveness[key] = v
		},
		func() runtime.Liveness { return runtime.ObserveLiveness(p.base, name, processNames) },
	)
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

// Relaunch forwards a warm-box agent relaunch to the wrapped provider when it
// supports one, so the reconciler's RelaunchProvider type-assert is not masked
// by the status wrapper. Not bounded — it is a mutation, not a status probe.
func (p *statusProvider) Relaunch(ctx context.Context, name string, cfg runtime.Config) error {
	if rp, ok := p.base.(runtime.RelaunchProvider); ok {
		return rp.Relaunch(ctx, name, cfg)
	}
	return runtime.ErrRelaunchUnsupported
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

// Package storehealth implements the controller-internal store health
// patrol (city-scale architecture plan item 1.5). Per scope, every
// [storehealth] interval, it runs a two-probe matrix:
//
//   - Probe A: a routed-store read that forces a FRESH backend connection
//     (the HQ poison hit new opens only — a pooled ride-along would miss
//     it).
//   - Probe B: a direct SELECT 1 against the managed dolt endpoint.
//
// The decision matrix (all thresholds from config, no judgment calls in
// Go — the package holds only counters and comparisons):
//
//   - A-fail ∧ B-ok for ConsecutiveFails cycles ⇒ proxy poison. Capture
//     forensics FIRST (goroutine dump + lsof + log tail into a quarantine
//     dir), then reap the proxy via the injected lifecycle, trip the
//     scope breaker until A passes, emit proxy.reaped + store.degraded
//     (class transport). Rate-limited to one reap per scope per
//     ReapCooldown; a second poison inside the window is alert-only with
//     forensics kept.
//   - A-ok ∧ B-fail ⇒ trip the breaker + doctor.alert, NEVER auto-kill the
//     sql-server (emit store.degraded class backend).
//   - A-ok ∧ B-ok ⇒ healthy; reset the consecutive counter and, if the
//     scope was degraded, emit store.recovered.
//
// All side effects (probing, reaping, forensics, breaker, emission) are
// injected through Hooks so the state machine is unit-testable with fakes
// and holds no I/O of its own.
package storehealth

import (
	"context"
	"sync"
	"time"
)

// ProbeResult is the boolean outcome of one probe with an optional reason
// string for forensic events. Ok=true means the probe succeeded.
type ProbeResult struct {
	Ok     bool
	Reason string
}

// DegradeClass classifies a store degradation. The values mirror the
// events package's store-degraded class constants; the patrol passes them
// straight through to the emission hook.
type DegradeClass string

// Store degradation classes.
const (
	ClassTransport      DegradeClass = "transport"
	ClassBackend        DegradeClass = "backend"
	ClassWriteRejection DegradeClass = "write-rejection"
)

// Hooks are the injected side effects for one scope's patrol. Every field
// is required for a live patrol; tests supply fakes. The patrol never
// performs I/O directly — it only sequences these calls per the matrix.
type Hooks struct {
	// ProbeRoutedFresh runs probe A: a routed-store read forcing a fresh
	// backend connection (e.g. a one-shot `bd list --limit 1` subprocess).
	ProbeRoutedFresh func(ctx context.Context) ProbeResult
	// ProbeBackendDirect runs probe B: a direct SELECT 1 against the
	// managed dolt endpoint via the pooled connection.
	ProbeBackendDirect func(ctx context.Context) ProbeResult
	// CaptureForensics writes the pre-reap forensic bundle (SIGQUIT
	// goroutine dump, lsof, log tail) into a quarantine directory and
	// returns its path. Called before any reap.
	CaptureForensics func(ctx context.Context) (dir string, err error)
	// ReapProxy reaps the scope's db-proxy child(ren) via the existing
	// lifecycle and returns the number of PIDs signaled.
	ReapProxy func(ctx context.Context) (pidsSignaled int, err error)
	// TripBreaker opens the scope's transport breaker so callers fail fast
	// until probe A passes again. Idempotent.
	TripBreaker func()
	// EmitDegraded records a store.degraded event.
	EmitDegraded func(class DegradeClass, reason string, consecutiveFails int)
	// EmitRecovered records a store.recovered event.
	EmitRecovered func(class DegradeClass)
	// EmitProbeFailed records a per-cycle store.probe_failed breadcrumb.
	EmitProbeFailed func(probe, reason string)
	// EmitProxyReaped records a proxy.reaped event. rateLimited is true
	// when a poison inside the cooldown window suppressed the reap.
	EmitProxyReaped func(quarantineDir string, pidsSignaled int, rateLimited bool)
	// EmitDoctorAlert records a doctor.alert (used for the A-ok∧B-fail
	// backend-fault class, which never auto-kills the sql-server).
	EmitDoctorAlert func(detail string)
	// WriteProbe runs the write-path conformance probe: create+close one
	// ephemeral bead of each RequiredCustomType through the normal store
	// path. Ok=false with a reason means a persistent write rejection.
	// Optional: nil disables the write-path probe for the scope.
	WriteProbe func(ctx context.Context) ProbeResult
}

// Config holds the resolved thresholds for one scope's patrol. Durations
// and counts only — the patrol makes no judgment calls.
type Config struct {
	// ConsecutiveFails is how many A-fail∧B-ok cycles confirm a poison.
	ConsecutiveFails int
	// ReapCooldown is the minimum spacing between reaps for the scope.
	ReapCooldown time.Duration
	// WriteProbeEvery is the cadence of the write-path conformance probe.
	// Zero disables it even when Hooks.WriteProbe is set.
	WriteProbeEvery time.Duration
}

// scopeState is the per-scope mutable patrol state. All fields are guarded
// by mu so EvaluateCycle is safe to call from a single patrol goroutine
// while State() is read concurrently for diagnostics.
type scopeState struct {
	mu sync.Mutex
	// consecutiveTransportFails counts consecutive A-fail∧B-ok cycles.
	consecutiveTransportFails int
	// degraded is the current degradation class, or "" when healthy.
	degraded DegradeClass
	// lastReapAt is when the most recent reap fired (zero = never), used
	// for the cooldown rate-limit.
	lastReapAt time.Time
	// lastWriteProbeAt is when the write-path probe last ran.
	lastWriteProbeAt time.Time
}

// ScopePatrol runs the health matrix for one scope. Construct with
// NewScopePatrol; drive it with EvaluateCycle on the patrol ticker.
type ScopePatrol struct {
	cfg   Config
	hooks Hooks
	now   func() time.Time
	st    scopeState
}

// NewScopePatrol builds a patrol for one scope. now is injectable for
// deterministic tests; pass nil to use time.Now.
func NewScopePatrol(cfg Config, hooks Hooks, now func() time.Time) *ScopePatrol {
	if now == nil {
		now = time.Now
	}
	if cfg.ConsecutiveFails <= 0 {
		cfg.ConsecutiveFails = 3
	}
	return &ScopePatrol{cfg: cfg, hooks: hooks, now: now}
}

// EvaluateCycle runs one patrol cycle: probes A and B, applies the
// decision matrix, and performs the resulting side effects through the
// hooks. It is the single entry point so the matrix logic lives in one
// place. Safe to call serially from the patrol ticker.
func (p *ScopePatrol) EvaluateCycle(ctx context.Context) {
	a := p.hooks.ProbeRoutedFresh(ctx)
	b := p.hooks.ProbeBackendDirect(ctx)

	switch {
	case a.Ok && b.Ok:
		p.onHealthy(ctx)
	case !a.Ok && b.Ok:
		p.onTransportPoison(ctx, a.Reason)
	case a.Ok && !b.Ok:
		p.onBackendFault(b.Reason)
	default: // both fail
		p.onBothFail(a.Reason, b.Reason)
	}
}

// onHealthy resets the transport counter, emits store.recovered if the
// scope was degraded, and runs the write-path conformance probe at its
// configured cadence.
func (p *ScopePatrol) onHealthy(ctx context.Context) {
	p.st.mu.Lock()
	wasDegraded := p.st.degraded
	p.st.consecutiveTransportFails = 0
	// A transport recovery clears the transport class; a write-rejection
	// degradation only clears when the write probe passes (below).
	if p.st.degraded == ClassTransport || p.st.degraded == ClassBackend {
		p.st.degraded = ""
	}
	p.st.mu.Unlock()

	if wasDegraded == ClassTransport || wasDegraded == ClassBackend {
		p.hooks.EmitRecovered(wasDegraded)
	}

	p.maybeWriteProbe(ctx)
}

// onTransportPoison handles A-fail∧B-ok: the proxy poison signature. It
// counts toward the confirm threshold and, once reached, captures
// forensics, reaps (rate-limited), trips the breaker, and emits.
func (p *ScopePatrol) onTransportPoison(ctx context.Context, reason string) {
	p.hooks.EmitProbeFailed("routed", reason)

	p.st.mu.Lock()
	p.st.consecutiveTransportFails++
	count := p.st.consecutiveTransportFails
	confirmed := count >= p.cfg.ConsecutiveFails
	p.st.mu.Unlock()

	if !confirmed {
		return
	}

	// Forensics FIRST, always — even when the reap is rate-limited — so the
	// trigger stays diagnosable (reap-only destroys the evidence each
	// cycle, the rejected alternative in the plan).
	dir, _ := p.hooks.CaptureForensics(ctx)

	p.st.mu.Lock()
	withinCooldown := !p.st.lastReapAt.IsZero() && p.now().Sub(p.st.lastReapAt) < p.cfg.ReapCooldown
	p.st.degraded = ClassTransport
	p.st.mu.Unlock()

	p.hooks.TripBreaker()
	p.hooks.EmitDegraded(ClassTransport, reason, count)

	if withinCooldown {
		// A second poison inside the window: alert-only, keep forensics.
		p.hooks.EmitProxyReaped(dir, 0, true)
		return
	}

	pids, _ := p.hooks.ReapProxy(ctx)
	p.st.mu.Lock()
	p.st.lastReapAt = p.now()
	p.st.mu.Unlock()
	p.hooks.EmitProxyReaped(dir, pids, false)
}

// onBackendFault handles A-ok∧B-fail: the sql-server itself is unreachable
// or read-only. Trip the breaker and alert, but NEVER auto-kill the
// sql-server. Reset the transport counter (the proxy path is fine).
func (p *ScopePatrol) onBackendFault(reason string) {
	p.hooks.EmitProbeFailed("backend", reason)
	p.st.mu.Lock()
	p.st.consecutiveTransportFails = 0
	p.st.degraded = ClassBackend
	p.st.mu.Unlock()
	p.hooks.TripBreaker()
	p.hooks.EmitDegraded(ClassBackend, reason, 0)
	p.hooks.EmitDoctorAlert("managed dolt backend probe failed (A-ok, B-fail); sql-server not auto-killed: " + reason)
}

// onBothFail handles A-fail∧B-fail: the whole path is down. This is not the
// isolated proxy-poison signature (B is also failing), so it is treated as
// a backend-class degradation — trip + alert, never auto-kill — and the
// transport poison counter is not advanced (we cannot attribute the
// failure to the proxy when the direct backend is also unreachable).
func (p *ScopePatrol) onBothFail(reasonA, reasonB string) {
	p.hooks.EmitProbeFailed("routed", reasonA)
	p.hooks.EmitProbeFailed("backend", reasonB)
	p.st.mu.Lock()
	p.st.consecutiveTransportFails = 0
	p.st.degraded = ClassBackend
	p.st.mu.Unlock()
	p.hooks.TripBreaker()
	p.hooks.EmitDegraded(ClassBackend, reasonB, 0)
	p.hooks.EmitDoctorAlert("both store probes failed (A-fail, B-fail); sql-server not auto-killed")
}

// maybeWriteProbe runs the write-path conformance probe when due. A
// persistent write rejection (the post-cutover a74fefde8 class invisible
// to a transport-only breaker) feeds the same degraded quarantine path
// with class write-rejection. A passing probe clears a prior
// write-rejection degradation.
func (p *ScopePatrol) maybeWriteProbe(ctx context.Context) {
	if p.hooks.WriteProbe == nil || p.cfg.WriteProbeEvery <= 0 {
		return
	}
	p.st.mu.Lock()
	due := p.st.lastWriteProbeAt.IsZero() || p.now().Sub(p.st.lastWriteProbeAt) >= p.cfg.WriteProbeEvery
	if !due {
		p.st.mu.Unlock()
		return
	}
	p.st.lastWriteProbeAt = p.now()
	wasWriteRejected := p.st.degraded == ClassWriteRejection
	p.st.mu.Unlock()

	res := p.hooks.WriteProbe(ctx)
	if res.Ok {
		if wasWriteRejected {
			p.st.mu.Lock()
			p.st.degraded = ""
			p.st.mu.Unlock()
			p.hooks.EmitRecovered(ClassWriteRejection)
		}
		return
	}
	p.st.mu.Lock()
	p.st.degraded = ClassWriteRejection
	p.st.mu.Unlock()
	p.hooks.TripBreaker()
	p.hooks.EmitDegraded(ClassWriteRejection, res.Reason, 0)
}

// Degraded reports the scope's current degradation class, or "" when
// healthy. Safe for concurrent diagnostic reads.
func (p *ScopePatrol) Degraded() DegradeClass {
	p.st.mu.Lock()
	defer p.st.mu.Unlock()
	return p.st.degraded
}

// ConsecutiveTransportFails reports the current consecutive A-fail∧B-ok
// count. Test/diagnostic visibility.
func (p *ScopePatrol) ConsecutiveTransportFails() int {
	p.st.mu.Lock()
	defer p.st.mu.Unlock()
	return p.st.consecutiveTransportFails
}

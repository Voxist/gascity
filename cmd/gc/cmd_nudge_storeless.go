package main

import (
	"errors"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/gastownhall/gascity/internal/agent"
	"github.com/gastownhall/gascity/internal/api"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/runtime"
	"github.com/gastownhall/gascity/internal/session"
)

// vl-3hb WS-B: `gc session nudge` is ADR-0024's designated fallback delivery
// path when bd work-discovery is down, but its target resolution and shadow
// enqueue route through the SAME bead store (and shared Dolt server) as
// discovery — a store-level degradation used to disable discovery and its
// designated backup together. This file gives the nudge path a BOUNDED store
// attempt with a store-independent fallback transport: resolve the live
// session from the runtime provider (no store), enqueue into the flock'd
// state.json authority directly (shadow bead skipped), and report the path
// used. Feature-flagged default-off via GC_NUDGE_STORELESS_FALLBACK until the
// outage-soak regression passes.

// nudgeStorelessFallbackEnv gates the storeless fallback (default-off). Any of
// "1", "true", "yes", "on" (case-insensitive) enables it.
const nudgeStorelessFallbackEnv = "GC_NUDGE_STORELESS_FALLBACK"

// nudgeStoreBudget bounds the store-touching leg of nudge target resolution on
// the flagged path. Package var so hung-store tests can shrink it; <= 0 means
// unbounded (identical to the unflagged path's resolution).
var nudgeStoreBudget = 3 * time.Second

// errNudgeStoreBudgetExceeded classifies a store-resolution attempt that blew
// its bounded budget (hung open / store-slow), as opposed to an authoritative
// answer from a healthy store.
var errNudgeStoreBudgetExceeded = errors.New("nudge store resolution exceeded its bounded budget")

func parseNudgeStorelessFlag(raw string) bool {
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case "1", "true", "yes", "on":
		return true
	default:
		return false
	}
}

// nudgePath names the resolution/delivery path for reporting. Empty on the
// default store-backed path so the JSON field is omitted and legacy output
// stays byte-identical.
func (t nudgeTarget) nudgePath() string {
	if t.storeless {
		return "storeless-fallback"
	}
	return ""
}

// nudgePathSuffix is the human-output analog of nudgePath.
func (t nudgeTarget) nudgePathSuffix() string {
	if t.storeless {
		return " (storeless fallback)"
	}
	return ""
}

type nudgeStoreResolveResult struct {
	target nudgeTarget
	err    error
}

// resolveNudgeTargetViaStoreBounded runs the store-touching resolution leg
// under nudgeStoreBudget, mirroring api.withMailReadDeadline: if the budget
// fires, the resolution goroutine may keep running until the store call
// returns; its result is discarded through the buffered channel. A store-slow
// classification from a completed attempt is folded into the budget error so
// callers see one degradation signal.
func resolveNudgeTargetViaStoreBounded(cityPath string, cfg *config.City, identifier string) (nudgeTarget, error) {
	budget := nudgeStoreBudget
	if budget <= 0 {
		return resolveNudgeTargetViaStore(cityPath, cfg, identifier)
	}
	ch := make(chan nudgeStoreResolveResult, 1)
	go func() {
		defer func() {
			if recovered := recover(); recovered != nil {
				ch <- nudgeStoreResolveResult{err: fmt.Errorf("nudge store resolution panicked: %v", recovered)}
			}
		}()
		target, err := resolveNudgeTargetViaStore(cityPath, cfg, identifier)
		ch <- nudgeStoreResolveResult{target: target, err: err}
	}()
	timer := time.NewTimer(budget)
	defer timer.Stop()
	select {
	case res := <-ch:
		if res.err != nil && api.IsStoreSlowError(res.err) {
			return nudgeTarget{}, fmt.Errorf("%w: %w", errNudgeStoreBudgetExceeded, res.err)
		}
		return res.target, res.err
	case <-timer.C:
		return nudgeTarget{}, fmt.Errorf("%w (%s) resolving %q", errNudgeStoreBudgetExceeded, budget, identifier)
	}
}

// resolveNudgeTargetStoreless resolves a nudge target from the live runtime
// provider alone — no bead store, no Dolt. Candidate session names, most
// specific first: the config-derived name (session_template applied to the
// resolved agent identity), the deterministic tmux-safe encoding of the
// identifier, then the raw identifier. The first candidate with a live runtime
// session wins.
//
// The store-backed path reads the agent identity off the session bead and
// buildNudgeTarget resolves it to the config.Agent whose provider and session
// transport delivery branches on. Storeless, that identity comes from the live
// session's GC_AGENT / GC_TEMPLATE metadata (the same values the lifecycle
// stamps on every runtime session), with the caller's identifier as the last
// candidate — so a raw session name or a named-session alias still lands on
// the configured agent instead of the workspace default provider. Session id
// / continuation epoch are enriched best-effort the same way so queue items
// and generation matching keep working; queue items keep the caller's
// identifier as their Agent value (alias) so hook-drain matching is unchanged.
func resolveNudgeTargetStoreless(cityPath string, cfg *config.City, sp runtime.Provider, identifier string) (nudgeTarget, error) {
	if sp == nil {
		return nudgeTarget{}, fmt.Errorf("%w: %q (storeless resolution requires a runtime provider)", session.ErrSessionNotFound, identifier)
	}
	seen := map[string]bool{}
	var candidates []string
	add := func(name string) {
		name = strings.TrimSpace(name)
		if name == "" || seen[name] {
			return
		}
		seen[name] = true
		candidates = append(candidates, name)
	}
	if cfg != nil {
		if found, ok := resolveAgentIdentity(cfg, identifier, ""); ok {
			add(agent.SessionNameFor(loadedCityName(cfg, cityPath), found.QualifiedName(), cfg.Workspace.SessionTemplate))
		}
	}
	add(agent.SanitizeQualifiedNameForSession(identifier))
	add(identifier)
	for _, name := range candidates {
		if !sp.IsRunning(name) {
			continue
		}
		meta := func(key string) string {
			v, err := sp.GetMeta(name, key)
			if err != nil {
				return ""
			}
			return strings.TrimSpace(v)
		}
		target := buildNudgeTarget(cityPath, cfg, nudgeTargetFields{
			sessionName:       name,
			alias:             identifier,
			agentName:         firstNonEmpty(meta("GC_AGENT"), identifier),
			template:          meta("GC_TEMPLATE"),
			commonName:        identifier,
			sessionID:         meta("GC_SESSION_ID"),
			continuationEpoch: meta("GC_CONTINUATION_EPOCH"),
		})
		target.storeless = true
		return target, nil
	}
	return nudgeTarget{}, fmt.Errorf("%w: %q (storeless resolution found no live runtime session)", session.ErrSessionNotFound, identifier)
}

// sessionNudgeStorelessEntry is cmdSessionNudge's flagged entry: resolve city,
// config, and provider (none of which touch the bead store), then run the
// bounded-store-attempt + storeless-fallback orchestration.
func sessionNudgeStorelessEntry(identifier, message string, mode nudgeDeliveryMode, jsonOutput bool, stdout, stderr io.Writer) int {
	cityPath, err := resolveCity()
	if err != nil {
		fmt.Fprintf(stderr, "gc session nudge: %v\n", err) //nolint:errcheck // best-effort stderr
		return 1
	}
	cfg, err := loadCityConfig(cityPath, stderr)
	if err != nil {
		fmt.Fprintf(stderr, "gc session nudge: %v\n", err) //nolint:errcheck // best-effort stderr
		return 1
	}
	sp, err := newSessionProvider()
	if err != nil {
		fmt.Fprintf(stderr, "gc session nudge: %v\n", err) //nolint:errcheck // best-effort stderr
		return 1
	}
	return sessionNudgeWithStorelessFallback(cityPath, cfg, sp, identifier, message, mode, jsonOutput, stdout, stderr)
}

// sessionNudgeWithStorelessFallback attempts the normal store-backed
// resolution under the bounded budget and delivers normally when the store
// answers with a target. On ANY store-leg failure — budget exceeded, store
// slow, or an authoritative miss (an unreachable store presents as not-found
// today) — it re-resolves storeless against the live runtime and delivers with
// a nil store: live delivery goes provider-only (the worker handle's nil-store
// path) and queue delivery writes the flock'd authority directly, skipping the
// shadow bead. The degraded path is reported on stderr and in the JSON `path`
// field.
func sessionNudgeWithStorelessFallback(cityPath string, cfg *config.City, sp runtime.Provider, identifier, message string, mode nudgeDeliveryMode, jsonOutput bool, stdout, stderr io.Writer) int {
	target, err := resolveNudgeTargetViaStoreBounded(cityPath, cfg, identifier)
	if err == nil {
		return deliverSessionNudge(target, message, mode, jsonOutput, stdout, stderr)
	}
	fallbackTarget, fallbackErr := resolveNudgeTargetStoreless(cityPath, cfg, sp, identifier)
	if fallbackErr != nil {
		fmt.Fprintf(stderr, "gc session nudge: %v (storeless fallback: %v)\n", err, fallbackErr) //nolint:errcheck // best-effort stderr
		return 1
	}
	fmt.Fprintf(stderr, "gc session nudge: warning: store-backed resolution failed (%v); delivering via storeless fallback for %q\n", err, identifier) //nolint:errcheck // best-effort stderr
	return deliverSessionNudgeWithWorker(fallbackTarget, nil, sp, message, mode, jsonOutput, stdout, stderr)
}

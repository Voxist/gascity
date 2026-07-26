package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/doctor"
	"github.com/gastownhall/gascity/internal/events"
	"github.com/gastownhall/gascity/internal/storehealth"
)

// startStoreHealthPatrol launches the controller-internal store health
// patrol for every scope (city + bound rigs) when [storehealth] is enabled
// and the city is in proxied mode (the patrol exists to catch proxy poison;
// a non-proxied city has no db-proxy child to reap). One goroutine ticks
// all scopes serially every [storehealth] interval. Best-effort: a missing
// config or a non-proxied city is a clean no-op.
//
// The patrol is controller-driven and requires zero agents (SDK
// self-sufficiency): it composes existing infrastructure (the bd runner,
// the doltpool probe, the proxy reap lifecycle, the scope breaker) and
// emits typed events.
func (cs *controllerState) startStoreHealthPatrol(ctx context.Context) {
	cs.mu.RLock()
	cfg := cs.cfg
	cityPath := cs.cityPath
	ep := cs.eventProv
	cs.mu.RUnlock()
	if cfg == nil || cityPath == "" {
		return
	}
	if !cfg.StoreHealth.StoreHealthEnabledOrDefault() {
		return
	}
	if !cfg.Beads.ProxiedEnabled() {
		// No managed db-proxy child to probe/reap; the transport-poison
		// signature the patrol targets cannot occur off proxied mode.
		return
	}

	interval := cfg.StoreHealth.IntervalOrDefault()
	patrols := cs.buildScopePatrols(cfg, cityPath, ep)
	if len(patrols) == 0 {
		return
	}

	go func() {
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				for _, p := range patrols {
					if ctx.Err() != nil {
						return
					}
					p.EvaluateCycle(ctx)
				}
			}
		}
	}()
}

// buildScopePatrols constructs one *storehealth.ScopePatrol per scope
// (city + bound rigs), each wired to that scope's real probe/forensics/
// reap/breaker/emit hooks.
func (cs *controllerState) buildScopePatrols(cfg *config.City, cityPath string, ep events.Provider) []*storehealth.ScopePatrol {
	scopeCfg := storehealth.Config{
		ConsecutiveFails: cfg.StoreHealth.ConsecutiveFailsOrDefault(),
		ReapCooldown:     cfg.StoreHealth.ReapCooldownOrDefault(),
		WriteProbeEvery:  cfg.StoreHealth.WriteProbeIntervalOrDefault(),
	}
	scopes := storeHealthScopeRoots(cityPath, cfg)
	patrols := make([]*storehealth.ScopePatrol, 0, len(scopes))
	for _, scope := range scopes {
		hooks := cs.storeHealthHooks(cityPath, cfg, scope, ep)
		patrols = append(patrols, storehealth.NewScopePatrol(scopeCfg, hooks, nil))
	}
	return patrols
}

// storeHealthScopeRoots returns the canonical scope roots the patrol covers:
// the city root plus every bound rig (rigs with an empty path are skipped,
// matching buildStores).
func storeHealthScopeRoots(cityPath string, cfg *config.City) []string {
	roots := []string{filepath.Clean(cityPath)}
	for _, rig := range cfg.Rigs {
		if strings.TrimSpace(rig.Path) == "" {
			continue
		}
		roots = append(roots, resolveStoreScopeRoot(cityPath, rig.Path))
	}
	return roots
}

// storeHealthHooks builds the live side-effect hooks for one scope.
func (cs *controllerState) storeHealthHooks(cityPath string, cfg *config.City, scope string, ep events.Provider) storehealth.Hooks {
	emit := func(eventType, subject string, payload any) {
		if ep == nil {
			return
		}
		raw, err := json.Marshal(payload)
		if err != nil {
			return
		}
		ep.Record(events.Event{Type: eventType, Actor: eventActor(), Subject: subject, Payload: raw})
	}
	breaker := bdScopeBreaker(cityPath, scope)

	return storehealth.Hooks{
		ProbeRoutedFresh: func(ctx context.Context) storehealth.ProbeResult {
			return storeHealthProbeRoutedFresh(ctx, cityPath, cfg, scope)
		},
		ProbeBackendDirect: func(ctx context.Context) storehealth.ProbeResult {
			return storeHealthProbeBackendDirect(ctx, cityPath)
		},
		CaptureForensics: func(_ context.Context) (string, error) {
			return captureStoreHealthForensics(cityPath, cfg, scope)
		},
		ReapProxy: func(_ context.Context) (int, error) {
			return reapProxiedChildForScope(cityPath, cfg, scope), nil
		},
		TripBreaker: func() {
			// Open the scope breaker so subsequent bd calls fail fast with
			// ErrStoreUnavailable until a routed probe passes.
			breaker.Trip()
		},
		EmitDegraded: func(class storehealth.DegradeClass, reason string, consecutive int) {
			emit(events.StoreDegraded, scope, events.StoreDegradedPayload{
				Scope:            scope,
				Class:            string(class),
				Reason:           reason,
				ConsecutiveFails: consecutive,
			})
		},
		EmitRecovered: func(class storehealth.DegradeClass) {
			emit(events.StoreRecovered, scope, events.StoreRecoveredPayload{Scope: scope, Class: string(class)})
		},
		EmitProbeFailed: func(probe, reason string) {
			emit(events.StoreProbeFailed, scope, events.StoreProbeFailedPayload{Scope: scope, Probe: probe, Reason: reason})
		},
		EmitProxyReaped: func(dir string, pids int, rateLimited bool) {
			emit(events.ProxyReaped, scope, events.ProxyReapedPayload{
				Scope:         scope,
				QuarantineDir: dir,
				PIDsSignaled:  pids,
				RateLimited:   rateLimited,
			})
		},
		EmitDoctorAlert: func(detail string) {
			emit(events.DoctorAlert, scope, events.DoctorAlertPayload{
				Check:   "store_health_patrol",
				Detail:  detail,
				Subject: scope,
			})
		},
		WriteProbe: func(ctx context.Context) storehealth.ProbeResult {
			return storeHealthWriteProbe(ctx, cityPath, cfg, scope)
		},
	}
}

// bdRunnerForScope returns the bd command runner for a scope: the rig runner
// for a bound-rig scope, or the city runner when the scope is the city root.
func bdRunnerForScope(cityPath string, cfg *config.City, scope string) beads.CommandRunner {
	if samePath(scope, cityPath) {
		return bdCommandRunnerForCity(cityPath)
	}
	return bdCommandRunnerForRig(cityPath, cfg, scope)
}

// storeHealthProbeRoutedFresh runs probe A: a one-shot `bd list --limit 1`
// against the scope. A fresh subprocess forces a fresh backend connection
// (the HQ poison hit new opens only). It deliberately bypasses the scope
// breaker gate so it can serve as the recovery probe even while the breaker
// is open; a success records on the breaker, closing it.
func storeHealthProbeRoutedFresh(_ context.Context, cityPath string, cfg *config.City, scope string) storehealth.ProbeResult {
	runner := bdRunnerForScope(cityPath, cfg, scope)
	out, err := runner(scope, "bd", "list", "--limit", "1")
	if err != nil {
		// A breaker-open error is itself the degraded signal the patrol is
		// confirming; treat any routed failure as probe-A-fail.
		reason := strings.TrimSpace(err.Error())
		if reason == "" {
			reason = "routed store probe failed"
		}
		return storehealth.ProbeResult{Ok: false, Reason: reason}
	}
	// Record a routed success on the scope breaker so a recovered routed
	// read closes the breaker the TripBreaker hook opened.
	bdScopeBreaker(cityPath, scope).RecordSuccess()
	_ = out
	return storehealth.ProbeResult{Ok: true}
}

// storeHealthProbeBackendDirect runs probe B: a direct SELECT 1 against the
// managed dolt endpoint via the pooled connection. It resolves the live
// managed port from the process-owned runtime state (never the port file).
func storeHealthProbeBackendDirect(_ context.Context, cityPath string) storehealth.ProbeResult {
	port := currentResolvableManagedDoltPort(cityPath)
	if port == "" {
		return storehealth.ProbeResult{Ok: false, Reason: "managed dolt port unresolved"}
	}
	if err := managedDoltQueryProbeDirectFn(defaultManagedDoltHost, port, "root"); err != nil {
		reason := strings.TrimSpace(err.Error())
		if reason == "" {
			reason = "backend SELECT probe failed"
		}
		return storehealth.ProbeResult{Ok: false, Reason: reason}
	}
	return storehealth.ProbeResult{Ok: true}
}

// storeHealthWriteProbe runs the write-path conformance probe: it asks bd to
// create+close one ephemeral bead of each RequiredCustomType through the
// normal store path. A persistent type-rejection (the post-cutover
// a74fefde8 class) returns Ok=false so the patrol degrades class
// write-rejection. Implemented as a single best-effort bd round-trip per
// type; a transport error is NOT a write rejection (the transport breaker
// owns that), so it returns Ok=true to avoid double-counting.
func storeHealthWriteProbe(_ context.Context, cityPath string, cfg *config.City, scope string) storehealth.ProbeResult {
	runner := bdRunnerForScope(cityPath, cfg, scope)
	for _, typ := range requiredCustomTypeNames() {
		out, err := runner(scope, "bd", "create",
			"--type", typ,
			"--title", "gc storehealth write-probe",
			"--json")
		if err == nil {
			// Clean up the probe bead best-effort; its persistence is not the
			// signal — its acceptance is.
			if id := beadIDFromCreateJSON(out); id != "" {
				_, _ = runner(scope, "bd", "delete", id, "--force")
			}
			continue
		}
		msg := strings.ToLower(err.Error())
		// Transport-class failures belong to the breaker, not the write
		// probe: skip the whole probe so we don't misclassify a wedged proxy
		// as a type rejection.
		if isTransportClassMessage(msg) {
			return storehealth.ProbeResult{Ok: true}
		}
		if strings.Contains(msg, "invalid issue type") || strings.Contains(msg, "invalid type") || strings.Contains(msg, "unknown type") {
			return storehealth.ProbeResult{Ok: false, Reason: fmt.Sprintf("type %q rejected: %s", typ, strings.TrimSpace(err.Error()))}
		}
		// Any other application error is inconclusive for the write-rejection
		// class; treat as pass so the patrol does not flap on unrelated errors.
	}
	return storehealth.ProbeResult{Ok: true}
}

// isTransportClassMessage reports whether a lowercased error message matches
// the pinned transport-failure marker table.
func isTransportClassMessage(lowerMsg string) bool {
	for _, marker := range bdTransportRetryableMarkers {
		if strings.Contains(lowerMsg, marker) {
			return true
		}
	}
	return false
}

// beadIDFromCreateJSON best-effort extracts the created bead id from a
// `bd create --json` response.
func beadIDFromCreateJSON(out []byte) string {
	var payload struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(bytes.TrimSpace(out), &payload); err != nil {
		return ""
	}
	return strings.TrimSpace(payload.ID)
}

// captureStoreHealthForensics writes the pre-reap forensic bundle into
// .gc/trace/quarantine/<scope-token>-<seq>/ and returns the directory path.
// The sequence is the deterministic monotonic counter from storehealth, not
// wall-clock/random. The bundle is best-effort: SIGQUIT to the scope's
// db-proxy child(ren) (goroutine dump to their own log), an lsof snapshot of
// their connections, and the last ~200 lines of proxy.log.
func captureStoreHealthForensics(cityPath string, cfg *config.City, scope string) (string, error) {
	root := filepath.Join(cityPath, ".gc", "trace", "quarantine")
	if err := os.MkdirAll(root, 0o755); err != nil {
		return "", fmt.Errorf("creating quarantine root: %w", err)
	}
	dir, _ := storehealth.QuarantineDirPath(root, scope)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", fmt.Errorf("creating quarantine dir %s: %w", dir, err)
	}

	pids := proxyChildPIDsForScope(cityPath, cfg, scope)

	// SIGQUIT each proxy child so it dumps its goroutines into its own log,
	// then snapshot the connections it holds. SIGQUIT is forensic, not fatal
	// to the reap decision — the reap (SIGTERM/SIGKILL) follows separately.
	for _, pid := range pids {
		_ = syscall.Kill(pid, syscall.SIGQUIT)
		captureLsofForPID(dir, pid)
	}

	captureProxyLogTail(dir, cityPath, scope)
	writeForensicsSummary(dir, scope, pids)
	return dir, nil
}

// captureLsofForPID writes `lsof -p <pid>` output into the quarantine dir.
// It reuses lsofOutput so the forensics probe inherits the shared timeout,
// process-group isolation, and SIGKILL-on-cancel guards — a hung lsof against
// a dead process must not stall the patrol.
func captureLsofForPID(dir string, pid int) {
	out, err := lsofOutput("-n", "-P", "-p", fmt.Sprintf("%d", pid))
	if err != nil && len(out) == 0 {
		return
	}
	_ = os.WriteFile(filepath.Join(dir, fmt.Sprintf("lsof-%d.txt", pid)), out, 0o644)
}

// captureProxyLogTail copies the last ~200 lines of the scope's proxy.log
// into the quarantine dir.
func captureProxyLogTail(dir, cityPath, scope string) {
	logPath := filepath.Join(scopeBeadsDir(cityPath, scope), "proxieddb", "proxy.log")
	data, err := os.ReadFile(logPath)
	if err != nil {
		return
	}
	_ = os.WriteFile(filepath.Join(dir, "proxy.log.tail"), lastNLines(data, 200), 0o644)
}

// writeForensicsSummary records a small machine-readable manifest of what
// the bundle contains so a later reader knows the scope and signaled PIDs.
func writeForensicsSummary(dir, scope string, pids []int) {
	summary := struct {
		Scope        string `json:"scope"`
		PIDsSignaled []int  `json:"pids_signaled"`
	}{Scope: scope, PIDsSignaled: pids}
	if raw, err := json.MarshalIndent(summary, "", "  "); err == nil {
		_ = os.WriteFile(filepath.Join(dir, "summary.json"), raw, 0o644)
	}
}

// lastNLines returns the last n newline-delimited lines of data.
func lastNLines(data []byte, n int) []byte {
	lines := bytes.Split(data, []byte("\n"))
	if len(lines) <= n {
		return data
	}
	return bytes.Join(lines[len(lines)-n:], []byte("\n"))
}

// scopeBeadsDir returns the .beads directory for a scope root.
func scopeBeadsDir(cityPath, scope string) string {
	if strings.TrimSpace(scope) == "" {
		scope = cityPath
	}
	return filepath.Join(scope, ".beads")
}

// proxyChildPIDsForScope returns the db-proxy-child PIDs whose --root lives
// under the scope's .beads directory. Discovery is by live process table,
// never a pidfile (the process table is the single source of truth).
func proxyChildPIDsForScope(cityPath string, _ *config.City, scope string) []int {
	out, err := exec.Command("ps", "-axww", "-o", "pid=,command=").Output() //nolint:gosec // fixed argv
	if err != nil {
		return nil
	}
	return proxyChildPIDsFromPS(string(out), []string{scopeBeadsDir(cityPath, scope)})
}

// reapProxiedChildForScope reaps only the given scope's db-proxy child(ren)
// via the existing signalProxyChild lifecycle (SIGTERM→grace→SIGKILL),
// returning the number of PIDs signaled. Per-scope variant of
// reapProxiedChildrenForCity so a poison in one scope never disturbs the
// others.
func reapProxiedChildForScope(cityPath string, cfg *config.City, scope string) int {
	if cfg == nil || !cfg.Beads.ProxiedEnabled() {
		return 0
	}
	pids := proxyChildPIDsForScope(cityPath, cfg, scope)
	for _, pid := range pids {
		signalProxyChild(pid)
	}
	return len(pids)
}

// requiredCustomTypeNames returns the bead types every Gas City scope must
// accept, sourced from the doctor package's RequiredCustomTypes so the patrol
// and the preflight stay aligned. Indirected through a var for tests.
var requiredCustomTypeNames = func() []string {
	return append([]string(nil), doctor.RequiredCustomTypes...)
}

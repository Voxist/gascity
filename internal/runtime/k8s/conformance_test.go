package k8s

import (
	"fmt"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/gastownhall/gascity/internal/runtime"
	"github.com/gastownhall/gascity/internal/runtime/runtimetest"
)

// TestK8sSeamBackedConformance proves NewSeamBacked()'s runtime.Provider
// contract against a fake k8sOps double (createPod/getPod/deletePod/
// listPods/execInPod), so the suite runs unconditionally — no live or kind
// Kubernetes cluster, no env-var opt-in. It shares the exact seam-wrapping
// path (newSeamBackedFromProvider) with the production NewSeamBacked(), so
// only the ops implementation differs from production.
//
// Scope: the real client-go Kubernetes API calls in realK8sOps (pod create/
// get/delete/list, SPDY exec) are not exercised by this proof — that
// transport is client-go's own concern, not this package's logic. What this
// proves is the provider logic built on top of k8sOps: pod lifecycle
// bookkeeping (Start/Stop/duplicate detection/stale-pod recreation),
// the tmux-environment meta sidecar (SetMeta/GetMeta/RemoveMeta), and the
// carrier-driven signaling (SendKeys/Nudge/Interrupt/Peek/ClearScrollback)
// — plus the seam wrapping itself.
func TestK8sSeamBackedConformance(t *testing.T) {
	var counter int64
	runtimetest.RunProviderTests(t, func(t *testing.T) (runtime.Provider, runtime.Config, string) {
		t.Helper()
		n := atomic.AddInt64(&counter, 1)
		ops := newFakeK8sOps()
		ops.execFunc = newConformanceExecFunc()
		raw := newProviderWithOps(ops)
		// prebaked skips the init-container staging + workspace-ready-touch
		// path (provider.go Start), which assumes a real entrypoint script
		// running inside the pod; nothing here executes that script, so
		// those steps would otherwise hang waiting for a file no one writes.
		raw.prebaked = true
		return newSeamBackedFromProvider(raw), runtime.Config{}, fmt.Sprintf("k8sconf-%d", n)
	})
}

// newConformanceExecFunc returns a minimal, stateful tmux-environment
// simulator for execInPod: `tmux set-environment`/`show-environment`
// round-trip through an in-memory per-pod map, which is SetMeta/GetMeta/
// RemoveMeta's backing store (provider.go). Every other command (has-session,
// pipe-pane, capture-pane, send-keys, …) succeeds with empty output, matching
// fakeK8sOps's existing unconfigured default — this only adds the one piece
// of state the generic default can't fake.
func newConformanceExecFunc() func(pod string, cmd []string) (string, error) {
	var mu sync.Mutex
	env := map[string]map[string]string{}
	return func(pod string, cmd []string) (string, error) {
		mu.Lock()
		defer mu.Unlock()
		switch {
		case matchesTmuxVerb(cmd, "set-environment"):
			rest := stripDashT(cmd[2:])
			if len(rest) >= 2 && rest[0] == "-u" {
				delete(env[pod], rest[1])
				return "", nil
			}
			if len(rest) >= 2 {
				if env[pod] == nil {
					env[pod] = map[string]string{}
				}
				env[pod][rest[0]] = rest[1]
			}
			return "", nil
		case matchesTmuxVerb(cmd, "show-environment"):
			rest := stripDashT(cmd[2:])
			if len(rest) >= 1 {
				if val, ok := env[pod][rest[0]]; ok {
					return rest[0] + "=" + val, nil
				}
			}
			return "", nil
		default:
			return "", nil
		}
	}
}

// matchesTmuxVerb reports whether cmd is a `tmux <verb> ...` invocation.
func matchesTmuxVerb(cmd []string, verb string) bool {
	return len(cmd) >= 2 && cmd[0] == "tmux" && cmd[1] == verb
}

// stripDashT drops a leading "-t <target>" pair, if present.
func stripDashT(args []string) []string {
	if len(args) >= 2 && args[0] == "-t" {
		return args[2:]
	}
	return args
}

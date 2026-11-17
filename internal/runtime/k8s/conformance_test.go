package k8s

import (
	"fmt"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/gastownhall/gascity/internal/runtime"
	"github.com/gastownhall/gascity/internal/runtime/runtimetest"
)

// TestK8sSeamBackedConformance runs the shared runtime.Provider contract
// against the seam-backed k8s provider on a fake k8sOps, so it runs in every
// lane with no Kubernetes cluster. newSeamBackedFromProvider is the wrapping
// NewSeamBacked applies, so only the ops differ from production.
//
// It proves the provider logic built on k8sOps: pod lifecycle (Start, Stop,
// duplicate detection, stale-pod replacement), the tmux-environment meta store
// (SetMeta, GetMeta, RemoveMeta), the carrier-driven input and output
// (SendKeys, Nudge, Interrupt, Peek, ClearScrollback), and the seam wrapping.
// The client-go calls in realK8sOps (pod CRUD, SPDY exec) are not exercised.
//
// It is not the ledger's proof for runtime.builtin.k8s: NewSeamBacked returns
// (runtime.Provider, error), and providerledger.ValidateProofRefs needs a
// factory that returns the ledger's constructor call directly.
func TestK8sSeamBackedConformance(t *testing.T) {
	var counter int64
	runtimetest.RunProviderTests(t, func(t *testing.T) (runtime.Provider, runtime.Config, string) {
		t.Helper()
		ops := newFakeK8sOps()
		ops.execFunc = newConformanceExecFunc()
		raw := newProviderWithOps(ops)
		// A prebaked image skips the init-container staging and the
		// workspace-ready wait, which need the pod's entrypoint script to run.
		raw.prebaked = true
		return newSeamBackedFromProvider(raw), runtime.Config{}, fmt.Sprintf("k8sconf-%d", atomic.AddInt64(&counter, 1))
	})
}

// newConformanceExecFunc answers execInPod for the conformance suite: tmux
// set-environment and show-environment round-trip through a per-pod map, the
// store behind SetMeta, GetMeta and RemoveMeta. Every other command succeeds
// with empty output, as the fake does by default.
func newConformanceExecFunc() func(pod string, cmd []string) (string, error) {
	var mu sync.Mutex
	env := map[string]map[string]string{}
	return func(pod string, cmd []string) (string, error) {
		mu.Lock()
		defer mu.Unlock()
		switch {
		case isTmuxVerb(cmd, "set-environment"):
			rest := withoutTarget(cmd[2:])
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
		case isTmuxVerb(cmd, "show-environment"):
			rest := withoutTarget(cmd[2:])
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

// isTmuxVerb reports whether cmd is a `tmux <verb> ...` invocation.
func isTmuxVerb(cmd []string, verb string) bool {
	return len(cmd) >= 2 && cmd[0] == "tmux" && cmd[1] == verb
}

// withoutTarget drops a leading "-t <target>" pair, if present.
func withoutTarget(args []string) []string {
	if len(args) >= 2 && args[0] == "-t" {
		return args[2:]
	}
	return args
}

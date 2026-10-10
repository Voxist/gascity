package hybrid

import (
	"fmt"
	"sync/atomic"
	"testing"

	"github.com/gastownhall/gascity/internal/runtime"
	"github.com/gastownhall/gascity/internal/runtime/runtimetest"
)

// hybridConformanceCounter is package-level (not a local var in
// TestHybridConformance) so the test function's body is exactly the one
// contract-runner call the provider ledger's ValidateProofRefs requires, with
// nothing preceding it to classify.
var hybridConformanceCounter int64

// TestHybridConformance proves the composite Provider's own routing/wrapping
// logic against the shared runtime.Provider contract, using two
// runtime.NewFake() backends and the default no-remote-match pattern (every
// session routes to local) — mirroring how runtime.composition.auto proves
// its default route. The remote-match route is covered by this package's
// focused tests (TestStart_RoutesToRemote and friends); the registry's real
// tmux+k8s backend composition (cmd/gc.newHybridProvider) is covered by the
// runtime.builtin.tmux and runtime.builtin.k8s entries, not by this proof.
func TestHybridConformance(t *testing.T) {
	runtimetest.RunProviderTests(t, func(_ *testing.T) (runtime.Provider, runtime.Config, string) {
		return New(runtime.NewFake(), runtime.NewFake(), func(string) bool { return false }), runtime.Config{}, fmt.Sprintf("hybrid-conform-%d", atomic.AddInt64(&hybridConformanceCounter, 1))
	})
}

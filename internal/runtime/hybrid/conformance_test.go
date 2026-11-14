package hybrid

import (
	"fmt"
	"sync/atomic"
	"testing"

	"github.com/gastownhall/gascity/internal/runtime"
	"github.com/gastownhall/gascity/internal/runtime/runtimetest"
)

// TestHybridConformance proves the composite Provider's own routing and
// wrapping against the shared runtime.Provider contract, using two
// runtime.NewFake backends. hybridConformanceRemote sends every other session
// to the remote backend, so both routes and the merged listing are exercised.
// The registry's real tmux+k8s backends in cmd/gc.newHybridProvider are not.
func TestHybridConformance(t *testing.T) {
	var counter int64

	runtimetest.RunProviderTests(t, func(_ *testing.T) (runtime.Provider, runtime.Config, string) {
		return New(runtime.NewFake(), runtime.NewFake(), hybridConformanceRemote), runtime.Config{}, fmt.Sprintf("hybrid-conform-%d", atomic.AddInt64(&counter, 1))
	})
}

// hybridConformanceRemote routes the conformance sessions whose name ends in
// an odd digit to the remote backend and the rest to the local one.
func hybridConformanceRemote(name string) bool {
	if name == "" {
		return false
	}
	switch name[len(name)-1] {
	case '1', '3', '5', '7', '9':
		return true
	}
	return false
}

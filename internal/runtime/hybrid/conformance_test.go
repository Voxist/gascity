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
// runtime.NewFake backends and a predicate that matches no session, so every
// session routes to the local backend (as runtime.composition.auto proves its
// default route). The remote route is covered by this package's focused
// routing tests; the registry's real tmux+k8s backends in
// cmd/gc.newHybridProvider are not exercised here.
func TestHybridConformance(t *testing.T) {
	var counter int64

	runtimetest.RunProviderTests(t, func(_ *testing.T) (runtime.Provider, runtime.Config, string) {
		return New(runtime.NewFake(), runtime.NewFake(), func(string) bool { return false }), runtime.Config{}, fmt.Sprintf("hybrid-conform-%d", atomic.AddInt64(&counter, 1))
	})
}

package herdr

import (
	"fmt"
	"sync/atomic"
	"testing"

	"github.com/gastownhall/gascity/internal/runtime"
	"github.com/gastownhall/gascity/internal/runtime/runtimetest"
)

// TestHerdrConformance runs the full runtime.Provider conformance suite against
// New, backed by the fake herdr CLI and the bound server socket TestMain
// installs (conformance_fake_test.go), so it runs in every lane with no herdr
// binary. Every factory call shares the conformance session, as production
// shares one herdr session per city; the per-session name passed to Start is
// what places each session in its own workspace and tab.
func TestHerdrConformance(t *testing.T) {
	var counter int64

	runtimetest.RunProviderTests(t, func(t *testing.T) (runtime.Provider, runtime.Config, string) {
		return New(conformanceHerdrSession, t.TempDir(), t.TempDir(), 0, 0), runtime.Config{WorkDir: t.TempDir()}, fmt.Sprintf("conf-%d", atomic.AddInt64(&counter, 1))
	})
}

// TestHerdrConformance_Live runs the same suite against a real herdr binary.
// Opt-in live tier: see requireLiveHerdr. Each session gets its own isolated
// herdr session-server so the contract's session-scoped assertions
// (ListRunning, orphan detection, …) don't observe sibling sessions.
func TestHerdrConformance_Live(t *testing.T) {
	requireLiveHerdr(t)

	var counter int64
	runtimetest.RunProviderTests(t, func(t *testing.T) (runtime.Provider, runtime.Config, string) {
		n := atomic.AddInt64(&counter, 1)
		p := New(fmt.Sprintf("gctest-conf-%d", n), t.TempDir(), t.TempDir(), 0, 0)
		t.Cleanup(func() { _ = p.TeardownServer() })
		return p, runtime.Config{WorkDir: t.TempDir()}, fmt.Sprintf("conf-%d", n)
	})
}

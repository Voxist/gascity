package herdr

import (
	"fmt"
	"sync/atomic"
	"testing"

	"github.com/gastownhall/gascity/internal/runtime"
	"github.com/gastownhall/gascity/internal/runtime/runtimetest"
)

// TestHerdrConformance runs the full runtime.Provider conformance suite
// against New(...) backed by the fake herdr CLI TestMain installs on $PATH
// (see conformance_fake_test.go) and the pre-bound shared session-server
// socket it binds — so this always runs, with no real herdr binary and no
// -short/live-journey skip. Every session in this suite launches with an
// empty cfg.Command, so Start never issues a kind launch (agent start/prompt)
// and every lifecycle op resolves through the pane binding Start persists
// locally rather than through herdr's own agent registry; the fake only
// needs to answer the registry/pane verbs that path actually reaches.
var herdrConformanceCounter int64

func TestHerdrConformance(t *testing.T) {
	runtimetest.RunProviderTests(t, func(t *testing.T) (runtime.Provider, runtime.Config, string) {
		return New(conformanceHerdrSession, t.TempDir(), t.TempDir(), 0, 0), runtime.Config{WorkDir: t.TempDir()}, fmt.Sprintf("conf-%d", atomic.AddInt64(&herdrConformanceCounter, 1))
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

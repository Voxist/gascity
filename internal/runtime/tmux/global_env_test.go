//go:build tmux_integration

package tmux

import (
	"testing"

	"github.com/gastownhall/gascity/test/tmuxtest"
)

func TestTmux_SetAndGetGlobalEnvironment(t *testing.T) {
	tmuxtest.RequireTmux(t)
	guard := tmuxtest.NewGuard(t)

	tm := NewTmuxWithConfig(Config{SocketName: guard.SocketName()})
	// Start a dummy session to initialise the isolated tmux server.
	if _, err := tm.run("new-session", "-d", "-s", "init", "sleep 60"); err != nil {
		t.Skipf("cannot start tmux server on test socket: %v", err)
	}
	t.Cleanup(func() { _, _ = tm.run("kill-server") })

	t.Run("round-trip", func(t *testing.T) {
		if err := tm.SetGlobalEnvironment("GC_ROTATE_TEST_KEY", "hello"); err != nil {
			t.Fatalf("SetGlobalEnvironment: %v", err)
		}
		got, err := tm.GetGlobalEnvironment("GC_ROTATE_TEST_KEY")
		if err != nil {
			t.Fatalf("GetGlobalEnvironment: %v", err)
		}
		if got != "hello" {
			t.Errorf("GetGlobalEnvironment = %q; want %q", got, "hello")
		}
	})

	t.Run("empty key returns error", func(t *testing.T) {
		if err := tm.SetGlobalEnvironment("", "value"); err == nil {
			t.Error("SetGlobalEnvironment with empty key: want error, got nil")
		}
	})
}

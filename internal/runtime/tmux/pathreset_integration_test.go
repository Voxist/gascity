//go:build integration

package tmux

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/shellquote"
)

// TestNewSessionWithCommandAndEnv_PATHSurvivesZshenv is the real-tmux proof
// for the bug this package's PATH-reassert fix (withEnvUnsetPrefix's
// resetPath parameter) closes: a pane's *default shell* runs its own startup
// files — a zsh ~/.zshenv that unconditionally rebuilds PATH (the common
// nvm/volta/asdf/fnm pattern) — before it ever interprets the command string
// tmux was told to run. `-e PATH=...` sets the session environment, but that
// startup file still runs first and clobbers it, so the pane's actual command
// sees the operator's PATH, not the caller's.
//
// Each subtest gets its own private tmux server (a fresh socket forked from
// THIS process — see privateSocketName), a fresh $HOME with a hostile
// ~/.zshenv, and $SHELL pointed at zsh, so the pane's default shell is
// guaranteed to run that file the way a real macOS login shell would.
func TestNewSessionWithCommandAndEnv_PATHSurvivesZshenv(t *testing.T) {
	if !hasTmux() {
		t.Skip("tmux not installed")
	}
	zsh, err := exec.LookPath("zsh")
	if err != nil {
		t.Skip("zsh not installed")
	}

	// Spawning a real tmux server plus a zsh login shell under this host's
	// contention (documented load spikes well above 300) can outrun the
	// production 30s tmuxSubprocessTimeout on its own, independent of this
	// fix — the same ceiling has been observed killing TestTmuxConformance's
	// unrelated Start call under identical load. Widen it for this test only.
	origTimeout := tmuxSubprocessTimeout
	tmuxSubprocessTimeout = 2 * time.Minute
	t.Cleanup(func() { tmuxSubprocessTimeout = origTimeout })

	const callerPATH = "/pathreset-marker/bin:/usr/bin:/bin"
	const zshenvPATH = "/usr/bin:/bin" // what the hostile ~/.zshenv rebuilds

	tests := []struct {
		name    string
		command func(outFile string) string
	}{
		{
			name: "simple command",
			command: func(outFile string) string {
				return "printf %s \"$PATH\" > " + shellquote.Quote(outFile)
			},
		},
		{
			name: "caller-authored compound command (cd && exec)",
			command: func(outFile string) string {
				// Mirrors a real agent.toml/pack-template start_command that
				// changes directory before launching — the exact shape the
				// sh -c wrap exists to keep working as one opaque unit.
				return "cd " + shellquote.Quote(filepath.Dir(outFile)) +
					" && printf %s \"$PATH\" > " + shellquote.Quote(outFile)
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			home := t.TempDir()
			zshenv := "export PATH=" + shellquote.Quote(zshenvPATH) + "\n"
			if err := os.WriteFile(filepath.Join(home, ".zshenv"), []byte(zshenv), 0o644); err != nil {
				t.Fatalf("writing .zshenv: %v", err)
			}
			t.Setenv("HOME", home)
			t.Setenv("SHELL", zsh)
			// ZDOTDIR, if inherited from the operator's own shell, would
			// point zsh at a *different* rc directory than HOME and this
			// test's .zshenv would never run. Force zsh's default:
			// ZDOTDIR unset means "use HOME".
			t.Setenv("ZDOTDIR", "")
			_ = os.Unsetenv("ZDOTDIR")

			cfg := DefaultConfig()
			cfg.SocketName = privateSocketName("pathreset")
			tm := NewTmuxWithConfig(cfg)
			t.Cleanup(func() { _ = tm.KillServer() })

			workDir := t.TempDir()
			outFile := filepath.Join(t.TempDir(), "path-out")
			session := "pathresetsess"

			if err := tm.NewSessionWithCommandAndEnv(session, workDir, tt.command(outFile),
				map[string]string{"PATH": callerPATH}); err != nil {
				t.Fatalf("NewSessionWithCommandAndEnv: %v", err)
			}

			got := waitForFileContents(t, outFile, 5*time.Second)
			if got == zshenvPATH {
				t.Fatalf("pane saw the hostile ~/.zshenv PATH (%q): the caller's PATH was clobbered by the pane's default-shell startup file", got)
			}
			if got != callerPATH {
				t.Fatalf("pane PATH = %q, want the caller's %q", got, callerPATH)
			}
		})
	}
}

// TestNewSessionWithCommandAndEnv_PaneProcessIdentity pins the process-tree
// side of the resetPath wrap: every pane-liveness consumer that cares about
// pane identity must still find the real agent process, whether the wrap
// forced `exec` (the simple-command case) or left the caller's own compound
// command to run as-is (where a shell can legitimately linger as
// pane_current_command with the real process as its child — the same shape
// GC_AGENT_SLICE's systemd-run wrap and a caller's own "bash -c 'exec x'"
// already produce, and which FindAgentPane/IsRuntimeRunning/WaitForCommand
// already handle via a descendant walk).
func TestNewSessionWithCommandAndEnv_PaneProcessIdentity(t *testing.T) {
	if !hasTmux() {
		t.Skip("tmux not installed")
	}

	origTimeout := tmuxSubprocessTimeout
	tmuxSubprocessTimeout = 2 * time.Minute
	t.Cleanup(func() { tmuxSubprocessTimeout = origTimeout })

	newTm := func() *Tmux {
		cfg := DefaultConfig()
		cfg.SocketName = privateSocketName("paneident")
		tm := NewTmuxWithConfig(cfg)
		t.Cleanup(func() { _ = tm.KillServer() })
		return tm
	}

	t.Run("simple command: exec forced, pane IS the real process", func(t *testing.T) {
		tm := newTm()
		session := "paneidentsimple"
		if err := tm.NewSessionWithCommandAndEnv(session, "", "sleep 60",
			map[string]string{"PATH": "/usr/bin:/bin"}); err != nil {
			t.Fatalf("NewSessionWithCommandAndEnv: %v", err)
		}
		var cmd string
		var err error
		waitForPaneCondition(t, 5*time.Second, "pane_current_command becomes non-empty", func() bool {
			cmd, err = tm.GetPaneCommand(session)
			return err == nil && cmd != ""
		})
		if err != nil {
			t.Fatalf("GetPaneCommand: %v", err)
		}
		if cmd != "sleep" {
			t.Errorf("pane_current_command = %q, want %q (exec should have replaced the sh wrapper)", cmd, "sleep")
		}
		// The exact-match liveness check (IsAgentRunning's fallback path)
		// depends on this identity directly — assert it agrees.
		if !tm.IsAgentRunning(session, "sleep") {
			t.Errorf("IsAgentRunning(session, \"sleep\") = false, want true")
		}
	})

	t.Run("compound command: shell may linger, descendant walk still finds it", func(t *testing.T) {
		tm := newTm()
		session := "paneidentcompound"
		workDir := t.TempDir()
		command := "cd " + shellquote.Quote(workDir) + " && sleep 60"
		if err := tm.NewSessionWithCommandAndEnv(session, "", command,
			map[string]string{"PATH": "/usr/bin:/bin"}); err != nil {
			t.Fatalf("NewSessionWithCommandAndEnv: %v", err)
		}
		// However the pane looks (sh lingering as parent, or sleep directly if
		// the shell's own tail-call optimization happened to fire), the
		// package's descendant-walk-aware liveness check must see it running —
		// this is the actual production consumer (IsRuntimeRunning backs
		// IsAgentAlive, WaitForCommand's fallback, and CheckSessionHealth).
		var alive bool
		waitForPaneCondition(t, 5*time.Second, "IsRuntimeRunning becomes true", func() bool {
			alive = tm.IsRuntimeRunning(session, []string{"sleep"})
			return alive
		})
		if !alive {
			cmd, _ := tm.GetPaneCommand(session)
			t.Errorf("IsRuntimeRunning(session, [\"sleep\"]) = false after compound command started (pane_current_command=%q); descendant walk did not find the real process", cmd)
		}
	})
}

// waitForPaneCondition polls check every 25ms until it returns true, failing
// the test if that does not happen within timeout. Mirrors the timer+ticker
// idiom this package's other integration tests already use for condition
// waits (waitForFileContents, waitForProcessTargetsGone) rather than a fixed
// sleep, so a slow-but-healthy run isn't penalized and a genuinely stuck one
// still fails promptly.
func waitForPaneCondition(t *testing.T, timeout time.Duration, describe string, check func() bool) {
	t.Helper()
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	ticker := time.NewTicker(25 * time.Millisecond)
	defer ticker.Stop()
	for {
		if check() {
			return
		}
		select {
		case <-timer.C:
			t.Fatalf("%s did not become true within %s", describe, timeout)
		case <-ticker.C:
		}
	}
}

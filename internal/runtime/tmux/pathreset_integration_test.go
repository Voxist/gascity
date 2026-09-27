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

// hostileShellFixture describes one shell family's unconditional (or
// reliably-forceable) startup-file mechanism: the one nvm/volta/asdf/fnm-style
// installers exploit to rebuild PATH regardless of whether the shell is
// interactive or a login shell.
type hostileShellFixture struct {
	name string
	// lookupBinary is the shell's own binary name (exec.LookPath).
	lookupBinary string
	// writeHostileFile writes the fixture's startup file under home (and
	// returns any extra session env the pane needs — e.g. bash only reads a
	// startup file non-interactively via $BASH_ENV, which has to ride the
	// session environment like any other var).
	writeHostileFile func(t *testing.T, home, hostilePATH string) (extraEnv map[string]string)
}

// hostileShellFixtures covers every shell family this package treats as a
// pane's possible default shell (see supportedShells) whose startup-file
// mechanism can be triggered deterministically in a `-c` (non-interactive)
// invocation, which is what tmux always uses for an explicit pane command.
// ksh is omitted: none of ENV, a login-only mechanism gives a reliable
// non-interactive hook portable across ksh implementations, and it shares
// pathAssignmentStatement's `export` branch with sh/bash/zsh/dash, which are
// already covered.
//
// Plain POSIX `sh` is also omitted deliberately, not by oversight: unlike
// zsh's ~/.zshenv or csh/tcsh's ~/.cshrc, sh's non-interactive startup hook
// ($ENV) is honored by dash but NOT by macOS's own /bin/sh (bash 3.2 in
// posix mode) — verified directly: `ENV=hostile.sh sh -c 'echo $PATH'` shows
// the ambient PATH unchanged on this host. Since `sh` shares the same
// `export` branch as bash (verified in TestPathAssignmentStatement and
// TestWithEnvUnsetPrefix), and bash's BASH_ENV case below exercises that
// exact branch through a real non-interactive shell, sh needs no separate
// fixture to prove the mechanism works — only a portable way to force sh's
// own hostile startup file, which does not exist across the platforms this
// runs on.
func hostileShellFixtures() []hostileShellFixture {
	return []hostileShellFixture{
		{
			name:         "zsh",
			lookupBinary: "zsh",
			writeHostileFile: func(t *testing.T, home, hostilePATH string) map[string]string {
				t.Helper()
				content := "export PATH=" + shellquote.Quote(hostilePATH) + "\n"
				if err := os.WriteFile(filepath.Join(home, ".zshenv"), []byte(content), 0o644); err != nil {
					t.Fatalf("writing .zshenv: %v", err)
				}
				// ZDOTDIR, if inherited from the operator's own shell, would
				// point zsh at a different rc directory than HOME, and this
				// fixture's .zshenv would never run. Force zsh's default:
				// ZDOTDIR unset means "use HOME".
				t.Setenv("ZDOTDIR", "")
				_ = os.Unsetenv("ZDOTDIR")
				return nil
			},
		},
		{
			name:         "bash",
			lookupBinary: "bash",
			writeHostileFile: func(t *testing.T, home, hostilePATH string) map[string]string {
				t.Helper()
				// bash does NOT read ~/.bashrc for a non-interactive `-c`
				// invocation (verified: `bash -c 'echo $PATH'` with a hostile
				// ~/.bashrc present leaves PATH untouched) — that file is
				// nvm/volta's real-world hook, but it only fires for
				// interactive shells. bash's own non-interactive hook is
				// $BASH_ENV (verified: `BASH_ENV=hostile.sh bash -c ...`
				// DOES rebuild PATH), so exercise that instead — it is the
				// mechanism that would actually bite a tmux pane running a
				// bash -c command, which is always non-interactive.
				path := filepath.Join(home, "bash_env.sh")
				content := "export PATH=" + shellquote.Quote(hostilePATH) + "\n"
				if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
					t.Fatalf("writing BASH_ENV file: %v", err)
				}
				return map[string]string{"BASH_ENV": path}
			},
		},
		{
			name:         "tcsh",
			lookupBinary: "tcsh",
			writeHostileFile: func(t *testing.T, home, hostilePATH string) map[string]string {
				t.Helper()
				content := "setenv PATH " + shellquote.Quote(hostilePATH) + "\n"
				if err := os.WriteFile(filepath.Join(home, ".cshrc"), []byte(content), 0o644); err != nil {
					t.Fatalf("writing .cshrc: %v", err)
				}
				return nil
			},
		},
		{
			name:         "fish",
			lookupBinary: "fish",
			writeHostileFile: func(t *testing.T, home, hostilePATH string) map[string]string {
				t.Helper()
				// fish reads $XDG_CONFIG_HOME/fish/config.fish (default
				// ~/.config/fish/config.fish) for every invocation, including
				// -c. Force the default so an inherited ambient
				// XDG_CONFIG_HOME can't redirect it elsewhere.
				t.Setenv("XDG_CONFIG_HOME", "")
				_ = os.Unsetenv("XDG_CONFIG_HOME")
				dir := filepath.Join(home, ".config", "fish")
				if err := os.MkdirAll(dir, 0o755); err != nil {
					t.Fatalf("creating fish config dir: %v", err)
				}
				content := "set -gx PATH " + fishListQuote(hostilePATH) + "\n"
				if err := os.WriteFile(filepath.Join(dir, "config.fish"), []byte(content), 0o644); err != nil {
					t.Fatalf("writing config.fish: %v", err)
				}
				return nil
			},
		},
	}
}

// fishListQuote renders a colon-separated PATH as fish's space-separated,
// individually single-quoted list syntax for `set -gx PATH ...`.
func fishListQuote(colonSeparatedPath string) string {
	parts := filepath.SplitList(colonSeparatedPath)
	quoted := make([]string, len(parts))
	for i, p := range parts {
		quoted[i] = shellquote.Quote(p)
	}
	return joinSpace(quoted)
}

func joinSpace(parts []string) string {
	out := ""
	for i, p := range parts {
		if i > 0 {
			out += " "
		}
		out += p
	}
	return out
}

// TestNewSessionWithCommandAndEnv_PATHSurvivesHostileStartupFile is the
// real-tmux proof for the bug this package's PATH-reassert fix
// (withEnvUnsetPrefix's resetPath parameter) closes: a pane's *default
// shell* runs its own startup files — a zsh ~/.zshenv, a bash $BASH_ENV
// script, a tcsh ~/.cshrc, or a fish config.fish, all of which unconditionally
// rebuild PATH (the common nvm/volta/asdf/fnm pattern) — before it ever
// interprets the command string tmux was told to run. `-e PATH=...` sets
// the session environment, but the startup file still runs first and
// clobbers it, so the pane's actual command sees the operator's PATH, not
// the caller's.
//
// Covers every shell family in hostileShellFixtures, and for the two
// representative syntactic branches (bash's POSIX `export`, tcsh's `setenv`)
// also a caller-authored compound command, a command that already starts
// with `exec`, a command carrying a leading `VAR=val` assignment, and a
// multi-line command — proving the fix's prefix-only approach never
// inspects or depends on the caller's command shape at all.
func TestNewSessionWithCommandAndEnv_PATHSurvivesHostileStartupFile(t *testing.T) {
	if !hasTmux() {
		t.Skip("tmux not installed")
	}

	// Spawning a real tmux server plus a real login shell under this host's
	// contention (documented load spikes well above 300) can outrun the
	// production 30s tmuxSubprocessTimeout on its own, independent of this
	// fix — the same ceiling has been observed killing TestTmuxConformance's
	// unrelated Start call under identical load. Widen it for this test only.
	origTimeout := tmuxSubprocessTimeout
	tmuxSubprocessTimeout = 2 * time.Minute
	t.Cleanup(func() { tmuxSubprocessTimeout = origTimeout })

	const callerPATH = "/pathreset-marker/bin:/usr/bin:/bin"
	const hostilePATH = "/usr/bin:/bin" // what every fixture's startup file rebuilds

	type commandCase struct {
		name    string
		command func(outFile, workDir string) string
	}
	simpleCase := commandCase{
		name: "simple command",
		command: func(outFile, _ string) string {
			return "printf %s \"$PATH\" > " + shellquote.Quote(outFile)
		},
	}
	// Valid in both the Bourne family (bash) and the csh family (tcsh): &&,
	// exec, and embedded newlines are all ordinary command separators or
	// builtins in csh/tcsh too.
	universalExtraCases := []commandCase{
		{
			name: "caller-authored compound command (cd && exec)",
			command: func(outFile, workDir string) string {
				return "cd " + shellquote.Quote(workDir) +
					" && exec printf %s \"$PATH\" > " + shellquote.Quote(outFile)
			},
		},
		{
			name: "command already starting with exec",
			command: func(outFile, _ string) string {
				return "exec printf %s \"$PATH\" > " + shellquote.Quote(outFile)
			},
		},
		{
			name: "multi-line command",
			command: func(outFile, workDir string) string {
				return "cd " + shellquote.Quote(workDir) + "\n" +
					"printf %s \"$PATH\" > " + shellquote.Quote(outFile)
			},
		},
	}
	// `VAR=val cmd` is Bourne-family syntax only — csh/tcsh has no such form
	// and would try (and fail) to execute a program literally named
	// "MARKER=1". That is a pre-existing csh-family language limitation,
	// nothing this fix changes, so only bash exercises it.
	bourneOnlyExtraCases := []commandCase{
		{
			name: "command with a leading VAR=val assignment",
			command: func(outFile, _ string) string {
				return "MARKER=1 printf %s \"$PATH\" > " + shellquote.Quote(outFile)
			},
		},
	}

	for _, fixture := range hostileShellFixtures() {
		fixture := fixture
		shellPath, err := exec.LookPath(fixture.lookupBinary)
		if err != nil {
			t.Run(fixture.name, func(t *testing.T) {
				t.Skipf("%s not installed", fixture.lookupBinary)
			})
			continue
		}

		cases := []commandCase{simpleCase}
		switch fixture.name {
		case "bash":
			cases = append(cases, universalExtraCases...)
			cases = append(cases, bourneOnlyExtraCases...)
		case "tcsh":
			cases = append(cases, universalExtraCases...)
		}

		for _, tc := range cases {
			t.Run(fixture.name+"/"+tc.name, func(t *testing.T) {
				home := t.TempDir()
				extraEnv := fixture.writeHostileFile(t, home, hostilePATH)
				t.Setenv("HOME", home)
				t.Setenv("SHELL", shellPath)

				cfg := DefaultConfig()
				cfg.SocketName = privateSocketName("pathreset")
				tm := NewTmuxWithConfig(cfg)
				t.Cleanup(func() { _ = tm.KillServer() })

				workDir := t.TempDir()
				outFile := filepath.Join(t.TempDir(), "path-out")
				session := "pathresetsess"

				env := map[string]string{"PATH": callerPATH}
				for k, v := range extraEnv {
					env[k] = v
				}

				if err := tm.NewSessionWithCommandAndEnv(session, workDir, tc.command(outFile, workDir), env); err != nil {
					t.Fatalf("NewSessionWithCommandAndEnv: %v", err)
				}

				got := waitForFileContents(t, outFile, 5*time.Second)
				if got == hostilePATH {
					t.Fatalf("pane saw the hostile startup-file PATH (%q): the caller's PATH was clobbered by the pane's default-shell startup file", got)
				}
				if got != callerPATH {
					t.Fatalf("pane PATH = %q, want the caller's %q", got, callerPATH)
				}
			})
		}
	}
}

// TestRespawnAgent_PATHSurvivesHostileStartupFile is the respawn-path
// counterpart: respawn-pane runs the command through the pane's default
// shell exactly like the initial new-session exec does, so a warm-relaunch
// that never re-asserted PATH would reproduce the same clobber on every
// relaunch, not only at create (ga-tg8t5's respawn half). Create a session
// with the caller's PATH proven correct, then respawn it and prove PATH
// survives a second time in the same pane.
func TestRespawnAgent_PATHSurvivesHostileStartupFile(t *testing.T) {
	if !hasTmux() {
		t.Skip("tmux not installed")
	}
	zsh, err := exec.LookPath("zsh")
	if err != nil {
		t.Skip("zsh not installed")
	}

	origTimeout := tmuxSubprocessTimeout
	tmuxSubprocessTimeout = 2 * time.Minute
	t.Cleanup(func() { tmuxSubprocessTimeout = origTimeout })

	const callerPATH = "/pathreset-marker/bin:/usr/bin:/bin"
	const hostilePATH = "/usr/bin:/bin"

	home := t.TempDir()
	zshenv := "export PATH=" + shellquote.Quote(hostilePATH) + "\n"
	if err := os.WriteFile(filepath.Join(home, ".zshenv"), []byte(zshenv), 0o644); err != nil {
		t.Fatalf("writing .zshenv: %v", err)
	}
	t.Setenv("HOME", home)
	t.Setenv("SHELL", zsh)
	t.Setenv("ZDOTDIR", "")
	_ = os.Unsetenv("ZDOTDIR")

	cfg := DefaultConfig()
	cfg.SocketName = privateSocketName("respawnpath")
	tm := NewTmuxWithConfig(cfg)
	t.Cleanup(func() { _ = tm.KillServer() })

	workDir := t.TempDir()
	session := "respawnpathsess"
	env := map[string]string{"PATH": callerPATH}

	// The create-step command must not exit before respawnAgent runs: an
	// exited pane with no session left on the server takes the whole tmux
	// server down with it (verified: without the trailing sleep, respawnAgent
	// below failed with "no tmux server running"), and respawn-pane needs a
	// live pane to kill and restart in the first place.
	commandFor := func(outFile string) string {
		return "printf %s \"$PATH\" > " + shellquote.Quote(outFile) + "; sleep 30"
	}

	createOut := filepath.Join(t.TempDir(), "path-out-create")
	if err := tm.NewSessionWithCommandAndEnv(session, workDir, commandFor(createOut), env); err != nil {
		t.Fatalf("NewSessionWithCommandAndEnv: %v", err)
	}
	got := waitForFileContents(t, createOut, 5*time.Second)
	if got != callerPATH {
		t.Fatalf("create: pane PATH = %q, want the caller's %q (hostile PATH %q)", got, callerPATH, hostilePATH)
	}

	ops := &tmuxStartOps{tm: tm}
	respawnOut := filepath.Join(t.TempDir(), "path-out-respawn")
	if err := ops.respawnAgent(session, workDir, commandFor(respawnOut), env); err != nil {
		t.Fatalf("respawnAgent: %v", err)
	}

	got = waitForFileContents(t, respawnOut, 5*time.Second)
	if got == hostilePATH {
		t.Fatalf("respawn: pane saw the hostile ~/.zshenv PATH (%q) — PATH was NOT re-asserted on respawn-pane, reproducing ga-tg8t5 on every relaunch", got)
	}
	if got != callerPATH {
		t.Fatalf("respawn: pane PATH = %q, want the caller's %q", got, callerPATH)
	}
}

// TestNewSessionWithCommandAndEnv_PaneProcessIdentity pins the process-tree
// side of the resetPath prefix: for `export PATH=...; cmd`, whether the
// pane's shell tail-call-optimizes through the leading export builtin and
// replaces itself with the final command, or keeps itself as the pane's own
// process with the final command as a child, is not just shell-family
// dependent but shell-VERSION dependent — verified directly with real tmux:
// zsh consistently execs through to "sleep"; macOS's /bin/bash (3.2) keeps
// itself as "bash" with "sleep" as a child, while exec.LookPath("bash") on
// this same host resolves to Homebrew's bash (5.3), which DOES exec through,
// giving "sleep" too. Since this can vary even between two bash builds on
// the same machine, only zsh's outcome is asserted exactly; bash logs
// whatever pane_current_command it produced without requiring a specific
// value. Both shapes must still be found by the package's actual liveness
// consumer, IsRuntimeRunning (which backs IsAgentAlive, WaitForCommand's
// fallback, and CheckSessionHealth) — IsAgentRunning's bare exact-match is
// not exercised here because it is reachable only through
// EnsureSessionFresh's bare NewSession path, which never calls
// withEnvUnsetPrefix.
func TestNewSessionWithCommandAndEnv_PaneProcessIdentity(t *testing.T) {
	if !hasTmux() {
		t.Skip("tmux not installed")
	}

	origTimeout := tmuxSubprocessTimeout
	tmuxSubprocessTimeout = 2 * time.Minute
	t.Cleanup(func() { tmuxSubprocessTimeout = origTimeout })

	tests := []struct {
		shellBinary     string
		wantPaneCommand string // "" means "don't assert an exact value, only liveness"
	}{
		{shellBinary: "zsh", wantPaneCommand: "sleep"},
		{shellBinary: "bash", wantPaneCommand: ""},
	}

	for _, tt := range tests {
		t.Run(tt.shellBinary, func(t *testing.T) {
			shellPath, err := exec.LookPath(tt.shellBinary)
			if err != nil {
				t.Skipf("%s not installed", tt.shellBinary)
			}
			t.Setenv("SHELL", shellPath)

			cfg := DefaultConfig()
			cfg.SocketName = privateSocketName("paneident")
			tm := NewTmuxWithConfig(cfg)
			t.Cleanup(func() { _ = tm.KillServer() })

			session := "paneident" + tt.shellBinary
			if err := tm.NewSessionWithCommandAndEnv(session, "", "sleep 60",
				map[string]string{"PATH": "/usr/bin:/bin"}); err != nil {
				t.Fatalf("NewSessionWithCommandAndEnv: %v", err)
			}

			var alive bool
			waitForPaneCondition(t, 5*time.Second, "IsRuntimeRunning becomes true", func() bool {
				alive = tm.IsRuntimeRunning(session, []string{"sleep"})
				return alive
			})
			if !alive {
				cmd, _ := tm.GetPaneCommand(session)
				t.Fatalf("IsRuntimeRunning(session, [\"sleep\"]) = false (pane_current_command=%q); descendant walk did not find the real process", cmd)
			}

			cmd, err := tm.GetPaneCommand(session)
			if err != nil {
				t.Fatalf("GetPaneCommand: %v", err)
			}
			if tt.wantPaneCommand != "" && cmd != tt.wantPaneCommand {
				t.Errorf("pane_current_command = %q, want %q", cmd, tt.wantPaneCommand)
			}
			t.Logf("shell=%s pane_current_command=%q", tt.shellBinary, cmd)
		})
	}
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

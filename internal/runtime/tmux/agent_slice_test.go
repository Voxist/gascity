package tmux

import (
	"context"
	"errors"
	"os"
	osexec "os/exec"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"
)

// newSliceTestTmux returns a Tmux backed by a fakeExecutor with the agent
// slice probe stubbed to succeed, plus the executor for argv inspection.
func newSliceTestTmux(t *testing.T) (*Tmux, *fakeExecutor) {
	t.Helper()
	exec := &fakeExecutor{}
	tm := NewTmux()
	tm.exec = exec
	tm.agentSlice.probe = func(string) error { return nil }
	tm.agentSlice.warn = &strings.Builder{}
	return tm, exec
}

func TestAgentSliceWrapsNewSessionWithCommand(t *testing.T) {
	t.Setenv(AgentSliceEnv, "gascity-agents.slice")
	tm, exec := newSliceTestTmux(t)

	if err := tm.NewSessionWithCommand("gc-test-slice", "/work", "exec env GT_ROLE=crew claude"); err != nil {
		t.Fatalf("NewSessionWithCommand: %v", err)
	}
	if len(exec.calls) == 0 {
		t.Fatal("no tmux calls recorded")
	}
	args := exec.calls[0]
	got := args[len(args)-1]
	want := "systemd-run --user --scope --slice=gascity-agents.slice --collect --quiet -- sh -c 'exec env GT_ROLE=crew claude'"
	if got != want {
		t.Fatalf("pane command = %q, want %q", got, want)
	}
}

func TestAgentSliceWrapsNewSessionWithCommandAndEnv(t *testing.T) {
	t.Setenv(AgentSliceEnv, "gascity-agents.slice")
	tm, exec := newSliceTestTmux(t)

	env := map[string]string{"LANG": "en_US.UTF-8", "LC_ALL": ""}
	if err := tm.NewSessionWithCommandAndEnv("gc-test-slice-env", "/work", "claude", env); err != nil {
		t.Fatalf("NewSessionWithCommandAndEnv: %v", err)
	}
	if len(exec.calls) == 0 {
		t.Fatal("no tmux calls recorded")
	}

	// ADR-0051 transport: no -e flags anywhere (secrets would pin to argv), and
	// the agent command is started via respawn-pane, which wrapPaneCommand still
	// wraps in the systemd scope. Empty-value vars are removed via set-environment
	// -r, NOT via an env -u prefix on the command — so the wrapped command is
	// the plain agent command.
	for i, call := range exec.calls {
		for _, a := range call {
			if a == "-e" {
				t.Fatalf("ADR-0051: call %d still uses -e flag: %v", i, call)
			}
		}
	}

	// LANG is delivered via set-environment over the socket.
	foundLANG := false
	for _, call := range exec.calls {
		if sub := setEnvironmentArgs(call); contains(sub, "LANG") && contains(sub, "en_US.UTF-8") {
			foundLANG = true
		}
	}
	if !foundLANG {
		t.Errorf("missing set-environment LANG en_US.UTF-8; calls: %v", exec.calls)
	}
	assertEnvRemovedForNewProcess(t, exec.calls, "LC_ALL")

	// The command is started via respawn-pane and STILL wrapped in the systemd
	// scope (wrapPaneCommand applies inside RespawnPane).
	var respawnCmd string
	for _, call := range exec.calls {
		if cmd(call) == "respawn-pane" {
			respawnCmd = call[len(call)-1]
			break
		}
	}
	want := "systemd-run --user --scope --slice=gascity-agents.slice --collect --quiet -- sh -c claude"
	if respawnCmd == "" {
		t.Fatalf("missing respawn-pane call; calls: %v", exec.calls)
	}
	if respawnCmd != want {
		t.Fatalf("respawn pane command = %q, want %q", respawnCmd, want)
	}
}

func TestAgentSliceWrapsRespawnPane(t *testing.T) {
	t.Setenv(AgentSliceEnv, "gascity-agents.slice")
	tm, exec := newSliceTestTmux(t)

	if err := tm.RespawnPane("%0", "claude --resume"); err != nil {
		t.Fatalf("RespawnPane: %v", err)
	}
	args := exec.calls[0]
	got := args[len(args)-1]
	want := "systemd-run --user --scope --slice=gascity-agents.slice --collect --quiet -- sh -c 'claude --resume'"
	if got != want {
		t.Fatalf("respawn command = %q, want %q", got, want)
	}

	if err := tm.RespawnPaneWithWorkDir("%0", "/work", "claude --resume"); err != nil {
		t.Fatalf("RespawnPaneWithWorkDir: %v", err)
	}
	args = exec.calls[1]
	if got := args[len(args)-1]; got != want {
		t.Fatalf("respawn-with-workdir command = %q, want %q", got, want)
	}
}

func TestAgentSliceUnsetLeavesCommandPlain(t *testing.T) {
	t.Setenv(AgentSliceEnv, "")
	tm, exec := newSliceTestTmux(t)

	if err := tm.NewSessionWithCommand("gc-test-plain", "/work", "claude"); err != nil {
		t.Fatalf("NewSessionWithCommand: %v", err)
	}
	args := exec.calls[0]
	if got := args[len(args)-1]; got != "claude" {
		t.Fatalf("pane command = %q, want plain %q", got, "claude")
	}
}

func TestAgentSliceProbeFailureFallsBackPlainWithWarning(t *testing.T) {
	t.Setenv(AgentSliceEnv, "gascity-agents.slice")
	probeCalls := 0
	exec := &fakeExecutor{}
	tm := NewTmux()
	tm.exec = exec
	var warnings strings.Builder
	tm.agentSlice.probe = func(string) error {
		probeCalls++
		return errors.New("user manager not responding")
	}
	tm.agentSlice.warn = &warnings

	if err := tm.NewSessionWithCommand("gc-test-fallback", "/work", "claude"); err != nil {
		t.Fatalf("NewSessionWithCommand: %v", err)
	}
	if err := tm.NewSessionWithCommand("gc-test-fallback2", "/work", "claude"); err != nil {
		t.Fatalf("NewSessionWithCommand (second): %v", err)
	}

	// Recorded argv carries global flags first (-u, optional -L <socket>);
	// scan for the new-session token rather than relying on position.
	newSessionCalls := 0
	for i, call := range exec.calls {
		if !slices.Contains(call, "new-session") {
			continue
		}
		newSessionCalls++
		if got := call[len(call)-1]; got != "claude" {
			t.Fatalf("new-session call %d pane command = %q, want plain %q", i, got, "claude")
		}
	}
	if newSessionCalls != 2 {
		t.Fatalf("recorded %d new-session calls, want 2 (assertion must not pass vacuously)", newSessionCalls)
	}
	if probeCalls != 1 {
		t.Fatalf("probe called %d times, want 1 (result must be cached)", probeCalls)
	}
	if !strings.Contains(warnings.String(), "user manager not responding") {
		t.Fatalf("warning output missing probe error: %q", warnings.String())
	}
	if !strings.Contains(warnings.String(), AgentSliceEnv) {
		t.Fatalf("warning output missing env var name: %q", warnings.String())
	}
}

func TestAgentSliceEmptyCommandNotWrapped(t *testing.T) {
	t.Setenv(AgentSliceEnv, "gascity-agents.slice")
	tm, exec := newSliceTestTmux(t)

	// Empty command + env-only session: a bare new-session (no command) starts
	// tmux's default shell, env is set via set-environment, and the pane is then
	// respawned with NO shell-command so the shell restarts carrying that env.
	//
	// The respawn is required, not optional: the shell new-session started
	// captured its environment before set-environment ran, so skipping the
	// respawn delivers nothing to the pane's process — while show-environment
	// still reports every variable as set. Because no shell-command is passed,
	// wrapPaneCommand does not apply and the systemd wrapper must not appear.
	if err := tm.NewSessionWithCommandAndEnv("gc-test-empty", "/work", "", map[string]string{"LANG": "C"}); err != nil {
		t.Fatalf("NewSessionWithCommandAndEnv: %v", err)
	}
	for i, call := range exec.calls {
		if c := call[len(call)-1]; strings.Contains(c, "systemd-run") {
			t.Fatalf("empty command should not be systemd-wrapped; call %d: %v", i, call)
		}
	}
	var respawn []string
	for _, call := range exec.calls {
		if cmd(call) == "respawn-pane" {
			respawn = call
			break
		}
	}
	if respawn == nil {
		t.Fatalf("env-only session must respawn the pane so the shell picks up the "+
			"session env; calls: %v", exec.calls)
	}
	// respawn-pane -k -t <session>, and nothing after the target: a trailing
	// shell-command would be the wrapped form this test exists to exclude.
	if got, want := respawn[len(respawn)-1], "gc-test-empty"; got != want {
		t.Fatalf("env-only respawn should carry no shell-command; last arg = %q, want %q (call: %v)",
			got, want, respawn)
	}
	// LANG still delivered via set-environment. Shape only — the behavioral
	// counterpart is TestNewSessionWithCommandAndEnvDeliversEnvToEnvOnlyShell.
	delivered := false
	for _, call := range exec.calls {
		if sub := setEnvironmentArgs(call); contains(sub, "LANG") && contains(sub, "C") {
			delivered = true
			break
		}
	}
	if !delivered {
		t.Fatalf("missing set-environment LANG C; calls: %v", exec.calls)
	}
}

func TestAgentSliceQuotesEmbeddedSingleQuotes(t *testing.T) {
	t.Setenv(AgentSliceEnv, "gascity-agents.slice")
	tm, exec := newSliceTestTmux(t)

	if err := tm.NewSessionWithCommand("gc-test-quote", "/work", "claude --msg 'hi there'"); err != nil {
		t.Fatalf("NewSessionWithCommand: %v", err)
	}
	args := exec.calls[0]
	got := args[len(args)-1]
	want := `systemd-run --user --scope --slice=gascity-agents.slice --collect --quiet -- sh -c 'claude --msg '\''hi there'\'''`
	if got != want {
		t.Fatalf("pane command = %q, want %q", got, want)
	}
}

// TestFindAgentPane_WrappedPane pins the detection contract for panes whose
// root command is a wrapper such as systemd-run (GC_AGENT_SLICE): none of the
// direct-name, shell-descendant, or version-as-argv[0] checks match the
// wrapper process, so FindAgentPane must identify the agent through the
// unconditional descendant walk, mirroring IsRuntimeRunning.
func TestFindAgentPane_WrappedPane(t *testing.T) {
	// Real process tree: the test binary spawns "sleep 60", standing in for
	// systemd-run spawning the agent. The fake executor reports the pane
	// command as "systemd-run" with the test binary's PID, so only the
	// descendant fallback can identify the agent pane.
	agent := osexec.Command("sleep", "60")
	if err := agent.Start(); err != nil {
		t.Fatalf("starting agent stand-in: %v", err)
	}
	t.Cleanup(func() {
		_ = agent.Process.Kill()
		_ = agent.Wait()
	})

	panePID := strconv.Itoa(os.Getpid())
	deadline := time.Now().Add(5 * time.Second)
	for !hasDescendantWithNames(panePID, []string{"sleep"}, 0) {
		if time.Now().After(deadline) {
			t.Fatal("sleep child never became visible to the process-tree walk")
		}
		time.Sleep(10 * time.Millisecond)
	}

	exec := &fakeExecutor{outs: []string{
		// list-panes -s: a user's split pane plus the wrapped agent pane.
		// The bogus PID has no live process, so the first pane cannot match.
		"%1\tvim\t999999999\n%2\tsystemd-run\t" + panePID,
		// show-environment GT_PROCESS_NAMES
		"GT_PROCESS_NAMES=sleep",
	}}
	tm := NewTmux()
	tm.exec = exec

	paneID, err := tm.FindAgentPane("gc-test-wrapped")
	if err != nil {
		t.Fatalf("FindAgentPane: %v", err)
	}
	if paneID != "%2" {
		t.Fatalf("FindAgentPane = %q, want %q (wrapped agent pane via descendant fallback)", paneID, "%2")
	}
}

// wrappedWaitExecutor simulates a pane whose root command is the systemd-run
// wrapper for the pane's whole lifetime. The agent descendant becomes visible
// to the process-tree walk only after livePIDAfter pane-PID requests: earlier
// requests return a dead PID with no descendants, modeling the startup window
// where systemd-run exists but the agent has not exec'd yet.
type wrappedWaitExecutor struct {
	deadPID      string
	livePID      string
	livePIDAfter int
	pidRequests  int
}

func (e *wrappedWaitExecutor) execute(args []string) (string, error) {
	// args carry global flags first (-u, optional -L <socket>); scan for the
	// subcommand token.
	for _, a := range args {
		switch a {
		case "display-message":
			switch args[len(args)-1] {
			case "#{pane_current_command}":
				return "systemd-run", nil
			case "#{pane_pid}":
				e.pidRequests++
				if e.pidRequests <= e.livePIDAfter {
					return e.deadPID, nil
				}
				return e.livePID, nil
			}
			return "", nil
		case "show-environment":
			return "GT_PROCESS_NAMES=sleep", nil
		}
	}
	return "", nil
}

func (e *wrappedWaitExecutor) executeCtx(_ context.Context, args []string) (string, error) {
	return e.execute(args)
}

// TestWaitForCommand_WrappedPane pins the startup-wait contract for wrapper
// roots (systemd-run under GC_AGENT_SLICE): the pane reports the wrapper as
// pane_current_command for its whole lifetime, so WaitForCommand must not
// treat first sight of the wrapper as "agent command appeared". It must keep
// polling until the agent is detectable as a pane descendant, and time out
// when no agent ever appears.
func TestWaitForCommand_WrappedPane(t *testing.T) {
	// Real process tree: the test binary spawns "sleep 60" standing in for
	// the agent, as in TestFindAgentPane_WrappedPane.
	agent := osexec.Command("sleep", "60")
	if err := agent.Start(); err != nil {
		t.Fatalf("starting agent stand-in: %v", err)
	}
	t.Cleanup(func() {
		_ = agent.Process.Kill()
		_ = agent.Wait()
	})

	livePID := strconv.Itoa(os.Getpid())
	deadline := time.Now().Add(5 * time.Second)
	for !hasDescendantWithNames(livePID, []string{"sleep"}, 0) {
		if time.Now().After(deadline) {
			t.Fatal("sleep child never became visible to the process-tree walk")
		}
		time.Sleep(10 * time.Millisecond)
	}

	t.Run("times out when no agent descendant ever appears", func(t *testing.T) {
		exec := &wrappedWaitExecutor{deadPID: "999999999", livePID: "999999999"}
		tm := NewTmux()
		tm.exec = exec

		err := tm.WaitForCommand(context.Background(), "gc-test-wrapped-wait", supportedShells, 250*time.Millisecond)
		if err == nil {
			t.Fatal("WaitForCommand returned nil on wrapper sighting; want timeout while no agent descendant exists")
		}
	})

	t.Run("returns once the agent descendant appears", func(t *testing.T) {
		// The first two liveness probes see a dead pane PID (agent not yet
		// exec'd inside the scope); later probes see the live tree.
		exec := &wrappedWaitExecutor{deadPID: "999999999", livePID: livePID, livePIDAfter: 2}
		tm := NewTmux()
		tm.exec = exec

		if err := tm.WaitForCommand(context.Background(), "gc-test-wrapped-wait", supportedShells, 10*time.Second); err != nil {
			t.Fatalf("WaitForCommand: %v (agent descendant was live)", err)
		}
		if exec.pidRequests < 3 {
			t.Fatalf("pane PID requested %d times, want >= 3 (wait must keep polling until the descendant appears)", exec.pidRequests)
		}
	})
}

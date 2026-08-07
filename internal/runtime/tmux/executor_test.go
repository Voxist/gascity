package tmux

import (
	"context"
	"errors"
	"strconv"
	"strings"
	"testing"
	"time"
)

// fakeExecutor captures tmux command arguments for unit testing.
type fakeExecutor struct {
	calls [][]string // each call's full args
	out   string
	err   error
	outs  []string
	errs  []error
	idx   int
}

func (f *fakeExecutor) execute(args []string) (string, error) {
	// Copy args to avoid aliasing with the caller's slice.
	cp := make([]string, len(args))
	copy(cp, args)
	f.calls = append(f.calls, cp)
	if f.idx < len(f.outs) || f.idx < len(f.errs) {
		var out string
		var err error
		if f.idx < len(f.outs) {
			out = f.outs[f.idx]
		}
		if f.idx < len(f.errs) {
			err = f.errs[f.idx]
		}
		f.idx++
		return out, err
	}
	return f.out, f.err
}

func (f *fakeExecutor) executeCtx(_ context.Context, args []string) (string, error) {
	return f.execute(args)
}

func TestNewSessionWithCommandAndEnvClearsEmptyVars(t *testing.T) {
	exec := &fakeExecutor{}
	tm := NewTmux()
	tm.exec = exec

	env := map[string]string{
		"LANG":     "en_US.UTF-8",
		"LC_ALL":   "",
		"LC_CTYPE": "",
	}
	if err := tm.NewSessionWithCommandAndEnv("gc-test-locale-clear", "", "claude", env); err != nil {
		t.Fatalf("NewSessionWithCommandAndEnv: %v", err)
	}
	if len(exec.calls) == 0 {
		t.Fatal("no tmux calls recorded")
	}

	// C2 (ADR-0051): no -e flag anywhere in the launch sequence — that is the
	// argv-exposure defect this change removes. (runCtx prepends -u/-L to every
	// call, so scan every arg of every call.)
	for i, args := range exec.calls {
		for _, a := range args {
			if a == "-e" {
				t.Fatalf("call %d still uses -e flag (ADR-0051 transport): %v", i, args)
			}
		}
	}

	// The session is created bare (no command, no -e). cmd() finds the tmux
	// subcommand token past the -u/-L prefix injected by runCtx.
	newSession := exec.calls[0]
	if cmd(newSession) != "new-session" {
		t.Fatalf("first call = %q, want new-session: %v", cmd(newSession), newSession)
	}
	for _, a := range newSession {
		if a == "claude" || strings.HasPrefix(a, "env ") {
			t.Fatalf("new-session should not carry the command: %v", newSession)
		}
	}

	// LANG is set over the socket via set-environment; empty values via -u.
	foundSet := false
	foundUnsetAll := false
	foundUnsetCtype := false
	for _, args := range exec.calls {
		if cmd(args) != "set-environment" {
			continue
		}
		// set-environment -t <session> KEY VALUE  (or -u KEY)
		if contains(args, "LANG") && contains(args, "en_US.UTF-8") {
			foundSet = true
		}
		if contains(args, "-u") && contains(args, "LC_ALL") {
			foundUnsetAll = true
		}
		if contains(args, "-u") && contains(args, "LC_CTYPE") {
			foundUnsetCtype = true
		}
	}
	if !foundSet {
		t.Errorf("missing set-environment LANG en_US.UTF-8")
	}
	if !foundUnsetAll {
		t.Errorf("missing set-environment -u LC_ALL (empty-value unset)")
	}
	if !foundUnsetCtype {
		t.Errorf("missing set-environment -u LC_CTYPE (empty-value unset)")
	}

	// The command is started via respawn-pane -k -t <session> <wrapped command>.
	var respawn []string
	for _, args := range exec.calls {
		if cmd(args) == "respawn-pane" {
			respawn = args
			break
		}
	}
	if respawn == nil {
		t.Fatal("missing respawn-pane call to start the command")
	}
	if got := respawn[len(respawn)-1]; !strings.Contains(got, "claude") {
		t.Fatalf("respawn-pane command = %q, want it to contain claude", got)
	}
	// The env -u prefix the old transport bolted onto the command is gone —
	// unsetting is now a session-level set-environment -u, not a command prefix.
	if got := respawn[len(respawn)-1]; strings.HasPrefix(got, "env ") {
		t.Fatalf("respawn-pane command should not carry an env -u prefix: %q", got)
	}
}

// contains reports whether args contains s.
func contains(args []string, s string) bool {
	for _, a := range args {
		if a == s {
			return true
		}
	}
	return false
}

// cmd returns the tmux subcommand token from a recorded call, skipping the -u
// and -L <socket> flags that runCtx prepends to every invocation.
func cmd(args []string) string {
	for i := 0; i < len(args); i++ {
		switch args[i] {
		case "-u":
			continue
		case "-L":
			i++ // skip socket name
			continue
		default:
			return args[i]
		}
	}
	return ""
}

// TestNewSessionWithCommandAndEnvNoSecretInArgv is the load-bearing ADR-0051
// regression (Acceptance criterion C2): no secret value may be pinned to a
// long-lived process's argv. The -e transport placed every secret on the
// new-session command, which becomes the persistent tmux *server* argv; the
// set-environment transport must not reintroduce that.
//
// C2 scope note (ADR-0051 "C2 scope correction"): set-environment takes the value
// as a positional argv argument of a short-lived tmux *client* that exits in
// milliseconds. That transient client argv is the acknowledged bounded residual —
// NOT what this test guards. This test guards the persistent surface: the
// new-session call (server argv) and the respawn-pane call (the long-lived pane
// process). It also asserts no -e flag exists anywhere in the launch sequence.
func TestNewSessionWithCommandAndEnvNoSecretInArgv(t *testing.T) {
	exec := &fakeExecutor{}
	tm := NewTmux()
	tm.exec = exec

	const secretValue = "sk-SECRET-v1-0123456789-do-not-leak"
	env := map[string]string{
		"ANTHROPIC_AUTH_TOKEN": secretValue,
		"OPENROUTER_API_KEY":   secretValue,
		"GC_INSTANCE_TOKEN":    secretValue,
		"BEADS_HOLDER_TOKEN":   secretValue, // alias of GC_INSTANCE_TOKEN (same value, different name)
		"GT_ROLE":              "testrig/crew/x",
	}
	if err := tm.NewSessionWithCommandAndEnv("gc-test-no-leak", "", "claude", env); err != nil {
		t.Fatalf("NewSessionWithCommandAndEnv: %v", err)
	}
	if len(exec.calls) == 0 {
		t.Fatal("no tmux calls recorded")
	}

	// 1. No -e flag anywhere in the launch sequence (transport is clean).
	for i, args := range exec.calls {
		for _, a := range args {
			if a == "-e" {
				t.Fatalf("ADR-0051 C2 violation: call %d (%s) still uses -e flag: %v", i, args[0], args)
			}
		}
	}

	// 2. No secret value in the PERSISTENT surfaces: new-session (server argv)
	//    and respawn-pane (the long-lived pane process). set-environment calls are
	//    the transient client and are intentionally excluded.
	persistent := []string{"new-session", "respawn-pane"}
	for i, args := range exec.calls {
		if !contains(persistent, cmd(args)) {
			continue
		}
		joined := strings.Join(args, "\x00")
		if strings.Contains(joined, secretValue) {
			t.Fatalf("ADR-0051 C2 violation: secret value leaked into persistent %s "+
				"argv (call %d): %v", cmd(args), i, args)
		}
		// Even a "KEY=VALUE" pair (the -e serialization shape) must not appear.
		if strings.Contains(joined, "ANTHROPIC_AUTH_TOKEN=") ||
			strings.Contains(joined, "OPENROUTER_API_KEY=") ||
			strings.Contains(joined, "GC_INSTANCE_TOKEN=") ||
			strings.Contains(joined, "BEADS_HOLDER_TOKEN=") {
			t.Fatalf("ADR-0051 C2 violation: KEY=VALUE pair in persistent %s argv "+
				"(call %d): %v", cmd(args), i, args)
		}
	}

	// 3. Sanity: env WAS delivered — via set-environment (not -e). At least one
	//    set-environment call carries the GT_ROLE value (non-secret).
	delivered := false
	for _, args := range exec.calls {
		if cmd(args) == "set-environment" && contains(args, "testrig/crew/x") {
			delivered = true
			break
		}
	}
	if !delivered {
		t.Errorf("expected a set-environment call delivering GT_ROLE; calls: %v", exec.calls)
	}
}

type promptFooterExecutor struct {
	calls [][]string
}

func (p *promptFooterExecutor) execute(args []string) (string, error) {
	cp := make([]string, len(args))
	copy(cp, args)
	p.calls = append(p.calls, cp)
	if len(args) == 0 {
		return "", nil
	}
	for i := 0; i < len(args)-1; i++ {
		if args[i] != "-S" {
			continue
		}
		lines, err := strconv.Atoi(strings.TrimPrefix(args[i+1], "-"))
		if err != nil {
			return "", nil
		}
		if lines >= promptObservationLines {
			return strings.Join([]string{
				"Claude Code v2.1.112",
				"status line",
				"❯\u00a0",
				"",
				"",
				"",
				"",
				"",
				"",
				"",
				"",
				"",
				"",
				"",
			}, "\n"), nil
		}
		return strings.Repeat("\n", 20), nil
	}
	return "", nil
}

func (p *promptFooterExecutor) executeCtx(_ context.Context, args []string) (string, error) {
	return p.execute(args)
}

// ctxBlockingExecutor blocks executeCtx until ctx is canceled. Used to
// verify that callers honor a wall-clock deadline on the subprocess.
type ctxBlockingExecutor struct {
	calls [][]string
}

func (b *ctxBlockingExecutor) execute(args []string) (string, error) {
	cp := make([]string, len(args))
	copy(cp, args)
	b.calls = append(b.calls, cp)
	return "", nil
}

func (b *ctxBlockingExecutor) executeCtx(ctx context.Context, args []string) (string, error) {
	cp := make([]string, len(args))
	copy(cp, args)
	b.calls = append(b.calls, cp)
	<-ctx.Done()
	return "", ctx.Err()
}

// TestRunBoundsByTmuxSubprocessTimeout verifies that Tmux.run applies a
// wall-clock cap to subprocess invocations. A wedged tmux subprocess must
// not be able to hang the shutdown path indefinitely.
func TestRunBoundsByTmuxSubprocessTimeout(t *testing.T) {
	orig := tmuxSubprocessTimeout
	tmuxSubprocessTimeout = 50 * time.Millisecond
	t.Cleanup(func() { tmuxSubprocessTimeout = orig })

	bx := &ctxBlockingExecutor{}
	tm := &Tmux{cfg: DefaultConfig(), exec: bx}

	type result struct {
		err error
	}
	done := make(chan result, 1)
	start := time.Now()
	go func() {
		_, err := tm.run("list-sessions")
		done <- result{err: err}
	}()

	select {
	case r := <-done:
		elapsed := time.Since(start)
		if r.err == nil {
			t.Fatalf("err = nil after %s, want context.DeadlineExceeded", elapsed)
		}
		if !errors.Is(r.err, context.DeadlineExceeded) {
			t.Fatalf("err = %v, want context.DeadlineExceeded chain", r.err)
		}
		if elapsed > 500*time.Millisecond {
			t.Fatalf("elapsed = %s, want < 500ms", elapsed)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("tm.run did not return within 2s — tmuxSubprocessTimeout not applied")
	}
}

func TestRunInjectsSocketFlag(t *testing.T) {
	fe := &fakeExecutor{}
	tm := &Tmux{cfg: Config{SocketName: "bright-lights"}, exec: fe}
	_, _ = tm.run("list-sessions")

	if len(fe.calls) != 1 {
		t.Fatalf("expected 1 call, got %d", len(fe.calls))
	}
	got := fe.calls[0]
	want := []string{"-u", "-L", "bright-lights", "list-sessions"}
	if len(got) != len(want) {
		t.Fatalf("args = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("args[%d] = %q, want %q", i, got[i], want[i])
		}
	}
}

func TestRunNoSocketFlagWhenEmpty(t *testing.T) {
	fe := &fakeExecutor{}
	tm := &Tmux{cfg: DefaultConfig(), exec: fe}
	_, _ = tm.run("list-sessions")

	if len(fe.calls) != 1 {
		t.Fatalf("expected 1 call, got %d", len(fe.calls))
	}
	got := fe.calls[0]
	want := []string{"-u", "list-sessions"}
	if len(got) != len(want) {
		t.Fatalf("args = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("args[%d] = %q, want %q", i, got[i], want[i])
		}
	}
}

func TestHiddenAttachedKeyBytesSupportsArrowNavigation(t *testing.T) {
	tests := map[string]string{
		"Up":    "\x1b[A",
		"Down":  "\x1b[B",
		"Right": "\x1b[C",
		"Left":  "\x1b[D",
	}
	for key, want := range tests {
		got, ok := hiddenAttachedKeyBytes(key)
		if !ok {
			t.Fatalf("hiddenAttachedKeyBytes(%q) not supported", key)
		}
		if string(got) != want {
			t.Fatalf("hiddenAttachedKeyBytes(%q) = %q, want %q", key, string(got), want)
		}
	}
}

func TestRunAlwaysPrependsUTF8Flag(t *testing.T) {
	fe := &fakeExecutor{}
	tm := &Tmux{cfg: Config{SocketName: "x"}, exec: fe}
	_, _ = tm.run("new-session", "-s", "test")

	if len(fe.calls) != 1 {
		t.Fatalf("expected 1 call, got %d", len(fe.calls))
	}
	got := fe.calls[0]
	if got[0] != "-u" {
		t.Errorf("args[0] = %q, want %q", got[0], "-u")
	}
	// Verify full arg list: -u -L x new-session -s test
	want := []string{"-u", "-L", "x", "new-session", "-s", "test"}
	if len(got) != len(want) {
		t.Fatalf("args = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("args[%d] = %q, want %q", i, got[i], want[i])
		}
	}
}

func TestLatestActivityTimestamp(t *testing.T) {
	tests := []struct {
		name    string
		input   string
		want    int64
		wantErr bool
	}{
		{name: "single timestamp", input: "123", want: 123},
		{name: "multiple timestamps", input: "123\n456\n234", want: 456},
		{name: "blank lines ignored", input: "\n123\n\n456\n", want: 456},
		{name: "invalid timestamp", input: "123\nnope", wantErr: true},
		{name: "no timestamps", input: "\n\n", wantErr: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := latestActivityTimestamp(tt.input)
			if tt.wantErr {
				if err == nil {
					t.Fatalf("latestActivityTimestamp(%q) error = nil, want error", tt.input)
				}
				return
			}
			if err != nil {
				t.Fatalf("latestActivityTimestamp(%q) error = %v", tt.input, err)
			}
			if got != tt.want {
				t.Fatalf("latestActivityTimestamp(%q) = %d, want %d", tt.input, got, tt.want)
			}
		})
	}
}

func TestIsSessionRunningFalseWhenPaneDead(t *testing.T) {
	fe := &fakeExecutor{
		outs: []string{"", "1"},
	}
	tm := &Tmux{cfg: Config{SocketName: "x"}, exec: fe}

	if tm.IsSessionRunning("runner") {
		t.Fatal("IsSessionRunning = true, want false for dead pane")
	}

	if len(fe.calls) != 2 {
		t.Fatalf("expected 2 calls, got %d", len(fe.calls))
	}
	want := [][]string{
		{"-u", "-L", "x", "has-session", "-t", "=runner"},
		{"-u", "-L", "x", "display-message", "-t", "runner:^.0", "-p", "#{pane_dead}"},
	}
	for i := range want {
		if len(fe.calls[i]) != len(want[i]) {
			t.Fatalf("call %d = %v, want %v", i, fe.calls[i], want[i])
		}
		for j := range want[i] {
			if fe.calls[i][j] != want[i][j] {
				t.Errorf("call %d arg %d = %q, want %q", i, j, fe.calls[i][j], want[i][j])
			}
		}
	}
}

func TestIsSessionRunningFallsBackToSessionExistsOnPaneQueryError(t *testing.T) {
	fe := &fakeExecutor{
		outs: []string{""},
		errs: []error{nil, ErrNoServer},
	}
	tm := &Tmux{cfg: Config{SocketName: "x"}, exec: fe}

	if !tm.IsSessionRunning("runner") {
		t.Fatal("IsSessionRunning = false, want true when pane query fails after session exists")
	}
}

func TestProviderIsDeadRuntimeSessionRequiresEveryPaneDead(t *testing.T) {
	fe := &fakeExecutor{
		out: "1\n0",
	}
	tm := &Tmux{cfg: Config{SocketName: "x"}, exec: fe}
	p := &Provider{tm: tm}

	dead, err := p.IsDeadRuntimeSession("runner")
	if err != nil {
		t.Fatalf("IsDeadRuntimeSession: %v", err)
	}
	if dead {
		t.Fatal("IsDeadRuntimeSession = true, want false when any pane is live")
	}

	if len(fe.calls) != 1 {
		t.Fatalf("expected 1 call, got %d", len(fe.calls))
	}
	want := []string{"-u", "-L", "x", "list-panes", "-s", "-t", "=runner", "-F", "#{pane_dead}"}
	if len(fe.calls[0]) != len(want) {
		t.Fatalf("call = %v, want %v", fe.calls[0], want)
	}
	for i := range want {
		if fe.calls[0][i] != want[i] {
			t.Fatalf("call arg %d = %q, want %q; call=%v", i, fe.calls[0][i], want[i], fe.calls[0])
		}
	}
}

func TestProviderIsDeadRuntimeSessionTrueWhenAllPanesDead(t *testing.T) {
	fe := &fakeExecutor{
		out: "1\n1",
	}
	tm := &Tmux{cfg: Config{SocketName: "x"}, exec: fe}
	p := &Provider{tm: tm}

	dead, err := p.IsDeadRuntimeSession("runner")
	if err != nil {
		t.Fatalf("IsDeadRuntimeSession: %v", err)
	}
	if !dead {
		t.Fatal("IsDeadRuntimeSession = false, want true when all panes are dead")
	}
}

func TestProviderIsDeadRuntimeSessionTreatsAbsentSessionAsNotDead(t *testing.T) {
	fe := &fakeExecutor{
		err: ErrSessionNotFound,
	}
	tm := &Tmux{cfg: Config{SocketName: "x"}, exec: fe}
	p := &Provider{tm: tm}

	dead, err := p.IsDeadRuntimeSession("missing")
	if err != nil {
		t.Fatalf("IsDeadRuntimeSession: %v", err)
	}
	if dead {
		t.Fatal("IsDeadRuntimeSession = true, want false for absent session")
	}
}

func TestWaitForRuntimeReadyCapturesPromptAboveBlankFooter(t *testing.T) {
	fe := &promptFooterExecutor{}
	tm := &Tmux{cfg: DefaultConfig(), exec: fe}

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()

	err := tm.WaitForRuntimeReady(ctx, "mayor", &RuntimeConfig{
		Tmux: &RuntimeTmuxConfig{ReadyPromptPrefix: "❯ "},
	}, time.Second)
	if err != nil {
		t.Fatalf("WaitForRuntimeReady() error = %v, want nil", err)
	}

	if len(fe.calls) == 0 {
		t.Fatal("expected capture-pane call")
	}
	got := fe.calls[0]
	want := []string{"-u", "capture-pane", "-p", "-t", "mayor", "-S", "-120"}
	if len(got) != len(want) {
		t.Fatalf("first call = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("first call arg %d = %q, want %q", i, got[i], want[i])
		}
	}
}

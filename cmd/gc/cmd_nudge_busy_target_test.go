package main

import (
	"bytes"
	"context"
	"strings"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/runtime"
	"github.com/gastownhall/gascity/internal/session"
	"github.com/gastownhall/gascity/internal/worker"
)

// These tests cover the mail and sling siblings of the session-nudge busy
// pre-check (gco-90ui, vp-rqs8q). A wait-idle nudge to a running claude
// session whose activity clock reads busy must go to the durable queue
// without entering the worker's WaitForIdle, which would hold the caller for
// up to runtimeHandleWaitIdleTimeout. The plain Fake takes no idle snapshot,
// so the worker's own screen pre-read (runtime.IdleBoundaryNow) reads
// unknown and cannot catch the busy session: only the activity clock can.

// stubNudgePoller replaces startNudgePoller for the test and reports how
// many times it was called.
func stubNudgePoller(t *testing.T) *int {
	t.Helper()
	calls := 0
	prev := startNudgePoller
	startNudgePoller = func(_, _, _ string) error {
		calls++
		return nil
	}
	t.Cleanup(func() { startNudgePoller = prev })
	return &calls
}

// assertNoLiveWaitIdleLeg fails when calls show the live wait-idle leg ran:
// an idle wait or a delivery.
func assertNoLiveWaitIdleLeg(t *testing.T, calls []runtime.Call) {
	t.Helper()
	for _, call := range calls {
		switch call.Method {
		case "WaitForIdle":
			t.Fatalf("busy session entered WaitForIdle, the synchronous stall the busy pre-check prevents")
		case "Nudge", "NudgeNow":
			t.Fatalf("busy session took the live wait-idle leg; saw %s", call.Method)
		}
	}
}

// sawLiveDelivery reports whether calls include a live delivery.
func sawLiveDelivery(calls []runtime.Call) bool {
	for _, call := range calls {
		if call.Method == "Nudge" || call.Method == "NudgeNow" {
			return true
		}
	}
	return false
}

// runningMailTarget starts a store-less running session on a fresh Fake
// whose provider is the given family, with its activity clock at activity.
func runningMailTarget(t *testing.T, family string, activity time.Time) (nudgeTarget, *runtime.Fake) {
	t.Helper()
	t.Setenv("GC_BEADS", "file")
	dir := t.TempDir()
	fake := runtime.NewFake()
	if err := fake.Start(context.Background(), "sess-worker", runtime.Config{}); err != nil {
		t.Fatalf("Start: %v", err)
	}
	fake.SetActivity("sess-worker", activity)
	target := nudgeTarget{
		cityPath:    dir,
		agent:       config.Agent{Name: "worker", MaxActiveSessions: intPtrNudge(1)},
		resolved:    &config.ResolvedProvider{Name: family},
		sessionName: "sess-worker",
	}
	return target, fake
}

func TestSendMailNotifyBusyClaudeSessionQueuesWithoutWaitIdle(t *testing.T) {
	target, fake := runningMailTarget(t, "claude", time.Now())
	fake.WaitForIdleErrors["sess-worker"] = nil
	pollers := stubNudgePoller(t)

	if err := sendMailNotifyWithProvider(target, fake); err != nil {
		t.Fatalf("sendMailNotifyWithProvider: %v", err)
	}

	assertNoLiveWaitIdleLeg(t, fake.Calls)
	pending, inFlight, dead, err := listQueuedNudges(target.cityPath, target.agentKey(), time.Now())
	if err != nil {
		t.Fatalf("listQueuedNudges: %v", err)
	}
	if len(pending) != 1 || len(inFlight) != 0 || len(dead) != 0 {
		t.Fatalf("pending/inFlight/dead = %d/%d/%d, want 1/0/0", len(pending), len(inFlight), len(dead))
	}
	if pending[0].Source != "mail" {
		t.Fatalf("queued nudge source = %q, want mail", pending[0].Source)
	}
	if *pollers != 1 {
		t.Fatalf("startNudgePoller calls = %d, want 1 (the poller delivers at the next idle boundary)", *pollers)
	}
}

func TestSendMailNotifyQuietClaudeSessionStillDeliversLive(t *testing.T) {
	target, fake := runningMailTarget(t, "claude", time.Now().Add(-2*defaultNudgePollQuiescence))
	fake.WaitForIdleErrors["sess-worker"] = nil
	stubNudgePoller(t)

	if err := sendMailNotifyWithProvider(target, fake); err != nil {
		t.Fatalf("sendMailNotifyWithProvider: %v", err)
	}

	if !sawLiveDelivery(fake.Calls) {
		t.Fatalf("quiet session must still take the live leg; calls = %+v", fake.Calls)
	}
	pending, _, _, err := listQueuedNudges(target.cityPath, target.agentKey(), time.Now())
	if err != nil {
		t.Fatalf("listQueuedNudges: %v", err)
	}
	if len(pending) != 0 {
		t.Fatalf("live delivery also queued %d nudges, want 0", len(pending))
	}
}

// runningSlingTarget creates and starts a session-backed claude worker on a
// file store, with its activity clock at activity.
func runningSlingTarget(t *testing.T, activity time.Time) (nudgeTarget, *runtime.Fake, string) {
	t.Helper()
	clearGCEnv(t)
	disableManagedDoltRecoveryForTest(t)
	clearInheritedCityRoutingEnv(t)
	t.Setenv("GC_BEADS", "file")
	dir := t.TempDir()
	store := openNudgeBeadStore(dir)
	fake := runtime.NewFake()
	mgr := newSessionManagerWithConfig(dir, store, fake, nil)
	info, err := mgr.CreateSession(context.Background(), session.CreateOptions{Template: "worker", Title: "Worker", Command: "claude", WorkDir: dir, Provider: "claude", Env: nil, Resume: session.ProviderResume{}, Hints: runtime.Config{WorkDir: dir}, ExtraMeta: map[string]string{"session_origin": "manual"}})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if err := mgr.Start(context.Background(), testOperatorIn(t, dir), info.ID, "", runtime.Config{WorkDir: dir}); err != nil {
		t.Fatalf("Start: %v", err)
	}
	fake.WaitForIdleErrors[info.SessionName] = nil
	fake.SetActivity(info.SessionName, activity)
	target := nudgeTarget{
		cityPath:    dir,
		agent:       config.Agent{Name: "worker"},
		resolved:    &config.ResolvedProvider{Name: "claude"},
		sessionID:   info.ID,
		sessionName: info.SessionName,
	}
	return target, fake, dir
}

func TestDeliverSlingNudgeBusyClaudeSessionQueuesWithoutWaitIdle(t *testing.T) {
	target, fake, dir := runningSlingTarget(t, time.Now())
	pollers := stubNudgePoller(t)
	store := openNudgeBeadStore(dir)
	before := len(fake.Calls)

	var stdout, stderr bytes.Buffer
	deliverSlingNudge(target, fake, store, store, dir, "", &stdout, &stderr)

	assertNoLiveWaitIdleLeg(t, fake.Calls[before:])
	pending, inFlight, dead, err := listQueuedNudges(dir, target.agent.QualifiedName(), time.Now())
	if err != nil {
		t.Fatalf("listQueuedNudges: %v", err)
	}
	if len(pending) != 1 || len(inFlight) != 0 || len(dead) != 0 {
		t.Fatalf("pending/inFlight/dead = %d/%d/%d, want 1/0/0", len(pending), len(inFlight), len(dead))
	}
	if pending[0].Source != "sling" {
		t.Fatalf("queued nudge source = %q, want sling", pending[0].Source)
	}
	out := stdout.String()
	if strings.Contains(out, "Nudged") {
		t.Fatalf("stdout reports a live delivery for a queued nudge: %q", out)
	}
	if strings.Contains(out, "asleep") {
		t.Fatalf("stdout calls a running session asleep: %q", out)
	}
	if !strings.Contains(out, "Queued nudge for worker") {
		t.Fatalf("stdout = %q, want the queued line", out)
	}
	if *pollers != 1 {
		t.Fatalf("startNudgePoller calls = %d, want 1", *pollers)
	}
}

func TestDeliverSlingNudgeQuietClaudeSessionStillDeliversLive(t *testing.T) {
	target, fake, dir := runningSlingTarget(t, time.Now().Add(-2*defaultNudgePollQuiescence))
	stubNudgePoller(t)
	store := openNudgeBeadStore(dir)
	before := len(fake.Calls)

	var stdout, stderr bytes.Buffer
	deliverSlingNudge(target, fake, store, store, dir, "", &stdout, &stderr)

	if !sawLiveDelivery(fake.Calls[before:]) {
		t.Fatalf("quiet session must still take the live leg; calls = %+v", fake.Calls[before:])
	}
	if !strings.Contains(stdout.String(), "Nudged worker") {
		t.Fatalf("stdout = %q, want the live delivery line", stdout.String())
	}
}

// Only a claude-family session off ACP on a provider that waits for idle can
// hold the caller in WaitForIdle; every other shape keeps its live attempt,
// which the worker answers at once.
func TestNudgeWaitIdleMayBlockOnlyForClaudeOffACPWithAnIdleWaiter(t *testing.T) {
	fake := runtime.NewFake()
	claude := nudgeTarget{agent: config.Agent{Name: "worker"}, resolved: &config.ResolvedProvider{Name: "claude"}}
	if !nudgeWaitIdleMayBlock(claude, fake) {
		t.Fatal("claude off ACP with an idle-waiting provider: want may block")
	}
	wrapped := nudgeTarget{agent: config.Agent{Name: "worker"}, resolved: &config.ResolvedProvider{Name: "fast-claude", BuiltinAncestor: "claude"}}
	if !nudgeWaitIdleMayBlock(wrapped, fake) {
		t.Fatal("a provider wrapping builtin claude is claude-family: want may block")
	}
	acp := claude
	acp.transport = "acp"
	if nudgeWaitIdleMayBlock(acp, fake) {
		t.Fatal("ACP delivers in process: want no block")
	}
	codex := nudgeTarget{agent: config.Agent{Name: "worker"}, resolved: &config.ResolvedProvider{Name: "codex"}}
	if nudgeWaitIdleMayBlock(codex, fake) {
		t.Fatal("non-claude family returns at once: want no block")
	}
	if nudgeWaitIdleMayBlock(claude, nil) {
		t.Fatal("no provider cannot wait: want no block")
	}
}

// A busy session of a family that never waits for idle keeps the live leg.
func TestNudgeLiveLegBusyKeepsNonClaudeLive(t *testing.T) {
	fake := runtime.NewFake()
	now := time.Now()
	busy := worker.LiveObservation{Running: true, LastActivity: &now}
	codex := nudgeTarget{agent: config.Agent{Name: "worker"}, resolved: &config.ResolvedProvider{Name: "codex"}, sessionName: "sess-worker"}
	if nudgeLiveLegBusy(codex, fake, busy) {
		t.Fatal("busy codex session: want the live leg kept")
	}
	claude := codex
	claude.resolved = &config.ResolvedProvider{Name: "claude"}
	if !nudgeLiveLegBusy(claude, fake, busy) {
		t.Fatal("busy claude session: want the live leg skipped")
	}
	if nudgeLiveLegBusy(claude, fake, worker.LiveObservation{Running: true}) {
		t.Fatal("unknown activity: want the live leg kept")
	}
	if nudgeLiveLegBusy(claude, fake, worker.LiveObservation{LastActivity: &now}) {
		t.Fatal("not running: want no busy verdict")
	}
}

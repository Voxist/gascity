package main

import (
	"bytes"
	"context"
	"strings"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/runtime"
	"github.com/gastownhall/gascity/internal/session"
)

// busyClaudeTarget is the reproducer for the T-00B1 half of vp-rqs8q: a
// RUNNING claude, non-ACP seat whose LastActivity is "just now" — the
// nudgeObservationBusy shape (activity newer than defaultNudgePollQuiescence)
// that makes a wait-idle Nudge enter the worker's synchronous WaitForIdle
// (runtimeHandleWaitIdleTimeout, 30s). The busy state is configured on the
// Fake provider (SetActivity) so the observation flows through the REAL
// workerObserveNudgeTarget path — no observe seam is patched.
func busyClaudeTarget(t *testing.T) (nudgeTarget, *runtime.Fake, beads.Store) {
	t.Helper()
	t.Setenv("GC_BEADS", "file")
	dir := t.TempDir()
	store := openNudgeBeadStore(dir)
	fake := runtime.NewFake()
	mgr := newSessionManagerWithConfig(dir, store, fake, nil)

	info, err := mgr.CreateSession(context.Background(), session.CreateOptions{
		Template: "worker", Title: "Worker", Command: "claude", WorkDir: dir,
		Provider: "claude", Env: nil, Resume: session.ProviderResume{},
		Hints:     runtime.Config{WorkDir: dir},
		ExtraMeta: map[string]string{"session_origin": "manual"},
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	// NOT suspended: the seat is running — that is the trap. The old code
	// gates the live wait-idle leg on obs.Running alone, and a busy seat is
	// running. SetActivity(now) is what makes it BUSY: activity newer than
	// the poll quiescence window.
	fake.SetActivity(info.SessionName, time.Now())

	target := nudgeTarget{
		cityPath:    dir,
		cfg:         &config.City{Agents: []config.Agent{{Name: "worker", Provider: "claude"}}},
		sessionID:   info.ID,
		sessionName: info.SessionName,
		identity:    "worker",
		agent:       config.Agent{Name: "worker", Provider: "claude"},
	}

	// Supervisor dispatcher: the queue's owner. Keeps the fallback legs
	// (maybeStartNudgePoller) from spawning a sidecar poller in the test.
	target.cfg.Daemon.NudgeDispatcher = "supervisor"

	return target, fake, store
}

// T-00B1 site 2 (vp-rqs8q): gc mail --notify to a busy claude seat must not
// enter the worker's synchronous WaitForIdle. The busy pre-check the session-
// nudge path has had since gco-90ui applies here too: skip the live leg and
// let the durable queue own delivery at the next idle boundary. RED today:
// the live leg runs (fake sees "Nudge"), the queue stays empty, and the
// caller stalls for the full 30s window on a real seat.
func TestSendMailNotifyWithWorkerBusyClaudeTargetQueuesInsteadOfWaitIdle(t *testing.T) {
	target, fake, _ := busyClaudeTarget(t)
	dir := target.cityPath

	beforeCalls := len(fake.Calls)
	if err := sendMailNotifyWithWorker(target, openNudgeBeadStore(dir), fake, "human", ""); err != nil {
		t.Fatalf("sendMailNotifyWithWorker: %v", err)
	}

	for _, call := range fake.Calls[beforeCalls:] {
		switch call.Method {
		case "WaitForIdle":
			t.Fatalf("busy target must not enter WaitForIdle (the synchronous stall); saw WaitForIdle")
		case "Nudge", "NudgeNow":
			t.Fatalf("busy target must not enter the live wait-idle leg; saw %s", call.Method)
		}
	}

	pending, inFlight, dead, err := listQueuedNudgesForTarget(dir, target, time.Now())
	if err != nil {
		t.Fatalf("listQueuedNudgesForTarget: %v", err)
	}
	if len(pending) != 1 || len(inFlight) != 0 || len(dead) != 0 {
		t.Fatalf("pending/inFlight/dead = %d/%d/%d, want 1/0/0 (durable queue owns a busy-seat delivery)",
			len(pending), len(inFlight), len(dead))
	}
	if pending[0].Source != "mail" {
		t.Fatalf("source = %q, want mail", pending[0].Source)
	}
}

// T-00B1 site 3 (vp-rqs8q): gc sling's nudge leg to a busy claude seat must
// not enter WaitForIdle either. sling is the fleet's prescribed pool-
// delivery instrument — the stall here is the load-bearing one. RED today:
// the fake sees "Nudge" and stdout claims "Nudged worker" after the caller
// has in fact stalled.
func TestDeliverSlingNudgeBusyClaudeTargetQueuesInsteadOfWaitIdle(t *testing.T) {
	target, fake, store := busyClaudeTarget(t)
	dir := target.cityPath

	var stdout, stderr bytes.Buffer
	beforeCalls := len(fake.Calls)
	deliverSlingNudge(target, fake, store, dir, &stdout, &stderr)

	for _, call := range fake.Calls[beforeCalls:] {
		switch call.Method {
		case "WaitForIdle":
			t.Fatalf("busy target must not enter WaitForIdle (the synchronous stall); saw WaitForIdle")
		case "Nudge", "NudgeNow":
			t.Fatalf("busy target must not enter the live wait-idle leg; saw %s", call.Method)
		}
	}

	pending, inFlight, dead, err := listQueuedNudgesForTarget(dir, target, time.Now())
	if err != nil {
		t.Fatalf("listQueuedNudgesForTarget: %v", err)
	}
	if len(pending) != 1 || len(inFlight) != 0 || len(dead) != 0 {
		t.Fatalf("pending/inFlight/dead = %d/%d/%d, want 1/0/0 (durable queue owns a busy-seat delivery)",
			len(pending), len(inFlight), len(dead))
	}
	if pending[0].Source != "sling" {
		t.Fatalf("source = %q, want sling", pending[0].Source)
	}
	if strings.Contains(stdout.String(), "Nudged") {
		t.Fatalf("stdout claims live delivery of a nudge that went to the queue: %q", stdout.String())
	}
}

// Positive controls: an IDLE running seat (activity past the quiescence
// window) must still take the LIVE leg on both paths — proving the busy
// tests above skip the leg because of the busy gate, not because the
// fixture cannot deliver live at all.
func TestSendMailNotifyWithWorkerIdleTargetStillDeliversLive(t *testing.T) {
	target, fake, _ := busyClaudeTarget(t)
	dir := target.cityPath
	// Overwrite the busy activity with a quiesced one.
	fake.SetActivity(target.sessionName, time.Now().Add(-defaultNudgePollQuiescence*2))
	// The Fake's WaitForIdle defaults to ErrInteractionUnsupported when no
	// result is configured; permit the idle boundary so the control can
	// reach the live delivery tail.
	fake.WaitForIdleErrors[target.sessionName] = nil

	beforeCalls := len(fake.Calls)
	if err := sendMailNotifyWithWorker(target, openNudgeBeadStore(dir), fake, "human", ""); err != nil {
		t.Fatalf("sendMailNotifyWithWorker: %v", err)
	}
	sawNudge := false
	for _, call := range fake.Calls[beforeCalls:] {
		if call.Method == "Nudge" || call.Method == "NudgeNow" {
			sawNudge = true
		}
	}
	if !sawNudge {
		t.Fatalf("idle target must still deliver live; fake calls after: %+v", fake.Calls[beforeCalls:])
	}
	pending, _, _, err := listQueuedNudgesForTarget(dir, target, time.Now())
	if err != nil {
		t.Fatalf("listQueuedNudgesForTarget: %v", err)
	}
	if len(pending) != 0 {
		t.Fatalf("idle live delivery must not also enqueue; pending = %d", len(pending))
	}
}

func TestDeliverSlingNudgeIdleTargetStillDeliversLive(t *testing.T) {
	target, fake, store := busyClaudeTarget(t)
	dir := target.cityPath
	fake.SetActivity(target.sessionName, time.Now().Add(-defaultNudgePollQuiescence*2))
	fake.WaitForIdleErrors[target.sessionName] = nil

	var stdout, stderr bytes.Buffer
	beforeCalls := len(fake.Calls)
	deliverSlingNudge(target, fake, store, dir, &stdout, &stderr)
	sawNudge := false
	for _, call := range fake.Calls[beforeCalls:] {
		if call.Method == "Nudge" || call.Method == "NudgeNow" {
			sawNudge = true
		}
	}
	if !sawNudge {
		t.Fatalf("idle target must still deliver live; fake calls after: %+v", fake.Calls[beforeCalls:])
	}
	if !strings.Contains(stdout.String(), "Nudged") {
		t.Fatalf("idle live delivery must report Nudged; stdout = %q", stdout.String())
	}
}

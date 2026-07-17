package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/agent"
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/nudgequeue"
	"github.com/gastownhall/gascity/internal/runtime"
	"github.com/gastownhall/gascity/internal/session"
)

// installHangingNudgeStoreSeam swaps openNudgeBeadStore for a fake whose open
// blocks until test cleanup, simulating a hung shared Dolt server (the vl-3hb
// incident shape: the store open itself stalls, not just a query). The
// abandoned resolution goroutine unblocks at cleanup, receives an empty store,
// and drains through the buffered result channel. Tests using it must stay
// serial (package-var seam); do not use t.Parallel.
func installHangingNudgeStoreSeam(t *testing.T) {
	t.Helper()
	prev := openNudgeBeadStore
	unblock := make(chan struct{})
	openNudgeBeadStore = func(string) beads.NudgesStore {
		<-unblock
		return beads.NudgesStore{}
	}
	t.Cleanup(func() {
		openNudgeBeadStore = prev
		close(unblock)
	})
}

// withShortNudgeStoreBudget shrinks the bounded store-resolution budget so
// hung-store tests complete quickly. Serial-only (package-var seam).
func withShortNudgeStoreBudget(t *testing.T, d time.Duration) {
	t.Helper()
	prev := nudgeStoreBudget
	nudgeStoreBudget = d
	t.Cleanup(func() { nudgeStoreBudget = prev })
}

// storelessTestConfig routes queued-nudge delivery to the supervisor
// dispatcher so queue-mode tests never attempt to spawn a per-session poller
// subprocess.
func storelessTestConfig() *config.City {
	return &config.City{Daemon: config.DaemonConfig{NudgeDispatcher: "supervisor"}}
}

func TestParseNudgeStorelessFlagValues(t *testing.T) {
	for _, v := range []string{"1", "true", "TRUE", " yes ", "on", "On"} {
		if !parseNudgeStorelessFlag(v) {
			t.Errorf("parseNudgeStorelessFlag(%q) = false; want true", v)
		}
	}
	for _, v := range []string{"", "0", "false", "off", "no", "junk"} {
		if parseNudgeStorelessFlag(v) {
			t.Errorf("parseNudgeStorelessFlag(%q) = true; want false (flag is default-off)", v)
		}
	}
}

// TestEnqueueQueuedNudgeIntoWithoutStoreSkipsShadowOpen pins the store-independent
// enqueue transport: an explicitly absent shadow store must write the item to the
// flock'd state.json authority without ever opening a bead store (no Dolt
// dependency), leaving the shadow-bead id empty.
func TestEnqueueQueuedNudgeIntoWithoutStoreSkipsShadowOpen(t *testing.T) {
	opens, _ := installCountingNudgeStoreSeam(t)
	dir := t.TempDir()

	item := newQueuedNudgeWithOptions("worker", "storeless hello", "session", time.Now(), queuedNudgeOptions{ID: "n-storeless-1"})
	if err := enqueueQueuedNudgeInto(dir, beads.NudgesStore{}, item); err != nil {
		t.Fatalf("enqueueQueuedNudgeInto: %v", err)
	}
	if *opens != 0 {
		t.Fatalf("storeless enqueue opened the bead store %d times; want 0", *opens)
	}

	var pending []nudgequeue.Item
	if err := nudgequeue.WithState(dir, func(s *nudgequeue.State) error {
		pending = append([]nudgequeue.Item(nil), s.Pending...)
		return nil
	}); err != nil {
		t.Fatalf("reading queue state: %v", err)
	}
	if len(pending) != 1 {
		t.Fatalf("pending = %d items; want 1", len(pending))
	}
	if pending[0].ID != "n-storeless-1" {
		t.Fatalf("pending item ID = %q; want n-storeless-1", pending[0].ID)
	}
	if pending[0].BeadID != "" {
		t.Fatalf("pending item BeadID = %q; want empty (no shadow bead without a store)", pending[0].BeadID)
	}
}

// TestResolveNudgeTargetViaStoreBoundedTripsOnHungOpen pins the bounded store
// attempt: a store open that hangs (shared Dolt server stalled) must trip the
// budget instead of blocking the caller for the legacy 30/60s.
func TestResolveNudgeTargetViaStoreBoundedTripsOnHungOpen(t *testing.T) {
	installHangingNudgeStoreSeam(t)
	withShortNudgeStoreBudget(t, 50*time.Millisecond)

	start := time.Now()
	_, err := resolveNudgeTargetViaStoreBounded(t.TempDir(), &config.City{}, "anyone")
	elapsed := time.Since(start)

	if err == nil {
		t.Fatal("resolveNudgeTargetViaStoreBounded returned nil error under a hung store open")
	}
	if !errors.Is(err, errNudgeStoreBudgetExceeded) {
		t.Fatalf("err = %v; want errNudgeStoreBudgetExceeded", err)
	}
	if elapsed > 5*time.Second {
		t.Fatalf("bounded resolve took %s under a hung store; budget was 50ms", elapsed)
	}
}

// TestResolveNudgeTargetViaStoreBoundedPassesThroughFastErrors pins that a
// healthy store's authoritative answer is not misclassified as degradation.
func TestResolveNudgeTargetViaStoreBoundedPassesThroughFastErrors(t *testing.T) {
	installCountingNudgeStoreSeam(t)

	_, err := resolveNudgeTargetViaStoreBounded(t.TempDir(), &config.City{}, "nobody")
	if err == nil {
		t.Fatal("expected a not-found error from an empty healthy store")
	}
	if !errors.Is(err, session.ErrSessionNotFound) {
		t.Fatalf("err = %v; want session.ErrSessionNotFound", err)
	}
	if errors.Is(err, errNudgeStoreBudgetExceeded) {
		t.Fatalf("fast store error misclassified as budget exceeded: %v", err)
	}
}

// TestResolveNudgeTargetStorelessResolvesLiveRuntimeSession pins the
// store-independent target resolution: a qualified identity maps to its
// tmux-safe runtime session name via the naming convention and resolves
// against the live runtime provider, with no bead store involved.
func TestResolveNudgeTargetStorelessResolvesLiveRuntimeSession(t *testing.T) {
	opens, _ := installCountingNudgeStoreSeam(t)
	fake := runtime.NewFake()
	const identity = "voxist-platform/voxist.planner-1"
	sessionName := agent.SanitizeQualifiedNameForSession(identity)
	if err := fake.Start(context.Background(), sessionName, runtime.Config{}); err != nil {
		t.Fatalf("fake.Start: %v", err)
	}

	target, err := resolveNudgeTargetStoreless(t.TempDir(), &config.City{}, fake, identity)
	if err != nil {
		t.Fatalf("resolveNudgeTargetStoreless: %v", err)
	}
	if !target.storeless {
		t.Fatal("target.storeless = false; want true")
	}
	if target.sessionName != sessionName {
		t.Fatalf("target.sessionName = %q; want %q", target.sessionName, sessionName)
	}
	if target.alias != identity {
		t.Fatalf("target.alias = %q; want %q (queue items must match the caller's identifier)", target.alias, identity)
	}
	if *opens != 0 {
		t.Fatalf("storeless resolution opened the bead store %d times; want 0", *opens)
	}
}

func TestResolveNudgeTargetStorelessErrsWhenNoLiveSession(t *testing.T) {
	fake := runtime.NewFake()
	_, err := resolveNudgeTargetStoreless(t.TempDir(), &config.City{}, fake, "ghost/agent")
	if err == nil {
		t.Fatal("expected an error when no live runtime session matches")
	}
	if !errors.Is(err, session.ErrSessionNotFound) {
		t.Fatalf("err = %v; want session.ErrSessionNotFound", err)
	}
}

// TestSessionNudgeStorelessFallbackQueuesDespiteHungStore is the vl-3hb WS-B
// acceptance regression: with the shared store made artificially unreachable
// (open hangs), `gc session nudge --delivery queue` must deliver via the
// store-independent transport within its bounded budget — no 30/60s hang —
// and report the path used.
func TestSessionNudgeStorelessFallbackQueuesDespiteHungStore(t *testing.T) {
	installHangingNudgeStoreSeam(t)
	withShortNudgeStoreBudget(t, 50*time.Millisecond)
	dir := t.TempDir()
	cfg := storelessTestConfig()
	fake := runtime.NewFake()
	const identity = "voxist-platform/voxist.planner-1"
	sessionName := agent.SanitizeQualifiedNameForSession(identity)
	if err := fake.Start(context.Background(), sessionName, runtime.Config{}); err != nil {
		t.Fatalf("fake.Start: %v", err)
	}

	var stdout, stderr bytes.Buffer
	start := time.Now()
	rc := sessionNudgeWithStorelessFallback(dir, cfg, fake, identity, "wake up, store is down", nudgeDeliveryQueue, true, &stdout, &stderr)
	elapsed := time.Since(start)

	if rc != 0 {
		t.Fatalf("rc = %d; stderr:\n%s", rc, stderr.String())
	}
	if elapsed > 5*time.Second {
		t.Fatalf("nudge under hung store took %s; must complete within the bounded budget", elapsed)
	}

	var out sessionNudgeJSON
	if err := json.Unmarshal(bytes.TrimSpace(stdout.Bytes()), &out); err != nil {
		t.Fatalf("parsing JSON output %q: %v", stdout.String(), err)
	}
	if !out.OK || !out.Queued {
		t.Fatalf("JSON output = %+v; want OK && Queued", out)
	}
	if out.Path != "storeless-fallback" {
		t.Fatalf("JSON path = %q; want storeless-fallback (must report the path used)", out.Path)
	}
	if !strings.Contains(stderr.String(), "storeless fallback") {
		t.Fatalf("stderr does not report the degraded store path:\n%s", stderr.String())
	}

	var pending []nudgequeue.Item
	if err := nudgequeue.WithState(dir, func(s *nudgequeue.State) error {
		pending = append([]nudgequeue.Item(nil), s.Pending...)
		return nil
	}); err != nil {
		t.Fatalf("reading queue state: %v", err)
	}
	if len(pending) != 1 {
		t.Fatalf("pending = %d items; want 1", len(pending))
	}
	if pending[0].Agent != identity {
		t.Fatalf("queued item Agent = %q; want %q", pending[0].Agent, identity)
	}
	if pending[0].Message != "wake up, store is down" {
		t.Fatalf("queued item Message = %q", pending[0].Message)
	}
	if pending[0].BeadID != "" {
		t.Fatalf("queued item BeadID = %q; want empty (shadow bead must be skipped under fallback)", pending[0].BeadID)
	}
}

// TestSessionNudgeStorelessFallbackDeliversLiveImmediate pins live delivery on
// the fallback path: an immediate nudge to a running session goes straight to
// the runtime provider with no store open.
func TestSessionNudgeStorelessFallbackDeliversLiveImmediate(t *testing.T) {
	installHangingNudgeStoreSeam(t)
	withShortNudgeStoreBudget(t, 50*time.Millisecond)
	dir := t.TempDir()
	cfg := storelessTestConfig()
	fake := runtime.NewFake()
	const identity = "voxist-platform/voxist.planner-1"
	sessionName := agent.SanitizeQualifiedNameForSession(identity)
	if err := fake.Start(context.Background(), sessionName, runtime.Config{}); err != nil {
		t.Fatalf("fake.Start: %v", err)
	}

	var stdout, stderr bytes.Buffer
	rc := sessionNudgeWithStorelessFallback(dir, cfg, fake, identity, "wake up now", nudgeDeliveryImmediate, true, &stdout, &stderr)
	if rc != 0 {
		t.Fatalf("rc = %d; stderr:\n%s", rc, stderr.String())
	}

	var out sessionNudgeJSON
	if err := json.Unmarshal(bytes.TrimSpace(stdout.Bytes()), &out); err != nil {
		t.Fatalf("parsing JSON output %q: %v", stdout.String(), err)
	}
	if !out.OK || out.Queued {
		t.Fatalf("JSON output = %+v; want OK && !Queued", out)
	}
	if out.Path != "storeless-fallback" {
		t.Fatalf("JSON path = %q; want storeless-fallback", out.Path)
	}

	delivered := false
	for _, call := range fake.SnapshotCalls() {
		if (call.Method == "Nudge" || call.Method == "NudgeNow") && strings.Contains(call.Message, "wake up now") {
			delivered = true
		}
	}
	if !delivered {
		t.Fatalf("runtime provider never received the nudge; calls: %+v", fake.SnapshotCalls())
	}
}

// TestSessionNudgeStorelessFallbackOnStoreMiss pins the unreachable-store
// shape: a store whose open fails fast presents as not-found today, so the
// fallback must also engage on an authoritative miss and deliver via the live
// runtime when a matching session exists.
func TestSessionNudgeStorelessFallbackOnStoreMiss(t *testing.T) {
	installCountingNudgeStoreSeam(t)
	dir := t.TempDir()
	cfg := storelessTestConfig()
	fake := runtime.NewFake()
	const identity = "voxist-platform/voxist.planner-1"
	sessionName := agent.SanitizeQualifiedNameForSession(identity)
	if err := fake.Start(context.Background(), sessionName, runtime.Config{}); err != nil {
		t.Fatalf("fake.Start: %v", err)
	}

	var stdout, stderr bytes.Buffer
	rc := sessionNudgeWithStorelessFallback(dir, cfg, fake, identity, "store missed you", nudgeDeliveryQueue, true, &stdout, &stderr)
	if rc != 0 {
		t.Fatalf("rc = %d; stderr:\n%s", rc, stderr.String())
	}
	var out sessionNudgeJSON
	if err := json.Unmarshal(bytes.TrimSpace(stdout.Bytes()), &out); err != nil {
		t.Fatalf("parsing JSON output %q: %v", stdout.String(), err)
	}
	if out.Path != "storeless-fallback" {
		t.Fatalf("JSON path = %q; want storeless-fallback", out.Path)
	}
}

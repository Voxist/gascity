package main

import (
	"strings"
	"testing"
	"time"
)

var epoch = time.Date(2026, 6, 2, 0, 0, 0, 0, time.UTC)

// --- sessionProgressStalled unit tests ---

func TestProgressStalled_DisabledWhenThresholdZero(t *testing.T) {
	stale := epoch.Add(-2 * time.Hour)
	if sessionProgressStalled(0, false, true, false, stale, epoch) {
		t.Fatal("threshold=0 must not stall")
	}
}

func TestProgressStalled_HoldsClaimNotStalled(t *testing.T) {
	stale := epoch.Add(-2 * time.Hour)
	if sessionProgressStalled(time.Hour, true, true, false, stale, epoch) {
		t.Fatal("session holding claim must not be stalled")
	}
}

func TestProgressStalled_ProviderRedNotStalled(t *testing.T) {
	stale := epoch.Add(-2 * time.Hour)
	if sessionProgressStalled(time.Hour, false, false, false, stale, epoch) {
		t.Fatal("provider unhealthy must not trigger stall (provider gate wins)")
	}
}

func TestProgressStalled_ExemptNotStalled(t *testing.T) {
	stale := epoch.Add(-2 * time.Hour)
	if sessionProgressStalled(time.Hour, false, true, true, stale, epoch) {
		t.Fatal("exempt session must not be stalled")
	}
}

func TestProgressStalled_ZeroProgressUnknownNotStalled(t *testing.T) {
	if sessionProgressStalled(time.Hour, false, true, false, time.Time{}, epoch) {
		t.Fatal("zero progress_at (unknown) must not trigger stall")
	}
}

func TestProgressStalled_FreshProgressNotStalled(t *testing.T) {
	// Progress 10 minutes ago, threshold 45 minutes — should not stall.
	recent := epoch.Add(-10 * time.Minute)
	if sessionProgressStalled(45*time.Minute, false, true, false, recent, epoch) {
		t.Fatal("recent progress must not trigger stall")
	}
}

func TestProgressStalled_StaleProgressFires(t *testing.T) {
	// Progress 50 minutes ago, threshold 45 minutes — should stall.
	stale := epoch.Add(-50 * time.Minute)
	if !sessionProgressStalled(45*time.Minute, false, true, false, stale, epoch) {
		t.Fatal("stale progress must trigger stall")
	}
}

func TestProgressStalled_ExactThresholdNotStalled(t *testing.T) {
	// Exactly at the threshold: progress at exactly T ago — NOT stalled (>).
	atThreshold := epoch.Add(-45 * time.Minute)
	if sessionProgressStalled(45*time.Minute, false, true, false, atThreshold, epoch) {
		t.Fatal("progress exactly at threshold must not stall (> not >=)")
	}
}

// --- progressEpisodeTracker tests ---

func TestProgressEpisode_FirstRestartOpensEpisode(t *testing.T) {
	tr := newProgressEpisodeTracker()
	count, escalate := tr.recordStallRestart("sess-1", epoch)
	if count != 1 {
		t.Fatalf("expected count=1, got %d", count)
	}
	if escalate {
		t.Fatal("should not escalate on first restart")
	}
}

func TestProgressEpisode_KThresholdEscalates(t *testing.T) {
	tr := newProgressEpisodeTracker()
	var escalated bool
	for i := 0; i < progressEscalationK; i++ {
		_, esc := tr.recordStallRestart("sess-1", epoch.Add(time.Duration(i)*time.Minute))
		if esc {
			escalated = true
		}
	}
	if !escalated {
		t.Fatalf("expected escalation after %d restarts", progressEscalationK)
	}
}

func TestProgressEpisode_EscalationOnlyOnce(t *testing.T) {
	tr := newProgressEpisodeTracker()
	alerts := 0
	for i := 0; i < progressEscalationK*2; i++ {
		_, esc := tr.recordStallRestart("sess-1", epoch.Add(time.Duration(i)*time.Minute))
		if esc {
			alerts++
		}
	}
	if alerts != 1 {
		t.Fatalf("expected exactly 1 escalation alert, got %d", alerts)
	}
}

func TestProgressEpisode_IsEscalatedAfterK(t *testing.T) {
	tr := newProgressEpisodeTracker()
	for i := 0; i < progressEscalationK; i++ {
		tr.recordStallRestart("sess-1", epoch.Add(time.Duration(i)*time.Minute))
	}
	if !tr.isEscalated("sess-1") {
		t.Fatal("expected isEscalated=true after K restarts")
	}
}

func TestProgressEpisode_AdvanceProgressResetsEpisode(t *testing.T) {
	tr := newProgressEpisodeTracker()
	// Record K restarts.
	var lastRestart time.Time
	for i := 0; i < progressEscalationK; i++ {
		lastRestart = epoch.Add(time.Duration(i) * time.Minute)
		tr.recordStallRestart("sess-1", lastRestart)
	}
	if !tr.isEscalated("sess-1") {
		t.Fatal("precondition: expected escalated after K restarts")
	}

	// Advance progress past last restart.
	tr.advanceProgress("sess-1", lastRestart.Add(time.Second))

	// Episode should be reset.
	if tr.isEscalated("sess-1") {
		t.Fatal("expected isEscalated=false after progress advances")
	}
	// New restarts should work from zero.
	count, _ := tr.recordStallRestart("sess-1", epoch.Add(time.Hour))
	if count != 1 {
		t.Fatalf("expected fresh episode count=1 after reset, got %d", count)
	}
}

func TestProgressEpisode_AdvanceProgressBeforeLastRestartNoOp(t *testing.T) {
	tr := newProgressEpisodeTracker()
	tr.recordStallRestart("sess-1", epoch.Add(10*time.Minute))
	tr.recordStallRestart("sess-1", epoch.Add(20*time.Minute))

	// Progress at epoch+15 is BEFORE last restart at epoch+20 — no reset.
	tr.advanceProgress("sess-1", epoch.Add(15*time.Minute))
	// Verify episode still exists (not escalated after only 2 restarts, but episode present).
	tr.mu.Lock()
	if tr.episodes["sess-1"] == nil {
		tr.mu.Unlock()
		t.Fatal("episode must not be reset when progress is before last restart")
	}
	tr.mu.Unlock()
	count, _ := tr.recordStallRestart("sess-1", epoch.Add(30*time.Minute))
	if count != 3 {
		t.Fatalf("expected count=3 (no reset), got %d", count)
	}
}

func TestProgressEpisode_ResetOnReclaim(t *testing.T) {
	tr := newProgressEpisodeTracker()
	for i := 0; i < progressEscalationK; i++ {
		tr.recordStallRestart("sess-1", epoch.Add(time.Duration(i)*time.Minute))
	}
	tr.resetOnReclaim("sess-1")
	if tr.isEscalated("sess-1") {
		t.Fatal("expected episode cleared after reclaim")
	}
}

func TestEmitProgressStallEscalationAlert_Format(t *testing.T) {
	var captured string
	w := &capWriter{fn: func(b []byte) { captured += string(b) }}
	redSince := time.Date(2026, 6, 2, 12, 0, 0, 0, time.UTC)
	emitProgressStallEscalationAlert(nil, w, "myagent", "bead-1", "worker", "ep-123", redSince, 3)

	for _, want := range []string{"myagent", "ep-123", "worker", "2026-06-02T12:00:00Z", "restarts=3"} {
		if !strings.Contains(captured, want) {
			t.Errorf("alert missing %q\ngot: %s", want, captured)
		}
	}
}

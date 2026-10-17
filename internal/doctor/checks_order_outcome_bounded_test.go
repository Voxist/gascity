package doctor

// Regression guards for vc-sen0: order-outcome-healthy's event reads used to
// carry no Since bound at all, so readOrderOutcomeEvents and
// controllerStartTimes streamed and gunzipped every rotation archive on every
// doctor run — measured ~7.9GB across 56 archives on a live city, which alone
// blew the doctor's check budget regardless of the live log's size.

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/events"
)

// TestSinceWindowForAppliesFloorAndScale pins the arithmetic both
// order-firing-current and order-outcome-healthy derive their Since window
// from: 3x the widest monitored interval, floored at eventLogSinceFloor so a
// city with only short-interval orders still keeps enough trailing history.
func TestSinceWindowForAppliesFloorAndScale(t *testing.T) {
	now := time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)

	cases := []struct {
		name        string
		maxExpected time.Duration
		wantWindow  time.Duration
	}{
		{"zero expected uses floor", 0, eventLogSinceFloor},
		{"short interval uses floor", 5 * time.Minute, eventLogSinceFloor},
		{"3x still under floor uses floor", time.Hour, eventLogSinceFloor},
		{"wide interval uses 3x", 6 * time.Hour, 18 * time.Hour},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := sinceWindowFor(tc.maxExpected, now)
			want := now.Add(-tc.wantWindow)
			if !got.Equal(want) {
				t.Fatalf("sinceWindowFor(%v, now) = %v, want %v", tc.maxExpected, got, want)
			}
		})
	}
}

// TestOrderOutcomeHealthy_SkipsUnreadableArchiveOutsideWindow is the direct
// regression guard for the defect: a corrupt (truncated-rotation-shaped)
// archive that predates the derived Since window must never be opened, so it
// can never fail the read. Without Since-bounding, readFilteredCore opens
// every archive unconditionally and this test would fail with "reading
// archive: ...".
func TestOrderOutcomeHealthy_SkipsUnreadableArchiveOutsideWindow(t *testing.T) {
	now := time.Date(2026, 9, 10, 12, 0, 0, 0, time.UTC)
	cityPath, cfg := orderFiringTestCity(t)
	writeOrderFiringTestOrder(t, cityPath, "cleanup-cooldown", "cooldown", "1h")

	// expected=1h -> window = max(3h, floor) = eventLogSinceFloor (6h), so
	// since = now-6h. Place the corrupt archive well before that boundary.
	writeOrderFiringCorruptArchive(t, cityPath, now.Add(-30*24*time.Hour), 1, 100)

	writeOrderFiringTestEvents(t, cityPath,
		events.Event{Type: events.OrderFailed, Ts: now.Add(-3 * time.Hour), Subject: "cleanup-cooldown", Message: "exit status 1"},
		events.Event{Type: events.OrderFailed, Ts: now.Add(-2 * time.Hour), Subject: "cleanup-cooldown", Message: "exit status 1"},
		events.Event{Type: events.OrderFailed, Ts: now.Add(-1 * time.Hour), Subject: "cleanup-cooldown", Message: "exit status 1"},
	)

	check := NewOrderOutcomeHealthyCheck(cfg, cityPath)
	check.clock = func() time.Time { return now }
	result := check.Run(&CheckContext{CityPath: cityPath})

	if result.Status != StatusWarning {
		t.Fatalf("status = %v, want StatusWarning; msg=%s details=%v (a non-skip would surface an archive read error instead)", result.Status, result.Message, result.Details)
	}
}

// TestOrderOutcomeHealthy_ReadsStillErrorOnUnreadableArchiveInsideWindow is
// the inverse of the skip guard above: a corrupt archive the window cannot
// exclude must still surface as a read error, proving the skip above is
// Since-driven and not a change that silently ignores every corrupt archive.
func TestOrderOutcomeHealthy_ReadsStillErrorOnUnreadableArchiveInsideWindow(t *testing.T) {
	now := time.Date(2026, 9, 10, 12, 0, 0, 0, time.UTC)
	cityPath, cfg := orderFiringTestCity(t)
	writeOrderFiringTestOrder(t, cityPath, "cleanup-cooldown", "cooldown", "1h")

	// Rotated 1 hour ago: well inside the 6h floor window, so it must be opened.
	writeOrderFiringCorruptArchive(t, cityPath, now.Add(-1*time.Hour), 1, 100)

	check := NewOrderOutcomeHealthyCheck(cfg, cityPath)
	check.clock = func() time.Time { return now }
	result := check.Run(&CheckContext{CityPath: cityPath})

	if result.Status != StatusError {
		t.Fatalf("status = %v, want StatusError (the corrupt archive sits inside the window and must fail loud, never be silently skipped); msg=%s", result.Status, result.Message)
	}
}

// TestOrderOutcomeHealthy_LargeArchiveChainStaysInsideBudget is the
// behavioral half of the guard: with many archives old enough to be excluded
// by the window, the check must still finish well inside its budget. It
// fails if anyone reinstates an unbounded read, independent of the
// skip-correctness assertions above.
func TestOrderOutcomeHealthy_LargeArchiveChainStaysInsideBudget(t *testing.T) {
	now := time.Date(2026, 9, 10, 12, 0, 0, 0, time.UTC)
	cityPath, cfg := orderFiringTestCity(t)
	writeOrderFiringTestOrder(t, cityPath, "cleanup-cooldown", "cooldown", "1h")

	// 50 archives, oldest-first, each rotated a day further back than the
	// last and each unreadable — any one of them being opened fails the test
	// loudly (read error) long before the elapsed-time budget would catch it.
	for i := 0; i < 50; i++ {
		rotatedAt := now.Add(-time.Duration(30+i) * 24 * time.Hour)
		writeOrderFiringCorruptArchive(t, cityPath, rotatedAt, uint64(i*100+1), uint64(i*100+100))
	}

	writeOrderFiringTestEvents(t, cityPath,
		events.Event{Type: events.OrderFailed, Ts: now.Add(-3 * time.Hour), Subject: "cleanup-cooldown", Message: "exit status 1"},
		events.Event{Type: events.OrderFailed, Ts: now.Add(-2 * time.Hour), Subject: "cleanup-cooldown", Message: "exit status 1"},
		events.Event{Type: events.OrderFailed, Ts: now.Add(-1 * time.Hour), Subject: "cleanup-cooldown", Message: "exit status 1"},
	)

	check := NewOrderOutcomeHealthyCheck(cfg, cityPath)
	check.clock = func() time.Time { return now }

	start := time.Now()
	result := check.Run(&CheckContext{CityPath: cityPath})
	elapsed := time.Since(start)

	if result.Status != StatusWarning {
		t.Fatalf("status = %v, want StatusWarning; msg=%s details=%v", result.Status, result.Message, result.Details)
	}
	if budget := 5 * time.Second; elapsed > budget {
		t.Fatalf("check took %s against 50 archives, want under %s; the archive chain is likely being read unbounded again", elapsed, budget)
	}
}

// TestOrderOutcomeHealthy_LargeLiveLogStaysInsideBudget covers the live-file
// half of the budget: a large body of unrelated noise in the active log must
// not blow the check's time budget either.
func TestOrderOutcomeHealthy_LargeLiveLogStaysInsideBudget(t *testing.T) {
	now := time.Date(2026, 9, 10, 12, 0, 0, 0, time.UTC)
	cityPath, cfg := orderFiringTestCity(t)
	writeOrderFiringTestOrder(t, cityPath, "cleanup-cooldown", "cooldown", "1h")

	evts := make([]events.Event, 0, 40003)
	for i := 0; i < 40000; i++ {
		evts = append(evts, events.Event{
			Type:    events.OrderCompleted,
			Subject: fmt.Sprintf("noise-order-%d", i%64),
			Ts:      now.Add(-3 * time.Hour),
		})
	}
	evts = append(evts,
		events.Event{Type: events.OrderFailed, Ts: now.Add(-3 * time.Hour), Subject: "cleanup-cooldown", Message: "exit status 1"},
		events.Event{Type: events.OrderFailed, Ts: now.Add(-2 * time.Hour), Subject: "cleanup-cooldown", Message: "exit status 1"},
		events.Event{Type: events.OrderFailed, Ts: now.Add(-1 * time.Hour), Subject: "cleanup-cooldown", Message: "exit status 1"},
	)
	writeOrderFiringTestEvents(t, cityPath, evts...)

	check := NewOrderOutcomeHealthyCheck(cfg, cityPath)
	check.clock = func() time.Time { return now }

	start := time.Now()
	result := check.Run(&CheckContext{CityPath: cityPath})
	elapsed := time.Since(start)

	if result.Status != StatusWarning {
		t.Fatalf("status = %v, want StatusWarning; msg=%s details=%v", result.Status, result.Message, result.Details)
	}
	if budget := 5 * time.Second; elapsed > budget {
		t.Fatalf("check took %s against a 40k-line live log, want under %s", elapsed, budget)
	}
}

// TestOrderOutcomeHealthy_SinceWindowIsLosslessForTrailingStreak pins
// AC2 (vc-sen0): a genuine trailing failure streak of exactly
// orderOutcomeFailureThreshold failures, with the oldest sitting right at the
// derived window's edge, must still be reported in full — the window must
// never truncate a streak it is supposed to contain by construction.
func TestOrderOutcomeHealthy_SinceWindowIsLosslessForTrailingStreak(t *testing.T) {
	now := time.Date(2026, 9, 10, 12, 0, 0, 0, time.UTC)
	cityPath, cfg := orderFiringTestCity(t)
	// expected=2h -> window = 3*2h = 6h (already at the floor, so this also
	// exercises the non-floor scaling branch at the boundary).
	writeOrderFiringTestOrder(t, cityPath, "flaky-cooldown", "cooldown", "2h")

	writeOrderFiringTestEvents(t, cityPath,
		// Oldest failure sits exactly at the window edge (now-6h): must be
		// included, not pruned, per the Filter.Since "at or after" contract.
		events.Event{Type: events.OrderFailed, Ts: now.Add(-6 * time.Hour), Subject: "flaky-cooldown", Message: "exit status 1"},
		events.Event{Type: events.OrderFailed, Ts: now.Add(-4 * time.Hour), Subject: "flaky-cooldown", Message: "exit status 1"},
		events.Event{Type: events.OrderFailed, Ts: now.Add(-2 * time.Hour), Subject: "flaky-cooldown", Message: "exit status 1"},
	)

	check := NewOrderOutcomeHealthyCheck(cfg, cityPath)
	check.clock = func() time.Time { return now }
	result := check.Run(&CheckContext{CityPath: cityPath})

	if result.Status != StatusWarning {
		t.Fatalf("status = %v, want StatusWarning; msg=%s details=%v", result.Status, result.Message, result.Details)
	}
	joined := fmt.Sprint(result.Details)
	if want := "3 consecutive failures"; !strings.Contains(joined, want) {
		t.Fatalf("details = %v, want them to report the full 3-failure streak (the window must not truncate it)", result.Details)
	}
}

// TestOrderOutcomeHealthy_ControllerStartLookbackCoversGraceBeforeWindow pins
// FIX CONTRACT item b: the controller-start read must look back further than
// the outcome window's own since by the post-start grace margin, or a start
// that itself falls just before the window's edge would be invisible to
// nearControllerStart — misclassifying a spurious post-restart failure right
// at the window boundary as a real one.
func TestOrderOutcomeHealthy_ControllerStartLookbackCoversGraceBeforeWindow(t *testing.T) {
	now := time.Date(2026, 9, 10, 12, 0, 0, 0, time.UTC)
	cityPath, cfg := orderFiringTestCity(t)
	writeOrderFiringTestOrder(t, cityPath, "flaky-cooldown", "cooldown", "2h")

	// window = 6h, since = now-6h. The controller start sits 8 minutes before
	// the window edge — outside the OUTCOME read (Since: since) but inside the
	// START read (Since: since-grace), which is precisely the margin fix-contract
	// item b buys. The failure lands 1 minute inside the window and 9 minutes
	// after the start, i.e. within the 10m grace: it must be skipped, not
	// counted. A failure OUTSIDE the window would prove nothing here — the
	// outcome read prunes it before classification ever sees it.
	since := now.Add(-6 * time.Hour)
	start := since.Add(-8 * time.Minute)
	writeOrderFiringTestEvents(t, cityPath,
		events.Event{Type: events.ControllerStarted, Ts: start},
		events.Event{Type: events.OrderFailed, Ts: since.Add(time.Minute), Subject: "flaky-cooldown", Message: "exit status 1"},
	)

	check := NewOrderOutcomeHealthyCheck(cfg, cityPath)
	check.clock = func() time.Time { return now }
	result := check.Run(&CheckContext{CityPath: cityPath})

	if result.Status != StatusOK {
		t.Fatalf("status = %v, want StatusOK; msg=%s details=%v (the sole failure is a spurious post-restart burst and must not alarm)", result.Status, result.Message, result.Details)
	}
	joined := fmt.Sprint(result.Details)
	if want := "within controller-start grace window"; !strings.Contains(joined, want) {
		t.Fatalf("details = %v, want them to attribute the suppression to the grace window, proving the controller start was actually seen", result.Details)
	}
}

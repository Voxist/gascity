package main

import (
	"encoding/json"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/events"
	"github.com/gastownhall/gascity/internal/supervisordoctor"
)

// newDoctorTestState builds a minimal controllerState wired with a fake
// event provider for doctor-subset tests.
func newDoctorTestState(t *testing.T, cfg *config.City) (*controllerState, *events.Fake) {
	t.Helper()
	ep := events.NewFake()
	cs := &controllerState{
		cfg:       cfg,
		cityName:  "testcity",
		cityPath:  t.TempDir(),
		eventProv: ep,
	}
	return cs, ep
}

// alertsOfCheck returns the doctor.alert events whose payload Check matches.
func alertsOfCheck(t *testing.T, ep *events.Fake, check string) []events.DoctorAlertPayload {
	t.Helper()
	var out []events.DoctorAlertPayload
	for _, e := range ep.Events {
		if e.Type != events.DoctorAlert {
			continue
		}
		decoded, _, err := events.DecodePayload(e.Type, e.Payload)
		if err != nil {
			t.Fatalf("decode doctor.alert: %v", err)
		}
		p, ok := decoded.(events.DoctorAlertPayload)
		if ok && p.Check == check {
			out = append(out, p)
		}
	}
	return out
}

// TestDoctorSubsetTickAgeStaleEmitsAlert asserts a stale controller
// heartbeat (> 3× patrol) produces a tick_age doctor.alert.
func TestDoctorSubsetTickAgeStaleEmitsAlert(t *testing.T) {
	cfg := &config.City{}
	cfg.Daemon.PatrolInterval = "10s"
	cs, ep := newDoctorTestState(t, cfg)

	// Record a controller.tick_completed event well in the past so its age
	// exceeds 3×10s = 30s. Pin the doctor clock for determinism.
	stale := time.Date(2026, 6, 10, 0, 0, 0, 0, time.UTC)
	ep.Record(events.Event{Type: events.ControllerTickCompleted, Ts: stale})

	prev := supervisorDoctorClock
	supervisorDoctorClock = func() time.Time { return stale.Add(2 * time.Minute) }
	defer func() { supervisorDoctorClock = prev }()

	evaluateCityDoctorSubset(cs, io.Discard)

	alerts := alertsOfCheck(t, ep, supervisordoctor.CheckNameTickAge)
	if len(alerts) != 1 {
		t.Fatalf("tick_age alerts = %d, want 1", len(alerts))
	}
	if alerts[0].City != "testcity" {
		t.Errorf("alert city = %q, want testcity", alerts[0].City)
	}
}

// TestDoctorSubsetTickAgeFreshNoAlert asserts a fresh heartbeat produces no
// tick_age alert.
func TestDoctorSubsetTickAgeFreshNoAlert(t *testing.T) {
	cfg := &config.City{}
	cfg.Daemon.PatrolInterval = "10s"
	cs, ep := newDoctorTestState(t, cfg)

	fresh := time.Date(2026, 6, 10, 0, 0, 0, 0, time.UTC)
	ep.Record(events.Event{Type: events.ControllerTickCompleted, Ts: fresh})

	prev := supervisorDoctorClock
	supervisorDoctorClock = func() time.Time { return fresh.Add(5 * time.Second) }
	defer func() { supervisorDoctorClock = prev }()

	evaluateCityDoctorSubset(cs, io.Discard)

	if got := len(alertsOfCheck(t, ep, supervisordoctor.CheckNameTickAge)); got != 0 {
		t.Fatalf("tick_age alerts = %d, want 0 (fresh heartbeat)", got)
	}
}

// TestDoctorSubsetNoHeartbeatNoTickAlert asserts that with no
// controller.tick_completed event yet, the tick-age check is skipped.
func TestDoctorSubsetNoHeartbeatNoTickAlert(t *testing.T) {
	cfg := &config.City{}
	cfg.Daemon.PatrolInterval = "10s"
	cs, ep := newDoctorTestState(t, cfg)

	evaluateCityDoctorSubset(cs, io.Discard)

	if got := len(alertsOfCheck(t, ep, supervisordoctor.CheckNameTickAge)); got != 0 {
		t.Fatalf("tick_age alerts = %d, want 0 (no heartbeat yet)", got)
	}
}

// recordTickEvent records a controller.tick_completed event with a typed
// payload at ts.
func recordTickEvent(t *testing.T, ep *events.Fake, ts time.Time, breach bool, durationMs int64) {
	t.Helper()
	payload, err := json.Marshal(events.ControllerTickCompletedPayload{
		DurationMs:      durationMs,
		Phase:           "patrol",
		ThresholdBreach: breach,
	})
	if err != nil {
		t.Fatalf("marshal tick payload: %v", err)
	}
	ep.Record(events.Event{Type: events.ControllerTickCompleted, Ts: ts, Payload: payload})
}

// TestDoctorSubsetSlowTicksBreachEmitsAlert asserts breached ticks inside
// the doctor window produce a slow_ticks doctor.alert — the consumer that
// makes the heartbeat's threshold_breach flag load-bearing (vp-qvqk
// defect 1: the flag was emitted and never read).
func TestDoctorSubsetSlowTicksBreachEmitsAlert(t *testing.T) {
	cfg := &config.City{}
	cfg.Daemon.PatrolInterval = "30s"
	cs, ep := newDoctorTestState(t, cfg)

	now := time.Date(2026, 7, 17, 1, 30, 0, 0, time.UTC)
	// Two breached ticks and one clean tick inside the default 10m window.
	recordTickEvent(t, ep, now.Add(-8*time.Minute), true, 443000)
	recordTickEvent(t, ep, now.Add(-5*time.Minute), false, 32000)
	recordTickEvent(t, ep, now.Add(-2*time.Minute), true, 91000)

	prev := supervisorDoctorClock
	supervisorDoctorClock = func() time.Time { return now }
	defer func() { supervisorDoctorClock = prev }()

	evaluateCityDoctorSubset(cs, io.Discard)

	alerts := alertsOfCheck(t, ep, supervisordoctor.CheckNameSlowTicks)
	if len(alerts) != 1 {
		t.Fatalf("slow_ticks alerts = %d, want 1", len(alerts))
	}
	if alerts[0].City != "testcity" {
		t.Errorf("alert city = %q, want testcity", alerts[0].City)
	}
	if !strings.Contains(alerts[0].Detail, "2 of 3") {
		t.Errorf("alert detail = %q, want it to count 2 of 3 breached ticks", alerts[0].Detail)
	}
}

// TestDoctorSubsetSlowTicksCleanWindowNoAlert asserts an all-clean window
// stays quiet.
func TestDoctorSubsetSlowTicksCleanWindowNoAlert(t *testing.T) {
	cfg := &config.City{}
	cfg.Daemon.PatrolInterval = "30s"
	cs, ep := newDoctorTestState(t, cfg)

	now := time.Date(2026, 7, 17, 1, 30, 0, 0, time.UTC)
	recordTickEvent(t, ep, now.Add(-4*time.Minute), false, 31000)
	recordTickEvent(t, ep, now.Add(-2*time.Minute), false, 47000)

	prev := supervisorDoctorClock
	supervisorDoctorClock = func() time.Time { return now }
	defer func() { supervisorDoctorClock = prev }()

	evaluateCityDoctorSubset(cs, io.Discard)

	if got := len(alertsOfCheck(t, ep, supervisordoctor.CheckNameSlowTicks)); got != 0 {
		t.Fatalf("slow_ticks alerts = %d, want 0 (clean window)", got)
	}
}

// TestDoctorSubsetSlowTicksOldBreachOutsideWindowNoAlert asserts a breach
// older than the doctor window does not re-alert forever.
func TestDoctorSubsetSlowTicksOldBreachOutsideWindowNoAlert(t *testing.T) {
	cfg := &config.City{}
	cfg.Daemon.PatrolInterval = "30s"
	cs, ep := newDoctorTestState(t, cfg)

	now := time.Date(2026, 7, 17, 1, 30, 0, 0, time.UTC)
	// Breach 2h ago — far outside the default 10m window. A fresh clean
	// tick keeps the tick-age check quiet so this isolates slow_ticks.
	recordTickEvent(t, ep, now.Add(-2*time.Hour), true, 443000)
	recordTickEvent(t, ep, now.Add(-1*time.Minute), false, 30000)

	prev := supervisorDoctorClock
	supervisorDoctorClock = func() time.Time { return now }
	defer func() { supervisorDoctorClock = prev }()

	evaluateCityDoctorSubset(cs, io.Discard)

	if got := len(alertsOfCheck(t, ep, supervisordoctor.CheckNameSlowTicks)); got != 0 {
		t.Fatalf("slow_ticks alerts = %d, want 0 (breach outside window)", got)
	}
}

// TestDoctorSubsetS6CeilingBreachEmitsAlert asserts the S6 connection
// ceiling fires when scopes × (pool+1) exceeds 0.8 × max_connections.
func TestDoctorSubsetS6CeilingBreachEmitsAlert(t *testing.T) {
	cfg := &config.City{}
	// One rig + city = 2 scopes; the breach comes from a large pool against
	// a tiny max_connections so 2×(pool+1) > 0.8×max.
	cfg.Rigs = []config.Rig{{Name: "r1", Path: "rigs/r1"}}
	pool := 30
	cfg.Beads.ProxyPoolSize = &pool
	cfg.Dolt.MaxConnections = 10 // ceiling = 8; projected = 2×31 = 62 > 8
	cs, ep := newDoctorTestState(t, cfg)

	evaluateCityDoctorSubset(cs, io.Discard)

	if got := len(alertsOfCheck(t, ep, supervisordoctor.CheckNameS6ConnectionCeiling)); got != 1 {
		t.Fatalf("s6 alerts = %d, want 1", got)
	}
}

// TestDoctorSubsetS6CeilingWithinBudgetNoAlert asserts the ceiling stays
// quiet within budget.
func TestDoctorSubsetS6CeilingWithinBudgetNoAlert(t *testing.T) {
	cfg := &config.City{}
	pool := 2
	cfg.Beads.ProxyPoolSize = &pool
	cfg.Dolt.MaxConnections = 256 // ceiling = 204.8; projected = 1×3 = 3
	cs, ep := newDoctorTestState(t, cfg)

	evaluateCityDoctorSubset(cs, io.Discard)

	if got := len(alertsOfCheck(t, ep, supervisordoctor.CheckNameS6ConnectionCeiling)); got != 0 {
		t.Fatalf("s6 alerts = %d, want 0 (within budget)", got)
	}
}

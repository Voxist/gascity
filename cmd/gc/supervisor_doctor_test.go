package main

import (
	"io"
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

package main

import (
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/gastownhall/gascity/internal/api"
	"github.com/gastownhall/gascity/internal/events"
	"github.com/gastownhall/gascity/internal/supervisordoctor"
)

// supervisorDoctorClock is injectable so tests can pin "now". Production
// uses time.Now.
var supervisorDoctorClock = time.Now

// defaultSupervisorDoctorInterval is the doctor cadence when no running
// city configures one ([doctor].supervisor_interval).
const defaultSupervisorDoctorInterval = 10 * time.Minute

// supervisorDoctorIntervalFor picks the doctor cadence: the smallest
// [doctor].supervisor_interval among running cities (the most vigilant city
// wins), falling back to the 10m default when none are running or none
// configure it. A single supervisor ticker serves all cities.
func supervisorDoctorIntervalFor(reg *cityRegistry) time.Duration {
	best := defaultSupervisorDoctorInterval
	found := false
	if reg != nil {
		if snap := reg.Snapshot(); snap != nil {
			for _, v := range snap.all {
				if v == nil || v.cs == nil {
					continue
				}
				cfg := v.cs.Config()
				if cfg == nil {
					continue
				}
				d := cfg.Doctor.SupervisorIntervalOrDefault()
				if d <= 0 {
					continue
				}
				if !found || d < best {
					best = d
					found = true
				}
			}
		}
	}
	return best
}

// runSupervisorDoctorSubset evaluates the cheap doctor-check subset across
// every running city and emits a doctor.alert event for each red finding
// (plan item 1.9). It is invoked by the supervisor on startup and on the
// [doctor] supervisor_interval — independent of any one city's tick or
// store path, so it can detect the failures an in-band sentinel cannot
// (incidents 5 and 11). Best-effort and side-effect-only: it never blocks
// the supervisor loop and surfaces findings exclusively through events.
//
// The subset here is the part buildable on this branch: tick-age,
// agent_config_isolation, and the S6 connection ceiling. Provenance and
// port-file-consistency checks are intentionally omitted — they are owned
// by sibling scale branches and are not registered here; this driver
// degrades gracefully without them rather than hard-depending on them.
func runSupervisorDoctorSubset(reg *cityRegistry, stderr io.Writer) {
	if reg == nil {
		return
	}
	snap := reg.Snapshot()
	if snap == nil {
		return
	}
	for _, v := range snap.all {
		if v == nil || !v.Started || v.cs == nil {
			continue
		}
		evaluateCityDoctorSubset(v.cs, stderr)
	}
}

// evaluateCityDoctorSubset runs the subset for one city's controller state.
func evaluateCityDoctorSubset(state api.State, _ io.Writer) {
	cfg := state.Config()
	if cfg == nil {
		return
	}
	cityName := state.CityName()
	cityPath := state.CityPath()
	ep := state.EventProvider()

	var alerts []supervisordoctor.Alert

	// Tick-age: stale controller heartbeat (> 3× patrol).
	age, hasHeartbeat := lastControllerTickAge(ep, cityName)
	if a := supervisordoctor.CheckTickAgeFor(supervisordoctor.TickAgeInput{
		City:           cityName,
		LastTickAge:    age,
		PatrolInterval: cfg.Daemon.PatrolIntervalDuration(),
		HasHeartbeat:   hasHeartbeat,
	}); a != nil {
		alerts = append(alerts, *a)
	}

	// Slow ticks: consume the heartbeat's threshold_breach flag (vp-qvqk —
	// a canary nothing reads carries no signal). The window is this city's
	// doctor cadence, so each pass reviews roughly the ticks since the
	// previous one.
	window := cfg.Doctor.SupervisorIntervalOrDefault()
	if window <= 0 {
		window = defaultSupervisorDoctorInterval
	}
	breaches, samples, slowestMs := recentTickBreaches(ep, window)
	if a := supervisordoctor.CheckSlowTicksFor(supervisordoctor.SlowTicksInput{
		City:        cityName,
		BreachCount: breaches,
		SampleCount: samples,
		SlowestMs:   slowestMs,
		Window:      window,
	}); a != nil {
		alerts = append(alerts, *a)
	}

	// S6 connection ceiling: scopes × (pool+1) ≤ 0.8 × max_connections.
	// collectStage1ScopeRoots dedups colliding rig paths, so the count never
	// over-estimates the connection ceiling when two rigs resolve to the same
	// scope root (the patrol's own enumeration keeps duplicates intentionally).
	scopes := len(collectStage1ScopeRoots(cityPath, cfg))
	if a := supervisordoctor.CheckS6ConnectionCeiling(supervisordoctor.S6Input{
		City:           cityName,
		Scopes:         scopes,
		PoolSize:       cfg.Beads.ProxyPoolSizeOrDefault(),
		MaxConnections: cfg.Dolt.EffectiveMaxConnections(),
	}); a != nil {
		alerts = append(alerts, *a)
	}

	// Agent config isolation: no symlink may escape an agent config dir.
	alerts = append(alerts, supervisordoctor.CheckAgentConfigIsolation(agentConfigDirRoots(cityPath))...)

	for i := range alerts {
		emitDoctorAlert(ep, cityName, alerts[i])
	}
}

// lastControllerTickAge returns the age of the most recent
// controller.tick_completed event for a city and whether one exists. A nil
// provider or read error yields (0, false) so the tick-age check is skipped
// rather than firing a false positive.
func lastControllerTickAge(ep events.Provider, _ string) (time.Duration, bool) {
	if ep == nil {
		return 0, false
	}
	filter := events.Filter{Type: events.ControllerTickCompleted, Limit: 1}
	var list []events.Event
	var err error
	if tp, ok := ep.(events.TailProvider); ok {
		list, err = tp.ListTail(filter, 1)
	} else {
		list, err = ep.List(filter)
	}
	if err != nil || len(list) == 0 {
		return 0, false
	}
	last := list[len(list)-1]
	if last.Ts.IsZero() {
		return 0, false
	}
	age := supervisorDoctorClock().Sub(last.Ts)
	if age < 0 {
		age = 0
	}
	return age, true
}

// maxTickEventsScanned bounds the slow-tick window read. At one heartbeat
// per tick and patrol-cadence ticks, a 10m doctor window holds ~20
// events; 512 leaves an order of magnitude of headroom without letting a
// pathological log turn the doctor pass into a full scan.
const maxTickEventsScanned = 512

// recentTickBreaches inspects the controller.tick_completed events inside
// the trailing window and returns how many carried threshold_breach, how
// many were inspected, and the slowest duration seen. A nil provider or
// read error yields zeros so the slow-ticks check is skipped rather than
// firing a false positive. Events whose payload does not decode to the
// tick payload are not counted as samples.
func recentTickBreaches(ep events.Provider, window time.Duration) (breaches, samples int, slowestMs int64) {
	if ep == nil || window <= 0 {
		return 0, 0, 0
	}
	filter := events.Filter{
		Type:  events.ControllerTickCompleted,
		Since: supervisorDoctorClock().Add(-window),
	}
	var list []events.Event
	var err error
	if tp, ok := ep.(events.TailProvider); ok {
		list, err = tp.ListTail(filter, maxTickEventsScanned)
	} else {
		filter.Limit = maxTickEventsScanned
		list, err = ep.List(filter)
	}
	if err != nil {
		return 0, 0, 0
	}
	for _, e := range list {
		decoded, _, derr := events.DecodePayload(e.Type, e.Payload)
		if derr != nil {
			continue
		}
		p, ok := decoded.(events.ControllerTickCompletedPayload)
		if !ok {
			continue
		}
		samples++
		if p.DurationMs > slowestMs {
			slowestMs = p.DurationMs
		}
		if p.ThresholdBreach {
			breaches++
		}
	}
	return breaches, samples, slowestMs
}

// agentConfigDirRoots returns the agent config-dir roots the isolation check
// should scan for escaping symlinks. It is conservative and read-only: the
// city's .gc/agents scaffold tree plus any per-agent config dirs gc
// maintains under the user's ~/.gc/agent-* (the incident-11 location). Roots
// that do not exist are skipped by the check itself.
func agentConfigDirRoots(cityPath string) []string {
	var roots []string
	if cityPath != "" {
		roots = append(roots, filepath.Join(cityPath, ".gc", "agents"))
	}
	if home, err := os.UserHomeDir(); err == nil && home != "" {
		gcDir := filepath.Join(home, ".gc")
		if entries, err := os.ReadDir(gcDir); err == nil {
			for _, e := range entries {
				if e.IsDir() && strings.HasPrefix(e.Name(), "agent-") && e.Name() != "agent-" {
					roots = append(roots, filepath.Join(gcDir, e.Name()))
				}
			}
		}
	}
	return roots
}

// emitDoctorAlert records one doctor.alert event. Best-effort: a nil
// provider or marshal failure is a no-op.
func emitDoctorAlert(ep events.Provider, cityName string, alert supervisordoctor.Alert) {
	if ep == nil {
		return
	}
	payload, err := json.Marshal(events.DoctorAlertPayload{
		Check:   alert.Check,
		City:    cityName,
		Detail:  alert.Detail,
		Subject: alert.Subject,
	})
	if err != nil {
		return
	}
	ep.Record(events.Event{
		Type:    events.DoctorAlert,
		Actor:   eventActor(),
		Subject: alert.Subject,
		Payload: payload,
	})
}

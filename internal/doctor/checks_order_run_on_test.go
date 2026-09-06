package doctor

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/events"
	"github.com/gastownhall/gascity/internal/orders"
)

// writeRunOnTestOrder writes a cooldown exec order carrying run_on.
func writeRunOnTestOrder(t *testing.T, cityPath, name, runOn string) {
	t.Helper()
	body := "[order]\nexec = \"true\"\ntrigger = \"cooldown\"\ninterval = \"5m\"\n"
	if runOn != "" {
		body += "run_on = \"" + runOn + "\"\n"
	}
	path := filepath.Join(cityPath, "orders", name+".toml")
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatalf("write order %s: %v", name, err)
	}
}

func TestOrderRunOnRole_SeatWithFleetHostOrdersWarns(t *testing.T) {
	cityPath, cfg := orderFiringTestCity(t)
	writeRunOnTestOrder(t, cityPath, "merge-sweep", orders.RunOnFleetHost)
	writeRunOnTestOrder(t, cityPath, "review-gate", orders.RunOnFleetHost)
	writeRunOnTestOrder(t, cityPath, "local-lint", "")

	check := NewOrderRunOnRoleCheck(cfg, cityPath, WithOrderRunOnRoleEnv(""))
	result := check.Run(&CheckContext{CityPath: cityPath})

	if result.Status != StatusWarning {
		t.Fatalf("status = %v, want Warning; msg = %s", result.Status, result.Message)
	}
	if result.Severity != SeverityAdvisory {
		t.Fatalf("severity = %v, want advisory — inert orders must not gate automation", result.Severity)
	}
	for _, want := range []string{"merge-sweep", "review-gate"} {
		if !strings.Contains(result.Message, want) {
			t.Errorf("message = %q, want it to name %q", result.Message, want)
		}
	}
	if strings.Contains(result.Message, "local-lint") {
		t.Errorf("message = %q, should not name an order with no run_on", result.Message)
	}
	if len(result.Details) != 2 {
		t.Errorf("details = %v, want both fleet-host orders", result.Details)
	}
	if !strings.Contains(result.FixHint, config.FleetRoleEnvVar) {
		t.Errorf("fix hint = %q, want it to name %s for an undeclared role", result.FixHint, config.FleetRoleEnvVar)
	}
}

func TestOrderRunOnRole_FleetHostIsOK(t *testing.T) {
	cityPath, cfg := orderFiringTestCity(t)
	cfg.CityRole = config.CityRoleConfig{Role: orders.RoleFleetHost}
	writeRunOnTestOrder(t, cityPath, "merge-sweep", orders.RunOnFleetHost)

	result := NewOrderRunOnRoleCheck(cfg, cityPath, WithOrderRunOnRoleEnv("")).Run(&CheckContext{CityPath: cityPath})
	if result.Status != StatusOK {
		t.Fatalf("status = %v, want OK on the fleet host; msg = %s", result.Status, result.Message)
	}
}

// The env var alone must be enough to clear the warning: a fleet host that
// declares its role through the supervisor environment is correctly configured.
func TestOrderRunOnRole_EnvRoleClearsWarning(t *testing.T) {
	cityPath, cfg := orderFiringTestCity(t)
	writeRunOnTestOrder(t, cityPath, "merge-sweep", orders.RunOnFleetHost)

	result := NewOrderRunOnRoleCheck(cfg, cityPath, WithOrderRunOnRoleEnv(orders.RoleFleetHost)).Run(&CheckContext{CityPath: cityPath})
	if result.Status != StatusOK {
		t.Fatalf("status = %v, want OK with %s=fleet-host; msg = %s", result.Status, config.FleetRoleEnvVar, result.Message)
	}
}

func TestOrderRunOnRole_SeatWithNoFleetHostOrdersIsOK(t *testing.T) {
	cityPath, cfg := orderFiringTestCity(t)
	writeRunOnTestOrder(t, cityPath, "local-lint", "")
	writeRunOnTestOrder(t, cityPath, "seat-probe", orders.RunOnSeat)

	result := NewOrderRunOnRoleCheck(cfg, cityPath, WithOrderRunOnRoleEnv("")).Run(&CheckContext{CityPath: cityPath})
	if result.Status != StatusOK {
		t.Fatalf("status = %v, want OK; msg = %s", result.Status, result.Message)
	}
}

// A fleet-host order the operator has already skipped is not a finding: the
// city has made the durable choice this check exists to prompt.
func TestOrderRunOnRole_SkippedOrdersAreNotReported(t *testing.T) {
	cityPath, cfg := orderFiringTestCity(t)
	cfg.Orders.Skip = []string{"merge-sweep"}
	writeRunOnTestOrder(t, cityPath, "merge-sweep", orders.RunOnFleetHost)

	result := NewOrderRunOnRoleCheck(cfg, cityPath, WithOrderRunOnRoleEnv("")).Run(&CheckContext{CityPath: cityPath})
	if result.Status != StatusOK {
		t.Fatalf("status = %v, want OK for a skipped order; msg = %s", result.Status, result.Message)
	}
}

// A disabled fleet-host order is likewise not a finding.
func TestOrderRunOnRole_DisabledOrdersAreNotReported(t *testing.T) {
	cityPath, cfg := orderFiringTestCity(t)
	body := "[order]\nexec = \"true\"\ntrigger = \"cooldown\"\ninterval = \"5m\"\nrun_on = \"fleet-host\"\nenabled = false\n"
	if err := os.WriteFile(filepath.Join(cityPath, "orders", "merge-sweep.toml"), []byte(body), 0o644); err != nil {
		t.Fatalf("write order: %v", err)
	}

	result := NewOrderRunOnRoleCheck(cfg, cityPath, WithOrderRunOnRoleEnv("")).Run(&CheckContext{CityPath: cityPath})
	if result.Status != StatusOK {
		t.Fatalf("status = %v, want OK for a disabled order; msg = %s", result.Status, result.Message)
	}
}

func TestOrderRunOnRole_DeclaredSeatFixHintNamesTheDeclaration(t *testing.T) {
	cityPath, cfg := orderFiringTestCity(t)
	cfg.CityRole = config.CityRoleConfig{Role: orders.RoleSeat}
	writeRunOnTestOrder(t, cityPath, "merge-sweep", orders.RunOnFleetHost)

	result := NewOrderRunOnRoleCheck(cfg, cityPath, WithOrderRunOnRoleEnv("")).Run(&CheckContext{CityPath: cityPath})
	if result.Status != StatusWarning {
		t.Fatalf("status = %v, want Warning; msg = %s", result.Status, result.Message)
	}
	if !strings.Contains(result.FixHint, "[orders].skip") {
		t.Errorf("fix hint = %q, want the skip-list remedy", result.FixHint)
	}
	if strings.Contains(result.FixHint, config.FleetRoleEnvVar) {
		t.Errorf("fix hint = %q, should not mention the env var when the role is declared", result.FixHint)
	}
}

func TestOrderRunOnRole_NoConfigIsOK(t *testing.T) {
	result := NewOrderRunOnRoleCheck(nil, t.TempDir()).Run(&CheckContext{})
	if result.Status != StatusOK {
		t.Fatalf("status = %v, want OK with no config", result.Status)
	}
}

func TestSummarizeOrderNames(t *testing.T) {
	names := []string{"a", "b", "c", "d", "e", "f", "g"}
	got := summarizeOrderNames(names, 5)
	if got != "a, b, c, d, e, and 2 more" {
		t.Fatalf("summarizeOrderNames = %q", got)
	}
	if got := summarizeOrderNames([]string{"a", "b"}, 5); got != "a, b" {
		t.Fatalf("summarizeOrderNames under limit = %q", got)
	}
}

// order-firing-current must not report a fleet-host order on a DECLARED seat as
// stale: the dispatcher skips it on purpose, so it has no firing history by
// design.
func TestOrderFiringCurrent_SkipsOrdersExcludedByRunOn(t *testing.T) {
	t.Setenv(config.FleetRoleEnvVar, "")
	now := time.Date(2026, 5, 17, 12, 0, 0, 0, time.UTC)
	cityPath, cfg := orderFiringTestCity(t)
	cfg.CityRole = config.CityRoleConfig{Role: orders.RoleSeat}
	writeRunOnTestOrder(t, cityPath, "merge-sweep", orders.RunOnFleetHost)
	writeOrderFiringTestEvents(t, cityPath,
		events.Event{Type: events.ControllerStarted, Ts: now.Add(-24 * time.Hour)},
	)

	result := runOrderFiringCurrentTest(t, cfg, cityPath, now)
	if result.Status != StatusOK {
		t.Fatalf("status = %v, want OK; msg = %s; details = %v", result.Status, result.Message, result.Details)
	}
	if strings.Contains(strings.Join(append(result.Details, result.Message), "\n"), "merge-sweep") {
		t.Fatalf("result mentions merge-sweep (%s / %v); a run_on-excluded order is not stale", result.Message, result.Details)
	}
}

// The same order on the fleet host IS monitored: the run_on filter must not
// become a blanket exemption.
func TestOrderFiringCurrent_MonitorsFleetHostOrdersOnTheHost(t *testing.T) {
	t.Setenv(config.FleetRoleEnvVar, orders.RoleFleetHost)
	now := time.Date(2026, 5, 17, 12, 0, 0, 0, time.UTC)
	cityPath, cfg := orderFiringTestCity(t)
	writeRunOnTestOrder(t, cityPath, "merge-sweep", orders.RunOnFleetHost)
	writeOrderFiringTestEvents(t, cityPath,
		events.Event{Type: events.ControllerStarted, Ts: now.Add(-24 * time.Hour)},
		events.Event{Type: events.OrderFired, Subject: "merge-sweep", Ts: now.Add(-24 * time.Hour)},
	)

	result := runOrderFiringCurrentTest(t, cfg, cityPath, now)
	if !strings.Contains(strings.Join(append(result.Details, result.Message), "\n"), "merge-sweep") {
		t.Fatalf("result = %s / %v, want merge-sweep monitored on the fleet host", result.Message, result.Details)
	}
}

// The exclusion is gated on the role being DECLARED. A fleet host that loses
// its role declaration defaults to seat and stops dispatching its fleet-host
// orders; if the defaulted role also excluded them from the staleness check,
// the watchdog would be switched off by the very fault it exists to report.
func TestOrderFiringCurrent_UndeclaredRoleStillMonitorsFleetHostOrders(t *testing.T) {
	t.Setenv(config.FleetRoleEnvVar, "")
	now := time.Date(2026, 5, 17, 12, 0, 0, 0, time.UTC)
	cityPath, cfg := orderFiringTestCity(t)
	writeRunOnTestOrder(t, cityPath, "merge-sweep", orders.RunOnFleetHost)
	writeOrderFiringTestEvents(t, cityPath,
		events.Event{Type: events.ControllerStarted, Ts: now.Add(-24 * time.Hour)},
		events.Event{Type: events.OrderFired, Subject: "merge-sweep", Ts: now.Add(-24 * time.Hour)},
	)

	result := runOrderFiringCurrentTest(t, cfg, cityPath, now)
	if !strings.Contains(strings.Join(append(result.Details, result.Message), "\n"), "merge-sweep") {
		t.Fatalf("result = %s / %v; an undeclared role must not exempt fleet-host orders from staleness", result.Message, result.Details)
	}
}

// A role declared through the environment is a declaration too, so it does
// quiet the check — the fleet already exports VOXIST_FLEET_ROLE.
func TestOrderFiringCurrent_EnvDeclaredSeatExcludesFleetHostOrders(t *testing.T) {
	t.Setenv(config.FleetRoleEnvVar, orders.RoleSeat)
	now := time.Date(2026, 5, 17, 12, 0, 0, 0, time.UTC)
	cityPath, cfg := orderFiringTestCity(t)
	writeRunOnTestOrder(t, cityPath, "merge-sweep", orders.RunOnFleetHost)
	writeOrderFiringTestEvents(t, cityPath,
		events.Event{Type: events.ControllerStarted, Ts: now.Add(-24 * time.Hour)},
	)

	result := runOrderFiringCurrentTest(t, cfg, cityPath, now)
	if strings.Contains(strings.Join(append(result.Details, result.Message), "\n"), "merge-sweep") {
		t.Fatalf("result = %s / %v; a declared seat should exempt the order", result.Message, result.Details)
	}
}

// An unrecognized environment value states nothing, so it must not count as a
// declaration — a typo must not disarm the staleness check.
func TestOrderFiringCurrent_UnknownEnvRoleDoesNotCountAsDeclared(t *testing.T) {
	t.Setenv(config.FleetRoleEnvVar, "fleet")
	now := time.Date(2026, 5, 17, 12, 0, 0, 0, time.UTC)
	cityPath, cfg := orderFiringTestCity(t)
	writeRunOnTestOrder(t, cityPath, "merge-sweep", orders.RunOnFleetHost)
	writeOrderFiringTestEvents(t, cityPath,
		events.Event{Type: events.ControllerStarted, Ts: now.Add(-24 * time.Hour)},
		events.Event{Type: events.OrderFired, Subject: "merge-sweep", Ts: now.Add(-24 * time.Hour)},
	)

	result := runOrderFiringCurrentTest(t, cfg, cityPath, now)
	if !strings.Contains(strings.Join(append(result.Details, result.Message), "\n"), "merge-sweep") {
		t.Fatalf("result = %s / %v; an unknown env value must not exempt the order", result.Message, result.Details)
	}
}

// --- the second signal: a city that is evidently the fleet city ---

// A city that declares rigs is running fleet automation. If its role is not
// fleet-host, its fleet-singleton orders dispatch NOWHERE, which is an outage
// and must gate — not read as the benign seat case.
func TestOrderRunOnRole_FleetCityByRigsIsBlockingError(t *testing.T) {
	cityPath, cfg := orderFiringTestCity(t)
	cfg.Rigs = []config.Rig{{Name: "alpha", Path: filepath.Join(cityPath, "rigs", "alpha")}}
	writeRunOnTestOrder(t, cityPath, "merge-sweep", orders.RunOnFleetHost)

	result := NewOrderRunOnRoleCheck(cfg, cityPath, WithOrderRunOnRoleEnv("")).Run(&CheckContext{CityPath: cityPath})
	if result.Status != StatusError {
		t.Fatalf("status = %v, want Error for a role-less fleet city; msg = %s", result.Status, result.Message)
	}
	if result.Severity != SeverityBlocking {
		t.Fatalf("severity = %v, want blocking", result.Severity)
	}
	if !strings.Contains(result.Message, "merge-sweep") {
		t.Errorf("message = %q, want it to name the skipped order", result.Message)
	}
}

// The default-rig import pin is the other half of the same signal, and matches
// the rule the pack half applies.
func TestOrderRunOnRole_FleetCityByDefaultRigImportsIsBlockingError(t *testing.T) {
	cityPath, cfg := orderFiringTestCity(t)
	cfg.Defaults = config.PackDefaults{Rig: config.PackRigDefaults{
		Imports: map[string]config.Import{"core": {}},
	}}
	writeRunOnTestOrder(t, cityPath, "merge-sweep", orders.RunOnFleetHost)

	result := NewOrderRunOnRoleCheck(cfg, cityPath, WithOrderRunOnRoleEnv("")).Run(&CheckContext{CityPath: cityPath})
	if result.Status != StatusError {
		t.Fatalf("status = %v, want Error; msg = %s", result.Status, result.Message)
	}
}

// Declaring role = "seat" does NOT clear the error on a fleet city: the orders
// still dispatch nowhere. The remedy is the skip list, and the hint says so.
func TestOrderRunOnRole_FleetCityDeclaringSeatStillErrors(t *testing.T) {
	cityPath, cfg := orderFiringTestCity(t)
	cfg.Rigs = []config.Rig{{Name: "alpha", Path: filepath.Join(cityPath, "rigs", "alpha")}}
	cfg.CityRole = config.CityRoleConfig{Role: orders.RoleSeat}
	writeRunOnTestOrder(t, cityPath, "merge-sweep", orders.RunOnFleetHost)

	result := NewOrderRunOnRoleCheck(cfg, cityPath, WithOrderRunOnRoleEnv("")).Run(&CheckContext{CityPath: cityPath})
	if result.Status != StatusError {
		t.Fatalf("status = %v, want Error; msg = %s", result.Status, result.Message)
	}
	if !strings.Contains(result.FixHint, "[orders].skip") {
		t.Errorf("fix hint = %q, want the skip-list remedy", result.FixHint)
	}
}

// A fleet city that declares fleet-host is healthy.
func TestOrderRunOnRole_FleetCityDeclaringFleetHostIsOK(t *testing.T) {
	cityPath, cfg := orderFiringTestCity(t)
	cfg.Rigs = []config.Rig{{Name: "alpha", Path: filepath.Join(cityPath, "rigs", "alpha")}}
	cfg.CityRole = config.CityRoleConfig{Role: orders.RoleFleetHost}
	writeRunOnTestOrder(t, cityPath, "merge-sweep", orders.RunOnFleetHost)

	result := NewOrderRunOnRoleCheck(cfg, cityPath, WithOrderRunOnRoleEnv("")).Run(&CheckContext{CityPath: cityPath})
	if result.Status != StatusOK {
		t.Fatalf("status = %v, want OK; msg = %s", result.Status, result.Message)
	}
}

// A fleet city with no fleet-host orders has nothing skipped, so nothing to say.
func TestOrderRunOnRole_FleetCityWithNoFleetHostOrdersIsOK(t *testing.T) {
	cityPath, cfg := orderFiringTestCity(t)
	cfg.Rigs = []config.Rig{{Name: "alpha", Path: filepath.Join(cityPath, "rigs", "alpha")}}
	writeRunOnTestOrder(t, cityPath, "local-lint", "")

	result := NewOrderRunOnRoleCheck(cfg, cityPath, WithOrderRunOnRoleEnv("")).Run(&CheckContext{CityPath: cityPath})
	if result.Status != StatusOK {
		t.Fatalf("status = %v, want OK; msg = %s", result.Status, result.Message)
	}
}

// The seat case must stay ADVISORY and must NOT be worded like the outage: the
// two were byte-identical before the second signal existed, which is what made
// the check useless as a detector.
func TestOrderRunOnRole_SeatAndFleetCityMessagesDiffer(t *testing.T) {
	seatPath, seatCfg := orderFiringTestCity(t)
	writeRunOnTestOrder(t, seatPath, "merge-sweep", orders.RunOnFleetHost)
	seat := NewOrderRunOnRoleCheck(seatCfg, seatPath, WithOrderRunOnRoleEnv("")).Run(&CheckContext{CityPath: seatPath})

	fleetPath, fleetCfg := orderFiringTestCity(t)
	fleetCfg.Rigs = []config.Rig{{Name: "alpha", Path: filepath.Join(fleetPath, "rigs", "alpha")}}
	writeRunOnTestOrder(t, fleetPath, "merge-sweep", orders.RunOnFleetHost)
	fleet := NewOrderRunOnRoleCheck(fleetCfg, fleetPath, WithOrderRunOnRoleEnv("")).Run(&CheckContext{CityPath: fleetPath})

	if seat.Status != StatusWarning || seat.Severity != SeverityAdvisory {
		t.Fatalf("seat = %v/%v, want Warning/Advisory", seat.Status, seat.Severity)
	}
	if fleet.Status != StatusError || fleet.Severity != SeverityBlocking {
		t.Fatalf("fleet city = %v/%v, want Error/Blocking", fleet.Status, fleet.Severity)
	}
	if seat.Message == fleet.Message {
		t.Fatalf("seat and fleet-city messages are identical; the check cannot distinguish them:\n%s", seat.Message)
	}
}

// An ignored VOXIST_FLEET_ROLE value is surfaced by the check rather than
// silently dropping the city to seat.
func TestOrderRunOnRole_UnknownEnvValueIsReported(t *testing.T) {
	cityPath, cfg := orderFiringTestCity(t)
	writeRunOnTestOrder(t, cityPath, "merge-sweep", orders.RunOnFleetHost)

	result := NewOrderRunOnRoleCheck(cfg, cityPath, WithOrderRunOnRoleEnv("fleet")).Run(&CheckContext{CityPath: cityPath})
	joined := strings.Join(result.Details, "\n")
	if !strings.Contains(joined, config.FleetRoleEnvVar) || !strings.Contains(joined, "not a known city role") {
		t.Fatalf("details = %v, want the ignored-env-value warning", result.Details)
	}
}

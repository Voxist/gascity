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

// order-firing-current must not report a fleet-host order on a seat as stale:
// the dispatcher skips it on purpose, so it has no firing history by design.
func TestOrderFiringCurrent_SkipsOrdersExcludedByRunOn(t *testing.T) {
	t.Setenv(config.FleetRoleEnvVar, "")
	now := time.Date(2026, 5, 17, 12, 0, 0, 0, time.UTC)
	cityPath, cfg := orderFiringTestCity(t)
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

package orders

import (
	"strings"
	"testing"
)

func TestParseRunOn(t *testing.T) {
	a, err := Parse([]byte(`
[order]
description = "fleet-wide merge sweep"
trigger = "cooldown"
interval = "5m"
exec = "scripts/sweep.sh"
run_on = "fleet-host"
`))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if a.RunOn != RunOnFleetHost {
		t.Fatalf("RunOn = %q, want %q", a.RunOn, RunOnFleetHost)
	}
}

func TestParseRunOnUnsetDefaultsToAny(t *testing.T) {
	a, err := Parse([]byte(`
[order]
trigger = "cooldown"
interval = "5m"
exec = "scripts/sweep.sh"
`))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if a.RunOn != "" {
		t.Fatalf("RunOn = %q, want empty", a.RunOn)
	}
	if got := a.RunOnOrDefault(); got != RunOnAny {
		t.Fatalf("RunOnOrDefault() = %q, want %q", got, RunOnAny)
	}
}

// run_on is a NEW field: it must not collide with the pre-existing scope field,
// which decides pack expansion (city vs rig) and means something else entirely.
func TestRunOnIsIndependentOfScope(t *testing.T) {
	a, err := Parse([]byte(`
[order]
trigger = "cooldown"
interval = "5m"
exec = "scripts/sweep.sh"
scope = "city"
run_on = "fleet-host"
`))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if !a.IsCityScoped() {
		t.Error("scope = city not preserved")
	}
	if a.RunOn != RunOnFleetHost {
		t.Errorf("RunOn = %q, want %q", a.RunOn, RunOnFleetHost)
	}
}

func TestValidateRunOn(t *testing.T) {
	for _, tc := range []struct {
		runOn   string
		wantErr bool
	}{
		{"", false},
		{RunOnAny, false},
		{RunOnFleetHost, false},
		{RunOnSeat, false},
		{"city", true},
		{"fleet", true},
		{"Fleet-Host", true},
	} {
		a := Order{Name: "sweep", Exec: "scripts/sweep.sh", Trigger: "cooldown", Interval: "5m", RunOn: tc.runOn}
		err := Validate(a)
		if tc.wantErr {
			if err == nil {
				t.Errorf("Validate(run_on=%q) = nil, want error", tc.runOn)
				continue
			}
			if !strings.Contains(err.Error(), "unknown run_on") {
				t.Errorf("Validate(run_on=%q) error = %v, want it to name run_on", tc.runOn, err)
			}
			continue
		}
		if err != nil {
			t.Errorf("Validate(run_on=%q) = %v, want nil", tc.runOn, err)
		}
	}
}

func TestRunsOnRoleMatrix(t *testing.T) {
	for _, tc := range []struct {
		runOn string
		role  string
		want  bool
	}{
		// Undeclared run_on runs everywhere — the pre-existing behavior.
		{"", RoleFleetHost, true},
		{"", RoleSeat, true},
		{"", "", true},
		{RunOnAny, RoleFleetHost, true},
		{RunOnAny, RoleSeat, true},
		{RunOnAny, "", true},
		// fleet-host orders run only on the fleet host.
		{RunOnFleetHost, RoleFleetHost, true},
		{RunOnFleetHost, RoleSeat, false},
		{RunOnFleetHost, "", false},
		// seat orders run everywhere except the fleet host.
		{RunOnSeat, RoleFleetHost, false},
		{RunOnSeat, RoleSeat, true},
		{RunOnSeat, "", true},
	} {
		if got := RunsOnRole(tc.runOn, tc.role); got != tc.want {
			t.Errorf("RunsOnRole(%q, %q) = %v, want %v", tc.runOn, tc.role, got, tc.want)
		}
		a := Order{RunOn: tc.runOn}
		if got := a.RunsOn(tc.role); got != tc.want {
			t.Errorf("Order{RunOn:%q}.RunsOn(%q) = %v, want %v", tc.runOn, tc.role, got, tc.want)
		}
	}
}

// An unvalidated order carrying a value gc does not recognize must fail OPEN.
// Validation rejects such a value at load, so the only way to reach this is an
// order that was never validated; stopping its dispatch silently would be a
// worse outcome than running it as it ran before run_on existed.
func TestRunsOnRoleUnknownValueFailsOpen(t *testing.T) {
	if !RunsOnRole("nonsense", RoleSeat) {
		t.Error("unknown run_on should not suppress dispatch")
	}
}

func TestNormalizeRole(t *testing.T) {
	for in, want := range map[string]string{
		RoleFleetHost: RoleFleetHost,
		RoleSeat:      RoleSeat,
		"":            RoleSeat,
		"anything":    RoleSeat,
	} {
		if got := NormalizeRole(in); got != want {
			t.Errorf("NormalizeRole(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestIsValidRole(t *testing.T) {
	for in, want := range map[string]bool{
		"":            true,
		RoleFleetHost: true,
		RoleSeat:      true,
		"host":        false,
		"FLEET-HOST":  false,
	} {
		if got := IsValidRole(in); got != want {
			t.Errorf("IsValidRole(%q) = %v, want %v", in, got, want)
		}
	}
}

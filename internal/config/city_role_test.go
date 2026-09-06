package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/fsys"
	"github.com/gastownhall/gascity/internal/orders"
)

func TestParseCityRole(t *testing.T) {
	cfg, err := Parse([]byte(`
[workspace]
name = "hq"

[city]
role = "fleet-host"
`))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if cfg.CityRole.Role != orders.RoleFleetHost {
		t.Fatalf("[city] role = %q, want %q", cfg.CityRole.Role, orders.RoleFleetHost)
	}
	if !FleetRoleIsDeclared(cfg) {
		t.Error("FleetRoleIsDeclared = false for a declared role")
	}
}

func TestLoadRejectsUnknownCityRole(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "city.toml")
	if err := os.WriteFile(path, []byte("[workspace]\nname = \"hq\"\n\n[city]\nrole = \"fleet\"\n"), 0o644); err != nil {
		t.Fatalf("write city.toml: %v", err)
	}
	_, err := Load(fsys.OSFS{}, path)
	if err == nil {
		t.Fatal("Load = nil error, want a rejection of role = \"fleet\"")
	}
	if !strings.Contains(err.Error(), "[city] role") {
		t.Fatalf("error = %v, want it to name [city] role", err)
	}
}

func TestValidateCityRole(t *testing.T) {
	for _, tc := range []struct {
		role    string
		wantErr bool
	}{
		{"", false},
		{orders.RoleFleetHost, false},
		{orders.RoleSeat, false},
		{"fleet", true},
		{"Seat", true},
	} {
		cfg := &City{CityRole: CityRoleConfig{Role: tc.role}}
		err := ValidateCityRole(cfg, "city.toml")
		if tc.wantErr != (err != nil) {
			t.Errorf("ValidateCityRole(role=%q) = %v, wantErr %v", tc.role, err, tc.wantErr)
		}
	}
	if err := ValidateCityRole(nil, "city.toml"); err != nil {
		t.Errorf("ValidateCityRole(nil) = %v, want nil", err)
	}
}

func TestEffectiveFleetRole(t *testing.T) {
	for _, tc := range []struct {
		name     string
		declared string
		env      string
		want     string
	}{
		{"undeclared defaults to seat", "", "", orders.RoleSeat},
		{"declared fleet-host wins", orders.RoleFleetHost, "", orders.RoleFleetHost},
		{"declared seat wins", orders.RoleSeat, "", orders.RoleSeat},
		// The env var is the host-local declaration the fleet already exports:
		// it must work with no city.toml change at all.
		{"env supplies the role when undeclared", "", orders.RoleFleetHost, orders.RoleFleetHost},
		{"env seat when undeclared", "", orders.RoleSeat, orders.RoleSeat},
		{"env is whitespace tolerant", "", "  fleet-host  ", orders.RoleFleetHost},
		// A declared role is authored config; ambient environment must not
		// override it in either direction.
		{"declared beats env", orders.RoleSeat, orders.RoleFleetHost, orders.RoleSeat},
		{"declared host beats env seat", orders.RoleFleetHost, orders.RoleSeat, orders.RoleFleetHost},
		// A typo in a shell profile must not promote a seat or demote a host.
		{"unknown env is ignored", "", "fleet", orders.RoleSeat},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := &City{CityRole: CityRoleConfig{Role: tc.declared}}
			if got := EffectiveFleetRole(cfg, tc.env); got != tc.want {
				t.Fatalf("EffectiveFleetRole(%q, %q) = %q, want %q", tc.declared, tc.env, got, tc.want)
			}
		})
	}
	if got := EffectiveFleetRole(nil, orders.RoleFleetHost); got != orders.RoleFleetHost {
		t.Errorf("EffectiveFleetRole(nil, fleet-host) = %q, want %q", got, orders.RoleFleetHost)
	}
	if got := EffectiveFleetRole(nil, ""); got != orders.RoleSeat {
		t.Errorf("EffectiveFleetRole(nil, \"\") = %q, want %q", got, orders.RoleSeat)
	}
}

func TestEffectiveFleetRoleFromEnv(t *testing.T) {
	t.Setenv(FleetRoleEnvVar, orders.RoleFleetHost)
	if got := EffectiveFleetRoleFromEnv(&City{}); got != orders.RoleFleetHost {
		t.Fatalf("EffectiveFleetRoleFromEnv = %q, want %q", got, orders.RoleFleetHost)
	}
	t.Setenv(FleetRoleEnvVar, "")
	if got := EffectiveFleetRoleFromEnv(&City{}); got != orders.RoleSeat {
		t.Fatalf("EffectiveFleetRoleFromEnv with empty env = %q, want %q", got, orders.RoleSeat)
	}
}

// A city with no [city] table at all must load and resolve to a seat: this
// field is additive and every existing city.toml predates it.
func TestCityRoleAbsentTableLoads(t *testing.T) {
	cfg, err := Parse([]byte("[workspace]\nname = \"hq\"\n"))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if FleetRoleIsDeclared(cfg) {
		t.Error("FleetRoleIsDeclared = true with no [city] table")
	}
	if got := EffectiveFleetRole(cfg, ""); got != orders.RoleSeat {
		t.Fatalf("EffectiveFleetRole = %q, want %q", got, orders.RoleSeat)
	}
}

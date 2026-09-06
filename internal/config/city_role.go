package config

import (
	"fmt"
	"os"
	"strings"

	"github.com/gastownhall/gascity/internal/orders"
)

// FleetRoleEnvVar is the host-local declaration of the city's fleet role. It is
// read only when `[city] role` is unset, so a fleet that already exports the
// variable from its supervisor environment gets the role without editing any
// city.toml, while a city that declares the key is not silently overridden by a
// stray variable in an operator's shell.
const FleetRoleEnvVar = "VOXIST_FLEET_ROLE"

// CityRoleConfig is the `[city]` table: this city's place in a multi-city
// fleet. It is deliberately separate from `[workspace]`, which describes the
// city's own identity and defaults; the role describes its relationship to the
// other cities that install the same packs.
type CityRoleConfig struct {
	// Role is "fleet-host" (the single city that owns fleet-wide automation)
	// or "seat" (everything else). Empty means undeclared and resolves to
	// "seat" unless VOXIST_FLEET_ROLE says otherwise. Orders select against
	// it with their run_on field.
	Role string `toml:"role,omitempty" jsonschema:"enum=fleet-host,enum=seat"`
}

// EffectiveFleetRole resolves the city's role: the declared `[city] role` when
// set, else envValue (the VOXIST_FLEET_ROLE reading) when it names a valid
// role, else orders.RoleSeat. An unrecognized envValue is ignored rather than
// fatal: the variable is ambient host state, not authored config, and a typo in
// a shell profile must not promote a seat to the fleet host or stop its orders.
func EffectiveFleetRole(cfg *City, envValue string) string {
	if cfg != nil {
		if declared := strings.TrimSpace(cfg.CityRole.Role); declared != "" {
			return orders.NormalizeRole(declared)
		}
	}
	env := strings.TrimSpace(envValue)
	if env != "" && orders.IsValidRole(env) {
		return orders.NormalizeRole(env)
	}
	return orders.RoleSeat
}

// EffectiveFleetRoleFromEnv is EffectiveFleetRole reading the process
// environment. Callers that need a deterministic role in tests use
// EffectiveFleetRole directly.
func EffectiveFleetRoleFromEnv(cfg *City) string {
	return EffectiveFleetRole(cfg, os.Getenv(FleetRoleEnvVar))
}

// FleetRoleIsDeclared reports whether the role came from `[city] role` rather
// than from the environment or the default. Diagnostics use it to tell an
// operator where the role they are seeing came from.
func FleetRoleIsDeclared(cfg *City) bool {
	return cfg != nil && strings.TrimSpace(cfg.CityRole.Role) != ""
}

// ValidateCityRole rejects an unknown `[city] role`. Unlike the environment
// variable, an authored role that gc does not understand is a hard error: the
// operator meant to select a role, and silently falling back to "seat" would
// leave fleet-host orders unrun with nothing to show for it.
func ValidateCityRole(cfg *City, source string) error {
	if cfg == nil {
		return nil
	}
	role := strings.TrimSpace(cfg.CityRole.Role)
	if orders.IsValidRole(role) {
		return nil
	}
	return fmt.Errorf("%s: [city] role must be %s, or empty, got %q",
		source, strings.Join(quoteAll(orders.ValidRoles), " or "), cfg.CityRole.Role)
}

func quoteAll(values []string) []string {
	out := make([]string, 0, len(values))
	for _, v := range values {
		out = append(out, `"`+v+`"`)
	}
	return out
}

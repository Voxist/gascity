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
// Ignoring it is never silent — see EffectiveFleetRoleWithWarning, which every
// caller that can write a log line uses instead.
func EffectiveFleetRole(cfg *City, envValue string) string {
	role, _ := EffectiveFleetRoleWithWarning(cfg, envValue)
	return role
}

// EffectiveFleetRoleWithWarning is EffectiveFleetRole plus the one-line warning
// an ignored VOXIST_FLEET_ROLE value must produce. The warning is empty in
// every other case.
//
// A dropped fleet-host role is the expensive failure this whole field guards
// against: the fleet host silently becomes a seat, stops running every
// fleet-singleton order, and nothing says why. So a value that is present but
// unrecognized is reported, loudly, at each place that resolves the role.
func EffectiveFleetRoleWithWarning(cfg *City, envValue string) (role, warning string) {
	declared := ""
	if cfg != nil {
		declared = strings.TrimSpace(cfg.CityRole.Role)
	}
	env := strings.TrimSpace(envValue)
	if env != "" && !orders.IsValidRole(env) {
		warning = fmt.Sprintf("%s=%q is not a known city role (want %s); ignoring it",
			FleetRoleEnvVar, envValue, strings.Join(quoteAll(orders.ValidRoles), " or "))
		if declared == "" {
			warning += fmt.Sprintf(" and treating this city as %q", orders.RoleSeat)
		}
	}
	if declared != "" {
		return orders.NormalizeRole(declared), warning
	}
	if env != "" && orders.IsValidRole(env) {
		return orders.NormalizeRole(env), warning
	}
	return orders.RoleSeat, warning
}

// EffectiveFleetRoleFromEnv is EffectiveFleetRole reading the process
// environment. Callers that need a deterministic role in tests use
// EffectiveFleetRole directly.
func EffectiveFleetRoleFromEnv(cfg *City) string {
	return EffectiveFleetRole(cfg, os.Getenv(FleetRoleEnvVar))
}

// EffectiveFleetRoleFromEnvWithWarning is EffectiveFleetRoleWithWarning reading
// the process environment.
func EffectiveFleetRoleFromEnvWithWarning(cfg *City) (role, warning string) {
	return EffectiveFleetRoleWithWarning(cfg, os.Getenv(FleetRoleEnvVar))
}

// FleetRoleIsDeclared reports whether the role came from `[city] role` rather
// than from the environment or the default. Diagnostics use it to tell an
// operator where the role they are seeing came from.
func FleetRoleIsDeclared(cfg *City) bool {
	return cfg != nil && strings.TrimSpace(cfg.CityRole.Role) != ""
}

// FleetRoleDeclared reports whether the city states its role at all — through
// `[city] role` or through a VOXIST_FLEET_ROLE value that names a real role.
//
// This is the difference between "this city says it is a seat" and "nobody said
// anything, so we assumed seat", and consumers that DISARM a check on the
// strength of the role must gate on it. A fleet host whose role declaration is
// lost resolves to seat by default; treating that default as a statement would
// let the fault silently switch off the very checks that would report it.
//
// An unrecognized environment value does NOT count as declared: its value is
// ignored (with a warning), so it states nothing.
func FleetRoleDeclared(cfg *City, envValue string) bool {
	if FleetRoleIsDeclared(cfg) {
		return true
	}
	env := strings.TrimSpace(envValue)
	return env != "" && orders.IsValidRole(env)
}

// FleetRoleDeclaredFromEnv is FleetRoleDeclared reading the process environment.
func FleetRoleDeclaredFromEnv(cfg *City) bool {
	return FleetRoleDeclared(cfg, os.Getenv(FleetRoleEnvVar))
}

// LooksLikeFleetCity reports the second, independent signal that a city is the
// fleet city: it declares rigs, or it pins default-rig imports. Both are
// properties of the composed config, never of a filesystem path, so the signal
// is the same on every host and matches the rule the pack half applies
// (packs.lock default-rig pin + declared rigs).
//
// It exists because the role alone cannot detect its own absence. A fleet host
// that loses its role declaration is indistinguishable, by role, from an
// ordinary seat — but not by this: a city that fans work out to rigs or pins
// what every rig imports is running fleet automation whatever its role says.
//
// The signal is deliberately broad — nearly every working city declares rigs —
// so it is NEVER sufficient on its own. Its only consumer pairs it with
// "the city states no role" (see FleetRoleDeclared), because a seat that
// declares rigs and installs a shared pack is the normal case run_on is built
// to serve, not a fault. Read together the pair means "fleet-shaped, and nobody
// ever said what this city is".
//
// It reads the AUTHORED [defaults.rig.imports] table, not the runtime-only
// DefaultRigImports that compose populates from the root pack, so the rigs
// clause is what matches in practice on template-generated cities.
func LooksLikeFleetCity(cfg *City) bool {
	if cfg == nil {
		return false
	}
	return len(cfg.Rigs) > 0 || len(cfg.Defaults.Rig.Imports) > 0
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

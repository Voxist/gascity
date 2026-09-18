package orders

// Fleet roles and the order-level run_on selector.
//
// A city declares its role in the fleet with `[city] role` (or the
// VOXIST_FLEET_ROLE environment variable — see config.EffectiveFleetRole). An
// order declares which role it belongs to with `run_on`. The dispatcher runs an
// order only when the two agree, so a fleet-singleton order (a merge sweep, a
// review gate, a delivery warden) installed on every seat through a shared pack
// still fires exactly once — on the fleet host.
//
// The role vocabulary lives here rather than in internal/config because the
// run_on decision is an order-scheduling rule: internal/config already imports
// this package, and this package deliberately imports no config.
const (
	// RoleFleetHost is the single city per fleet that owns fleet-wide
	// automation.
	RoleFleetHost = "fleet-host"
	// RoleSeat is an ordinary city (a developer's laptop, a per-project
	// city). It is the default when no role is declared.
	RoleSeat = "seat"
)

const (
	// RunOnAny runs the order regardless of the city's role. It is the
	// default when run_on is unset, so every order authored before this
	// field existed keeps its current behavior.
	RunOnAny = "any"
	// RunOnFleetHost runs the order only on the fleet host.
	RunOnFleetHost = "fleet-host"
	// RunOnSeat runs the order only on cities that are not the fleet host.
	RunOnSeat = "seat"
)

// ValidRoles lists the accepted `[city] role` values, for error messages.
var ValidRoles = []string{RoleFleetHost, RoleSeat}

// ValidRunOn lists the accepted `run_on` values, for error messages.
var ValidRunOn = []string{RunOnFleetHost, RunOnSeat, RunOnAny}

// IsValidRole reports whether value is a declarable city role. The empty
// string is valid: it means "not declared" and resolves to RoleSeat.
func IsValidRole(value string) bool {
	switch value {
	case "", RoleFleetHost, RoleSeat:
		return true
	}
	return false
}

// NormalizeRole maps a role to the effective value the dispatcher compares
// against: anything that is not the fleet host is a seat.
func NormalizeRole(role string) string {
	if role == RoleFleetHost {
		return RoleFleetHost
	}
	return RoleSeat
}

// IsValidRunOn reports whether value is an accepted run_on selector. The empty
// string is valid: it means "not declared" and resolves to RunOnAny.
func IsValidRunOn(value string) bool {
	switch value {
	case "", RunOnAny, RunOnFleetHost, RunOnSeat:
		return true
	}
	return false
}

// RunsOnRole reports whether an order carrying runOn should dispatch on a city
// whose effective role is role. An unset or unknown runOn runs everywhere:
// validation rejects unknown values at load, so the permissive fallback here
// only ever applies to an order that was never validated, and failing open
// preserves the pre-run_on behavior rather than silently stopping dispatch.
func RunsOnRole(runOn, role string) bool {
	switch runOn {
	case RunOnFleetHost:
		return NormalizeRole(role) == RoleFleetHost
	case RunOnSeat:
		return NormalizeRole(role) != RoleFleetHost
	default:
		return true
	}
}

// RunOnOrDefault returns the order's declared run_on, or RunOnAny when unset.
func (a *Order) RunOnOrDefault() string {
	if a.RunOn == "" {
		return RunOnAny
	}
	return a.RunOn
}

// RunsOn reports whether this order dispatches on a city whose effective role
// is role.
func (a *Order) RunsOn(role string) bool {
	return RunsOnRole(a.RunOn, role)
}

// quoteAll renders values as a quoted list for enum error messages, so
// `want "fleet-host", "seat", "any"` reads the same way the other order-field
// enum errors do.
func quoteAll(values []string) []string {
	out := make([]string, 0, len(values))
	for _, v := range values {
		out = append(out, `"`+v+`"`)
	}
	return out
}

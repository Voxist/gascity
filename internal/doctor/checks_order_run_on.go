package doctor

import (
	"fmt"
	"log"
	"os"
	"sort"
	"strings"

	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/orderdiscovery"
	"github.com/gastownhall/gascity/internal/orders"
)

const (
	orderRunOnRoleName = "order-run-on-role"
	// orderRunOnRoleDetailLimit bounds the named orders in the summary line so
	// a city that installs a large fleet pack gets a readable message; the
	// full list is still available from `gc order list`.
	orderRunOnRoleDetailLimit = 5
)

// OrderRunOnRoleCheck reports fleet-host orders installed on a city that is not
// the fleet host, at one of two severities.
//
// On an ordinary seat this is advisory. The orders are inert — the dispatcher
// skips them on their run_on — so nothing is broken; the city has simply
// installed a pack whose fleet-singleton orders were only ever meant to run in
// one place, and the durable fix is to put them in [orders].skip so they stop
// being scanned at all.
//
// On a city that LOOKS LIKE THE FLEET CITY by a signal independent of the role
// — it declares rigs, or it pins default-rig imports — the same finding is an
// ERROR, and that distinction is the point of this check. The role cannot
// detect its own absence: a fleet host whose `[city] role` declaration is lost
// resolves to seat by default and starts skipping every fleet-singleton order,
// which by role alone is byte-identical to a healthy seat. The second signal is
// what tells those two apart, so this check reports the outage rather than
// describing it in the same words it uses for the benign case.
type OrderRunOnRoleCheck struct {
	cfg      *config.City
	cityPath string
	// roleEnv is the VOXIST_FLEET_ROLE reading. Injected rather than read from
	// the process so a test can pin the resolved role without mutating the
	// environment of a parallel run.
	roleEnv    string
	roleEnvSet bool
}

// OrderRunOnRoleOption configures the fleet-role order check.
type OrderRunOnRoleOption func(*OrderRunOnRoleCheck)

// WithOrderRunOnRoleEnv pins the VOXIST_FLEET_ROLE value the check resolves the
// city's role against.
func WithOrderRunOnRoleEnv(value string) OrderRunOnRoleOption {
	return func(c *OrderRunOnRoleCheck) {
		c.roleEnv = value
		c.roleEnvSet = true
	}
}

// NewOrderRunOnRoleCheck creates the fleet-role order check.
func NewOrderRunOnRoleCheck(cfg *config.City, cityPath string, opts ...OrderRunOnRoleOption) *OrderRunOnRoleCheck {
	check := &OrderRunOnRoleCheck{cfg: cfg, cityPath: cityPath}
	for _, opt := range opts {
		opt(check)
	}
	return check
}

// Name returns the check identifier shown by gc doctor.
func (c *OrderRunOnRoleCheck) Name() string { return orderRunOnRoleName }

// CanFix reports whether the check supports automatic remediation. It does not:
// the two remedies (declare the role, or skip the orders) are opposite
// decisions about what this city is for, and only the operator knows which.
func (c *OrderRunOnRoleCheck) CanFix() bool { return false }

// Fix is a no-op because the remedy is an operator decision.
func (c *OrderRunOnRoleCheck) Fix(_ *CheckContext) error { return nil }

// WarmupEligible keeps the check out of `gc start`'s warm-up scan; it reads the
// order set from disk and reports a config-shape opinion, neither of which
// needs to gate a start.
func (c *OrderRunOnRoleCheck) WarmupEligible() bool { return false }

// Run reports enabled, un-skipped run_on = "fleet-host" orders on a city whose
// effective role is not fleet-host.
func (c *OrderRunOnRoleCheck) Run(ctx *CheckContext) *CheckResult {
	result := &CheckResult{Name: orderRunOnRoleName, Status: StatusOK, Severity: SeverityAdvisory}

	if c.cfg == nil {
		result.Message = "no city config loaded"
		return result
	}
	cityPath := c.cityPath
	if cityPath == "" && ctx != nil {
		cityPath = ctx.CityPath
	}
	if cityPath == "" {
		result.Status = StatusError
		result.Message = "city path unavailable"
		return result
	}

	role, roleWarning := config.EffectiveFleetRoleWithWarning(c.cfg, c.envValue())
	if roleWarning != "" {
		result.Details = append(result.Details, roleWarning)
	}
	if role == orders.RoleFleetHost {
		result.Message = "city role is fleet-host"
		return result
	}

	scanned, err := c.scanOrders(cityPath)
	if err != nil {
		result.Status = StatusError
		result.Message = fmt.Sprintf("scan orders: %v", err)
		return result
	}

	var fleetHostOnly []string
	for _, order := range scanned {
		if order.RunOn == orders.RunOnFleetHost {
			fleetHostOnly = append(fleetHostOnly, order.ScopedName())
		}
	}
	if len(fleetHostOnly) == 0 {
		result.Message = fmt.Sprintf("city role is %s; no fleet-host orders installed", role)
		return result
	}
	sort.Strings(fleetHostOnly)
	result.Details = append(result.Details, fleetHostOnly...)
	named := summarizeOrderNames(fleetHostOnly, orderRunOnRoleDetailLimit)

	if config.LooksLikeFleetCity(c.cfg) {
		// The city fans work out to rigs or pins what every rig imports, so it
		// is running fleet automation — but its role does not say fleet-host,
		// so the dispatcher is skipping every fleet-singleton order it holds.
		// Either the role declaration was lost (an outage: nothing is running
		// this automation anywhere) or these orders do not belong here. Both
		// need a human, so this gates.
		result.Status = StatusError
		result.Severity = SeverityBlocking
		result.Message = fmt.Sprintf(
			"this city declares rigs or default-rig imports, so it runs fleet automation, but its role is %s: %d fleet-host order(s) are enabled here and dispatch nowhere: %s",
			role, len(fleetHostOnly), named)
		result.FixHint = fmt.Sprintf(
			"set [city] role = %q in city.toml (or export %s=%s) if this is the fleet host — otherwise add these orders to [orders].skip so they stop being scanned here",
			orders.RoleFleetHost, config.FleetRoleEnvVar, orders.RoleFleetHost)
		return result
	}

	result.Status = StatusWarning
	result.Severity = SeverityAdvisory
	result.Message = fmt.Sprintf(
		"city role is %s but %d fleet-host order(s) are enabled here and never dispatch: %s",
		role, len(fleetHostOnly), named)
	if config.FleetRoleIsDeclared(c.cfg) {
		result.FixHint = fmt.Sprintf(
			"this city declares [city] role = %q, so these orders are inert: add them to [orders].skip in city.toml, or set role = %q if this city is the fleet host",
			c.cfg.CityRole.Role, orders.RoleFleetHost)
	} else {
		result.FixHint = fmt.Sprintf(
			"no [city] role is declared (and %s does not name one), so this city defaults to %q: set [city] role = %q if it is the fleet host, otherwise add these orders to [orders].skip",
			config.FleetRoleEnvVar, orders.RoleSeat, orders.RoleFleetHost)
	}
	return result
}

func (c *OrderRunOnRoleCheck) envValue() string {
	if c.roleEnvSet {
		return c.roleEnv
	}
	return os.Getenv(config.FleetRoleEnvVar)
}

// scanOrders returns the orders this city would actually schedule: the skip
// list is applied by the scanner and disabled orders (including ones an
// override disables) are dropped, so what remains is "enabled and un-skipped".
func (c *OrderRunOnRoleCheck) scanOrders(cityPath string) ([]orders.Order, error) {
	scanned, err := orderdiscovery.ScanAll(cityPath, c.cfg, orderdiscovery.ScanOptions{
		OnOverrideError: func(err error) error {
			log.Printf("gc doctor: skipping invalid order override for %s: %v", cityPath, err)
			return nil
		},
		OnValidateError: func(orderName string, err error) error {
			log.Printf("gc doctor: skipping invalid order %s for %s: %v", orderName, cityPath, err)
			return nil
		},
		ValidateOrder: orders.ValidateExecEnvOverrides,
	})
	if err != nil {
		return nil, err
	}
	return orders.FilterEnabled(scanned), nil
}

// summarizeOrderNames renders at most limit names, then an "and N more" tail.
func summarizeOrderNames(names []string, limit int) string {
	if limit <= 0 || len(names) <= limit {
		return strings.Join(names, ", ")
	}
	return fmt.Sprintf("%s, and %d more", strings.Join(names[:limit], ", "), len(names)-limit)
}

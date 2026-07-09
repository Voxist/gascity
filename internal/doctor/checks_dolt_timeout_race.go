package doctor

import (
	"fmt"
	"os"
	"time"

	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/doltpool"

	"gopkg.in/yaml.v3"
)

// DoltTimeoutRaceCheck asserts the vc-wz5 "read-timeout death match" invariant:
// the Go-native doltpool must reap an idle pooled connection client-side BEFORE
// the managed Dolt server's read_timeout_millis kills it server-side. When the
// client's idle-connection ceiling (doltpool.IdleConnCeiling) is >= the server
// read_timeout, the server closes idle conns the client still trusts; the
// database/sql pool then hands a dead conn to the next operation ("closing bad
// idle connection: EOF / connection reset / broken pipe"), taxing every op and
// — under churn — losing the dispatcher's last-fired write, which surfaced as
// town-wide scheduled-order staleness (vc-wz5).
//
// The invariant this check enforces is client idle-conn ceiling < server
// read_timeout. NOTE this is deliberately NOT "client readTimeout < server
// read_timeout" as the originating bead phrased it: the client per-query
// readTimeout is the response-read deadline for an in-flight query and has no
// bearing on idle-connection reaping. The failure surface is idle reuse, so the
// guarded knob is doltpool's ConnMaxIdleTime/ConnMaxLifetime, surfaced as
// IdleConnCeiling().
type DoltTimeoutRaceCheck struct {
	cityPath        string
	skip            bool
	applicableKnown bool
	applicable      bool
	doltConfig      config.DoltConfig
	// clientIdleCeiling is injectable for tests; nil uses doltpool.IdleConnCeiling.
	clientIdleCeiling func() time.Duration
}

// NewDoltTimeoutRaceCheck creates the check, resolving managed-Dolt
// applicability lazily from the city path (mirrors NewDoltConfigCheck).
func NewDoltTimeoutRaceCheck(cityPath string, skip bool) *DoltTimeoutRaceCheck {
	return &DoltTimeoutRaceCheck{cityPath: cityPath, skip: skip}
}

// NewDoltTimeoutRaceCheckForConfig creates the check using preloaded city config,
// mirroring NewDoltConfigCheckForConfig so both dolt checks share applicability.
func NewDoltTimeoutRaceCheckForConfig(cityPath string, skip bool, cfg *config.City, cfgErr error) *DoltTimeoutRaceCheck {
	var doltConfig config.DoltConfig
	if cfg != nil {
		doltConfig = cfg.Dolt
	}
	return &DoltTimeoutRaceCheck{
		cityPath:        cityPath,
		skip:            skip,
		applicableKnown: true,
		applicable:      ManagedLocalDoltChecksApplicableForConfig(cityPath, cfg, cfgErr),
		doltConfig:      doltConfig,
	}
}

func (c *DoltTimeoutRaceCheck) managedApplicable() bool {
	if c.applicableKnown {
		return c.applicable
	}
	return managedLocalDoltChecksApplicable(c.cityPath)
}

// Name returns the check identifier.
func (c *DoltTimeoutRaceCheck) Name() string { return "dolt-timeout-race" }

// Run compares the doltpool client idle-connection ceiling against the managed
// Dolt server read_timeout and fails (blocking) when the ordering is unsafe.
func (c *DoltTimeoutRaceCheck) Run(_ *CheckContext) *CheckResult {
	r := &CheckResult{Name: c.Name()}
	if c.skip || !c.managedApplicable() {
		r.Status = StatusOK
		r.Message = "skipped (file backend, external dolt endpoint, or GC_DOLT=skip)"
		return r
	}

	clientIdle := doltpool.IdleConnCeiling()
	if c.clientIdleCeiling != nil {
		clientIdle = c.clientIdleCeiling()
	}
	if clientIdle <= 0 {
		// No client-side idle reaping at all: every idle conn lives until the
		// server kills it — the pre-vc-wz5 death match.
		r.Status = StatusError
		r.Message = "doltpool performs no client-side idle-connection reaping (IdleConnCeiling=0); the managed Dolt server will kill idle pooled conns the client still trusts (read-timeout death match)"
		r.FixHint = "set doltpool connMaxIdleTime below the managed dolt read_timeout_millis"
		return r
	}

	serverTimeout, source, ok := c.serverReadTimeout()
	if !ok {
		// Server read_timeout is unknowable — cannot assert the ordering.
		r.Status = StatusWarning
		r.Message = fmt.Sprintf(
			"cannot determine managed Dolt read_timeout_millis; unable to verify the client idle ceiling (%s) sits below the server read timeout",
			clientIdle)
		r.FixHint = "run gc start (or gc dolt restart) to materialize managed dolt-config.yaml"
		return r
	}

	if clientIdle >= serverTimeout {
		r.Status = StatusError
		r.Message = fmt.Sprintf(
			"dolt read-timeout death match: client idle-conn ceiling %s >= server read_timeout %s (%s) — the server kills idle pooled conns the client still trusts",
			clientIdle, serverTimeout, source)
		r.FixHint = "lower doltpool connMaxIdleTime below the server read_timeout_millis, or raise [dolt] read_timeout_millis in city.toml above the client idle ceiling"
		return r
	}

	r.Status = StatusOK
	r.Message = fmt.Sprintf(
		"dolt timeout ordering OK: client idle ceiling %s < server read_timeout %s (%s)",
		clientIdle, serverTimeout, source)
	return r
}

// serverReadTimeout resolves the managed Dolt server read timeout. It prefers
// the live deployed dolt-config.yaml value (what the running server actually
// enforces, including any drift) and falls back to the configured effective
// value. Returns (timeout, human-readable source, ok).
func (c *DoltTimeoutRaceCheck) serverReadTimeout() (time.Duration, string, bool) {
	path := resolveManagedDoltConfigPath(c.cityPath)
	if data, err := os.ReadFile(path); err == nil { //nolint:gosec // path is derived from city layout
		var doc map[string]any
		if yaml.Unmarshal(data, &doc) == nil {
			if v, present := lookupYAMLPath(doc, "listener.read_timeout_millis"); present {
				if millis, ok := yamlScalarInt(v); ok && millis > 0 {
					return time.Duration(millis) * time.Millisecond, "managed dolt-config.yaml", true
				}
			}
		}
	}
	// Fall back to the configured effective value (default when unset).
	if millis := c.doltConfig.EffectiveReadTimeoutMillis(); millis > 0 {
		return time.Duration(millis) * time.Millisecond, "configured [dolt] effective default", true
	}
	return 0, "", false
}

// yamlScalarInt extracts an int from a decoded YAML scalar, accepting the int
// widths gopkg.in/yaml.v3 may produce.
func yamlScalarInt(v any) (int, bool) {
	switch n := v.(type) {
	case int:
		return n, true
	case int64:
		return int(n), true
	case uint64:
		return int(n), true //nolint:gosec // managed read_timeout is a small positive value
	case float64:
		return int(n), true
	}
	return 0, false
}

// CanFix returns false: remediation is a code (doltpool constant) or city.toml
// change, not something the doctor can safely auto-apply.
func (c *DoltTimeoutRaceCheck) CanFix() bool { return false }

// Fix is a no-op. See CanFix.
func (c *DoltTimeoutRaceCheck) Fix(_ *CheckContext) error { return nil }

// WarmupEligible returns false: this is a steady-state ordering invariant, run
// on demand via gc doctor, not part of the gc start warm-up scan.
func (c *DoltTimeoutRaceCheck) WarmupEligible() bool { return false }

package doctor

import (
	"encoding/json"
	"errors"
	"fmt"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/beads/contract"
)

// CustomTypesPreflightCheck data-gates the dolt_mode_safe flip for a scope.
//
// Unlike CustomTypesCheck, which reads only the types.custom config value,
// this check reads the store's fully-resolved registered custom-type set
// (the custom_types table ∪ types.custom config) via `bd types --json` and
// compares it against RequiredCustomTypes using the pure
// contract.EvaluateCustomTypesRegistered preflight. It exists so an operator
// (or a future cutover lever) can confirm a proxied-server scope is prepared
// before native storage is enabled: until every required type resolves, the
// flip must stay refused. The check is read-only and never mutates the store.
type CustomTypesPreflightCheck struct {
	// Dir is the directory to check (city root or rig path).
	Dir string
	// Label identifies this check instance (e.g., "city" or rig name).
	Label string
	// ReadRegistered reads the scope's fully-resolved registered custom-type
	// set. A nil reader uses the live `bd types --json` reader, whose Dolt
	// access is integration/CI-gated. Tests inject a deterministic reader.
	ReadRegistered func(dir string) ([]string, error)
}

// NewCustomTypesPreflightCheck creates a preflight check for a store directory.
func NewCustomTypesPreflightCheck(dir, label string) *CustomTypesPreflightCheck {
	return &CustomTypesPreflightCheck{Dir: dir, Label: label}
}

// Name returns the check identifier.
func (c *CustomTypesPreflightCheck) Name() string {
	return "custom-types-preflight:" + c.Label
}

// Run reads the registered custom-type set and reports whether the scope
// satisfies the native-store data precondition. A satisfied scope passes; an
// unsatisfied scope is a hard error naming the missing types, because flipping
// dolt_mode_safe on it would reproduce the zero-session class. An unreadable
// store is a warning: eligibility cannot be confirmed, so the flip stays
// refused by default.
func (c *CustomTypesPreflightCheck) Run(_ *CheckContext) *CheckResult {
	r := &CheckResult{Name: c.Name()}

	beadsDir := filepath.Join(c.Dir, ".beads")
	if !dirExists(beadsDir) {
		r.Status = StatusOK
		r.Message = "no .beads directory, skipping"
		return r
	}

	read := c.ReadRegistered
	if read == nil {
		read = readRegisteredCustomTypes
	}
	registered, err := read(c.Dir)
	if err != nil {
		r.Status = StatusWarning
		r.Message = fmt.Sprintf("could not read registered custom types: %v", err)
		r.FixHint = "ensure bd is reachable for this scope; the native-store flip stays refused until the registered set is confirmed"
		return r
	}

	result := contract.EvaluateCustomTypesRegistered(RequiredCustomTypes, registered)
	if result.Satisfied {
		r.Status = StatusOK
		r.Message = fmt.Sprintf("all %d required types registered; native-store flip is data-eligible", len(RequiredCustomTypes))
		return r
	}

	r.Status = StatusError
	r.Message = fmt.Sprintf("missing %d required type(s): %s — native-store flip refused", len(result.Missing), strings.Join(result.Missing, ", "))
	r.FixHint = "run gc doctor --fix on the custom-types check to register missing types before any native-store cutover"
	return r
}

// CanFix returns false — this check is read-only. Registration of missing
// types is owned by CustomTypesCheck; keeping the two responsibilities
// separate avoids mutating a store from a preflight gate.
func (c *CustomTypesPreflightCheck) CanFix() bool { return false }

// Fix is a no-op; this check does not mutate state.
func (c *CustomTypesPreflightCheck) Fix(_ *CheckContext) error { return nil }

// WarmupEligible reports whether this check runs during `gc start` warm-up.
// It does not: the native-store flip is an explicit operator action, so the
// preflight runs on demand via `gc doctor`, not on every start.
func (c *CustomTypesPreflightCheck) WarmupEligible() bool { return false }

// bdTypesJSON is the shape of `bd types --json`. The custom_types field is the
// store's fully-resolved registered custom-type set (custom_types table ∪
// types.custom config), which is exactly the set the native-store data gate
// must consult.
type bdTypesJSON struct {
	CustomTypes []string `json:"custom_types"`
}

// readRegisteredCustomTypes reads the scope's fully-resolved registered
// custom-type set via `bd types --json`. The Dolt access this performs is
// integration/CI-gated on macOS (ICU); tests inject a deterministic reader
// through CustomTypesPreflightCheck.ReadRegistered instead.
func readRegisteredCustomTypes(dir string) ([]string, error) {
	start := time.Now()
	args := []string{"types", "--json"}
	cmd := exec.Command("bd", args...)
	cmd.Dir = dir
	out, err := cmd.Output()
	exitCode := 0
	if err != nil {
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) {
			exitCode = exitErr.ExitCode()
		} else {
			exitCode = -1
		}
	}
	beads.TraceBDCall("go:doctor.readRegisteredCustomTypes", dir, args, start, exitCode, err)
	if err != nil {
		return nil, err
	}
	return parseBdTypesJSON(out)
}

// parseBdTypesJSON decodes the custom_types field of `bd types --json`.
func parseBdTypesJSON(out []byte) ([]string, error) {
	var parsed bdTypesJSON
	if err := json.Unmarshal(out, &parsed); err != nil {
		return nil, fmt.Errorf("parsing bd types output: %w", err)
	}
	return parsed.CustomTypes, nil
}

package contract

import (
	"errors"
	"fmt"
	"sort"
	"strings"
)

// ErrCustomTypesNotRegistered is the typed sentinel returned by the
// custom-types cutover gate when the target store has not registered every
// required custom bead type. Any lever that would flip dolt_mode_safe to PASS
// for a proxied-server scope must treat this error as a hard refusal: the
// mode is never flipped on un-prepared data. Callers detect it with
// errors.Is(err, ErrCustomTypesNotRegistered).
var ErrCustomTypesNotRegistered = errors.New("required custom bead types are not registered")

// CustomTypesPreflightResult is the typed outcome of comparing a store's
// registered custom bead types against the set Gas City requires before the
// in-process native store may be activated for a scope.
//
// Satisfied is true only when every required type is present in the registered
// set. Missing lists the required types absent from the registered set, sorted
// for stable output. Required and Registered echo the normalized inputs so the
// diagnostic is self-describing.
type CustomTypesPreflightResult struct {
	// Satisfied reports whether every required type is registered.
	Satisfied bool `json:"satisfied"`
	// Missing lists required types absent from the registered set, sorted.
	Missing []string `json:"missing,omitempty"`
	// Required is the normalized required-type set used for the comparison.
	Required []string `json:"required"`
	// Registered is the normalized registered-type set read from the store.
	Registered []string `json:"registered"`
}

// EvaluateCustomTypesRegistered compares a store's registered custom bead
// types against the required set and returns a typed result.
//
// The comparison is a pure set operation (ZFC-clean): the required set is a
// fixed threshold supplied by the caller (the RequiredCustomTypes constant in
// production), the registered set is read from the store, and the function
// contains no judgment calls. Inputs are normalized (trimmed, empty entries
// dropped, de-duplicated) before comparison so callers may pass raw CLI output
// directly. The result is Satisfied iff every required type is present in the
// registered set.
func EvaluateCustomTypesRegistered(required, registered []string) CustomTypesPreflightResult {
	normalizedRequired := normalizeTypeSet(required)
	normalizedRegistered := normalizeTypeSet(registered)

	registeredSet := make(map[string]bool, len(normalizedRegistered))
	for _, t := range normalizedRegistered {
		registeredSet[t] = true
	}

	var missing []string
	for _, req := range normalizedRequired {
		if !registeredSet[req] {
			missing = append(missing, req)
		}
	}
	sort.Strings(missing)

	return CustomTypesPreflightResult{
		Satisfied:  len(missing) == 0,
		Missing:    missing,
		Required:   normalizedRequired,
		Registered: normalizedRegistered,
	}
}

// GateCustomTypesRegistered is the reusable cutover gate consulted by any
// future dolt_mode_safe flip lever. It evaluates the registered set against
// the required set and returns nil only when every required type is present.
// When any required type is missing it returns an error wrapping
// ErrCustomTypesNotRegistered so the flip is refused — the mode is never
// flipped on un-prepared data. The error message names the missing types so
// the refusal is self-diagnosing.
func GateCustomTypesRegistered(required, registered []string) error {
	result := EvaluateCustomTypesRegistered(required, registered)
	if result.Satisfied {
		return nil
	}
	return fmt.Errorf("%w: missing %s", ErrCustomTypesNotRegistered, strings.Join(result.Missing, ", "))
}

// normalizeTypeSet trims whitespace, drops empty entries, and de-duplicates a
// type list while preserving first-seen order.
func normalizeTypeSet(in []string) []string {
	seen := make(map[string]bool, len(in))
	out := make([]string, 0, len(in))
	for _, t := range in {
		trimmed := strings.TrimSpace(t)
		if trimmed == "" {
			continue
		}
		if seen[trimmed] {
			continue
		}
		seen[trimmed] = true
		out = append(out, trimmed)
	}
	return out
}

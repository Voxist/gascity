package beadmeta

import (
	"errors"
	"sort"
)

// Conventioned metadata keys: the store-wide value conventions, ruled once
// instead of per key (vp-gbxl9 / the metadata-emptiness upstream ask; ADR-0050
// C3 for the route key, vc-j1qgwn for the human-gate key).
//
// The ruling has three states per key, kept distinct on purpose:
//
//	absent  — the legal negation ("not routed", "not awaiting"); parks cleanly
//	present — a value inside the key's closed domain (see conventionedValues)
//	invalid — present-but-empty, or any out-of-domain value; never persisted
//
// Membership is data, not code: adding a third conventioned key is one entry
// in conventionedValues (plus the const in keys.go), not a new guard site.
// The validators are consulted at the metadata write path — every Store
// implementation's Set methods, compare-and-set, and the gc bd CLI boundary —
// so the invalid state is unrepresentable rather than detected after the fact.
// Raw bd writers that bypass both gc and this library are bound upstream, not
// here (see the upstream ask in voxist-city docs).

// ValidateMetadataValue returns a non-nil error when key is conventioned and
// value is outside its closed domain. Keys with no convention — the open
// world — validate as nil for any value, including "".
func ValidateMetadataValue(key, value string) error {
	rule, ok := conventionedValues[key]
	if !ok {
		return nil
	}
	return rule(value)
}

// ValidateMetadataValues applies ValidateMetadataValue to every pair and
// returns the first violation in sorted key order, so rejection is
// deterministic for a given batch regardless of map iteration order.
func ValidateMetadataValues(kvs map[string]string) error {
	keys := make([]string, 0, len(kvs))
	for k := range kvs {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		if err := ValidateMetadataValue(k, kvs[k]); err != nil {
			return err
		}
	}
	return nil
}

var conventionedValues = map[string]func(value string) error{
	// ADR-0050 C3: there is no legal "set to empty string". Empty-string is a
	// bug signature, not a value: it loops the reconciler into re-dispatching
	// the bead, where an absent key parks cleanly.
	RoutedToMetadataKey: func(value string) error {
		if value == "" {
			return errors.New("gc.routed_to: an empty value is invalid — clearing a route means unsetting the key (--unset-metadata gc.routed_to). An absent key parks cleanly; a present-but-empty value loops the reconciler into re-dispatching (ADR-0050 C3)")
		}
		return nil
	},
	// vc-j1qgwn canonical schema: presence with the literal "true" is the only
	// affirmative; negation is key absence. Owner identity and ask text
	// decompose onto their own keys so the predicate stays a predicate.
	AwaitingHumanMetadataKey: func(value string) error {
		if value != "true" {
			return errors.New("gc.awaiting_human: only the literal \"true\" is valid — negation is key absence (--unset-metadata gc.awaiting_human); owner identity moves to gc.human_owner and the ask text to gc.human_ask (vc-j1qgwn)")
		}
		return nil
	},
}

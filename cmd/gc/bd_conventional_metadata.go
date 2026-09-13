package main

import (
	"fmt"
	"strings"

	"github.com/gastownhall/gascity/internal/bdflags"
	"github.com/gastownhall/gascity/internal/beadmeta"
)

// conventionalMetadataValueRefusal reports whether a `gc bd` invocation would
// write a value outside a conventioned key's closed domain, and the message to
// print. The conventioned-key registry and the per-key rules live in
// internal/beadmeta; this guard only carries them to the CLI boundary.
//
// Mirrors mistypedMetadataPairRefusal: gc bd writes exec raw bd, so nothing
// upstream of this sees the pair — and bd accepts any metadata value, so the
// invalid state (absent ≠ empty conflation that loops the reconciler) would be
// minted silently. Refusal happens before any store work, so the exit code is
// honest and nothing is written.
func conventionalMetadataValueRefusal(bdArgs []string) (string, bool) {
	verb, _ := bdflags.SplitGlobalFlags(bdArgs)
	if verb != "update" {
		return "", false
	}
	var refusals []string
	for _, pair := range bdflags.SetMetadataPairs(bdArgs) {
		key, value, found := strings.Cut(pair, "=")
		if !found {
			continue // flag-shape errors belong to bd and the mistyped guard
		}
		if err := beadmeta.ValidateMetadataValue(key, value); err != nil {
			refusals = append(refusals, fmt.Sprintf("  --set-metadata %s: %v", pair, err))
		}
	}
	if len(refusals) == 0 {
		return "", false
	}
	return fmt.Sprintf(
		"gc bd: refusing update: metadata value(s) violate their key's convention:\n%s\n",
		strings.Join(refusals, "\n"),
	), true
}

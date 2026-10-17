package beadmeta

import "strings"

// IsOptOutValue reports whether a gc.do_not_auto_route metadata VALUE means
// "held". It mirrors the city pack's gclib.is_optout: bd stores
// `--set-metadata gc.do_not_auto_route=1` as the JSON integer 1 (and `=true`
// as the JSON boolean true), the beads StringMap decode coerces both to their
// string form, and a naive `== "1"` check would silently miss every other
// spelling a writer may stamp. Normalize across int/bool/string forms.
func IsOptOutValue(value string) bool {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "1", "true", "yes":
		return true
	}
	return false
}

// IsParkedMetadata reports whether a bead's metadata says "no more
// auto-agent work" — held or parked. It is the binary-side twin of the city
// pack's gclib.is_parked: the binary cannot import the pack's gclib, so the
// three-signal check is declared here, where every workflow package can
// share it. Three park signals, any one of which exempts the bead from
// route restore and every auto-router:
//
//   - gc.do_not_auto_route truthy (IsOptOutValue) — the explicit operator hold;
//   - gc.awaiting_human non-empty — the agent park convention for human-gated
//     steps (vp-cixi precedent);
//   - gc.blocked_on non-empty — the equivalent park signal some writers stamp
//     instead (e.g. vc-u9j's human-operator oauth-credential).
//
// A parked bead is left EXACTLY as its writer left it — not re-routed, not
// reclaimed, not restored. An empty gc.routed_to on a parked bead is the
// amended ADR-0066 D3 discharge shape, not data loss: restoring it re-arms
// human-gated work and costs a full triage cycle per pass (vc-klxi). A
// false-park is cheap (human-clearable); a false-unpark is the churn this
// predicate exists to eliminate (vp-dbck, vp-1zpd).
func IsParkedMetadata(md map[string]string) bool {
	return IsOptOutValue(md[DoNotAutoRouteMetadataKey]) ||
		strings.TrimSpace(md[AwaitingHumanMetadataKey]) != "" ||
		strings.TrimSpace(md[BlockedOnMetadataKey]) != ""
}

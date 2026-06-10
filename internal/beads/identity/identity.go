// Package identity provides the post-open identity assertion used to detect a
// silent-empty or misrouted beads database before a NativeDoltStore is trusted.
//
// The P2 native-store cutover canaries one scope at a time. The structural risk
// is that an empty env projection or a metadata load failure lets the cgo
// OpenBestAvailable path create or open an empty embedded database that passes
// every pre-open gate yet points at the wrong (or no) data. This package
// compares the identity the operator configured for a scope against the
// identity the opened storage actually reports. The comparison is a pure
// threshold compare — there is no judgment call in Go; the caller decides what
// to do with each Class.
package identity

import "strings"

// ScopeIdentity is the canonical identity of a beads scope: the project_id the
// scope is pinned to and the issue_prefix it stamps onto bead IDs. Both fields
// are compared case-insensitively after trimming surrounding whitespace.
type ScopeIdentity struct {
	// ProjectID is the scope's pinned project_id (from .beads/metadata.json for
	// the configured side, or the opened storage's project_id config key for the
	// opened side).
	ProjectID string
	// IssuePrefix is the scope's bead ID prefix without a trailing "-".
	IssuePrefix string
}

// Class enumerates the outcome of an identity assertion. It is a closed set so
// callers can switch exhaustively and map each outcome to a stable alert class.
type Class string

const (
	// ClassMatch means the configured and opened identities agree on every
	// populated field. This is the only outcome under which the opened store is
	// safe to trust.
	ClassMatch Class = "match"
	// ClassConfiguredEmpty means the configured side has no identity to assert
	// against (no project_id and no issue_prefix). The caller cannot prove the
	// opened store is correct, so it must be treated as degraded.
	ClassConfiguredEmpty Class = "configured-empty"
	// ClassOpenedEmpty means the opened storage reported no identity. This is the
	// silent-empty / freshly-created embedded-DB signature: the store opened but
	// has no project_id or issue_prefix to confirm it holds the scope's data.
	ClassOpenedEmpty Class = "opened-empty"
	// ClassMismatch means both sides reported an identity but at least one
	// populated field disagrees. This is the misrouted-DB signature: the store
	// opened a different scope's data.
	ClassMismatch Class = "mismatch"
)

// Degraded reports whether a Class represents a store that must NOT be trusted
// as the native store for its scope. Only ClassMatch is non-degraded.
func (c Class) Degraded() bool {
	return c != ClassMatch
}

// AlertClass is the stable identifier emitted on a typed store.degraded alert
// for every degraded outcome. P2.2's beads fix plus this assertion together
// guarantee a silent-empty or misrouted database surfaces as
// class=identity-mismatch rather than as a healthy NativeDoltStore.
const AlertClass = "identity-mismatch"

// Result is the typed outcome of Assert. It carries the Class plus the
// normalized identities that were compared, so an alert sink can report exactly
// what disagreed without re-reading either side.
type Result struct {
	// Class is the assertion outcome.
	Class Class
	// Configured is the normalized identity the operator configured for the scope.
	Configured ScopeIdentity
	// Opened is the normalized identity the opened storage reported.
	Opened ScopeIdentity
	// MismatchedFields lists the populated fields that disagreed, in a stable
	// order ("project_id" before "issue_prefix"). It is non-empty only for
	// ClassMismatch.
	MismatchedFields []string
}

// Degraded reports whether the assertion concluded the opened store is unsafe.
func (r Result) Degraded() bool {
	return r.Class.Degraded()
}

// normalize trims whitespace and lower-cases an identity field so the
// comparison is insensitive to incidental formatting differences between the
// metadata file and the storage config table.
func normalize(value string) string {
	return strings.ToLower(strings.TrimSpace(value))
}

func (s ScopeIdentity) normalized() ScopeIdentity {
	return ScopeIdentity{
		ProjectID:   normalize(s.ProjectID),
		IssuePrefix: normalize(s.IssuePrefix),
	}
}

func (s ScopeIdentity) empty() bool {
	return s.ProjectID == "" && s.IssuePrefix == ""
}

// Assert compares the identity an operator configured for a scope against the
// identity the opened storage reported and returns a typed Result.
//
// The comparison is purely mechanical:
//
//   - configured has no identity at all       -> ClassConfiguredEmpty
//   - opened has no identity at all            -> ClassOpenedEmpty
//   - both populated, every shared field agrees -> ClassMatch
//   - both populated, a shared field disagrees -> ClassMismatch
//
// A field only participates when both sides populate it; a side that omits a
// field neither confirms nor contradicts the other. This keeps the assertion
// strict where it can be (mismatched populated fields fail) without inventing a
// disagreement from missing data.
func Assert(configured, opened ScopeIdentity) Result {
	cfg := configured.normalized()
	opn := opened.normalized()
	result := Result{Configured: cfg, Opened: opn}

	if cfg.empty() {
		result.Class = ClassConfiguredEmpty
		return result
	}
	if opn.empty() {
		result.Class = ClassOpenedEmpty
		return result
	}

	var mismatched []string
	if cfg.ProjectID != "" && opn.ProjectID != "" && cfg.ProjectID != opn.ProjectID {
		mismatched = append(mismatched, "project_id")
	}
	if cfg.IssuePrefix != "" && opn.IssuePrefix != "" && cfg.IssuePrefix != opn.IssuePrefix {
		mismatched = append(mismatched, "issue_prefix")
	}
	if len(mismatched) > 0 {
		result.Class = ClassMismatch
		result.MismatchedFields = mismatched
		return result
	}
	result.Class = ClassMatch
	return result
}

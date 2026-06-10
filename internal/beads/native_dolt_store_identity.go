package beads

import (
	"github.com/gastownhall/gascity/internal/beads/identity"
)

// OpenedIdentity returns the identity the native store actually reported when it
// opened: the issue_prefix and project_id read from the opened storage's config
// table. A silently-misrouted or freshly-created embedded database returns an
// empty identity here, which the post-open identity assertion classifies as
// opened-empty.
func (s *NativeDoltStore) OpenedIdentity() identity.ScopeIdentity {
	if s == nil {
		return identity.ScopeIdentity{}
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	return identity.ScopeIdentity{
		ProjectID:   s.projectID,
		IssuePrefix: s.idPrefix,
	}
}

// AssertOpenedIdentity compares the identity the store reported at open against
// the configured scope identity and returns the typed assertion result. The
// caller decides what to do with a degraded result (close + fall back to
// BdStore, emit a typed alert, or both); this method makes the threshold
// compare, not the policy decision.
func (s *NativeDoltStore) AssertOpenedIdentity(configured identity.ScopeIdentity) identity.Result {
	return identity.Assert(configured, s.OpenedIdentity())
}

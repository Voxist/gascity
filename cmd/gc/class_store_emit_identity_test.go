package main

import (
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/beads/identity"
)

// TestEmittingClassStoreIdentityForwardFailsClosed pins the capability forward's
// answer when the wrapped store cannot report an identity.
//
// The wrapper always satisfies the AssertOpenedIdentity interface — a method
// cannot be conditionally absent — so every capability probe on it succeeds. A
// zero identity.Result would therefore hand a prober a Class matching none of
// the four documented values while Degraded() reports true, i.e. an alert with
// an empty class. The forward must return a documented class instead.
func TestEmittingClassStoreIdentityForwardFailsClosed(t *testing.T) {
	t.Parallel()

	// A backing store with no identity capability at all.
	s := &emittingClassStore{Store: beads.NewMemStore()}

	configured := identity.ScopeIdentity{ProjectID: "proj", IssuePrefix: "gc"}
	got := s.AssertOpenedIdentity(configured)

	switch got.Class {
	case identity.ClassMatch, identity.ClassConfiguredEmpty, identity.ClassOpenedEmpty, identity.ClassMismatch:
	default:
		t.Fatalf("AssertOpenedIdentity returned Class %q, which is none of the four documented classes; "+
			"a prober switching exhaustively on Class matches no case and any alert carries an empty class", got.Class)
	}
	if got.Class != identity.ClassOpenedEmpty {
		t.Fatalf("Class = %q, want %q for a store that cannot report an identity", got.Class, identity.ClassOpenedEmpty)
	}
}

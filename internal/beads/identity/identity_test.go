package identity

import (
	"reflect"
	"testing"
)

func TestAssert(t *testing.T) {
	tests := []struct {
		name           string
		configured     ScopeIdentity
		opened         ScopeIdentity
		wantClass      Class
		wantDegraded   bool
		wantMismatched []string
	}{
		{
			name:         "exact match",
			configured:   ScopeIdentity{ProjectID: "proj-vr", IssuePrefix: "vr"},
			opened:       ScopeIdentity{ProjectID: "proj-vr", IssuePrefix: "vr"},
			wantClass:    ClassMatch,
			wantDegraded: false,
		},
		{
			name:         "match ignores case and surrounding whitespace",
			configured:   ScopeIdentity{ProjectID: "Proj-VR", IssuePrefix: " VR "},
			opened:       ScopeIdentity{ProjectID: "proj-vr", IssuePrefix: "vr"},
			wantClass:    ClassMatch,
			wantDegraded: false,
		},
		{
			name:         "configured side has no identity",
			configured:   ScopeIdentity{},
			opened:       ScopeIdentity{ProjectID: "proj-vr", IssuePrefix: "vr"},
			wantClass:    ClassConfiguredEmpty,
			wantDegraded: true,
		},
		{
			name:         "configured whitespace-only counts as empty",
			configured:   ScopeIdentity{ProjectID: "   ", IssuePrefix: "\t"},
			opened:       ScopeIdentity{ProjectID: "proj-vr", IssuePrefix: "vr"},
			wantClass:    ClassConfiguredEmpty,
			wantDegraded: true,
		},
		{
			name:         "opened side empty is the silent-empty signature",
			configured:   ScopeIdentity{ProjectID: "proj-vr", IssuePrefix: "vr"},
			opened:       ScopeIdentity{},
			wantClass:    ClassOpenedEmpty,
			wantDegraded: true,
		},
		{
			name:           "project_id mismatch is misrouted-DB",
			configured:     ScopeIdentity{ProjectID: "proj-vr", IssuePrefix: "vr"},
			opened:         ScopeIdentity{ProjectID: "proj-hq", IssuePrefix: "vr"},
			wantClass:      ClassMismatch,
			wantDegraded:   true,
			wantMismatched: []string{"project_id"},
		},
		{
			name:           "issue_prefix mismatch is misrouted-DB",
			configured:     ScopeIdentity{ProjectID: "proj-vr", IssuePrefix: "vr"},
			opened:         ScopeIdentity{ProjectID: "proj-vr", IssuePrefix: "hq"},
			wantClass:      ClassMismatch,
			wantDegraded:   true,
			wantMismatched: []string{"issue_prefix"},
		},
		{
			name:           "both fields mismatch reported in stable order",
			configured:     ScopeIdentity{ProjectID: "proj-vr", IssuePrefix: "vr"},
			opened:         ScopeIdentity{ProjectID: "proj-hq", IssuePrefix: "hq"},
			wantClass:      ClassMismatch,
			wantDegraded:   true,
			wantMismatched: []string{"project_id", "issue_prefix"},
		},
		{
			name:         "field omitted by one side neither confirms nor contradicts",
			configured:   ScopeIdentity{ProjectID: "proj-vr"},
			opened:       ScopeIdentity{ProjectID: "proj-vr", IssuePrefix: "vr"},
			wantClass:    ClassMatch,
			wantDegraded: false,
		},
		{
			name:         "configured prefix only, opened project only, no shared field disagrees",
			configured:   ScopeIdentity{IssuePrefix: "vr"},
			opened:       ScopeIdentity{ProjectID: "proj-vr"},
			wantClass:    ClassMatch,
			wantDegraded: false,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := Assert(tc.configured, tc.opened)
			if got.Class != tc.wantClass {
				t.Errorf("Assert() class = %q, want %q", got.Class, tc.wantClass)
			}
			if got.Degraded() != tc.wantDegraded {
				t.Errorf("Assert() degraded = %v, want %v", got.Degraded(), tc.wantDegraded)
			}
			if !reflect.DeepEqual(got.MismatchedFields, tc.wantMismatched) {
				t.Errorf("Assert() mismatched = %v, want %v", got.MismatchedFields, tc.wantMismatched)
			}
		})
	}
}

func TestAssertNormalizesReportedIdentities(t *testing.T) {
	got := Assert(
		ScopeIdentity{ProjectID: " Proj-VR ", IssuePrefix: "VR"},
		ScopeIdentity{ProjectID: "proj-hq", IssuePrefix: "HQ"},
	)
	if got.Configured.ProjectID != "proj-vr" || got.Configured.IssuePrefix != "vr" {
		t.Errorf("configured not normalized: %+v", got.Configured)
	}
	if got.Opened.ProjectID != "proj-hq" || got.Opened.IssuePrefix != "hq" {
		t.Errorf("opened not normalized: %+v", got.Opened)
	}
}

func TestClassDegraded(t *testing.T) {
	if ClassMatch.Degraded() {
		t.Error("ClassMatch must not be degraded")
	}
	for _, c := range []Class{ClassConfiguredEmpty, ClassOpenedEmpty, ClassMismatch} {
		if !c.Degraded() {
			t.Errorf("%q must be degraded", c)
		}
	}
}

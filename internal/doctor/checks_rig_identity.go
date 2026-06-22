package doctor

import (
	"fmt"
	"path/filepath"

	"github.com/gastownhall/gascity/internal/beads/contract"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/fsys"
)

// RigIdentityTriadCheck asserts that the per-rig identity file layers are
// consistent: the L1 project identity project.id must equal
// .beads/metadata.json#project_id (L2). A mismatch surfaces as a blocking
// error so operators see it during `gc doctor` or `gc start` warmup, not
// silently as "0 agents" hours later (the 2026-06-21 incident pattern).
//
// Triad scope: the third layer — the rig's Dolt DB metadata._project_id (L3)
// — is enforced separately, at connect time, by `gc dolt-state
// ensure-project-id`, which refuses to connect on an L1/L3 mismatch (see
// formatL1L3MismatchError in cmd/gc/dolt_project_id.go). This doctor check
// deliberately stays DB-free so it can run during warmup before the bead
// store / Dolt server is reachable; it closes the remaining gap — silent
// drift between the two on-disk cache layers that `bd ready` consults, which
// is the exact failure that was invisible in the incident. Together the two
// mechanisms cover the full L1/L2/L3 triad.
//
// The check is WarmupEligible so it fires during `gc start` before the
// bead store is opened — a fail-fast gate before dispatch is unblocked.
type RigIdentityTriadCheck struct {
	cityPath string
	rig      config.Rig
	fs       fsys.FS
}

// NewRigIdentityTriadCheck creates a per-rig identity-triad doctor check.
func NewRigIdentityTriadCheck(cityPath string, rig config.Rig) *RigIdentityTriadCheck {
	return &RigIdentityTriadCheck{cityPath: cityPath, rig: rig, fs: fsys.OSFS{}}
}

// Name returns the check identifier ("rig:<name>:identity-triad").
func (c *RigIdentityTriadCheck) Name() string {
	return "rig:" + c.rig.Name + ":identity-triad"
}

// WarmupEligible returns true — fires during gc start before bead store open.
func (c *RigIdentityTriadCheck) WarmupEligible() bool { return true }

// CanFix returns false; identity reconciliation requires running
// gc dolt-state ensure-project-id with rig-specific connection flags.
func (c *RigIdentityTriadCheck) CanFix() bool { return false }

// Fix is a no-op.
func (c *RigIdentityTriadCheck) Fix(_ *CheckContext) error { return nil }

// Run checks that the L1 project identity project.id matches
// .beads/metadata.json#project_id for the rig.
func (c *RigIdentityTriadCheck) Run(_ *CheckContext) *CheckResult {
	r := &CheckResult{Name: c.Name()}
	rigPath := normalizedRigPath(c.cityPath, c.rig)

	l1, l1ok, err := contract.ReadProjectIdentity(c.fs, rigPath)
	if err != nil {
		r.Status = StatusWarning
		r.Severity = SeverityAdvisory
		r.Message = fmt.Sprintf("rig %q: cannot read project identity", c.rig.Name)
		r.Details = []string{err.Error()}
		return r
	}

	l2, l2ok, err := contract.ReadMetadataProjectID(c.fs, rigPath)
	if err != nil {
		r.Status = StatusWarning
		r.Severity = SeverityAdvisory
		r.Message = fmt.Sprintf("rig %q: cannot read metadata.json project_id", c.rig.Name)
		r.Details = []string{err.Error()}
		return r
	}

	switch {
	case !l1ok && !l2ok:
		r.Status = StatusOK
		r.Message = fmt.Sprintf("rig %q: no identity configured (legacy rig)", c.rig.Name)
		return r

	case l1ok && !l2ok:
		r.Status = StatusWarning
		r.Severity = SeverityAdvisory
		r.Message = fmt.Sprintf("rig %q: project identity has project.id but metadata.json project_id is absent — L2 not yet seeded", c.rig.Name)
		r.FixHint = "gc stop && gc start — city restart triggers automatic identity reconciliation"
		return r

	case !l1ok && l2ok:
		r.Status = StatusWarning
		r.Severity = SeverityAdvisory
		r.Message = fmt.Sprintf("rig %q: metadata.json has project_id but project identity is absent — run gc stop && gc start", c.rig.Name)
		r.FixHint = "gc stop && gc start"
		return r

	case l1 == l2:
		r.Status = StatusOK
		r.Message = fmt.Sprintf("rig %q: identity triad OK (%s)", c.rig.Name, l1)
		return r

	default:
		r.Status = StatusError
		// SeverityBlocking is the zero value — set explicitly for clarity.
		r.Severity = SeverityBlocking
		r.Message = fmt.Sprintf("rig %q: identity mismatch — project identity has %q, metadata.json has %q", c.rig.Name, l1, l2)
		r.Details = []string{
			"L1: " + contract.ProjectIdentityPath(rigPath),
			fmt.Sprintf("metadata.json: %s", filepath.Join(rigPath, ".beads", "metadata.json")),
		}
		r.FixHint = "gc stop && gc start — city restart triggers gc dolt-state ensure-project-id which reconciles all three identity layers"
		return r
	}
}

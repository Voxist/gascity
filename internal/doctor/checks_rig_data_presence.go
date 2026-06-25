package doctor

import (
	"fmt"
	"path/filepath"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/beads/contract"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/fsys"
)

// RigDataPresenceCheck verifies that a rig whose identity is configured has at
// least one row in its live bead store. An empty store with a configured
// identity is evidence of data loss (e.g. the va incident 2026-06-20 where
// 803 beads were missing). The check also catches the case where the local
// issues.jsonl export has more rows than the live store, which is a secondary
// signal of row deficit.
//
// Two degradation paths:
//   - No identity configured → StatusOK (legacy rig, skip silently).
//   - Store open fails → StatusWarning, SeverityAdvisory (Dolt may be offline).
type RigDataPresenceCheck struct {
	cityPath string
	rig      config.Rig
	newStore func(rigPath string) (beads.Store, error)
	fs       fsys.FS
}

// NewRigDataPresenceCheck creates a per-rig data-presence check.
func NewRigDataPresenceCheck(cityPath string, rig config.Rig, newStore func(string) (beads.Store, error)) *RigDataPresenceCheck {
	return &RigDataPresenceCheck{
		cityPath: cityPath,
		rig:      rig,
		newStore: newStore,
		fs:       fsys.OSFS{},
	}
}

// Name returns the check identifier ("rig:<name>:data-presence").
func (c *RigDataPresenceCheck) Name() string { return "rig:" + c.rig.Name + ":data-presence" }

// WarmupEligible returns false — this check needs the live Dolt store.
func (c *RigDataPresenceCheck) WarmupEligible() bool { return false }

// CanFix returns false — data restoration is operator policy.
func (c *RigDataPresenceCheck) CanFix() bool { return false }

// Fix is a no-op.
func (c *RigDataPresenceCheck) Fix(_ *CheckContext) error { return nil }

// Run executes the data-presence check.
func (c *RigDataPresenceCheck) Run(_ *CheckContext) *CheckResult {
	r := &CheckResult{Name: c.Name()}
	rigPath := c.rig.Path
	if !filepath.IsAbs(rigPath) {
		rigPath = filepath.Join(c.cityPath, rigPath)
	}

	_, ok, err := contract.ReadProjectIdentity(c.fs, rigPath)
	if err != nil {
		r.Status = StatusWarning
		r.Severity = SeverityAdvisory
		r.Message = fmt.Sprintf("cannot read identity for data-presence check: %v", err)
		return r
	}
	if !ok {
		// No identity configured — legacy rig; skip silently.
		r.Status = StatusOK
		r.Message = fmt.Sprintf("rig %q: no identity configured (legacy rig, skip)", c.rig.Name)
		return r
	}

	store, err := c.newStore(rigPath)
	if err != nil {
		r.Status = StatusWarning
		r.Severity = SeverityAdvisory
		r.Message = fmt.Sprintf("cannot open store for data-presence check: %v", err)
		r.FixHint = "run gc doctor again when Dolt is running"
		return r
	}

	rows, err := store.List(beads.ListQuery{AllowScan: true, IncludeClosed: true})
	if err != nil {
		r.Status = StatusWarning
		r.Severity = SeverityAdvisory
		r.Message = fmt.Sprintf("cannot list store rows for data-presence check: %v", err)
		r.FixHint = "run gc doctor again when Dolt is running"
		return r
	}

	if len(rows) == 0 {
		r.Status = StatusError
		r.Severity = SeverityBlocking
		r.Message = fmt.Sprintf("rig %s: live store is empty despite identity being configured", c.rig.Name)
		r.FixHint = dataPresenceFixHint()
		return r
	}

	r.Status = StatusOK
	r.Message = fmt.Sprintf("rig %s: data present (%d rows)", c.rig.Name, len(rows))
	return r
}

func dataPresenceFixHint() string {
	return "restore the rig from its last backup (`bd dolt pull` or `mol-dog restore`), " +
		"then run `gc stop && gc start` to re-stamp identity — " +
		"do NOT edit identity.toml or metadata.json to match an empty store"
}

package main

import (
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/doctor"
)

// Delivery phase constants (inlined from internal/delivery, which was reverted in #3334).
const (
	deliveryPhaseBuilding        = "building"
	deliveryPhaseCIPending       = "ci-pending"
	deliveryPhaseReviewPending   = "review-pending"
	deliveryPhaseRework          = "rework"
	deliveryPhaseDecisionPending = "decision-pending"
	deliveryPhaseMergePending    = "merge-pending"
	deliveryPhaseConflicted      = "conflicted"
	deliveryPhaseMerged          = "merged"
	deliveryPhaseAbandoned       = "abandoned"
)

// phaseBudgets maps each non-terminal delivery phase to its maximum dwell time.
var phaseBudgets = map[string]time.Duration{
	deliveryPhaseBuilding:        10 * time.Minute,
	deliveryPhaseCIPending:       30 * time.Minute,
	deliveryPhaseReviewPending:   60 * time.Minute,
	deliveryPhaseRework:          30 * time.Minute,
	deliveryPhaseDecisionPending: 20 * time.Minute,
	deliveryPhaseMergePending:    15 * time.Minute,
	deliveryPhaseConflicted:      60 * time.Minute,
}

// phaseEnteredAt returns when the bead entered its current phase.
// Reads gc.phase_entered_at (RFC3339); falls back to UpdatedAt then CreatedAt.
func phaseEnteredAt(b beads.Bead) time.Time {
	if raw, ok := b.Metadata[beadmeta.PhaseEnteredAtMetadataKey]; ok && raw != "" {
		if t, err := time.Parse(time.RFC3339, raw); err == nil {
			return t
		}
	}
	if !b.UpdatedAt.IsZero() {
		return b.UpdatedAt
	}
	return b.CreatedAt
}

type beadDeliveryRow struct {
	id        string
	phase     string
	age       time.Duration
	budget    time.Duration
	flag      string
	escalated string
}

type prDeliveryDoctorCheck struct {
	cityPath string
	newStore func(string) (beads.Store, error)
	rows     []beadDeliveryRow
}

// Name implements doctor.Check.
func (c *prDeliveryDoctorCheck) Name() string { return "pr-delivery" }

// CanFix implements doctor.Check.
func (c *prDeliveryDoctorCheck) CanFix() bool { return false }

// Fix implements doctor.Check.
func (c *prDeliveryDoctorCheck) Fix(_ *doctor.CheckContext) error { return nil }

// WarmupEligible implements doctor.Check.
func (c *prDeliveryDoctorCheck) WarmupEligible() bool { return false }

// Run implements doctor.Check.
func (c *prDeliveryDoctorCheck) Run(_ *doctor.CheckContext) *doctor.CheckResult {
	r := &doctor.CheckResult{Name: c.Name()}

	if c.newStore == nil {
		r.Status = doctor.StatusOK
		r.Message = "no delivery beads in flight"
		return r
	}

	store, err := c.newStore(c.cityPath)
	if err != nil {
		r.Status = doctor.StatusWarning
		r.Severity = doctor.SeverityAdvisory
		r.Message = fmt.Sprintf("pr-delivery check skipped: %v", err)
		return r
	}

	all, err := store.ListOpen()
	if err != nil {
		r.Status = doctor.StatusWarning
		r.Severity = doctor.SeverityAdvisory
		r.Message = fmt.Sprintf("pr-delivery check skipped: %v", err)
		return r
	}

	var inFlight []beads.Bead
	for _, b := range all {
		phase := strings.TrimSpace(b.Metadata[beadmeta.PhaseMetadataKey])
		if phase == "" || phase == deliveryPhaseMerged || phase == deliveryPhaseAbandoned {
			continue
		}
		inFlight = append(inFlight, b)
	}

	if len(inFlight) == 0 {
		r.Status = doctor.StatusOK
		r.Message = "no delivery beads in flight"
		return r
	}

	c.rows = nil
	var stuckCount, atRiskCount int
	var details []string
	for _, b := range inFlight {
		phase := b.Metadata[beadmeta.PhaseMetadataKey]
		age := time.Since(phaseEnteredAt(b))
		budget := phaseBudgets[phase]
		escalated := b.Metadata[beadmeta.WardenEscalatedMetadataKey]
		var flag string
		if budget > 0 {
			switch {
			case age > budget:
				stuckCount++
				flag = "STUCK"
			case age > budget*4/5:
				atRiskCount++
				flag = "at-risk"
			default:
				flag = "ok"
			}
		} else {
			flag = "unknown-budget"
		}
		c.rows = append(c.rows, beadDeliveryRow{
			id:        b.ID,
			phase:     phase,
			age:       age,
			budget:    budget,
			flag:      flag,
			escalated: escalated,
		})
		details = append(details, fmt.Sprintf("%s: %s %s/%s %s",
			b.ID, phase, formatDuration(age), formatDuration(budget), flag))
	}

	r.Details = details
	if stuckCount > 0 || atRiskCount > 0 {
		r.Status = doctor.StatusWarning
		r.Severity = doctor.SeverityAdvisory
	} else {
		r.Status = doctor.StatusOK
	}
	r.Message = fmt.Sprintf("%d delivery bead(s) in flight (%d stuck, %d at-risk)",
		len(inFlight), stuckCount, atRiskCount)
	return r
}

// RenderExtras implements doctor.Check.
func (c *prDeliveryDoctorCheck) RenderExtras(_ *doctor.CheckContext, w io.Writer) {
	if len(c.rows) == 0 {
		return
	}
	fmt.Fprintf(w, "%-12s %-16s %-9s %-9s %s\n", "ID", "PHASE", "AGE", "BUDGET", "STATUS") //nolint:errcheck
	for _, row := range c.rows {
		fmt.Fprintf(w, "%-12s %-16s %-9s %-9s %s\n", //nolint:errcheck
			row.id, row.phase, formatDuration(row.age), formatDuration(row.budget), row.flag)
	}
	fmt.Fprintln(w, "Escalated:") //nolint:errcheck
	seen := make(map[string]bool)
	hasEscalated := false
	for _, row := range c.rows {
		if row.escalated != "" && !seen[row.id] {
			seen[row.id] = true
			fmt.Fprintf(w, "  %s  %s  escalated=%s\n", row.id, row.phase, row.escalated) //nolint:errcheck
			hasEscalated = true
		}
	}
	if !hasEscalated {
		fmt.Fprintln(w, "  (none)") //nolint:errcheck
	}
}

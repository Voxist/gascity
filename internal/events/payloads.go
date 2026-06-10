package events

import "encoding/json"

// Domain payload types shared across packages. Payloads specific to one
// package live with their emitter (see internal/api/event_payloads.go and
// internal/extmsg/events.go); this file holds payload shapes that are
// used by multiple callers — today, the supervisor's Dolt maintenance
// loop and its CLI/API projections (beads ga-e3s, ga-zn8, ga-p5n).

// StoreMaintenanceDonePayload is the typed payload for
// gc.store.maintenance.done events. Emitted after a successful
// maintenance cycle (backup snapshot + CALL DOLT_GC + smoke test).
type StoreMaintenanceDonePayload struct {
	DurationSeconds float64 `json:"duration_s"`
	BeforeBytes     int64   `json:"before_bytes"`
	AfterBytes      int64   `json:"after_bytes"`
	SnapshotPath    string  `json:"snapshot_path"`
}

// IsEventPayload marks StoreMaintenanceDonePayload as an events.Payload variant.
func (StoreMaintenanceDonePayload) IsEventPayload() {}

// StoreMaintenanceFailedPayload is the typed payload for
// gc.store.maintenance.failed events. Emitted when a maintenance stage
// returns an error. Stage names the failing phase ("backup" | "gc" |
// "smoke-test" | "prune"); ErrorMsg carries the human-readable cause;
// SnapshotPath is populated when the backup stage completed before a
// later stage failed (so operators can recover from the snapshot).
type StoreMaintenanceFailedPayload struct {
	Stage           string  `json:"stage"`
	ErrorMsg        string  `json:"error_msg"`
	SnapshotPath    string  `json:"snapshot_path,omitempty"`
	DurationSeconds float64 `json:"duration_s"`
}

// IsEventPayload marks StoreMaintenanceFailedPayload as an events.Payload variant.
func (StoreMaintenanceFailedPayload) IsEventPayload() {}

// BeadWorktreeReapedPayload is the typed payload for bead.worktree.reaped
// events. Emitted when the worktree reaper successfully removes a merged
// worktree and its branch after a bead is closed.
type BeadWorktreeReapedPayload struct {
	BeadID string `json:"bead_id"`
	Path   string `json:"path"`
	Rig    string `json:"rig"`
	Branch string `json:"branch"`
}

// IsEventPayload marks BeadWorktreeReapedPayload as an events.Payload variant.
func (BeadWorktreeReapedPayload) IsEventPayload() {}

// BeadWorktreeReapSkippedPayload is the typed payload for
// bead.worktree.reap_skipped events. Emitted when the worktree reaper
// decides not to remove a worktree (e.g., unmerged changes, open bead).
type BeadWorktreeReapSkippedPayload struct {
	BeadID string `json:"bead_id"`
	Path   string `json:"path"`
	Rig    string `json:"rig"`
	Reason string `json:"reason"`
}

// IsEventPayload marks BeadWorktreeReapSkippedPayload as an events.Payload variant.
func (BeadWorktreeReapSkippedPayload) IsEventPayload() {}

func init() {
	RegisterPayload(BeadWorktreeReaped, BeadWorktreeReapedPayload{})
	RegisterPayload(BeadWorktreeReapSkipped, BeadWorktreeReapSkippedPayload{})
	RegisterPayload(OrderGateTimeoutFailOpen, OrderGateTimeoutFailOpenPayload{})
}

// OrderGateTimeoutFailOpenPayload is the typed payload for
// order.gate_timeout_fail_open events. The order dispatcher emits one per
// fail-open: an idempotent order whose bounded open-work gate timed out was
// dispatched anyway (single-flight unproven). Operators count these as the
// incident-12 tripwire — a rising rate means store contention is degrading
// gate evaluation and the flat-membership path (or the store) needs
// attention before silent fail-opens amplify.
type OrderGateTimeoutFailOpenPayload struct {
	// Order is the order's bare name without rig qualification.
	Order string `json:"order"`
	// Scope is the rig name the order is scoped to; empty for city-level
	// orders.
	Scope string `json:"scope,omitempty"`
	// ElapsedSeconds is how long the gate ran before the per-order bound
	// elapsed; 0 when the emitter could not measure it.
	ElapsedSeconds float64 `json:"elapsed_s"`
}

// IsEventPayload marks OrderGateTimeoutFailOpenPayload as an events.Payload variant.
func (OrderGateTimeoutFailOpenPayload) IsEventPayload() {}

// OrderGateTimeoutFailOpenPayloadJSON builds the JSON wire form for
// attachment to an Event.Payload field.
func OrderGateTimeoutFailOpenPayloadJSON(order, scope string, elapsedSeconds float64) json.RawMessage {
	b, _ := json.Marshal(OrderGateTimeoutFailOpenPayload{
		Order:          order,
		Scope:          scope,
		ElapsedSeconds: elapsedSeconds,
	})
	return b
}

// StoreDiskWarnPayload is the typed payload for gc.store.disk_warn events.
// Emitted before CALL DOLT_GC when free space is below GC_DOLT_WARN_FREE_BYTES
// but above GC_DOLT_MIN_FREE_BYTES; the GC proceeds.
type StoreDiskWarnPayload struct {
	FreeBytes  int64  `json:"free_bytes"`
	WarnBytes  int64  `json:"warn_bytes"`
	FloorBytes int64  `json:"floor_bytes"`
	DataDir    string `json:"data_dir"`
}

// IsEventPayload marks StoreDiskWarnPayload as an events.Payload variant.
func (StoreDiskWarnPayload) IsEventPayload() {}

// StoreDiskCriticalPayload is the typed payload for gc.store.disk_critical
// events. Emitted before CALL DOLT_GC when free space is below
// GC_DOLT_MIN_FREE_BYTES; the GC is skipped to avoid growing the store.
type StoreDiskCriticalPayload struct {
	FreeBytes  int64  `json:"free_bytes"`
	FloorBytes int64  `json:"floor_bytes"`
	DataDir    string `json:"data_dir"`
}

// IsEventPayload marks StoreDiskCriticalPayload as an events.Payload variant.
func (StoreDiskCriticalPayload) IsEventPayload() {}

// SessionResetStalledPayload is the typed payload for
// session.reset_stalled events. It identifies the session whose reset
// completion has stalled and the reset timestamp used to compute the
// elapsed diagnostic threshold.
type SessionResetStalledPayload struct {
	SessionName      string `json:"session_name"`
	Template         string `json:"template"`
	ResetCommittedAt string `json:"reset_committed_at"`
	ElapsedSeconds   int    `json:"elapsed_s"`
}

// IsEventPayload marks SessionResetStalledPayload as an events.Payload variant.
func (SessionResetStalledPayload) IsEventPayload() {}

// SessionResetStalledPayloadJSON builds the JSON wire form for attachment to
// an Event.Payload field.
func SessionResetStalledPayloadJSON(sessionName, template, resetCommittedAt string, elapsedSeconds int) json.RawMessage {
	b, _ := json.Marshal(SessionResetStalledPayload{
		SessionName:      sessionName,
		Template:         template,
		ResetCommittedAt: resetCommittedAt,
		ElapsedSeconds:   elapsedSeconds,
	})
	return b
}

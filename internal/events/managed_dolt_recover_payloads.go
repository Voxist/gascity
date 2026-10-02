package events

// ManagedDoltRecoverAdmitted and its typed payload (ga-zfgsh).
//
// runGuardedManagedDoltRecover (cmd/gc/dolt_recover_gate.go) is the one
// route that actually stops and restarts a managed Dolt server. It
// already logged every time it DECLINED to run; it logged nothing when
// it was ADMITTED -- the most consequential thing this subsystem does
// was also the least observable one. Forensics on a 2026-09-30 live
// replacement could not identify which of the two call sites
// (recoverManagedBDCommand, the bd-runner's transport-failure retry
// path, or healthBeadsProviderContext, the health-check path) triggered
// it.

// ManagedDoltRecoverAdmitted fires every time runGuardedManagedDoltRecover
// actually runs the provider's recover op, naming the caller site and the
// evidence the admission rested on.
const ManagedDoltRecoverAdmitted = "managed_dolt.recover_admitted"

// ManagedDoltRecoverAdmittedPayload is the typed payload for
// managed_dolt.recover_admitted events.
type ManagedDoltRecoverAdmittedPayload struct {
	CallerSite string `json:"caller_site" doc:"Which call site admitted the recover: recoverManagedBDCommand or healthBeadsProviderContext."`
	Scope      string `json:"scope" doc:"Canonical scope root path (the city path) the recover ran against."`
	Evidence   string `json:"evidence" doc:"Evidence classification the admission rested on: call-failed or health-op-answered."`
	// Liveness is empty when the liveness check did not run at all: a
	// health-op-answered admission skips it by design (op_health is the
	// only observer that can see a wedged-but-listening server), and so
	// does a city with no gc-managed dolt runtime.
	Liveness string `json:"liveness,omitempty" doc:"Liveness verdict at admission time, when the liveness check applied: alive, confirmed-dead, or unknown."`
	// HealthOpExitCode and HealthOpStderr are populated only when the
	// evidence came from an actual provider health-op run
	// (healthBeadsProviderContext). The bd-runner path
	// (recoverManagedBDCommand) has no health op at all -- it reacts to
	// a bd transport failure, not a provider script exit -- so both are
	// zero/empty there.
	HealthOpExitCode int    `json:"health_op_exit_code,omitempty" doc:"The triggering health op's process exit code, when evidence came from a provider health-op run."`
	HealthOpStderr   string `json:"health_op_stderr,omitempty" doc:"Truncated stderr from the triggering health op, when available."`
}

// IsEventPayload marks ManagedDoltRecoverAdmittedPayload as an
// events.Payload variant.
func (ManagedDoltRecoverAdmittedPayload) IsEventPayload() {}

func init() {
	RegisterPayload(ManagedDoltRecoverAdmitted, ManagedDoltRecoverAdmittedPayload{})
}

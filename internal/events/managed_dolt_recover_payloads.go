package events

// ManagedDoltRecoverDecision and its typed payload (ga-zfgsh).
//
// runGuardedManagedDoltRecover (cmd/gc/dolt_recover_gate.go) is the one
// route that actually stops and restarts a managed Dolt server. It used
// to log only when it DECLINED; an admission, the most consequential
// thing this subsystem does, left no trace of which caller triggered it.
// Forensics on a 2026-09-30 live replacement could not tell the two call
// sites (the bd-runner's transport-failure retry and the health-check
// path) apart. Every decision, and the result of an admitted run, is now
// one event of this type, distinguished by Outcome.

// ManagedDoltRecoverDecision fires for every managed-Dolt recover
// decision and for the result of every admitted recover.
const ManagedDoltRecoverDecision = "managed_dolt.recover_decision"

// ManagedDoltRecoverOutcome classifies one managed_dolt.recover_decision
// event.
type ManagedDoltRecoverOutcome string

// The outcomes of a managed-Dolt recover decision. Admitted is emitted
// before the provider op runs, so a process killed mid-recover still
// leaves its trace; exactly one of Succeeded, DeclinedByProvider or
// Failed follows it.
const (
	// ManagedDoltRecoverAdmitted: the guard let the recover op run.
	ManagedDoltRecoverAdmitted ManagedDoltRecoverOutcome = "admitted"
	// ManagedDoltRecoverDeclinedLiveness: the server was not confirmed dead.
	ManagedDoltRecoverDeclinedLiveness ManagedDoltRecoverOutcome = "declined-liveness"
	// ManagedDoltRecoverDeclinedCooldown: another recover was admitted for
	// the city inside the cooldown window.
	ManagedDoltRecoverDeclinedCooldown ManagedDoltRecoverOutcome = "declined-cooldown"
	// ManagedDoltRecoverDeclinedByProvider: the provider script lost the
	// recovery-lock race to a concurrent recover (exit 4).
	ManagedDoltRecoverDeclinedByProvider ManagedDoltRecoverOutcome = "declined-by-provider"
	// ManagedDoltRecoverSucceeded: the admitted recover op exited cleanly.
	ManagedDoltRecoverSucceeded ManagedDoltRecoverOutcome = "succeeded"
	// ManagedDoltRecoverFailed: the admitted recover op failed.
	ManagedDoltRecoverFailed ManagedDoltRecoverOutcome = "failed"
)

// ManagedDoltRecoverDecisionPayload is the typed payload for
// managed_dolt.recover_decision events.
type ManagedDoltRecoverDecisionPayload struct {
	Outcome    ManagedDoltRecoverOutcome `json:"outcome" enum:"admitted,declined-liveness,declined-cooldown,declined-by-provider,succeeded,failed" doc:"What the guard decided, or how an admitted recover ended."`
	CallerSite string                    `json:"caller_site" doc:"Which call site asked for the recover: recoverManagedBDCommand or healthBeadsProviderContext."`
	Scope      string                    `json:"scope" doc:"Normalized city path the recover was decided for."`
	Evidence   string                    `json:"evidence" doc:"Evidence classification the decision rested on: call-failed or health-op-answered."`
	// Liveness is empty when the liveness check did not run: a
	// health-op-answered request skips it by design, and so does a city
	// with no gc-managed dolt runtime.
	Liveness string `json:"liveness,omitempty" doc:"Liveness verdict, when the liveness check applied: alive, confirmed-dead, or unknown."`
	Reason   string `json:"reason,omitempty" doc:"Why the recover was declined."`
	// HealthOpRan is false for the bd-runner path, which reacts to a bd
	// transport failure and runs no provider health op.
	HealthOpRan bool `json:"health_op_ran" doc:"Whether a provider health op produced the triggering evidence."`
	// HealthOpExitCode is -1 when the op did not exit normally (killed by
	// a signal) or carried no exit code.
	HealthOpExitCode int    `json:"health_op_exit_code,omitempty" doc:"The triggering health op's exit code; -1 when it was killed by a signal."`
	HealthOpStderr   string `json:"health_op_stderr,omitempty" doc:"Redacted, truncated stderr of the triggering health op."`
	// RecoverExitCode and RecoverStderr describe the recover op itself and
	// are set for the declined-by-provider and failed outcomes.
	RecoverExitCode int    `json:"recover_exit_code,omitempty" doc:"The recover op's exit code; -1 when it did not exit normally."`
	RecoverStderr   string `json:"recover_stderr,omitempty" doc:"Redacted, truncated stderr of the recover op."`
}

// IsEventPayload marks ManagedDoltRecoverDecisionPayload as an
// events.Payload variant.
func (ManagedDoltRecoverDecisionPayload) IsEventPayload() {}

func init() {
	RegisterPayload(ManagedDoltRecoverDecision, ManagedDoltRecoverDecisionPayload{})
}

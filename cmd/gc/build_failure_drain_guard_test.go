package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/config"
)

// ga-c8rck extends the ga-8a8fq guard from provider resolution to EVERY
// per-agent desired-state build failure, and adds an attached-session guard to
// the generic orphan drain. The governing rule both halves serve: an erroring
// or partial build must never cause a drain, and a human-attached session must
// never be drained as orphaned.
//
// Every assertion below is on observable behavior — the drain tracker, the
// runtime, and the build's own stderr — never on the internals the fix
// introduces, so this file compiles and fails against the pre-fix tree.

// breakWorkDirTemplate makes resolveTemplate fail for an agent for a reason
// that has nothing to do with its provider: work_dir is expanded with
// ExpandTemplateStrict, which refuses an unknown placeholder. Provider
// resolution (validateAgentSessionTransportForBuild, the ga-8a8fq guard's only
// check) still succeeds, so this reaches the resolveTemplate call sites the
// ga-8a8fq guard does not cover.
func breakWorkDirTemplate(agent *config.Agent) {
	agent.WorkDir = "{{.NoSuchPlaceholder}}"
}

// TestScaleCheckEnvFailureDoesNotDrainItsSessions covers the scale_check
// env-build failure: controllerQueryRuntimeEnv errors, the pool is dropped from
// the evaluation set, and the agent contributes no desired sessions. That is a
// failure of the demand PROBE, not evidence of zero demand, so the agent's live
// sessions must be kept.
func TestScaleCheckEnvFailureDoesNotDrainItsSessions(t *testing.T) {
	t.Setenv("GC_BEADS", "bd")
	env := newUnresolvedProviderEnv(t, "true")
	brokenRig := env.cfg.Rigs[1].Path
	writeUnregisteredBackendMetadata(t, brokenRig)
	if err := os.WriteFile(filepath.Join(brokenRig, ".beads", "config.yaml"), []byte(`issue_prefix: rigb
gc.endpoint_origin: managed_city
gc.endpoint_status: verified
dolt.auto-start: false
`), 0o644); err != nil {
		t.Fatal(err)
	}

	// Prove the fixture still reaches the guarded branch, and only for the one
	// agent — otherwise this test silently stops exercising it.
	if _, err := controllerQueryRuntimeEnv(env.cityPath, env.cfg, &env.cfg.Agents[1]); err == nil {
		t.Fatal("fixture did not produce a scale_check env error; the guarded branch is no longer reachable from this test")
	}
	if _, err := controllerQueryRuntimeEnv(env.cityPath, env.cfg, &env.cfg.Agents[0]); err != nil {
		t.Fatalf("fixture broke the healthy agent's env too: %v", err)
	}

	cr, _, stderr := env.tick(t)

	if !strings.Contains(stderr, "scaleCheck: building env for "+unresolvedBrokenTemplate) {
		t.Fatalf("scale_check env failure not reported; stderr:\n%s", stderr)
	}
	if ds := cr.sessionDrains.get(env.broken.ID); ds != nil {
		t.Fatalf("session of an agent whose scale_check env could not be built is draining (reason %q): a failed demand probe is not zero demand; stderr:\n%s", ds.reason, stderr)
	}
	if !env.sp.IsRunning("s-rig-b-worker") {
		t.Fatalf("session of the agent whose scale_check env failed was stopped; stderr:\n%s", stderr)
	}
	if ds := cr.sessionDrains.get(env.good.ID); ds != nil {
		t.Fatalf("healthy rig session is draining (reason %q): the failure must stay with the failing agent; stderr:\n%s", ds.reason, stderr)
	}
}

// TestNamedBackingScaleCheckEnvFailureDoesNotDrainItsSessions is the
// named-session-backing twin of TestScaleCheckEnvFailureDoesNotDrainItsSessions.
// A pool that ALSO backs a configured named session reaches
// pendingPoolWithProbeEnv through the separate backsNamedSession call site in
// buildDesiredState (cmd/gc/build_desired_state.go), not the generic-pool call
// site the sibling test exercises. After #209 moved the
// controllerQueryRuntimeEnv call from the generic branch into
// pendingPoolWithProbeEnv, that one helper gained two producers in the same
// `for i := range cfg.Agents` loop; a guard folded into only one of them (or
// left as a standalone call-site check) silently drops this half's failures
// from UnresolvedTemplates and drains its sessions as orphaned — the exact
// ga-c8rck outage, reintroduced at the OTHER call site by a merge that never
// touches it.
func TestNamedBackingScaleCheckEnvFailureDoesNotDrainItsSessions(t *testing.T) {
	t.Setenv("GC_BEADS", "bd")
	env := newUnresolvedProviderEnv(t, "true")
	// Name deliberately differs from Template: TemplateQualifiedName() (what
	// the pool loop's backsNamedSession match uses) still resolves to
	// "rig-b/worker" and activates the pool-loop branch under test, but
	// QualifiedName() (what the SEPARATE named-session materialization pass
	// keys namedSpecs by) resolves to "rig-b/custom-alias" instead — an
	// identity the existing "s-rig-b-worker" bead does not match by identity,
	// session name, or alias. hasCanonical is therefore false for it, and with
	// mode="on_demand" and no ready assigned work, the named-session
	// materialization pass (build_desired_state.go's separate `for identity,
	// spec := range namedSpecs` loop) skips this identity outright — it never
	// calls resolveTemplatePrepared and so never reaches ITS OWN, unrelated
	// bp.recordBuildFailure call. Without this split, that other pass's guard
	// independently protects the same session for the same underlying
	// failure and masks whether the pool loop's own guard did anything at
	// all; this fixture isolates the one guard actually under test.
	env.cfg.NamedSessions = []config.NamedSession{
		{Name: "custom-alias", Template: "worker", Dir: "rig-b", Mode: "on_demand"},
	}
	brokenRig := env.cfg.Rigs[1].Path
	writeUnregisteredBackendMetadata(t, brokenRig)
	if err := os.WriteFile(filepath.Join(brokenRig, ".beads", "config.yaml"), []byte(`issue_prefix: rigb
gc.endpoint_origin: managed_city
gc.endpoint_status: verified
dolt.auto-start: false
`), 0o644); err != nil {
		t.Fatal(err)
	}

	// Prove the fixture still reaches the guarded branch, and only for the one
	// agent — otherwise this test silently stops exercising it.
	if _, err := controllerQueryRuntimeEnv(env.cityPath, env.cfg, &env.cfg.Agents[1]); err == nil {
		t.Fatal("fixture did not produce a scale_check env error; the guarded branch is no longer reachable from this test")
	}
	if _, err := controllerQueryRuntimeEnv(env.cityPath, env.cfg, &env.cfg.Agents[0]); err != nil {
		t.Fatalf("fixture broke the healthy agent's env too: %v", err)
	}

	cr, result, stderr := env.tick(t)

	if !strings.Contains(stderr, "scaleCheck: building env for "+unresolvedBrokenTemplate) {
		t.Fatalf("scale_check env failure not reported; stderr:\n%s", stderr)
	}
	// This is the assertion the generic-only guard cannot make: the
	// named-backing call site must record its OWN build failure, not rely on
	// the generic call site having already recorded one for the same template.
	if _, ok := result.UnresolvedTemplates[unresolvedBrokenTemplate]; !ok {
		t.Fatalf("UnresolvedTemplates = %v, want %q recorded: the named-backing pendingPoolWithProbeEnv call site must record its build failure too; stderr:\n%s", result.UnresolvedTemplates, unresolvedBrokenTemplate, stderr)
	}
	if ds := cr.sessionDrains.get(env.broken.ID); ds != nil {
		t.Fatalf("session of a named-backing pool whose scale_check env could not be built is draining (reason %q): a failed demand probe is not zero demand; stderr:\n%s", ds.reason, stderr)
	}
	if !env.sp.IsRunning("s-rig-b-worker") {
		t.Fatalf("session of the named-backing agent whose scale_check env failed was stopped; stderr:\n%s", stderr)
	}
	if ds := cr.sessionDrains.get(env.good.ID); ds != nil {
		t.Fatalf("healthy rig session is draining (reason %q): the failure must stay with the failing agent; stderr:\n%s", ds.reason, stderr)
	}
	if !env.sp.IsRunning("s-rig-a-worker") {
		t.Fatalf("healthy rig session was stopped; stderr:\n%s", stderr)
	}
}

// TestTemplateResolveFailureDoesNotDrainItsSessions covers a resolveTemplate
// failure that survives provider validation: the agent resolves its provider
// fine and is still dropped from the desired set, so the ga-8a8fq guard alone
// does not see it.
func TestTemplateResolveFailureDoesNotDrainItsSessions(t *testing.T) {
	env := newUnresolvedProviderEnv(t, "true")
	breakWorkDirTemplate(&env.cfg.Agents[1])

	cr, _, stderr := env.tick(t)

	// The provider must still resolve, or this is the ga-8a8fq case again.
	if _, err := config.ResolveProvider(&env.cfg.Agents[1], &env.cfg.Workspace, env.cfg.Providers, exec.LookPath); err != nil {
		t.Fatalf("fixture broke provider resolution (%v); this is the ga-8a8fq case, not the ga-c8rck one", err)
	}
	if ds := cr.sessionDrains.get(env.broken.ID); ds != nil {
		t.Fatalf("session of an agent whose template could not be resolved is draining (reason %q): a build failure is not a removal; stderr:\n%s", ds.reason, stderr)
	}
	if !env.sp.IsRunning("s-rig-b-worker") {
		t.Fatalf("session of the agent whose template failed to resolve was stopped; stderr:\n%s", stderr)
	}
	if ds := cr.sessionDrains.get(env.good.ID); ds != nil {
		t.Fatalf("healthy rig session is draining (reason %q); stderr:\n%s", ds.reason, stderr)
	}
}

// TestNamedSessionResolveFailureDoesNotDrainItsSessions is the named-session
// twin: a configured named session whose template resolution fails for a
// non-provider reason is skipped by the named loop, and its holder must be kept.
func TestNamedSessionResolveFailureDoesNotDrainItsSessions(t *testing.T) {
	env := newUnresolvedProviderEnv(t, "true")
	// Named-only: drop the pool signal so the named loop is the path that
	// resolves this agent's template.
	env.cfg.Agents[1].MaxActiveSessions = nil
	env.cfg.Agents[1].ScaleCheck = ""
	env.cfg.NamedSessions = []config.NamedSession{
		{Name: "worker", Template: "worker", Dir: "rig-b", Mode: "always"},
	}
	breakWorkDirTemplate(&env.cfg.Agents[1])

	cr, _, stderr := env.tick(t)

	if !strings.Contains(stderr, "buildDesiredState: named session ") {
		t.Fatalf("named-session resolve failure not reported; the fixture no longer reaches the named loop; stderr:\n%s", stderr)
	}
	if ds := cr.sessionDrains.get(env.broken.ID); ds != nil {
		t.Fatalf("holder of a named session whose template could not be resolved is draining (reason %q); stderr:\n%s", ds.reason, stderr)
	}
	if !env.sp.IsRunning("s-rig-b-worker") {
		t.Fatalf("holder of the named session whose template failed to resolve was stopped; stderr:\n%s", stderr)
	}
}

// TestAttachedSessionIsNotDrainedAsOrphaned is the attached half of ga-c8rck.
// The agent really is gone from config, so the build is correct and the guard
// above is not armed — only attachment stands between the operator's terminal
// and a Ctrl-C. Before the fix, only the config-drift path checked attachment.
func TestAttachedSessionIsNotDrainedAsOrphaned(t *testing.T) {
	env := newUnresolvedProviderEnv(t, "true")
	env.cfg.Agents = env.cfg.Agents[:1]
	env.sp.SetAttached("s-rig-b-worker", true)

	cr, result, stderr := env.tick(t)

	if len(result.UnresolvedTemplates) != 0 {
		t.Fatalf("build recorded %v as unbuildable; this test must exercise the ATTACHED guard on a genuinely removed agent, not the build-failure guard; stderr:\n%s", result.UnresolvedTemplates, stderr)
	}
	if ds := cr.sessionDrains.get(env.broken.ID); ds != nil {
		t.Fatalf("attached session of a removed agent is draining (reason %q): draining sends Ctrl-C into the operator's terminal; stderr:\n%s", ds.reason, stderr)
	}
	if !env.sp.IsRunning("s-rig-b-worker") {
		t.Fatalf("attached session was stopped; stderr:\n%s", stderr)
	}
}

// TestDetachedRemovedAgentSessionStillDrains is the control for the attached
// guard: the same removed agent, detached, still drains as orphaned. A guard
// that skips the drain unconditionally — or one that cannot tell attached from
// detached — fails here.
func TestDetachedRemovedAgentSessionStillDrains(t *testing.T) {
	env := newUnresolvedProviderEnv(t, "true")
	env.cfg.Agents = env.cfg.Agents[:1]

	cr, _, stderr := env.tick(t)

	ds := cr.sessionDrains.get(env.broken.ID)
	if ds == nil || ds.reason != "orphaned" {
		t.Fatalf("detached removed agent's session drain = %+v, want an orphaned drain; stderr:\n%s", ds, stderr)
	}
}

// TestSuspendedAttachedSessionStillDrains pins the scope of the attached guard:
// suspension is an explicit operator instruction about that agent, so its drain
// proceeds even while a terminal is attached. Only the undesired-by-absence
// verdict — about controller ownership, not about use — is deferred.
//
// The drain's `reason` label is deliberately not asserted. It is derived from
// configuredNames, which holds only named-session runtime names, so a suspended
// POOL agent has always been labeled "orphaned" here. That pre-existing
// labeling is exactly why the guard reads suspension directly instead of off
// the label, and pinning it would tie this test to a spelling it does not own.
func TestSuspendedAttachedSessionStillDrains(t *testing.T) {
	env := newUnresolvedProviderEnv(t, "true")
	env.cfg.Agents[1].Suspended = true
	env.sp.SetAttached("s-rig-b-worker", true)

	cr, _, stderr := env.tick(t)

	if ds := cr.sessionDrains.get(env.broken.ID); ds == nil {
		t.Fatalf("suspended agent's attached session is not draining: the attached guard must not defer an explicit suspension; stderr:\n%s", stderr)
	}
}

// TestSuspendedRigAttachedSessionStillDrains is the rig-level twin: suspending
// the rig is the same explicit instruction, and an attached session in it still
// drains.
func TestSuspendedRigAttachedSessionStillDrains(t *testing.T) {
	env := newUnresolvedProviderEnv(t, "true")
	env.cfg.Rigs[1].Suspended = true
	env.sp.SetAttached("s-rig-b-worker", true)

	cr, _, stderr := env.tick(t)

	if ds := cr.sessionDrains.get(env.broken.ID); ds == nil {
		t.Fatalf("attached session of an agent in a suspended rig is not draining; stderr:\n%s", stderr)
	}
}

// TestPoollessTemplateResolveFailureDoesNotDrainItsSessions reaches the same
// resolveTemplate failure through the OTHER build path. With no pool config the
// pool realizer never runs for this agent, so its live session is re-resolved
// by the session-bead overlay — the seam that keeps `gc session new` sessions
// and dependency floors in the desired set — and a failure there drops the
// session just as squarely.
func TestPoollessTemplateResolveFailureDoesNotDrainItsSessions(t *testing.T) {
	env := newUnresolvedProviderEnv(t, "true")
	env.cfg.Agents[1].MaxActiveSessions = nil
	env.cfg.Agents[1].ScaleCheck = ""
	breakWorkDirTemplate(&env.cfg.Agents[1])

	cr, _, stderr := env.tick(t)

	if !strings.Contains(stderr, "buildDesiredState: bead ") {
		t.Fatalf("session-bead overlay resolve failure not reported; the fixture no longer reaches the overlay; stderr:\n%s", stderr)
	}
	if ds := cr.sessionDrains.get(env.broken.ID); ds != nil {
		t.Fatalf("poolless session whose template could not be resolved is draining (reason %q); stderr:\n%s", ds.reason, stderr)
	}
	if !env.sp.IsRunning("s-rig-b-worker") {
		t.Fatalf("poolless session whose template failed to resolve was stopped; stderr:\n%s", stderr)
	}
}

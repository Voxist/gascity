package dispatch

import (
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
)

// The fixtures in this file materialize the graph shape every graph.v2 formula
// compiled BEFORE #5202 carries in the store: a scope body blocked by the
// scope-check that closes it, and a workflow root blocked by the
// workflow-finalize that closes it. #5202 stopped the compiler from minting
// those edges; nothing rewrote the beads already on disk. The strict-close
// double refuses the close exactly the way bd does, so without the repair the
// dispatcher takes the semantic-refusal path (#5199) and, after the budget,
// quarantines a control whose only fault is the edge it was born with.

func legacyScopeCheckFixture(t *testing.T, store beads.Store) (control, body beads.Bead) {
	t.Helper()
	body = mustCreateWorkflowBead(t, store, beads.Bead{
		Title: "body",
		Type:  "task",
		Metadata: map[string]string{
			"gc.kind":         "scope",
			"gc.scope_role":   "body",
			"gc.root_bead_id": "wf-legacy",
			"gc.step_ref":     "demo.body",
		},
	})
	subject := mustCreateWorkflowBead(t, store, beads.Bead{
		Title:  "implement",
		Type:   "task",
		Status: "closed",
		Metadata: map[string]string{
			"gc.root_bead_id": "wf-legacy",
			"gc.scope_ref":    "body",
			"gc.scope_role":   "member",
			"gc.outcome":      "pass",
		},
	})
	control = mustCreateWorkflowBead(t, store, beads.Bead{
		Title: "Finalize scope for implement",
		Type:  "task",
		Metadata: map[string]string{
			"gc.kind":         "scope-check",
			"gc.root_bead_id": "wf-legacy",
			"gc.scope_ref":    "body",
			"gc.scope_role":   "control",
		},
	})
	mustDepAdd(t, store, control.ID, subject.ID, "blocks")
	// The pre-#5202 rewriteGraphStepRefs edge: the body waits on its own
	// scope-check.
	mustDepAdd(t, store, body.ID, control.ID, "blocks")
	return mustGetBead(t, store, control.ID), body
}

func legacyWorkflowFinalizeFixture(t *testing.T, store beads.Store) (finalizer, root beads.Bead) {
	t.Helper()
	root = mustCreateWorkflowBead(t, store, beads.Bead{
		Title: "workflow",
		Type:  "task",
		Metadata: map[string]string{
			"gc.kind":             "workflow",
			"gc.formula_contract": "graph.v2",
		},
	})
	sink := mustCreateWorkflowBead(t, store, beads.Bead{
		Title:  "last step",
		Type:   "task",
		Status: "closed",
		Metadata: map[string]string{
			"gc.root_bead_id": root.ID,
			"gc.outcome":      "pass",
		},
	})
	finalizer = mustCreateWorkflowBead(t, store, beads.Bead{
		Title: "Finalize workflow",
		Type:  "task",
		Metadata: map[string]string{
			"gc.kind":         "workflow-finalize",
			"gc.root_bead_id": root.ID,
		},
	})
	mustDepAdd(t, store, finalizer.ID, sink.ID, "blocks")
	// The pre-#5202 addWorkflowRootDeps edge: "blocks", where the compiler
	// now emits "tracks".
	mustDepAdd(t, store, root.ID, finalizer.ID, "blocks")
	return mustGetBead(t, store, finalizer.ID), root
}

func mustDepTypes(t *testing.T, store beads.Store, id string) map[string]string {
	t.Helper()
	deps, err := store.DepList(id, "down")
	if err != nil {
		t.Fatalf("dep list %s: %v", id, err)
	}
	out := make(map[string]string, len(deps))
	for _, dep := range deps {
		out[dep.DependsOnID] = dep.Type
	}
	return out
}

func TestProcessScopeCheckRepairsLegacySelfClosingBodyEdge(t *testing.T) {
	t.Parallel()

	store := newStrictCloseStore()
	control, body := legacyScopeCheckFixture(t, store)
	setupCloses := len(store.closeOrder)

	result, err := ProcessControl(store, control, ProcessOptions{})
	if err != nil {
		t.Fatalf("ProcessControl(scope-check on legacy graph): %v (tier %v)", err, ClassifyControllerError(err))
	}
	if !result.Processed || result.Action != "scope-pass" {
		t.Fatalf("result = %+v, want processed scope-pass", result)
	}

	bodyAfter := mustGetBead(t, store, body.ID)
	if bodyAfter.Status != "closed" || bodyAfter.Metadata["gc.outcome"] != "pass" {
		t.Fatalf("body = status %q outcome %q, want closed/pass", bodyAfter.Status, bodyAfter.Metadata["gc.outcome"])
	}
	controlAfter := mustGetBead(t, store, control.ID)
	if controlAfter.Status != "closed" || controlAfter.Metadata["gc.outcome"] != "pass" {
		t.Fatalf("control = status %q outcome %q, want closed/pass", controlAfter.Status, controlAfter.Metadata["gc.outcome"])
	}
	if deps := mustDepTypes(t, store, body.ID); deps[control.ID] != "" {
		t.Fatalf("body still depends on its own scope-check: %v", deps)
	}
	// The repair must close the body BEFORE the control, exactly as the
	// non-legacy path does; a fix that closed the control first would have
	// sidestepped the refusal without removing the edge.
	if got, want := store.closeOrder[setupCloses:], []string{body.ID, control.ID}; strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("close order = %v, want %v", got, want)
	}
}

func TestProcessWorkflowFinalizeRepairsLegacySelfClosingRootEdge(t *testing.T) {
	t.Parallel()

	store := newStrictCloseStore()
	finalizer, root := legacyWorkflowFinalizeFixture(t, store)
	setupCloses := len(store.closeOrder)

	result, err := ProcessControl(store, finalizer, ProcessOptions{})
	if err != nil {
		t.Fatalf("ProcessControl(workflow-finalize on legacy graph): %v (tier %v)", err, ClassifyControllerError(err))
	}
	if !result.Processed || result.Action != "workflow-pass" {
		t.Fatalf("result = %+v, want processed workflow-pass", result)
	}

	rootAfter := mustGetBead(t, store, root.ID)
	if rootAfter.Status != "closed" || rootAfter.Metadata["gc.outcome"] != "pass" {
		t.Fatalf("root = status %q outcome %q, want closed/pass", rootAfter.Status, rootAfter.Metadata["gc.outcome"])
	}
	finalizerAfter := mustGetBead(t, store, finalizer.ID)
	if finalizerAfter.Status != "closed" || finalizerAfter.Metadata["gc.outcome"] != "pass" {
		t.Fatalf("finalizer = status %q outcome %q, want closed/pass", finalizerAfter.Status, finalizerAfter.Metadata["gc.outcome"])
	}
	if deps := mustDepTypes(t, store, root.ID); deps[finalizer.ID] != "" {
		t.Fatalf("root still depends on its own finalizer: %v", deps)
	}
	if got, want := store.closeOrder[setupCloses:], []string{root.ID, finalizer.ID}; strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("close order = %v, want %v", got, want)
	}
}

// TestProcessScopeCheckAbortRepairsLegacySelfClosingBodyEdge covers the fail
// branch: abortScope closes the same body through the same edge, so a
// failed scope on a legacy graph would burn the semantic budget just as a
// passing one would.
func TestProcessScopeCheckAbortRepairsLegacySelfClosingBodyEdge(t *testing.T) {
	t.Parallel()

	store := newStrictCloseStore()
	control, body := legacyScopeCheckFixture(t, store)
	deps := mustDepTypes(t, store, control.ID)
	if len(deps) != 1 {
		t.Fatalf("control deps = %v, want exactly the subject", deps)
	}
	for subjectID := range deps {
		if err := store.SetMetadata(subjectID, "gc.outcome", "fail"); err != nil {
			t.Fatalf("mark subject failed: %v", err)
		}
	}

	result, err := ProcessControl(store, control, ProcessOptions{})
	if err != nil {
		t.Fatalf("ProcessControl(scope-check fail on legacy graph): %v (tier %v)", err, ClassifyControllerError(err))
	}
	if !result.Processed || result.Action != "scope-fail" {
		t.Fatalf("result = %+v, want processed scope-fail", result)
	}
	bodyAfter := mustGetBead(t, store, body.ID)
	if bodyAfter.Status != "closed" || bodyAfter.Metadata["gc.outcome"] != "fail" {
		t.Fatalf("body = status %q outcome %q, want closed/fail", bodyAfter.Status, bodyAfter.Metadata["gc.outcome"])
	}
	if deps := mustDepTypes(t, store, body.ID); deps[control.ID] != "" {
		t.Fatalf("body still depends on its own scope-check: %v", deps)
	}
}

// TestLegacyCycleRepairLeavesForeignBlockersToTheSemanticTier is the control
// test for the predicate: when the subject is ALSO blocked by an open bead
// that is not the closing control, the repair must not touch any edge — not
// even the legacy one — and the refusal must reach the caller unchanged so
// #5199's bounded retry and quarantine still own it.
func TestLegacyCycleRepairLeavesForeignBlockersToTheSemanticTier(t *testing.T) {
	t.Parallel()

	t.Run("scope body", func(t *testing.T) {
		t.Parallel()

		store := newStrictCloseStore()
		control, body := legacyScopeCheckFixture(t, store)
		foreign := mustCreateWorkflowBead(t, store, beads.Bead{Title: "unrelated open work", Type: "task"})
		mustDepAdd(t, store, body.ID, foreign.ID, "blocks")

		_, err := ProcessControl(store, control, ProcessOptions{})
		if err == nil {
			t.Fatal("ProcessControl succeeded; the foreign blocker should have refused the body close")
		}
		if got := ClassifyControllerError(err); got != TierSemantic {
			t.Fatalf("tier = %v, want TierSemantic for %v", got, err)
		}
		deps := mustDepTypes(t, store, body.ID)
		if deps[control.ID] != "blocks" || deps[foreign.ID] != "blocks" {
			t.Fatalf("body deps = %v, want both blockers untouched", deps)
		}
		if bodyAfter := mustGetBead(t, store, body.ID); bodyAfter.Status == "closed" {
			t.Fatal("body closed despite an open foreign blocker")
		}
		if controlAfter := mustGetBead(t, store, control.ID); controlAfter.Status == "closed" {
			t.Fatal("control closed despite its body close being refused")
		}
	})

	t.Run("workflow root", func(t *testing.T) {
		t.Parallel()

		store := newStrictCloseStore()
		finalizer, root := legacyWorkflowFinalizeFixture(t, store)
		foreign := mustCreateWorkflowBead(t, store, beads.Bead{Title: "unrelated open work", Type: "task"})
		mustDepAdd(t, store, root.ID, foreign.ID, "blocks")

		_, err := ProcessControl(store, finalizer, ProcessOptions{})
		if err == nil {
			t.Fatal("ProcessControl succeeded; the foreign blocker should have refused the root close")
		}
		if got := ClassifyControllerError(err); got != TierSemantic {
			t.Fatalf("tier = %v, want TierSemantic for %v", got, err)
		}
		deps := mustDepTypes(t, store, root.ID)
		if deps[finalizer.ID] != "blocks" || deps[foreign.ID] != "blocks" {
			t.Fatalf("root deps = %v, want both blockers untouched", deps)
		}
		if rootAfter := mustGetBead(t, store, root.ID); rootAfter.Status == "closed" {
			t.Fatal("root closed despite an open foreign blocker")
		}
	})
}

// TestLegacyCycleRepairIgnoresControlsOfOtherWorkflows pins the "same
// workflow" half of the predicate: a scope-check from a different root that
// happens to share the scope_ref is a foreign blocker, not a legacy edge.
func TestLegacyCycleRepairIgnoresControlsOfOtherWorkflows(t *testing.T) {
	t.Parallel()

	store := newStrictCloseStore()
	control, body := legacyScopeCheckFixture(t, store)
	otherControl := mustCreateWorkflowBead(t, store, beads.Bead{
		Title: "Finalize scope for implement (other workflow)",
		Type:  "task",
		Metadata: map[string]string{
			"gc.kind":         "scope-check",
			"gc.root_bead_id": "wf-other",
			"gc.scope_ref":    "body",
			"gc.scope_role":   "control",
		},
	})
	mustDepAdd(t, store, body.ID, otherControl.ID, "blocks")

	_, err := ProcessControl(store, control, ProcessOptions{})
	if err == nil {
		t.Fatal("ProcessControl succeeded; a control of another workflow must not be repaired away")
	}
	if got := ClassifyControllerError(err); got != TierSemantic {
		t.Fatalf("tier = %v, want TierSemantic for %v", got, err)
	}
	deps := mustDepTypes(t, store, body.ID)
	if deps[control.ID] != "blocks" || deps[otherControl.ID] != "blocks" {
		t.Fatalf("body deps = %v, want both blockers untouched", deps)
	}
}

// TestLegacyCycleRepairIsANoOpOnPostFixGraphs: a graph compiled after #5202
// carries no self-closing edge, so the ordinary path must not consult or
// touch the dep list at all beyond what it already did.
func TestLegacyCycleRepairIsANoOpOnPostFixGraphs(t *testing.T) {
	t.Parallel()

	store := newStrictCloseStore()
	finalizer, root := legacyWorkflowFinalizeFixture(t, store)
	if err := store.DepRemove(root.ID, finalizer.ID); err != nil {
		t.Fatalf("drop legacy edge: %v", err)
	}
	mustDepAdd(t, store, root.ID, finalizer.ID, "tracks")

	result, err := ProcessControl(store, finalizer, ProcessOptions{})
	if err != nil {
		t.Fatalf("ProcessControl(workflow-finalize): %v", err)
	}
	if result.Action != "workflow-pass" {
		t.Fatalf("result = %+v, want workflow-pass", result)
	}
	if deps := mustDepTypes(t, store, root.ID); deps[finalizer.ID] != "tracks" {
		t.Fatalf("root deps = %v, want the tracks edge preserved", deps)
	}
}

// TestReconcileTerminalScopedMemberRepairsLegacyEdgeOntoPreservedControl covers
// the second arm of isLegacySelfClosingBlocker, where the blocker is NOT the
// closer. A failed member reconciled on close aborts its scope itself while
// preserveScopeCheckForSubject keeps the subject's scope-check open as the
// replay path — so on a legacy graph the body is closed by the member while
// still blocked by an open control that is not doing the closing. The control
// belongs to the same workflow and declares this body as its target, so the
// edge is repaired; the preserved control must stay open.
func TestReconcileTerminalScopedMemberRepairsLegacyEdgeOntoPreservedControl(t *testing.T) {
	t.Parallel()

	store := newStrictCloseStore()
	control, body := legacyScopeCheckFixture(t, store)
	var subjectID string
	for id := range mustDepTypes(t, store, control.ID) {
		subjectID = id
	}
	if err := store.SetMetadata(subjectID, "gc.outcome", "fail"); err != nil {
		t.Fatalf("mark subject failed: %v", err)
	}

	result, err := reconcileTerminalScopedMember(store, mustGetBead(t, store, subjectID))
	if err != nil {
		t.Fatalf("reconcileTerminalScopedMember(fail on legacy graph): %v (tier %v)", err, ClassifyControllerError(err))
	}
	if result.Action != "scope-fail" {
		t.Fatalf("result = %+v, want scope-fail", result)
	}
	bodyAfter := mustGetBead(t, store, body.ID)
	if bodyAfter.Status != "closed" || bodyAfter.Metadata["gc.outcome"] != "fail" {
		t.Fatalf("body = status %q outcome %q, want closed/fail", bodyAfter.Status, bodyAfter.Metadata["gc.outcome"])
	}
	if deps := mustDepTypes(t, store, body.ID); deps[control.ID] != "" {
		t.Fatalf("body still depends on the preserved scope-check: %v", deps)
	}
	if controlAfter := mustGetBead(t, store, control.ID); controlAfter.Status != "open" {
		t.Fatalf("preserved control status = %q, want open — it is the replay path", controlAfter.Status)
	}
}

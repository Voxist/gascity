package delivery_test

import (
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/delivery"
)

func newBeadWithPhase(store beads.Store, phase string) beads.Bead {
	b, err := store.Create(beads.Bead{Title: "test bead"})
	if err != nil {
		panic(err)
	}
	if phase != "" {
		if err := store.SetMetadata(b.ID, delivery.MetaKeyPhase, phase); err != nil {
			panic(err)
		}
		b, err = store.Get(b.ID)
		if err != nil {
			panic(err)
		}
	}
	return b
}

func TestSetPhase_IdempotentSamePhase(t *testing.T) {
	store := beads.NewMemStore()
	b := newBeadWithPhase(store, delivery.PhaseBuilding)

	// First call: building → ci-pending.
	if err := delivery.SetPhase(store, b.ID, delivery.PhaseCIPending); err != nil {
		t.Fatalf("first SetPhase: %v", err)
	}
	b1, _ := store.Get(b.ID)
	hist1 := b1.Metadata[delivery.MetaKeyPhaseHistory]

	// Second call with the same phase: should be a no-op.
	if err := delivery.SetPhase(store, b.ID, delivery.PhaseCIPending); err != nil {
		t.Fatalf("second SetPhase (idempotent): %v", err)
	}
	b2, _ := store.Get(b.ID)
	hist2 := b2.Metadata[delivery.MetaKeyPhaseHistory]

	if b2.Metadata[delivery.MetaKeyPhase] != delivery.PhaseCIPending {
		t.Errorf("phase: got %q, want %q", b2.Metadata[delivery.MetaKeyPhase], delivery.PhaseCIPending)
	}
	// History must not have grown — idempotent call must not append.
	if hist2 != hist1 {
		t.Errorf("history changed on idempotent call: before=%q after=%q", hist1, hist2)
	}
}

func TestSetPhase_LogsHistory(t *testing.T) {
	store := beads.NewMemStore()
	b := newBeadWithPhase(store, "")

	transitions := []struct {
		to          string
		wantPhase   string
		wantHistory string
	}{
		{delivery.PhaseBuilding, delivery.PhaseBuilding, "→building"},
		{delivery.PhaseCIPending, delivery.PhaseCIPending, "→building,building→ci-pending"},
		{delivery.PhaseReviewPending, delivery.PhaseReviewPending, "→building,building→ci-pending,ci-pending→review-pending"},
	}

	for _, tt := range transitions {
		if err := delivery.SetPhase(store, b.ID, tt.to); err != nil {
			t.Fatalf("SetPhase(%q): %v", tt.to, err)
		}
		got, _ := store.Get(b.ID)
		if got.Metadata[delivery.MetaKeyPhase] != tt.wantPhase {
			t.Errorf("after →%s: phase got %q want %q", tt.to, got.Metadata[delivery.MetaKeyPhase], tt.wantPhase)
		}
		if got.Metadata[delivery.MetaKeyPhaseHistory] != tt.wantHistory {
			t.Errorf("after →%s: history got %q want %q", tt.to, got.Metadata[delivery.MetaKeyPhaseHistory], tt.wantHistory)
		}
	}
}

func TestSetPhase_IllegalTransition(t *testing.T) {
	store := beads.NewMemStore()
	b := newBeadWithPhase(store, delivery.PhaseMerged)

	err := delivery.SetPhase(store, b.ID, delivery.PhaseBuilding)
	if err == nil {
		t.Fatal("SetPhase(merged→building): expected error, got nil")
	}
}

func TestSetPhase_BeadNotFound(t *testing.T) {
	store := beads.NewMemStore()
	err := delivery.SetPhase(store, "nonexistent-id", delivery.PhaseBuilding)
	if err == nil {
		t.Fatal("SetPhase with nonexistent ID: expected error, got nil")
	}
}

func TestSetPhaseAt_StampsEnteredAtAndClearsRetries(t *testing.T) {
	store := beads.NewMemStore()
	b := newBeadWithPhase(store, delivery.PhaseBuilding)

	// Pre-populate stale metadata to confirm SetPhaseAt overwrites them.
	if err := store.SetMetadataBatch(b.ID, map[string]string{
		delivery.MetaKeyPhaseEnteredAt: "2020-01-01T00:00:00Z",
		delivery.MetaKeyWardenRetries:  "3",
	}); err != nil {
		t.Fatalf("pre-populate metadata: %v", err)
	}

	fixedTime := time.Date(2026, 6, 11, 10, 0, 0, 0, time.UTC)
	if err := delivery.SetPhaseAt(store, b.ID, delivery.PhaseCIPending, fixedTime); err != nil {
		t.Fatalf("SetPhaseAt: %v", err)
	}

	got, err := store.Get(b.ID)
	if err != nil {
		t.Fatalf("store.Get: %v", err)
	}
	if got.Metadata[delivery.MetaKeyPhaseEnteredAt] != "2026-06-11T10:00:00Z" {
		t.Errorf("phase_entered_at: got %q, want %q", got.Metadata[delivery.MetaKeyPhaseEnteredAt], "2026-06-11T10:00:00Z")
	}
	if got.Metadata[delivery.MetaKeyWardenRetries] != "" {
		t.Errorf("warden_retries: got %q, want empty (reset)", got.Metadata[delivery.MetaKeyWardenRetries])
	}
}

func TestSetPhaseAt_IdempotentDoesNotUpdateTimestamp(t *testing.T) {
	store := beads.NewMemStore()
	b := newBeadWithPhase(store, delivery.PhaseBuilding)

	firstTime := time.Date(2026, 6, 11, 9, 0, 0, 0, time.UTC)
	if err := delivery.SetPhaseAt(store, b.ID, delivery.PhaseCIPending, firstTime); err != nil {
		t.Fatalf("first SetPhaseAt: %v", err)
	}

	// Idempotent call with a different time — must not update the timestamp.
	laterTime := time.Date(2026, 6, 11, 10, 0, 0, 0, time.UTC)
	if err := delivery.SetPhaseAt(store, b.ID, delivery.PhaseCIPending, laterTime); err != nil {
		t.Fatalf("idempotent SetPhaseAt: %v", err)
	}

	got, _ := store.Get(b.ID)
	if got.Metadata[delivery.MetaKeyPhaseEnteredAt] != "2026-06-11T09:00:00Z" {
		t.Errorf("idempotent call updated phase_entered_at: got %q, want %q",
			got.Metadata[delivery.MetaKeyPhaseEnteredAt], "2026-06-11T09:00:00Z")
	}
}

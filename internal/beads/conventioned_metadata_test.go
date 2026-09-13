package beads

import (
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/beadmeta"
)

// The store-layer conventioned-key guards (vp-gbxl9 / ADR-0050 P1): the
// invalid state must be unrepresentable at every write path, while the two
// legal operations — set non-empty, and absence — keep working. Exercised on
// MemStore: the guards are pasted at the top of every implementation's write
// methods and delegate to the same beadmeta validator, so one in-process
// store pins the behavior without exec'ing bd.

func TestMemStoreSetMetadataRejectsEmptyRoute(t *testing.T) {
	s := NewMemStore()
	b, err := s.Create(Bead{Title: "route probe"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	err = s.SetMetadata(b.ID, beadmeta.RoutedToMetadataKey, "")
	if err == nil {
		t.Fatal("SetMetadata(gc.routed_to, \"\") = nil, want the ADR-0050 rejection")
	}
	if !strings.Contains(err.Error(), "unset-metadata") {
		t.Fatalf("rejection = %q, want it to steer to --unset-metadata", err)
	}
	got, err := s.Get(b.ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if _, present := got.Metadata[beadmeta.RoutedToMetadataKey]; present {
		t.Fatalf("rejected write still persisted: %q", got.Metadata[beadmeta.RoutedToMetadataKey])
	}
}

func TestMemStoreSetMetadataRejectsNonCanonicalAwaitingHuman(t *testing.T) {
	s := NewMemStore()
	b, err := s.Create(Bead{Title: "park probe"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	for _, value := range []string{"1", "false", "karel@voxist.com", "needs a human"} {
		if err := s.SetMetadata(b.ID, beadmeta.AwaitingHumanMetadataKey, value); err == nil {
			t.Fatalf("SetMetadata(gc.awaiting_human, %q) = nil, want rejection", value)
		}
	}
	if err := s.SetMetadata(b.ID, beadmeta.AwaitingHumanMetadataKey, "true"); err != nil {
		t.Fatalf("SetMetadata(gc.awaiting_human, \"true\") = %v, want nil", err)
	}
}

func TestMemStoreSetMetadataLegalOperationsStillWork(t *testing.T) {
	s := NewMemStore()
	b, err := s.Create(Bead{Title: "legal ops probe"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if err := s.SetMetadata(b.ID, beadmeta.RoutedToMetadataKey, "voxist.platform-architect"); err != nil {
		t.Fatalf("set non-empty route: %v", err)
	}
	if err := s.SetMetadata(b.ID, "gc.human_ask", ""); err != nil {
		t.Fatalf("unconventioned empty value must stay legal: %v", err)
	}
	got, err := s.Get(b.ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.Metadata[beadmeta.RoutedToMetadataKey] != "voxist.platform-architect" {
		t.Fatalf("route = %q, want the non-empty write persisted", got.Metadata[beadmeta.RoutedToMetadataKey])
	}
	if got.Metadata["gc.human_ask"] != "" {
		t.Fatalf("open-world key = %q, want \"\" persisted", got.Metadata["gc.human_ask"])
	}
}

func TestMemStoreSetMetadataBatchRejectsAtomically(t *testing.T) {
	s := NewMemStore()
	b, err := s.Create(Bead{Title: "batch probe"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	err = s.SetMetadataBatch(b.ID, map[string]string{
		"gc.human_owner":             "karel@voxist.com",
		beadmeta.RoutedToMetadataKey: "",
	})
	if err == nil {
		t.Fatal("SetMetadataBatch with an empty route = nil, want rejection")
	}
	got, err := s.Get(b.ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if _, present := got.Metadata["gc.human_owner"]; present {
		t.Fatal("rejected batch partially persisted: the legal pair landed")
	}
}

func TestMemStoreCompareAndSetRejectsInvalidNext(t *testing.T) {
	s := NewMemStore()
	b, err := s.Create(Bead{Title: "cas probe"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	ok, err := s.CompareAndSetMetadataKey(b.ID, beadmeta.RoutedToMetadataKey, "", "")
	if err == nil {
		t.Fatal("CompareAndSetMetadataKey to empty route = nil error, want rejection")
	}
	if ok {
		t.Fatal("CompareAndSetMetadataKey to empty route reported success")
	}
}

func TestMemStoreUpdateOptsRejectsInvalidRoute(t *testing.T) {
	s := NewMemStore()
	b, err := s.Create(Bead{Title: "update probe"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	err = s.Update(b.ID, UpdateOpts{Metadata: map[string]string{beadmeta.RoutedToMetadataKey: ""}})
	if err == nil {
		t.Fatal("Update with an empty route = nil, want rejection")
	}
}

func TestMemStoreCreateRejectsInvalidRoute(t *testing.T) {
	s := NewMemStore()
	_, err := s.Create(Bead{
		Title:    "create probe",
		Metadata: map[string]string{beadmeta.RoutedToMetadataKey: ""},
	})
	if err == nil {
		t.Fatal("Create with an empty route = nil, want rejection")
	}
}

package beads

import "testing"

// The storage class a caller passes to CreateWithStorage must reach the created
// bead as the Ephemeral/NoHistory fields the storage layer routes on. This runs
// the REAL method body against the native store's in-memory storage fixture, so
// it is always-on: no scratch Dolt server, no build tag. The live probe in
// native_dolt_storage_class_live_test.go measures the resulting Dolt commit
// cost; this one pins the routing itself.
func TestNativeDoltStoreCreateWithStorageStampsClass(t *testing.T) {
	cases := []struct {
		name          string
		in            Bead
		storage       StorageClass
		wantEphemeral bool
		wantNoHistory bool
	}{
		{
			// The session policy's class (vp-ia76): no_history, never ephemeral.
			name:          "no history",
			in:            Bead{Title: "native storage class session", Type: "session"},
			storage:       StorageNoHistory,
			wantNoHistory: true,
		},
		{
			// The wisp policy's class under bd-105 ready semantics.
			name:          "ephemeral",
			in:            Bead{Title: "native storage class wisp", Type: "wisp"},
			storage:       StorageEphemeral,
			wantEphemeral: true,
		},
		{
			// An explicit history class must CLEAR a class the caller stamped by
			// hand: the policy decides the tier, not the incoming bead.
			name:    "history overrides incoming fields",
			in:      Bead{Title: "native storage class history", NoHistory: true},
			storage: StorageHistory,
		},
		{
			// StorageDefault is "no opinion": the bead's own fields survive.
			name:          "default preserves incoming fields",
			in:            Bead{Title: "native storage class default", NoHistory: true},
			storage:       StorageDefault,
			wantNoHistory: true,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			store := newNativeDoltStoreForTest(newNativeDoltMemStorage())

			created, err := store.CreateWithStorage(tc.in, tc.storage)
			if err != nil {
				t.Fatalf("CreateWithStorage(%q): %v", tc.storage, err)
			}
			if created.Ephemeral != tc.wantEphemeral {
				t.Errorf("created.Ephemeral = %v, want %v: storage class %q did not reach "+
					"the created bead, so the storage layer routes it to the wrong tier",
					created.Ephemeral, tc.wantEphemeral, tc.storage)
			}
			if created.NoHistory != tc.wantNoHistory {
				t.Errorf("created.NoHistory = %v, want %v: storage class %q did not reach "+
					"the created bead, so the storage layer routes it to the wrong tier",
					created.NoHistory, tc.wantNoHistory, tc.storage)
			}

			// The class must be PERSISTED, not just echoed back by the create.
			got, err := store.Get(created.ID)
			if err != nil {
				t.Fatalf("Get: %v", err)
			}
			if got.Ephemeral != tc.wantEphemeral {
				t.Errorf("persisted Ephemeral = %v, want %v", got.Ephemeral, tc.wantEphemeral)
			}
			if got.NoHistory != tc.wantNoHistory {
				t.Errorf("persisted NoHistory = %v, want %v", got.NoHistory, tc.wantNoHistory)
			}
		})
	}
}

// An unknown class must be refused, not coerced into the quiet default
// (ADR-0043): a typo'd class that silently created a committed bead is exactly
// the failure this whole change exists to end. No bead may be persisted.
func TestNativeDoltStoreCreateWithStorageRejectsUnknownClass(t *testing.T) {
	storage := newNativeDoltMemStorage()
	store := newNativeDoltStoreForTest(storage)

	if _, err := store.CreateWithStorage(Bead{Title: "bogus class"}, StorageClass("nonsense")); err == nil {
		t.Fatal("CreateWithStorage with an unknown storage class returned no error")
	}
	beads, err := storage.store.List(ListQuery{AllowScan: true, IncludeClosed: true, TierMode: TierBoth})
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(beads) != 0 {
		t.Fatalf("unknown storage class persisted %d bead(s), want 0", len(beads))
	}
}

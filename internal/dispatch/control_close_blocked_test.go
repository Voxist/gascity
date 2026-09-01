package dispatch

import (
	"errors"
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
)

// blockedCloseRefusingStore models bd 0055-era: a status-close of a BLOCKED
// issue is refused on the Update path, while Close() carries the sanctioned
// force escape for that shape.
type blockedCloseRefusingStore struct {
	beads.Store
	metadataUpdates int
	closeCalls      int
}

func (s *blockedCloseRefusingStore) Update(id string, opts beads.UpdateOpts) error {
	if opts.Status != nil {
		return errors.New("issue is blocked: refusing status close on the update path")
	}
	s.metadataUpdates++
	return s.Store.Update(id, opts)
}

func (s *blockedCloseRefusingStore) Close(id string) error {
	s.closeCalls++
	return s.Store.Close(id)
}

// TestCloseBeadWithMetadataFallsBackWhenStatusCloseRefused pins the fallback
// the 2026-08-31 resync dropped. Control beads legitimately close their subject
// while still formally blocking it, so a store that refuses the combined
// status+metadata update must not leave the bead open — the metadata lands via
// a metadata-only update and the close via Close(). Without the fallback the
// control bead never closes and dispatch stalls.
func TestCloseBeadWithMetadataFallsBackWhenStatusCloseRefused(t *testing.T) {
	t.Parallel()

	mem := beads.NewMemStore()
	created, err := mem.Create(beads.Bead{Title: "control subject", Type: "task", Status: "open"})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	store := &blockedCloseRefusingStore{Store: mem}

	if err := closeBeadWithMetadata(store, created.ID, map[string]string{"gc.outcome": "pass"}); err != nil {
		t.Fatalf("closeBeadWithMetadata returned %v; want the refusal to fall back to metadata-update + Close", err)
	}
	if store.metadataUpdates == 0 {
		t.Error("metadata was never applied via the metadata-only fallback")
	}
	if store.closeCalls == 0 {
		t.Error("Close() was never called; the sanctioned force escape was not used")
	}

	got, err := mem.Get(created.ID)
	if err != nil {
		t.Fatalf("get after close: %v", err)
	}
	if got.Status != "closed" {
		t.Fatalf("bead status = %q, want closed; a refused status-close left the control bead open", got.Status)
	}
	if got.Metadata["gc.outcome"] != "pass" {
		t.Fatalf("metadata gc.outcome = %q, want pass; the fallback dropped the metadata", got.Metadata["gc.outcome"])
	}
}

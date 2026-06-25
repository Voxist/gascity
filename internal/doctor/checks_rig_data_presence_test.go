package doctor

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/beads/contract"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/fsys"
)

// --- RigDataPresenceCheck ---

func TestRigDataPresenceCheck_Name(t *testing.T) {
	c := NewRigDataPresenceCheck(t.TempDir(), config.Rig{Name: "myrig", Path: t.TempDir()}, nil)
	if got := c.Name(); got != "rig:myrig:data-presence" {
		t.Errorf("Name() = %q, want %q", got, "rig:myrig:data-presence")
	}
}

func TestRigDataPresenceCheck_WarmupEligible(t *testing.T) {
	c := NewRigDataPresenceCheck(t.TempDir(), config.Rig{Name: "myrig", Path: t.TempDir()}, nil)
	if c.WarmupEligible() {
		t.Error("WarmupEligible() = true, want false")
	}
}

func TestRigDataPresenceCheck_CanFix(t *testing.T) {
	c := NewRigDataPresenceCheck(t.TempDir(), config.Rig{Name: "myrig", Path: t.TempDir()}, nil)
	if c.CanFix() {
		t.Error("CanFix() = true, want false")
	}
}

func TestRigDataPresenceCheck_NoIdentity_OK(t *testing.T) {
	// No identity.toml → legacy rig, skip without error.
	rigDir := t.TempDir()
	c := NewRigDataPresenceCheck(t.TempDir(), config.Rig{Name: "myrig", Path: rigDir},
		func(_ string) (beads.Store, error) { return beads.NewMemStore(), nil })
	r := c.Run(&CheckContext{})
	if r.Status != StatusOK {
		t.Errorf("status = %d, want OK (no identity = legacy rig); msg = %s", r.Status, r.Message)
	}
}

func TestRigDataPresenceCheck_EmptyStoreWithIdentity_Error(t *testing.T) {
	// identity.toml present + store has zero rows → blocking error.
	rigDir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(rigDir, ".beads"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := contract.WriteProjectIdentity(fsys.OSFS{}, rigDir, "gc-test-proj"); err != nil {
		t.Fatalf("WriteProjectIdentity: %v", err)
	}
	c := NewRigDataPresenceCheck(t.TempDir(), config.Rig{Name: "myrig", Path: rigDir},
		func(_ string) (beads.Store, error) { return beads.NewMemStore(), nil })
	r := c.Run(&CheckContext{})
	if r.Status != StatusError {
		t.Errorf("status = %d, want Error (empty store + identity); msg = %s", r.Status, r.Message)
	}
	if r.Severity != SeverityBlocking {
		t.Errorf("severity = %d, want Blocking", r.Severity)
	}
	if r.FixHint == "" {
		t.Error("FixHint is empty, want restore-from-backup hint")
	}
}

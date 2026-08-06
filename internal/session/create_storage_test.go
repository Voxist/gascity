package session

import (
	"bytes"
	"io"
	"os"
	"strings"
	"sync"
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
)

// recordingStorageStore implements both Create and CreateWithStorage so a test can
// tell WHICH door a create went through. Deliberately not a mock of the function
// under test: it is a store, and CreateSessionInfo drives it for real.
type recordingStorageStore struct {
	beads.Store
	plainCreates   int
	storageCreates int
	lastStorage    beads.StorageClass
}

func (r *recordingStorageStore) Create(b beads.Bead) (beads.Bead, error) {
	r.plainCreates++
	return b, nil
}

func (r *recordingStorageStore) CreateWithStorage(b beads.Bead, storage beads.StorageClass) (beads.Bead, error) {
	r.storageCreates++
	r.lastStorage = storage
	return b, nil
}

// plainOnlyStore implements Create but NOT CreateWithStorage — the shape of
// NativeDoltStore, which is why the silent-fallback hazard is real.
type plainOnlyStore struct {
	beads.Store
	plainCreates int
}

func (p *plainOnlyStore) Create(b beads.Bead) (beads.Bead, error) {
	p.plainCreates++
	return b, nil
}

func newSpec() CreateSpec {
	return CreateSpec{ID: "vc-wisp-test1", Title: "t", AgentName: "a"}
}

// A session bead MUST be created under the no_history storage class. Before vp-ia76
// this front door called Create() directly, so gascity's own session policy was
// dropped and every session landed in the committed issues table with its own
// DOLT_COMMIT — 262/24h, measured.
func TestCreateSessionInfoAppliesNoHistoryStorage(t *testing.T) {
	rec := &recordingStorageStore{}
	s := NewStore(beads.SessionStore{Store: rec})

	if _, err := s.CreateSessionInfo(newSpec()); err != nil {
		t.Fatalf("CreateSessionInfo: %v", err)
	}
	if rec.storageCreates != 1 {
		t.Errorf("CreateWithStorage calls = %d, want 1 "+
			"(the session storage policy was dropped; beads land in the committed "+
			"issues table with a DOLT_COMMIT each)", rec.storageCreates)
	}
	if rec.plainCreates != 0 {
		t.Errorf("plain Create calls = %d, want 0 (policy bypassed)", rec.plainCreates)
	}
	if rec.lastStorage != beads.StorageNoHistory {
		t.Errorf("storage class = %q, want %q", rec.lastStorage, beads.StorageNoHistory)
	}
}

// no_history and ephemeral are NOT interchangeable. ephemeral sets ephemeral=1, which
// gascity's own policy declares incompatible for sessions and which matchesTier
// silently DROPS from query results — so using it would make sessions vanish from
// reads while looking like a successful fix.
func TestSessionStorageIsNoHistoryAndNotEphemeral(t *testing.T) {
	rec := &recordingStorageStore{}
	s := NewStore(beads.SessionStore{Store: rec})

	if _, err := s.CreateSessionInfo(newSpec()); err != nil {
		t.Fatalf("CreateSessionInfo: %v", err)
	}
	if rec.lastStorage == beads.StorageEphemeral {
		t.Fatal("session beads must NOT be created ephemeral: ephemeral=1 is declared " +
			"incompatible for sessions by bead_policy_store.go and matchesTier drops " +
			"such rows from query results")
	}
	if rec.lastStorage != beads.StorageNoHistory {
		t.Fatalf("storage class = %q, want %q", rec.lastStorage, beads.StorageNoHistory)
	}
}

// THE FIX MUST NOT BE ABLE TO SHIP INERT. CachingStore.CreateWithStorage silently
// degrades to Create when its backing store lacks StorageCreateStore, and
// NativeDoltStore lacks it. A chain assembled that way would take this fix, report
// success, and keep writing to issues. The create must still succeed — observability
// must never break the caller — but it must be REPORTED, not silent.
func TestUnsupportedStorageStillCreatesButIsReported(t *testing.T) {
	plain := &plainOnlyStore{}
	s := NewStore(beads.SessionStore{Store: plain})

	// Capture stderr for real. Asserting only that the create SUCCEEDED would make
	// this test named "...IsReported" while proving nothing about reporting — a guard
	// that cannot fail, which is the exact pattern this codebase keeps producing.
	old := os.Stderr
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatalf("pipe: %v", err)
	}
	os.Stderr = w
	sessionStorageWarnOnce = sync.Once{}

	_, createErr := s.CreateSessionInfo(newSpec())

	if err := w.Close(); err != nil {
		t.Fatalf("close stderr pipe: %v", err)
	}
	os.Stderr = old
	var buf bytes.Buffer
	if _, err := io.Copy(&buf, r); err != nil {
		t.Fatalf("read captured stderr: %v", err)
	}

	if createErr != nil {
		t.Fatalf("CreateSessionInfo must not fail when storage is unsupported: %v", createErr)
	}
	if plain.plainCreates != 1 {
		t.Errorf("plain Create calls = %d, want 1 (the bead must still be persisted)",
			plain.plainCreates)
	}
	if !strings.Contains(buf.String(), "session storage policy NOT applied") {
		t.Errorf("no warning on stderr when the storage class could not be applied; "+
			"the fix would ship INERT and INVISIBLE — sessions keep landing in the "+
			"committed issues table while the change reports success. got: %q",
			buf.String())
	}
	if !strings.Contains(buf.String(), "plainOnlyStore") {
		t.Errorf("warning does not name the offending store type, so an operator "+
			"cannot tell which chain dropped the policy. got: %q", buf.String())
	}
}

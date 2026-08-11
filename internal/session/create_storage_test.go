package session

import (
	"bytes"
	"io"
	"os"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
)

// captureStderr redirects os.Stderr for the duration of fn and returns what was
// written to it.
//
// The restore is DEFERRED, not sequential. A t.Fatalf anywhere inside fn runs
// runtime.Goexit, which skips straight past a restore written after the call —
// leaving the whole test binary with os.Stderr pointed at an orphaned pipe, so
// every later test's diagnostics vanish into a buffer nobody reads and the
// failure looks like it came from somewhere else entirely.
func captureStderr(t *testing.T, fn func()) string {
	t.Helper()
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatalf("pipe: %v", err)
	}
	defer r.Close() //nolint:errcheck

	old := os.Stderr
	os.Stderr = w
	restored := false
	restore := func() {
		if restored {
			return
		}
		restored = true
		os.Stderr = old
		w.Close() //nolint:errcheck
	}
	defer restore()

	fn()

	// Restore before reading: the copy below drains the pipe to EOF, which
	// only arrives once the write end is closed.
	restore()
	var buf bytes.Buffer
	if _, err := io.Copy(&buf, r); err != nil {
		t.Fatalf("read captured stderr: %v", err)
	}
	return buf.String()
}

// resetSessionStorageWarnTypes clears the per-type warn ledger so a test sees
// the first-warning behavior regardless of what ran before it, and leaves it
// clean for whatever runs after.
func resetSessionStorageWarnTypes(t *testing.T) {
	t.Helper()
	ResetStorageWarningsForTest()
	t.Cleanup(ResetStorageWarningsForTest)
}

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

// plainOnlyStore implements Create and nothing else — no storage class in, no
// policy of its own. It records the bead exactly as it arrives so a test can
// see what the front door managed to carry through the last resort.
type plainOnlyStore struct {
	beads.Store
	plainCreates int
	last         beads.Bead
}

func (p *plainOnlyStore) Create(b beads.Bead) (beads.Bead, error) {
	p.plainCreates++
	p.last = b
	return b, nil
}

func newSpec() CreateSpec {
	return CreateSpec{ID: "vc-wisp-test1", Title: "t", AgentName: "a"}
}

// A session bead MUST be created under the no_history storage class: a store
// that accepts a class out of band must be asked for one, not called through
// the plain Create that carries no policy at all.
func TestCreateSessionInfoAppliesNoHistoryStorage(t *testing.T) {
	rec := &recordingStorageStore{}
	s := NewStore(beads.SessionStore{Store: rec})

	if _, err := s.CreateSessionInfo(newSpec()); err != nil {
		t.Fatalf("CreateSessionInfo: %v", err)
	}
	if rec.storageCreates != 1 {
		t.Errorf("CreateWithStorage calls = %d, want 1 "+
			"(the session storage policy was dropped: the bead is created with no "+
			"class at all and lands in the backend's default, fully-retained tier)",
			rec.storageCreates)
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
// silently DROPS from default-tier query results — so using it would make sessions
// vanish from reads while looking like a successful fix.
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

// THE LAST RESORT MUST NOT BE INERT, AND MUST NOT BE SILENT. When a store offers
// neither route to a storage class, the front door stamps the class onto the
// bead's own field (the routing every backend performs) AND says so. Asserting
// only that the create succeeded would make a test named "...IsReported" prove
// nothing about reporting.
func TestUnsupportedStorageStampsClassAndIsReported(t *testing.T) {
	resetSessionStorageWarnTypes(t)

	plain := &plainOnlyStore{}
	s := NewStore(beads.SessionStore{Store: plain})

	var createErr error
	out := captureStderr(t, func() {
		_, createErr = s.CreateSessionInfo(newSpec())
	})

	if createErr != nil {
		t.Fatalf("CreateSessionInfo must not fail when storage is unsupported: %v", createErr)
	}
	if plain.plainCreates != 1 {
		t.Errorf("plain Create calls = %d, want 1 (the bead must still be persisted)",
			plain.plainCreates)
	}
	if !plain.last.NoHistory {
		t.Errorf("bead reached the store with NoHistory = false: warning without stamping " +
			"drops the policy on the floor even though the field routing that would have " +
			"carried it costs nothing")
	}
	if plain.last.Ephemeral {
		t.Errorf("bead reached the store with Ephemeral = true: ephemeral beads are " +
			"GC/TTL-eligible and are dropped from default-tier reads")
	}
	if !strings.Contains(out, "session storage policy NOT applied") {
		t.Errorf("no warning on stderr when the storage class could not be requested; "+
			"an operator has no signal that the chain is assembled wrong. got: %q", out)
	}
	if !strings.Contains(out, "plainOnlyStore") {
		t.Errorf("warning does not name the offending store type, so an operator "+
			"cannot tell which chain dropped the policy. got: %q", out)
	}
}

// The warning describes the POLICY that was lost, not a Dolt table or a commit
// count: the same path serves FileStore and MemStore chains where there is no
// issues table and nothing to commit, and a warning that asserts otherwise
// sends an operator hunting for a table that does not exist.
func TestWarningDoesNotAssertDoltSpecificConsequences(t *testing.T) {
	resetSessionStorageWarnTypes(t)

	out := captureStderr(t, func() { warnSessionStorageUnsupported(&plainOnlyStore{}) })

	for _, forbidden := range []string{"issues table", "DOLT_COMMIT", "Dolt commit"} {
		if strings.Contains(out, forbidden) {
			t.Errorf("warning asserts %q, which is false for non-Dolt backends: %q",
				forbidden, out)
		}
	}
	if !strings.Contains(out, "no_history") {
		t.Errorf("warning does not name the storage class that was lost: %q", out)
	}
}

// policySelfApplyingStore mimics cmd/gc's beadPolicyStore: no CreateWithStorage,
// but declares (and performs) its own policy application on Create.
type policySelfApplyingStore struct {
	beads.Store
	plainCreates int
}

func (p *policySelfApplyingStore) Create(b beads.Bead) (beads.Bead, error) {
	p.plainCreates++
	return b, nil
}
func (p *policySelfApplyingStore) AppliesBeadStoragePolicy() {}

// A policy-self-applying store must create WITHOUT any warning: warning here
// would be a false alarm about a policy that was in fact applied.
func TestPolicySelfApplyingStoreCreatesQuietly(t *testing.T) {
	resetSessionStorageWarnTypes(t)

	pol := &policySelfApplyingStore{}
	s := NewStore(beads.SessionStore{Store: pol})
	out := captureStderr(t, func() {
		if _, err := s.CreateSessionInfo(newSpec()); err != nil {
			t.Fatalf("CreateSessionInfo: %v", err)
		}
	})
	if pol.plainCreates != 1 {
		t.Errorf("plain creates = %d, want 1", pol.plainCreates)
	}
	if strings.Contains(out, "NOT applied") {
		t.Errorf("policy-self-applying store must not warn; got %q", out)
	}
}

// storageAndPolicyStore offers BOTH routes: an out-of-band storage class and its
// own policy application. The self-applying store must win — it is the only one
// that can see a CONFIGURED session storage class, while the class the front
// door would pass is a hardcoded default.
type storageAndPolicyStore struct {
	beads.Store
	plainCreates   int
	storageCreates int
}

func (p *storageAndPolicyStore) Create(b beads.Bead) (beads.Bead, error) {
	p.plainCreates++
	return b, nil
}

func (p *storageAndPolicyStore) CreateWithStorage(b beads.Bead, _ beads.StorageClass) (beads.Bead, error) {
	p.storageCreates++
	return b, nil
}
func (p *storageAndPolicyStore) AppliesBeadStoragePolicy() {}

func TestSelfAppliedPolicyWinsOverHardcodedClass(t *testing.T) {
	resetSessionStorageWarnTypes(t)

	both := &storageAndPolicyStore{}
	s := NewStore(beads.SessionStore{Store: both})
	if _, err := s.CreateSessionInfo(newSpec()); err != nil {
		t.Fatalf("CreateSessionInfo: %v", err)
	}
	if both.storageCreates != 0 {
		t.Errorf("CreateWithStorage calls = %d, want 0: the front door imposed its "+
			"hardcoded no_history class on a store that resolves the CONFIGURED class "+
			"itself, silently overriding a [beads.policies.session] storage override",
			both.storageCreates)
	}
	if both.plainCreates != 1 {
		t.Errorf("plain Create calls = %d, want 1", both.plainCreates)
	}
}

// One benign incapable type must not mute the warning for a DIFFERENT
// incapable type later in the same process (the burned-once-guard defect).
func TestWarnIsPerStoreTypeNotPerProcess(t *testing.T) {
	resetSessionStorageWarnTypes(t)

	// Capture from the FIRST call: it warns for real, and letting it reach the
	// real stderr both pollutes the test output and hides a regression where
	// the first call is the one that misbehaves.
	out := captureStderr(t, func() {
		warnSessionStorageUnsupported(&plainOnlyStore{})          // first of its type: warns
		warnSessionStorageUnsupported(&policySelfApplyingStore{}) // different type: must warn
		warnSessionStorageUnsupported(&plainOnlyStore{})          // repeat type: must stay quiet
	})

	if n := strings.Count(out, "NOT applied"); n != 2 {
		t.Errorf("want exactly 2 warnings (one per distinct type, the repeat muted), got %d: %q",
			n, out)
	}
	if !strings.Contains(out, "plainOnlyStore") || !strings.Contains(out, "policySelfApplyingStore") {
		t.Errorf("both offending types must be named; got %q", out)
	}
}

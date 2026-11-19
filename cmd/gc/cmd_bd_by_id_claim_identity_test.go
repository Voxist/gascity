package main

// Tests for the manual claim door's identity-spelling adoption (vp-trd6h): a
// class-store bead pinned to one of the caller's own identity spellings — the
// full session name form a continuation pin now carries — must be claimable
// through `gc bd update <id> --claim` even though BEADS_ACTOR carries the bare
// session bead ID. Without the adoption the CAS compares exact strings and the
// session conflicts with a pin that is already its own.

import (
	"bytes"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/storebinding"
)

func newClaimIdentityGraph(t *testing.T) storebinding.GraphStore {
	t.Helper()
	store, err := beads.OpenSQLiteStore(t.TempDir(), beads.WithSQLiteStoreIDPrefix("gcg"))
	if err != nil {
		t.Fatalf("opening the class binding store: %v", err)
	}
	t.Cleanup(func() {
		if closer, ok := store.(interface{ CloseStore() error }); ok {
			_ = closer.CloseStore()
		}
	})
	graph, err := storebinding.NewBeadsGraphStore(store)
	if err != nil {
		t.Fatalf("projecting the graph front door: %v", err)
	}
	return graph
}

// mintHeldBead creates an open bead and, when assignee is non-empty, pins it
// through the same CAS the door itself uses, so the held spelling is exactly
// what a pin producer would have written.
func mintHeldBead(t *testing.T, graph storebinding.GraphStore, id, assignee string) {
	t.Helper()
	if _, err := graph.Create(beads.Bead{ID: id, Title: "held " + id, Type: "task"}); err != nil {
		t.Fatalf("minting %s: %v", id, err)
	}
	if assignee == "" {
		return
	}
	if _, ok, err := graph.Claim(id, assignee); err != nil || !ok {
		t.Fatalf("pinning %s to %q: ok=%v err=%v", id, assignee, ok, err)
	}
}

func TestDoBdByIDClaimAdoptsOwnIdentitySpelling(t *testing.T) {
	const (
		sessionID   = "vc-wisp-mxp02"
		sessionName = "voxist__platform-engineer-vc-wisp-mxp02"
		alias       = "voxist-platform/furiosa"
	)
	sessionEnv := map[string]string{
		"BEADS_ACTOR":     sessionID,
		"GC_SESSION_ID":   sessionID,
		"GC_SESSION_NAME": sessionName,
		"GC_ALIAS":        "",
	}
	tests := []struct {
		name        string
		env         map[string]string
		held        string
		wantCode    int
		wantHeld    string
		wantHeldRef string
	}{
		{
			// The vp-trd6h shape: the sibling is pinned to the full session
			// name while the actor carries the bare bead id. The claim is
			// recorded AS the stored spelling — the same-owner no-op fires,
			// and the pin is not silently rewritten by the manual door.
			name:        "session-name pin is claimed as itself",
			env:         sessionEnv,
			held:        sessionName,
			wantCode:    0,
			wantHeldRef: sessionName,
		},
		{
			// The actor form on the bead already: the plain same-owner path.
			name:        "bare session-id holder is a plain no-op",
			env:         sessionEnv,
			held:        sessionID,
			wantCode:    0,
			wantHeldRef: sessionID,
		},
		{
			// An alias pin on an aliased session: the alias is one of the
			// caller's own identities, so the adoption covers it too.
			name: "alias pin is claimed as itself",
			env: map[string]string{
				"BEADS_ACTOR":     alias,
				"GC_SESSION_ID":   sessionID,
				"GC_SESSION_NAME": sessionName,
				"GC_ALIAS":        alias,
			},
			held:        alias,
			wantCode:    0,
			wantHeldRef: alias,
		},
		{
			// Nothing held: the plain actor claim, unchanged by this door.
			name:        "open unassigned bead claims under the actor",
			env:         sessionEnv,
			held:        "",
			wantCode:    0,
			wantHeldRef: sessionID,
		},
		{
			// A foreign holder fails the membership check and keeps the plain
			// actor claim, whose conflict names the holder.
			name:        "foreign holder still conflicts",
			env:         sessionEnv,
			held:        "vc-wisp-other99",
			wantCode:    1,
			wantHeldRef: "vc-wisp-other99",
		},
		{
			// The adoption needs the session-name identity in the caller's
			// environment: a shell that carries only the actor cannot claim a
			// session-name pin through this door, exactly as bd refuses it.
			name: "session-name pin without the name in env conflicts",
			env: map[string]string{
				"BEADS_ACTOR":     sessionID,
				"GC_SESSION_ID":   sessionID,
				"GC_SESSION_NAME": "",
				"GC_ALIAS":        "",
			},
			held:        sessionName,
			wantCode:    1,
			wantHeldRef: sessionName,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			for key, value := range tc.env {
				t.Setenv(key, value)
			}
			graph := newClaimIdentityGraph(t)
			mintHeldBead(t, graph, "gcg-1", tc.held)

			var stdout, stderr bytes.Buffer
			code := doBdByIDClaim(graph, "gcg-1", bdByIDClaimActor(), false, "test", &stdout, &stderr)
			if code != tc.wantCode {
				t.Fatalf("doBdByIDClaim() = %d, want %d; stderr=%s", code, tc.wantCode, stderr.String())
			}
			after, err := graph.Get("gcg-1")
			if err != nil {
				t.Fatalf("reading back gcg-1: %v", err)
			}
			if got := strings.TrimSpace(after.Assignee); got != tc.wantHeldRef {
				t.Fatalf("assignee after claim = %q, want %q", got, tc.wantHeldRef)
			}
			if tc.wantCode != 0 {
				return
			}
			if !strings.Contains(stdout.String(), "gcg-1") {
				t.Fatalf("success output does not name the bead: %q", stdout.String())
			}
		})
	}
}

func TestDoBdByIDClaimRequiresActor(t *testing.T) {
	t.Setenv("BEADS_ACTOR", "")
	graph := newClaimIdentityGraph(t)
	var stdout, stderr bytes.Buffer
	if code := doBdByIDClaim(graph, "gcg-1", bdByIDClaimActor(), false, "test", &stdout, &stderr); code != 1 {
		t.Fatalf("doBdByIDClaim() = %d, want 1", code)
	}
	if !strings.Contains(stderr.String(), "requires BEADS_ACTOR") {
		t.Fatalf("stderr does not name the missing actor: %q", stderr.String())
	}
}

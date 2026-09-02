package main

import (
	"bytes"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
)

// gc hook must distinguish "store unreachable" (exit 2 + token) from
// "no work" (exit 1): rendering an unreachable store as no-work is the
// chronic idle-agents-with-work-waiting dead-drop (R-INV, plan item 1.3).

func TestDoHookStoreUnavailableExitsTwoWithToken(t *testing.T) {
	runner := func(string, string) (string, error) {
		return "", fmt.Errorf("running work query %q: %w", "bd ready --json", beads.ErrStoreUnavailable)
	}
	var stdout, stderr bytes.Buffer
	code := doHook("bd ready --json", "/tmp/work", false, runner, &stdout, &stderr, hookVisibility{})
	if code != 2 {
		t.Fatalf("doHook = %d, want 2 for ErrStoreUnavailable; stderr=%s", code, stderr.String())
	}
	if !strings.Contains(stderr.String(), hookStoreUnavailableToken) {
		t.Fatalf("stderr %q missing token %q", stderr.String(), hookStoreUnavailableToken)
	}
}

func TestDoHookTransportClassErrorClassifiedUnavailable(t *testing.T) {
	// Work queries shell out to bd; a wedged store presents as a raw exec
	// error whose stderr carries the pinned transport markers.
	runner := func(string, string) (string, error) {
		return "", fmt.Errorf("running work query %q: exit status 1: Error: dial tcp 127.0.0.1:3307: connection refused", "bd ready --json")
	}
	var stdout, stderr bytes.Buffer
	code := doHook("bd ready --json", "/tmp/work", false, runner, &stdout, &stderr, hookVisibility{})
	if code != 2 {
		t.Fatalf("doHook = %d, want 2 for a transport-class work-query failure; stderr=%s", code, stderr.String())
	}
	if !strings.Contains(stderr.String(), hookStoreUnavailableToken) {
		t.Fatalf("stderr %q missing token %q", stderr.String(), hookStoreUnavailableToken)
	}
}

func TestDoHookOrdinaryErrorStaysExitOne(t *testing.T) {
	runner := func(string, string) (string, error) {
		return "", fmt.Errorf("running work query %q: exit status 1: unknown flag --bogus", "bd ready --bogus")
	}
	var stdout, stderr bytes.Buffer
	code := doHook("bd ready --bogus", "/tmp/work", false, runner, &stdout, &stderr, hookVisibility{})
	if code != 1 {
		t.Fatalf("doHook = %d, want 1 for an ordinary work-query failure", code)
	}
	if strings.Contains(stderr.String(), hookStoreUnavailableToken) {
		t.Fatalf("stderr %q carries the store-unavailable token for an application error", stderr.String())
	}
}

func TestDoHookNoWorkStaysExitOne(t *testing.T) {
	runner := func(string, string) (string, error) { return "", nil }
	var stdout, stderr bytes.Buffer
	if code := doHook("bd ready --json", "/tmp/work", false, runner, &stdout, &stderr, hookVisibility{}); code != 1 {
		t.Fatalf("doHook = %d, want 1 for empty output (no work)", code)
	}
}

func TestClassifyWorkQueryStoreUnavailable(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want bool
	}{
		{"nil", nil, false},
		{"already typed", fmt.Errorf("x: %w", beads.ErrStoreUnavailable), true},
		{"dial tcp", errors.New("Error: dial tcp 127.0.0.1:3307: connect: connection refused"), true},
		{"server unreachable", errors.New("bd: server unreachable"), true},
		{"silent fallback pair", errors.New("auto-importing 312 issues into empty database"), true},
		{"application error", errors.New("exit status 1: unknown flag"), false},
		{"timeout without marker", errors.New("timed out after 30s"), false},
	}
	for _, tc := range cases {
		got := classifyWorkQueryStoreUnavailable(tc.err)
		if tc.want && !errors.Is(got, beads.ErrStoreUnavailable) {
			t.Errorf("%s: classified err = %v, want ErrStoreUnavailable", tc.name, got)
		}
		if !tc.want && errors.Is(got, beads.ErrStoreUnavailable) {
			t.Errorf("%s: classified err = %v, want NOT ErrStoreUnavailable", tc.name, got)
		}
		if tc.err == nil && got != nil {
			t.Errorf("%s: classified nil to %v", tc.name, got)
		}
	}
}

// TestClaimHookStoreUnavailableEmitsToken pins the token on the --claim path.
//
// --claim is the form agents run in the dispatch loop, so this is where a
// transport-class failure most needs to be distinguishable from "no work". The
// token lived only on the read path (doHook) until this was added, which left
// the dead-drop open on exactly the path that matters.
func TestClaimHookStoreUnavailableEmitsToken(t *testing.T) {
	var stdout, stderr bytes.Buffer

	failing := func(string, string, []string) (string, error) {
		return "", fmt.Errorf("running work query %q: %w", "bd ready --json",
			errors.New("exit status 1: Error: dial tcp 127.0.0.1:3307: connection refused"))
	}

	code := claimHookWorkWithRunner(
		"bd ready --json", "", nil,
		[]hookStore{{dir: ""}},
		hookClaimOptions{Assignee: "worker"},
		hookClaimOps{},
		failing,
		func(string, error) {},
		&stdout, &stderr,
	)

	// The code, not just the token: hookStoreUnavailableToken's doc publishes
	// exit 2, and a consumer gating on the code must be able to tell a dead
	// store from no-work on the path agents actually run.
	if code != 2 {
		t.Fatalf("claim hook returned %d on a transport failure, want 2 (the exit code the token documents); stderr=%q",
			code, stderr.String())
	}
	if !strings.Contains(stderr.String(), hookStoreUnavailableToken) {
		t.Fatalf("claim path did not emit %s on a transport-class failure; a dead store is still "+
			"indistinguishable from a drained queue on the path agents actually run:\nstderr=%q",
			hookStoreUnavailableToken, stderr.String())
	}
}

// TestClaimHookStoreUnavailableEmitsTokenOnFederatedRevalidation pins the token
// on the SECOND read of a federated claim. With more than one leg,
// claimStoreWithFallback re-runs the query on the selected store before the
// mutation; when that store is the primary and the re-read is a transport-class
// failure, the claim must exit 2 with the token exactly as the first read does.
// TestClaimHookStoreUnavailableEmitsToken cannot see this path: with one leg
// claimStoreWithFallback short-circuits and never re-reads.
func TestClaimHookStoreUnavailableEmitsTokenOnFederatedRevalidation(t *testing.T) {
	stores := []hookStore{
		{dir: "city", env: []string{"GC_STORE=city"}}, // primary (own) store
		{dir: "riga", env: []string{"GC_STORE=riga"}}, // federated leg, empty
	}
	cityCalls := 0
	run := func(_, dir string, _ []string) (string, error) {
		switch dir {
		case "city":
			cityCalls++
			if cityCalls == 1 {
				// Discovery sees ready work, so the claim commits to this store.
				return `[{"id":"hw-city","status":"open","metadata":{"gc.routed_to":"worker"}}]`, nil
			}
			// The store died between discovery and claim-time re-validation.
			return "", fmt.Errorf("running work query %q: %w", "bd ready --json",
				errors.New("exit status 1: Error: dial tcp 127.0.0.1:3307: connect: connection refused"))
		case "riga":
			return "[]", nil
		default:
			t.Fatalf("unexpected store dir %q", dir)
			return "", nil
		}
	}
	opts := hookClaimOptions{
		Assignee:           "worker-1",
		IdentityCandidates: []string{"worker-1"},
		RouteTargets:       []string{"worker"},
		JSON:               true,
	}
	emitted := false
	var stdout, stderr bytes.Buffer
	code := claimHookWorkWithRunner("bd ready --json", "city", stores[0].env, stores, opts, hookClaimOps{}, run, func(string, error) { emitted = true }, &stdout, &stderr)

	if cityCalls < 2 {
		t.Fatalf("primary store queried %d times, want the claim-time re-validation to run (>= 2); the test never reached the path it pins", cityCalls)
	}
	if !emitted {
		t.Fatal("re-validation failure did not emit the work-query failure event")
	}
	if code != 2 {
		t.Fatalf("claim hook returned %d on a transport failure at claim-time re-validation, want 2; stderr=%q",
			code, stderr.String())
	}
	if !strings.Contains(stderr.String(), hookStoreUnavailableToken) {
		t.Fatalf("federated claim re-validation did not emit %s on a transport-class failure; a dead store "+
			"is indistinguishable from a drained queue on exactly the path a multi-store city runs:\nstderr=%q",
			hookStoreUnavailableToken, stderr.String())
	}
	if stdout.Len() != 0 {
		t.Fatalf("a failed read must not write a drain result; stdout=%q", stdout.String())
	}
}

// The non-transport counterpart: an application error on the same re-validation
// path stays an ordinary exit-1 failure without the token, so the token is a
// classification and not a blanket reaction to any re-read error.
func TestClaimHookApplicationErrorOnFederatedRevalidationStaysExitOne(t *testing.T) {
	stores := []hookStore{
		{dir: "city", env: []string{"GC_STORE=city"}},
		{dir: "riga", env: []string{"GC_STORE=riga"}},
	}
	cityCalls := 0
	run := func(_, dir string, _ []string) (string, error) {
		if dir == "riga" {
			return "[]", nil
		}
		cityCalls++
		if cityCalls == 1 {
			return `[{"id":"hw-city","status":"open","metadata":{"gc.routed_to":"worker"}}]`, nil
		}
		return "", fmt.Errorf("running work query %q: %w", "bd ready --json", errors.New("exit status 1: unknown flag"))
	}
	var stdout, stderr bytes.Buffer
	code := claimHookWorkWithRunner("bd ready --json", "city", stores[0].env, stores,
		hookClaimOptions{Assignee: "worker-1", IdentityCandidates: []string{"worker-1"}, RouteTargets: []string{"worker"}, JSON: true},
		hookClaimOps{}, run, func(string, error) {}, &stdout, &stderr)
	if code != 1 {
		t.Fatalf("claim hook returned %d on an application error at re-validation, want 1; stderr=%q", code, stderr.String())
	}
	if strings.Contains(stderr.String(), hookStoreUnavailableToken) {
		t.Fatalf("application error was reported as store-unavailable:\nstderr=%q", stderr.String())
	}
}

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
	code := doHook("bd ready --json", "/tmp/work", false, runner, &stdout, &stderr)
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
	code := doHook("bd ready --json", "/tmp/work", false, runner, &stdout, &stderr)
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
	code := doHook("bd ready --bogus", "/tmp/work", false, runner, &stdout, &stderr)
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
	if code := doHook("bd ready --json", "/tmp/work", false, runner, &stdout, &stderr); code != 1 {
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

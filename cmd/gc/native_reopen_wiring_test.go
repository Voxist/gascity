package main

import (
	"errors"
	"fmt"
	"os"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
)

// TestNativeReopenHookWiredAtBothStoreOpenSites pins the two production sites
// that must arm the NativeDoltStore read-path reconnect hook: the CLI provider
// store (openStoreResultAtForCity in main.go) and the controller reconcile rig
// store (openRigStore in api_state.go). Deleting either WithNativeReopen wiring
// would silently re-expose the managed-Dolt hard-kill/rebind read failure #4197
// fixed, so this test fails if either the hook or its ctx-threaded env
// re-resolution goes missing.
func TestNativeReopenHookWiredAtBothStoreOpenSites(t *testing.T) {
	for _, tc := range []struct {
		file string
		site string
	}{
		{file: "main.go", site: "openStoreResultAtForCity"},
		{file: "api_state.go", site: "openRigStore"},
	} {
		data, err := os.ReadFile(tc.file)
		if err != nil {
			t.Fatalf("read %s: %v", tc.file, err)
		}
		src := string(data)
		if !strings.Contains(src, "beads.WithNativeReopen(") {
			t.Fatalf("%s (%s): native reopen hook wiring beads.WithNativeReopen(...) is missing — the #4197 managed-Dolt rebind reconnect must stay armed", tc.file, tc.site)
		}
		if !strings.Contains(src, "nativeDoltOpenEnvForScopeContext(ctx") {
			t.Fatalf("%s (%s): the reopen hook must re-resolve the managed Dolt env under the wall context via nativeDoltOpenEnvForScopeContext(ctx, ...)", tc.file, tc.site)
		}
	}
}

// TestNativeStoreOpensCheckMigrationFreezeBeforeRecovery is ga-vwupk's
// regression for the gap the audit and two review rounds found: several
// closures re-resolve env (or, on the proxied lane, re-admit) BEFORE ever
// reaching a native open's own choke-point refusal
// (checkMigrationFreezeForNativeOpen / the proxied lane's equivalent), and
// that pre-open work is exactly what can trigger recovery -- restarting a
// managed Dolt server, or forking a bd command, that an operator
// deliberately stopped as part of installing a MIGRATION-FREEZE marker.
//
// Round 1 gated only the two RECONNECT closures (main.go, api_state.go).
// Round 2's delta review found the two INITIAL-open closures (the very
// first native store construction each site does, which every ordinary
// `gc hook --claim` / `gc ready` reaches) were still ungated, plus a third
// site entirely: the proxied lane's own reopen, whose admission ladder can
// escalate to a bd-forking recover rung before its own choke point runs.
// This test enumerates all five now known sites; a new site that needs the
// same guard and is not added here would defeat the WHOLE POINT of a
// regression test, so this table IS the allowlist ga-vwupk's review asked
// for -- a full non-test-file census (scanning for every future
// WithNativeReopen(/NativeReopenFunc/OpenNativeStore site automatically,
// AST-based, in the style of dolt_recover_gate_funnel_test.go's provider-op
// census) is a further hardening left for ga-ju39w rather than attempted
// here.
//
// Like its predecessor, this is a structural/textual pin (the established
// style for this file), not a live-recovery behavioral test: exercising a
// real managed-Dolt recovery attempt, or a real bd-fork, end to end belongs
// in the acceptance-tier tests, not here.
func TestNativeStoreOpensCheckMigrationFreezeBeforeRecovery(t *testing.T) {
	for _, tc := range []struct {
		file        string
		site        string
		anchor      string // marks where this closure's own body starts, so a shared guardCall/recoverCall text elsewhere in the file (e.g. a sibling closure) is not mistaken for this site's
		guardCall   string
		recoverCall string
	}{
		{
			file:        "main.go",
			site:        "openStoreResultAtForCity's initial OpenNativeStore",
			anchor:      "OpenNativeStore: func() (beads.Store, error) {",
			guardCall:   "beads.CheckMigrationFreeze(scopeRoot)",
			recoverCall: "nativeDoltOpenEnvForScope(runtimeCityPath, cfg, scopeRoot)",
		},
		{
			file:        "main.go",
			site:        "openStoreResultAtForCity's reconnect",
			anchor:      "reopen := func(ctx context.Context)",
			guardCall:   "beads.CheckMigrationFreeze(scopeRoot)",
			recoverCall: "nativeDoltOpenEnvForScopeContext(ctx",
		},
		{
			file:        "api_state.go",
			site:        "openRigStore's initial OpenNativeStore",
			anchor:      "OpenNativeStore: func() (beads.Store, error) {",
			guardCall:   "beads.CheckMigrationFreeze(scopeRoot)",
			recoverCall: "nativeDoltOpenEnvForScope(cs.cityPath, cfg, scopeRoot)",
		},
		{
			file:        "api_state.go",
			site:        "openRigStore's reconnect",
			anchor:      "reopen := func(ctx context.Context)",
			guardCall:   "beads.CheckMigrationFreeze(scopeRoot)",
			recoverCall: "nativeDoltOpenEnvForScopeContext(ctx",
		},
		{
			file:        "beads_proxied_native.go",
			site:        "proxiedNativeOpener.open",
			anchor:      "func (o *proxiedNativeOpener) open(parent context.Context, longLived bool) (beads.Store, beads.ProxiedOpenReport, error) {",
			guardCall:   "beads.CheckMigrationFreeze(o.scopeRoot)",
			recoverCall: "o.admit(ctx, longLived)",
		},
		{
			file:        "beads_proxied_native.go",
			site:        "proxiedNativeOpener.reopen",
			anchor:      "func (o *proxiedNativeOpener) reopen(longLived bool) beads.NativeReopenFunc {",
			guardCall:   "beads.CheckMigrationFreeze(o.scopeRoot)",
			recoverCall: "o.admit(ctx, longLived)",
		},
	} {
		t.Run(tc.file+"/"+tc.site, func(t *testing.T) {
			data, err := os.ReadFile(tc.file)
			if err != nil {
				t.Fatalf("read %s: %v", tc.file, err)
			}
			src := string(data)
			anchorAt := strings.Index(src, tc.anchor)
			if anchorAt < 0 {
				t.Fatalf("%s (%s): could not find the closure (anchor %q)", tc.file, tc.site, tc.anchor)
			}
			body, ok := closureBody(src, anchorAt+len(tc.anchor))
			if !ok {
				t.Fatalf("%s (%s): could not find the closure's matching closing brace (anchor %q)", tc.file, tc.site, tc.anchor)
			}
			guardAt := strings.Index(body, tc.guardCall)
			if guardAt < 0 {
				t.Fatalf("%s (%s): must check %s before triggering recovery — an active MIGRATION-FREEZE must never let gc "+
					"restart a database (or fork a bd command) an operator deliberately stopped (ga-vwupk)",
					tc.file, tc.site, tc.guardCall)
			}
			recoverAt := strings.Index(body, tc.recoverCall)
			if recoverAt < 0 {
				t.Fatalf("%s (%s): expected recovery-triggering call %q not found in this closure", tc.file, tc.site, tc.recoverCall)
			}
			if guardAt > recoverAt {
				t.Fatalf("%s (%s): %s must appear BEFORE %q in this closure, not after — checked at index %d vs "+
					"recovery-triggering call at %d (ga-vwupk)",
					tc.file, tc.site, tc.guardCall, tc.recoverCall, guardAt, recoverAt)
			}
		})
	}
}

// TestCheckMigrationFreezeErrorSurvivesClosureWrapping pins, from cmd/gc's
// own package, the exact error-wrapping idiom every guarded closure in this
// file uses: `if err := beads.CheckMigrationFreeze(scopeRoot); err != nil {
// return nil, fmt.Errorf("...: %w", err) }`. beads.ErrNativeOpenFrozen was
// exported in ga-vwupk round 3 precisely so a caller here can
// errors.Is(err, beads.ErrNativeOpenFrozen) after that wrapping instead of
// string-matching an error message; this proves the %w verb actually
// carries it through, the same wrapping every closure in this file (main.go,
// api_state.go, beads_proxied_native.go) performs.
func TestCheckMigrationFreezeErrorSurvivesClosureWrapping(t *testing.T) {
	dir := t.TempDir()
	beadsDir := dir + "/.beads"
	if err := os.MkdirAll(beadsDir, 0o755); err != nil {
		t.Fatalf("mkdir .beads: %v", err)
	}
	if err := os.WriteFile(beadsDir+"/MIGRATION-FREEZE", []byte(""), 0o644); err != nil {
		t.Fatalf("write marker: %v", err)
	}

	err := beads.CheckMigrationFreeze(dir)
	if err == nil {
		t.Fatal("CheckMigrationFreeze with a marker present = nil, want an error")
	}
	wrapped := fmt.Errorf("project native store env %s: %w", dir, err)
	if !errors.Is(wrapped, beads.ErrNativeOpenFrozen) {
		t.Fatalf("errors.Is(wrapped, beads.ErrNativeOpenFrozen) = false after %%w-wrapping %v; "+
			"every guarded closure in this file relies on this surviving", err)
	}
}

// closureBody returns the text of a closure whose opening brace sits at
// (or is the last character before) openAt, up to its MATCHING closing
// brace, found by counting brace depth. ga-vwupk round 3 LOW: the previous
// version of TestNativeStoreOpensCheckMigrationFreezeBeforeRecovery scanned
// from an anchor to END OF FILE, so a guardCall/recoverCall pair belonging
// to a LATER, unrelated closure in the same file could satisfy an earlier
// site's check by coincidence. Bounding to the matching brace is a plain
// depth count, not a real parser, so it does not account for a brace
// appearing inside a string literal or comment -- a known, accepted gap
// matching this file's other textual-pin tests, and not a risk in the
// specific closures this table scans today.
func closureBody(src string, openAt int) (string, bool) {
	depth := 0
	for i := openAt - 1; i < len(src); i++ {
		switch src[i] {
		case '{':
			depth++
		case '}':
			depth--
			if depth == 0 {
				return src[openAt : i+1], true
			}
		}
	}
	return "", false
}

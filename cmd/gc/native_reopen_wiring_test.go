package main

import (
	"os"
	"strings"
	"testing"
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
// WithNativeReopen(/NativeReopenFunc/OpenNativeStore site automatically) is
// a further hardening left for a follow-up rather than attempted here.
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
			body := src[anchorAt:]
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

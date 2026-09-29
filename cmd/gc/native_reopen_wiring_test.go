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

// TestNativeReopenHookChecksMigrationFrozenBeforeRecovery is ga-vwupk's
// regression for the SECOND gap the audit found: the reopen closures above
// re-resolve the managed-Dolt env (nativeDoltOpenEnvForScopeContext /
// nativeDoltOneShotOpenEnvForScopeContext) BEFORE OpenNativeStorage ever
// reaches checkMigrationFreezeForNativeOpen's choke-point refusal, and that
// re-resolution is exactly what can trigger managed-Dolt recovery/restart
// (allowRecovery=true). A frozen scope must never have gc restart a
// database an operator deliberately stopped, even for the brief window
// before the choke point's own refusal lands a few lines later — so
// beads.MigrationFrozen(scopeRoot) must appear, textually, BEFORE the env
// re-resolution call inside each reopen closure. Like its sibling above,
// this is a structural/textual pin (the established style for this file),
// not a live-recovery behavioral test: exercising a real managed-Dolt
// recovery attempt end to end belongs in the acceptance-tier tests, not
// here.
func TestNativeReopenHookChecksMigrationFrozenBeforeRecovery(t *testing.T) {
	for _, tc := range []struct {
		file        string
		site        string
		recoverCall string
	}{
		{file: "main.go", site: "openStoreResultAtForCity's long-lived reopen", recoverCall: "nativeDoltOpenEnvForScopeContext(ctx"},
		{file: "api_state.go", site: "openRigStore's reopen", recoverCall: "nativeDoltOpenEnvForScopeContext(ctx"},
	} {
		data, err := os.ReadFile(tc.file)
		if err != nil {
			t.Fatalf("read %s: %v", tc.file, err)
		}
		src := string(data)
		reopenAt := strings.Index(src, "reopen := func(ctx context.Context)")
		if reopenAt < 0 {
			t.Fatalf("%s (%s): could not find the reopen closure (reopen := func(ctx context.Context) ...)", tc.file, tc.site)
		}
		body := src[reopenAt:]
		guardAt := strings.Index(body, "beads.MigrationFrozen(scopeRoot)")
		if guardAt < 0 {
			t.Fatalf("%s (%s): the reopen closure must check beads.MigrationFrozen(scopeRoot) before re-resolving the "+
				"env — a frozen scope must never trigger managed-Dolt recovery on the way to the choke point's own refusal (ga-vwupk)",
				tc.file, tc.site)
		}
		recoverAt := strings.Index(body, tc.recoverCall)
		if recoverAt < 0 {
			t.Fatalf("%s (%s): expected env re-resolution call %q not found in the reopen closure", tc.file, tc.site, tc.recoverCall)
		}
		if guardAt > recoverAt {
			t.Fatalf("%s (%s): beads.MigrationFrozen(scopeRoot) must appear BEFORE %q in the reopen closure, "+
				"not after — checked at index %d vs recovery-triggering call at %d (ga-vwupk)",
				tc.file, tc.site, tc.recoverCall, guardAt, recoverAt)
		}
	}
}

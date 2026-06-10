package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"path/filepath"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/beads/contract"
	"github.com/gastownhall/gascity/internal/beads/identity"
	"github.com/gastownhall/gascity/internal/fsys"
)

// errNativeStoreIdentityMismatch is returned by the scope-open native opener
// when the post-open identity assertion fails. It signals the store factory to
// fall back to BdStore instead of trusting a silent-empty or misrouted native
// database. The factory treats any non-nil native-open error as a fallback
// trigger (diagnostic gate native_open), so the cutover is never silently
// pointed at the wrong database.
var errNativeStoreIdentityMismatch = errors.New("native store identity mismatch")

// IdentityAlert is the typed payload of a post-open identity assertion failure.
// It is the gc-side shape of a store.degraded(class=identity-mismatch) signal.
// The patrol/event branch, when present, maps this onto its typed event; until
// then the default sink degrades it to a structured log so this check never
// hard-depends on a sibling branch.
type IdentityAlert struct {
	// ScopeRoot is the scope whose native store failed the assertion.
	ScopeRoot string
	// Result is the typed assertion outcome (always degraded for an emitted alert).
	Result identity.Result
}

// IdentityAlertSink consumes a degraded identity assertion. Implementations emit
// a typed store.degraded / doctor.alert event; the default sink logs.
type IdentityAlertSink func(IdentityAlert)

// logIdentityAlertSink degrades an identity alert to a structured error log. It
// is the fallback when no typed event sink is wired, keeping the assertion
// observable without depending on the patrol branch's event types.
func logIdentityAlertSink(logger *slog.Logger) IdentityAlertSink {
	if logger == nil {
		logger = slog.Default()
	}
	return func(alert IdentityAlert) {
		logger.Error(
			"store.degraded",
			slog.String("class", identity.AlertClass),
			slog.String("scope", alert.ScopeRoot),
			slog.String("identity_class", string(alert.Result.Class)),
			slog.String("configured_project_id", alert.Result.Configured.ProjectID),
			slog.String("opened_project_id", alert.Result.Opened.ProjectID),
			slog.String("configured_issue_prefix", alert.Result.Configured.IssuePrefix),
			slog.String("opened_issue_prefix", alert.Result.Opened.IssuePrefix),
			slog.Any("mismatched_fields", alert.Result.MismatchedFields),
		)
	}
}

// configuredScopeIdentity reads the identity an operator configured for a scope
// from on-disk canonical files: project_id from .beads/metadata.json and
// issue_prefix from .beads/config.yaml. Missing files yield an empty field
// rather than an error, so a freshly-seeded scope surfaces as configured-empty
// rather than crashing the assertion.
func configuredScopeIdentity(scopeRoot string) identity.ScopeIdentity {
	scopeIdentity := identity.ScopeIdentity{}
	// Only a successful read populates ProjectID; any failure (missing file or a
	// parse error) leaves it empty so the compare reports configured-empty
	// (degraded) rather than silently passing.
	if projectID, err := readManagedMetadataProjectID(scopeMetadataJSONPath(scopeRoot)); err == nil {
		scopeIdentity.ProjectID = projectID
	}
	configPath := filepath.Join(scopeRoot, ".beads", "config.yaml")
	if prefix, ok, err := contract.ReadIssuePrefix(fsys.OSFS{}, configPath); err == nil && ok {
		scopeIdentity.IssuePrefix = prefix
	}
	return scopeIdentity
}

// openNativeStoreWithIdentityAssertion opens the native Dolt store for scopeRoot
// with the projected env, then runs the post-open identity assertion. On a
// degraded result it closes the opened store and returns
// errNativeStoreIdentityMismatch so the factory falls back to BdStore rather
// than handing back a wrong-database store. This is the load-bearing safety pair
// for the P2.3 canary lever: the lever opens the native store; this assertion
// guarantees the open targeted the scope's real data.
func openNativeStoreWithIdentityAssertion(ctx context.Context, scopeRoot string, env map[string]string, sink IdentityAlertSink) (beads.Store, error) {
	store, err := beads.OpenNativeDoltStoreAt(ctx, scopeRoot, env)
	if err != nil {
		return nil, err
	}
	result := assertNativeStoreIdentity(store, scopeRoot, sink)
	if result.Degraded() {
		if closeErr := store.CloseStore(); closeErr != nil {
			return nil, fmt.Errorf("%w (class=%s): closing store: %w", errNativeStoreIdentityMismatch, result.Class, closeErr)
		}
		return nil, fmt.Errorf("%w (class=%s) for scope %s", errNativeStoreIdentityMismatch, result.Class, scopeRoot)
	}
	return store, nil
}

// assertNativeStoreIdentity runs the post-open identity assertion for a native
// store at scopeRoot and dispatches a degraded result to sink. It returns the
// typed result so a scope-open caller can fall back to BdStore on a degraded
// outcome. A nil store or a nil sink is tolerated: a nil store yields an
// opened-empty result (the silent-empty signature), and a nil sink degrades to
// the default structured-log sink.
func assertNativeStoreIdentity(store *beads.NativeDoltStore, scopeRoot string, sink IdentityAlertSink) identity.Result {
	configured := configuredScopeIdentity(scopeRoot)
	var result identity.Result
	if store == nil {
		result = identity.Assert(configured, identity.ScopeIdentity{})
	} else {
		result = store.AssertOpenedIdentity(configured)
	}
	if result.Degraded() {
		if sink == nil {
			sink = logIdentityAlertSink(slog.Default())
		}
		sink(IdentityAlert{ScopeRoot: scopeRoot, Result: result})
	}
	return result
}

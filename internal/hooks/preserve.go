package hooks

import (
	"path"
	"path/filepath"
)

// managedOverlayHookPaths maps the flattened per-provider overlay path of every
// hook file that carries a version marker to its provider name. The paths are
// unique across providers, so a relative path alone identifies the file.
var managedOverlayHookPaths = map[string]string{
	path.Join(".pi", "extensions", "gc-hooks.js"):   "pi",
	path.Join(".opencode", "plugins", "gascity.js"): "opencode",
	path.Join(".mimocode", "plugin", "gascity.js"):  "mimocode",
	path.Join(".omp", "hooks", "gc-hook.ts"):        "omp",
	// kiro and kimi carry version markers on this fork but not upstream, so
	// upstream's list legitimately omits them and upstream's own run of
	// TestManagedOverlayHookPathsCoverEveryVersionedFile is green without
	// them. The 2026-09-13 resync combined the fork's MARKED overlay files
	// with upstream's list and test, which is what makes these entries
	// necessary here and only here: without them overlay staging reverts both
	// files on every reconcile tick, silently, which is the exact defect
	// (#5554) this map exists to prevent.
	path.Join(".kiro", "agents", "gascity.json"):            "kiro",
	path.Join(".kimi", "hooks", "gascity-session-start.py"): "kimi",
}

// PreserveManagedFile reports whether an existing file at relPath is a managed
// hook file that is already current, and so must not be replaced.
//
// Overlay staging copies the bundled per-provider tree over a work directory on
// every reconcile tick, with no version check and no backup, which silently
// reverted hook files that were current or newer (#5554). Passing this to
// runtime.WithPreserve lets staging defer to the same versioning policy
// internal/hooks already applies, without package runtime depending on this one.
//
// It is deliberately conservative: anything it does not recognize as a current
// managed hook file returns false and is staged exactly as before.
func PreserveManagedFile(relPath string, existing []byte) bool {
	provider, ok := managedOverlayHookPaths[filepath.Clean(relPath)]
	if !ok {
		return false
	}
	// The desired bytes are nil because every predicate reachable through
	// managedOverlayHookPaths decides from a version marker in the existing
	// file alone. Only the .cursor/hooks.json predicate compares against the
	// desired document, and that path is mergeable
	// (overlay.IsMergeablePath), so the staging caller skips it before the
	// preserve predicate is ever consulted — it is deliberately absent from
	// the map above.
	needsUpgrade := overlayManagedNeedsUpgrade(provider, filepath.Clean(relPath), nil)
	if needsUpgrade == nil {
		return false
	}
	return !needsUpgrade(existing)
}

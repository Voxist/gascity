package beads

// native_dolt_migration_freeze.go is gc's own read-side check for the fork
// bd's MIGRATION-FREEZE write-freeze marker (github.com/Voxist/beads,
// internal/migration/freeze.go). It is reimplemented here — not imported —
// because internal/migration is an internal package of a different Go
// module: Go's internal-package visibility rule (only the tree rooted at
// internal's parent may import it) forbids gascity from importing it
// directly, even though both binaries link against the SAME upstream beads
// library (github.com/steveyegge/beads) that the freeze marker is meant to
// gate.
//
// ga-vwupk. gc's in-process (native) opens go straight to
// beadslib.OpenBestAvailable with no options — a WRITABLE open — and the
// upstream library has no freeze check of its own (only the fork's cmd/bd
// does). Left alone, a human running `bd migrate` under an active freeze is
// protected, but a co-resident `gc` (the controller, a one-shot command, or
// the read-path reconnect after a managed-Dolt hard-kill/rebind) is not: it
// opens straight through and can commit (dolt_ignore seed/cursor heal on
// open) while bd refuses. Operator decision 2026-09-29 (bead notes): gc
// checks the marker itself before ANY native open, including reconnect, and
// falls back to the BdStore/CLI path, which honors the freeze on its own.
//
// DRIFT RISK, ACCEPTED BY THE OPERATOR. This file must track
// github.com/Voxist/beads internal/migration/freeze.go's marker FORMAT (file
// name, env override name, and the tab-separated operator/timestamp/reason
// payload) even though the two modules cannot share the Go code that reads
// it. native_dolt_migration_freeze_test.go's TestMigrationFreezeMarkerFixtureShape
// is the shared contract for THIS repo's side: it is written to be
// copy-pasteable (same fixture shape, same assertions) into the fork's own
// freeze_test.go, so a change to either side's marker format shows up as a
// fixture mismatch rather than a silent divergence. There is currently no
// automated check that actually spans both repositories -- ga-ijjie tracks
// building one (a shared fixture file, or a CI job that diffs the two).
//
// This is a deliberately close port of the fork's semantics — the ancestor
// walk, the env override, the ENOENT-is-not-frozen but
// cannot-tell-is-frozen distinction, the untrusted-symlink and
// sticky-directory carve-outs — for the same reason the fork built them:
// each one closes a real gap (an unreadable marker must not read as "not
// frozen"; a marker the untrusted ancestor walk merely passed through must
// not be redirectable via a symlink; a world-writable sticky directory like
// /tmp is forgeable by any local account). Two deliberate divergences from
// the fork's own Find: (1) gc always has an explicit scopeRoot for the store
// it is about to open, so this walks ONLY that scope's ancestry
// (migration.FindFrom's shape), never gc's own process cwd, which is not
// part of the operation being gated; (2) the fork's Find also walks every
// root in townFreezeRoots() (a Gas-Town multi-repo concept) in addition to
// the workspace's own ancestry -- gc has no equivalent walk today, tracked
// as ga-ejfzo rather than guessed at here.
//
// SCOPE, STATED PLAINLY: this closes the OPEN/RECONNECT path only, per
// operator decision A (gate at open time, not on every write). A
// NativeDoltStore handle that was ALREADY OPEN before a freeze marker
// appeared keeps writing straight through the freeze for as long as it
// stays open — the choke points below run once, at open, not on every
// subsequent write. gc's controller holds exactly such long-lived handles.
// This is NOT a gap this file closes; ga-6ou7x tracks a write-path check or
// a patrol that closes and reopens native handles when a freeze appears.
// Until ga-6ou7x ships, the pre-existing runbook rule is PERMANENT, not
// superseded by this file: stop the controller, or set
// GC_BEADS_FORCE_FALLBACK=1, before setting a MIGRATION-FREEZE marker.

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"time"
)

// migrationFreezeFileName mirrors github.com/Voxist/beads
// internal/migration.FileName. bd looks for this name in a start directory
// and every ancestor up to the filesystem root.
const migrationFreezeFileName = "MIGRATION-FREEZE"

// migrationFreezeEnvFile mirrors internal/migration.EnvFreezeFile. When set
// and non-empty it is authoritative: only that path is checked, and the
// ancestor walk is skipped entirely.
const migrationFreezeEnvFile = "BD_MIGRATION_FREEZE_FILE"

// migrationFreezeMaxMarkerSize mirrors internal/migration.maxMarkerSize: the
// payload is one short line, and the ancestor walk can reach directories gc
// does not control.
const migrationFreezeMaxMarkerSize = 64 << 10

// migrationFreezeInfo mirrors internal/migration.Info.
type migrationFreezeInfo struct {
	Operator  string
	Reason    string
	Timestamp time.Time
}

// migrationFreezeResult mirrors internal/migration.Result.
type migrationFreezeResult struct {
	// Path is the active marker's path, or "" when none was found.
	Path string
	// FromEnv reports that Path (or, on a failed lookup, the path that
	// failed) came from migrationFreezeEnvFile rather than the ancestor walk.
	FromEnv bool
	// Err reports that the lookup could not be completed. A non-nil Err is
	// always treated as frozen: an undeterminable gate is not an open gate.
	Err error
}

// Frozen mirrors internal/migration.Result.Frozen.
func (r migrationFreezeResult) Frozen() bool { return r.Path != "" || r.Err != nil }

// findMigrationFreezeFrom mirrors internal/migration.FindFrom(dir): it walks
// dir and its ancestors only, ignoring gc's own process cwd (see this file's
// header for why that diverges from the fork's Find). The env override still
// wins when set.
func findMigrationFreezeFrom(dir string) migrationFreezeResult {
	if r, handled := migrationFreezeEnvOverride(); handled {
		return r
	}
	return migrationFreezeWalk(dir)
}

func migrationFreezeEnvOverride() (migrationFreezeResult, bool) {
	override := os.Getenv(migrationFreezeEnvFile)
	if override == "" {
		return migrationFreezeResult{}, false
	}
	switch found, err := statMigrationFreezeMarker(override, true); {
	case err != nil:
		return migrationFreezeResult{FromEnv: true, Err: err}, true
	case found:
		return migrationFreezeResult{Path: override, FromEnv: true}, true
	default:
		return migrationFreezeResult{FromEnv: true}, true
	}
}

func migrationFreezeWalk(start string) migrationFreezeResult {
	if strings.TrimSpace(start) == "" {
		return migrationFreezeResult{}
	}
	dir, err := filepath.Abs(start)
	if err != nil {
		return migrationFreezeResult{}
	}
	for {
		candidate := filepath.Join(dir, migrationFreezeFileName)
		switch found, statErr := statMigrationFreezeMarker(candidate, false); {
		case statErr != nil:
			return migrationFreezeResult{Err: statErr}
		case found && !migrationFreezeInUntrustedDir(dir):
			return migrationFreezeResult{Path: candidate}
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			break
		}
		dir = parent
	}
	return migrationFreezeResult{}
}

// statMigrationFreezeMarker mirrors internal/migration.statMarker. trusted
// distinguishes the operator-named env-override path from one the untrusted
// ancestor walk stumbled onto, which decides how a symlink is treated.
func statMigrationFreezeMarker(path string, trusted bool) (bool, error) {
	info, err := os.Lstat(path)
	if err != nil {
		return classifyMigrationFreezeStatErr(path, err)
	}
	switch {
	case info.Mode().IsRegular():
		return true, nil
	case info.Mode()&os.ModeSymlink != 0:
		return statMigrationFreezeSymlinkMarker(path, trusted)
	default:
		// A directory, device, socket, or similar named MIGRATION-FREEZE is
		// not a marker.
		return false, nil
	}
}

// statMigrationFreezeSymlinkMarker mirrors internal/migration.statSymlinkMarker.
func statMigrationFreezeSymlinkMarker(path string, trusted bool) (bool, error) {
	if !trusted {
		fmt.Fprintf(os.Stderr, //nolint:errcheck
			"gc: ignoring symlinked %s at %s; a freeze marker must be a regular file (set %s to honor a symlink)\n",
			migrationFreezeFileName, path, migrationFreezeEnvFile)
		return false, nil
	}
	info, err := os.Stat(path)
	if err != nil {
		return classifyMigrationFreezeStatErr(path, err)
	}
	return info.Mode().IsRegular(), nil
}

// classifyMigrationFreezeStatErr mirrors internal/migration.classifyMarkerStatErr:
// a genuinely-absent path is "not a marker"; anything else is undeterminable
// and reported so the caller treats it as frozen.
func classifyMigrationFreezeStatErr(path string, err error) (bool, error) {
	switch {
	case errors.Is(err, fs.ErrNotExist), errors.Is(err, fs.ErrInvalid):
		return false, nil
	case errors.Is(err, syscall.ENOTDIR):
		// A component of path is not a directory, so the marker cannot exist
		// here. That is an answer, not a failure.
		return false, nil
	default:
		return false, fmt.Errorf("checking for a %s marker at %s: %w", migrationFreezeFileName, path, err)
	}
}

// migrationFreezeInUntrustedDir mirrors internal/migration.inUntrustedDir: a
// world-writable sticky directory (/tmp and its kin) is both forgeable by any
// local account and, thanks to the sticky bit, undeletable by whoever a
// refusal would tell to remove it. The env override is exempt (checked
// before this is ever called): that path is the operator's own stated
// intent, not something the walk stumbled onto.
func migrationFreezeInUntrustedDir(dir string) bool {
	info, err := os.Stat(dir)
	if err != nil {
		return false
	}
	mode := info.Mode()
	return mode.IsDir() && mode&os.ModeSticky != 0 && mode.Perm()&0o002 != 0
}

// readMigrationFreezeInfo mirrors internal/migration.ReadFile: it parses the
// marker at path, returning nil when the file is missing or unreadable.
func readMigrationFreezeInfo(path string) *migrationFreezeInfo {
	if path == "" {
		return nil
	}
	f, err := os.Open(path) //nolint:gosec // G304: path comes from findMigrationFreezeFrom — an ancestor walk for a fixed filename, or the operator's own BD_MIGRATION_FREEZE_FILE
	if err != nil {
		return nil
	}
	defer func() { _ = f.Close() }()
	data, err := io.ReadAll(io.LimitReader(f, migrationFreezeMaxMarkerSize))
	if err != nil {
		return nil
	}
	return parseMigrationFreezeInfo(string(data))
}

// parseMigrationFreezeInfo mirrors internal/migration.parse: operator,
// RFC3339 timestamp, and reason, tab-separated on the marker's first line.
// Every field is optional — an empty file is a valid freeze.
func parseMigrationFreezeInfo(content string) *migrationFreezeInfo {
	line, _, _ := strings.Cut(content, "\n")
	line = strings.TrimRight(line, "\r")
	if strings.TrimSpace(line) == "" {
		return &migrationFreezeInfo{}
	}
	parts := strings.SplitN(line, "\t", 3)
	info := &migrationFreezeInfo{}
	info.Operator = strings.TrimSpace(parts[0])
	if len(parts) >= 2 {
		if t, err := time.Parse(time.RFC3339, strings.TrimSpace(parts[1])); err == nil {
			info.Timestamp = t
		}
	}
	if len(parts) >= 3 {
		info.Reason = strings.TrimSpace(parts[2])
	}
	return info
}

// ErrNativeOpenFrozen is returned by checkMigrationFreezeForNativeOpen when
// an active MIGRATION-FREEZE marker covers scopeRoot. Every caller of it
// treats this as "do not open natively" and lets the error propagate to
// whatever the existing native-open failure path already does: for the
// factory's initial open (cmd/gc/main.go -> internal/beads/factory.go
// OpenStoreAtForCity), that is falling back to the BdStore/CLI path — which
// honors the freeze itself (github.com/Voxist/beads cmd/bd) — with a
// diagnostic logged via the existing native_open failure branch; for a
// read-path reconnect (beads.OpenNativeStorage as a NativeReopenFunc, which
// bypasses that factory path and its preflight entirely) there is no
// mid-operation fallback to fail over to, so the reconnect simply fails
// closed and the read it was serving returns this error.
var ErrNativeOpenFrozen = errors.New("beads: migration freeze is active; refusing a native (in-process) open")

// checkMigrationFreezeForNativeOpen is the ga-vwupk choke-point guard. Call
// it immediately before every native open — including a reconnect, which
// historically skipped preflight and every other check entirely (the very
// gap this bead exists to close). See this file's header for the marker
// format and the drift-risk note.
func checkMigrationFreezeForNativeOpen(scopeRoot string) error {
	result := findMigrationFreezeFrom(filepath.Join(scopeRoot, ".beads"))
	if !result.Frozen() {
		return nil
	}
	detail := migrationFreezeDetail(result, readMigrationFreezeInfo(result.Path))
	// No logger is threaded through this choke point (its callers are the
	// low-level native-open functions, several layers below anything that
	// carries a *slog.Logger today), so this matches this package's own
	// nil-logger convention (proxied_incident_log.go): slog.Default() rather
	// than a bespoke log.Printf line.
	slog.Default().Warn("gc: refusing a native (in-process) beads open",
		slog.String("scope", scopeRoot), slog.String("detail", detail))
	return fmt.Errorf("%w: %s", ErrNativeOpenFrozen, detail)
}

// MigrationFrozen reports whether an active MIGRATION-FREEZE marker covers
// scopeRoot, without logging or erroring. checkMigrationFreezeForNativeOpen
// above remains the actual choke-point gate for every native open and is
// what every caller in this package relies on; this export exists for a
// caller OUTSIDE this package whose own path has an irreversible side
// effect earlier than any native open.
//
// The PRIMARY gate, as of ga-vwupk round 3, is factory.go's
// OpenStoreAtForCity: it checks CheckMigrationFreeze before the preflight
// checker's bd-context probe (BDContext, which maps to bdRuntimeEnvFor with
// allowRecovery=true) or a proxied scope's admission ladder (which can fork
// a bd command to bring a stopped proxy back) ever run, both of which sit
// upstream of any native open. cmd/gc's native (direct and proxied)
// read-path reopen and initial-open closures ALSO call this (or
// CheckMigrationFreeze below) first, as defense in depth for a caller that
// constructs beads.StoreOpenOptions directly rather than through the
// factory -- in production the factory-level check means they never reach
// a frozen scope's own copy of this call at all.
//
// This claim is scoped to the native-open/reopen/admission layer and the
// factory's own preflight/admission gating on purpose: the BdStore/CLI
// FALLBACK's own env resolution (bdRuntimeEnvWithErrorRecoveryContext,
// reached via bdCommandRunnerWithManagedRetryFor whenever a bd command
// actually runs) is a separate, currently UNGATED recovery trigger -- once
// a frozen scope reaches the fallback (which it now does immediately), that
// path's first bd command can still restart a stopped managed Dolt, and on
// the proxied lane bd's own front door can still bring a stopped proxy back
// on its next read. See ga-lc97s, which names both: the preflight runner
// (bdCommandRunnerForCity / bdCommandRunnerWithManagedRetryFor, cmd/gc/bd_env.go)
// and the proxied front door (bd's own CLI, reached once gc has fallen back
// to BdStore for a proxied scope).
func MigrationFrozen(scopeRoot string) bool {
	return findMigrationFreezeFrom(filepath.Join(scopeRoot, ".beads")).Frozen()
}

// CheckMigrationFreeze is MigrationFrozen's error-returning sibling: nil
// when scopeRoot is not frozen, otherwise the same detail-carrying,
// errors.Is(err, ...)-comparable error checkMigrationFreezeForNativeOpen
// itself would return (including its slog.Warn line). Prefer this over the
// bare-bool MigrationFrozen when the caller is about to propagate an error
// anyway: it carries the marker's own operator/reason detail via %w
// instead of a hand-written message that would have to be kept in sync by
// hand.
//
// Multiple layers now call this (or MigrationFrozen) for the same scope as
// defense in depth -- factory.go's OpenStoreAtForCity, then, only if that
// somehow did not run, each native-open/reopen/admission closure in cmd/gc.
// This is not a double-logging risk IN PRACTICE, because every one of
// those callers is itself an early-return guard: whichever layer is
// reached FIRST in a given call chain returns immediately on a frozen
// result, so no later layer (including checkMigrationFreezeForNativeOpen's
// own choke point) is ever reached in that SAME chain to log again. It
// would double-log only if something called more than one of these layers
// back to back outside that normal chain (e.g. a test or a future caller
// probing the scope manually before also triggering an open) -- a case
// that is, by construction, an unusual use of these functions rather than
// the production call graph.
func CheckMigrationFreeze(scopeRoot string) error {
	return checkMigrationFreezeForNativeOpen(scopeRoot)
}

func migrationFreezeDetail(result migrationFreezeResult, info *migrationFreezeInfo) string {
	if result.Err != nil {
		return fmt.Sprintf("freeze marker lookup could not be completed, treated as frozen: %v", result.Err)
	}
	detail := result.Path
	switch {
	case info != nil && info.Operator != "" && info.Reason != "":
		detail += fmt.Sprintf(" (operator=%q, reason=%q)", info.Operator, info.Reason)
	case info != nil && info.Operator != "":
		detail += fmt.Sprintf(" (operator=%q)", info.Operator)
	case info != nil && info.Reason != "":
		detail += fmt.Sprintf(" (reason=%q)", info.Reason)
	}
	if result.FromEnv {
		detail += fmt.Sprintf(" [%s]", migrationFreezeEnvFile)
	}
	return detail
}

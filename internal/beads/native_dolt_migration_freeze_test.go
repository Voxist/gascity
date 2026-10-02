package beads

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	beadslib "github.com/steveyegge/beads"
)

// clearAmbientMigrationFreezeEnv neutralizes an ambient BD_MIGRATION_FREEZE_FILE
// the operator's own shell might export, so a developer machine configured
// with a real freeze override cannot make an "unfrozen" test in this file see
// a spurious freeze (or an "env override wins" test see a DIFFERENT marker
// than the one it set up). Setting it to "" rather than leaving it alone is
// equivalent to unset for migrationFreezeEnvOverride's own empty-string check,
// and t.Setenv restores the real value (if any) when the test ends.
func clearAmbientMigrationFreezeEnv(t *testing.T) {
	t.Helper()
	t.Setenv(migrationFreezeEnvFile, "")
}

// TestDirectNativeOpenEnvKeysWithholdBDAllowRemoteMigrate pins the ga-vwupk
// structure directly, alongside the behavioral tests below: a SEPARATE
// withheld-keys list (directNativeOpenWithheldKeys), merged only at the
// point of use into directNativeOpenEnvKeys, rather than an addition to the
// shared nativeDoltOpenEnvKeys — which TestDirectNativeOpenEnvKeyListUnchanged
// (native_dolt_proxied_open_test.go) pins to exactly its original fourteen
// BEADS_ keys, and which would otherwise land the same key in both the direct
// and proxied-only lists (a state that test independently forbids).
func TestDirectNativeOpenEnvKeysWithholdBDAllowRemoteMigrate(t *testing.T) {
	clearAmbientMigrationFreezeEnv(t)
	if !slices.Contains(directNativeOpenWithheldKeys, "BD_ALLOW_REMOTE_MIGRATE") {
		t.Fatalf("directNativeOpenWithheldKeys = %v, want it to contain BD_ALLOW_REMOTE_MIGRATE", directNativeOpenWithheldKeys)
	}
	if slices.Contains(nativeDoltOpenEnvKeys, "BD_ALLOW_REMOTE_MIGRATE") {
		t.Fatal("BD_ALLOW_REMOTE_MIGRATE belongs only in directNativeOpenWithheldKeys, not in the shared nativeDoltOpenEnvKeys")
	}
	for _, key := range nativeDoltOpenEnvKeys {
		if !slices.Contains(directNativeOpenEnvKeys, key) {
			t.Errorf("directNativeOpenEnvKeys drops shared key %q", key)
		}
	}
	for _, key := range directNativeOpenWithheldKeys {
		if !slices.Contains(directNativeOpenEnvKeys, key) {
			t.Errorf("directNativeOpenEnvKeys drops withheld key %q", key)
		}
	}
	if len(directNativeOpenEnvKeys) != len(nativeDoltOpenEnvKeys)+len(directNativeOpenWithheldKeys) {
		t.Errorf("directNativeOpenEnvKeys has %d keys, want %d + %d",
			len(directNativeOpenEnvKeys), len(nativeDoltOpenEnvKeys), len(directNativeOpenWithheldKeys))
	}
}

// writeMigrationFreezeMarker writes a MIGRATION-FREEZE marker at scopeRoot's
// .beads directory (the same directory checkMigrationFreezeForNativeOpen
// walks from), in the fork bd's own tab-separated
// operator\ttimestamp\treason shape, with "migrator" as the operator (every
// caller here cares about the freeze existing, not who set it). See
// TestMigrationFreezeMarkerFixtureShape below for the copy-pasteable
// contract this mirrors.
func writeMigrationFreezeMarker(t *testing.T, scopeRoot, reason string) {
	t.Helper()
	beadsDir := filepath.Join(scopeRoot, ".beads")
	if err := os.MkdirAll(beadsDir, 0o755); err != nil {
		t.Fatalf("mkdir .beads: %v", err)
	}
	content := "migrator" + "\t" + time.Now().UTC().Format(time.RFC3339) + "\t" + reason + "\n"
	if err := os.WriteFile(filepath.Join(beadsDir, migrationFreezeFileName), []byte(content), 0o644); err != nil {
		t.Fatalf("write marker: %v", err)
	}
}

// ---------------------------------------------------------------------
// checkMigrationFreezeForNativeOpen — the choke-point guard itself
// ---------------------------------------------------------------------

func TestCheckMigrationFreezeForNativeOpen_FrozenWhenMarkerPresent(t *testing.T) {
	clearAmbientMigrationFreezeEnv(t)
	scopeRoot := t.TempDir()
	writeMigrationFreezeMarker(t, scopeRoot, "dolt v2 migration")

	err := checkMigrationFreezeForNativeOpen(scopeRoot)
	if !errors.Is(err, ErrNativeOpenFrozen) {
		t.Fatalf("checkMigrationFreezeForNativeOpen = %v, want ErrNativeOpenFrozen", err)
	}
}

func TestCheckMigrationFreezeForNativeOpen_UnfrozenWhenNoMarker(t *testing.T) {
	clearAmbientMigrationFreezeEnv(t)
	scopeRoot := t.TempDir()
	if err := checkMigrationFreezeForNativeOpen(scopeRoot); err != nil {
		t.Fatalf("checkMigrationFreezeForNativeOpen with no marker = %v, want nil", err)
	}
}

func TestCheckMigrationFreezeForNativeOpen_MarkerInAncestorDirFreezes(t *testing.T) {
	clearAmbientMigrationFreezeEnv(t)
	// The fork's own semantics: a marker anywhere in the ancestry freezes
	// everything beneath it, not just the exact .beads directory.
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, migrationFreezeFileName), []byte(""), 0o644); err != nil {
		t.Fatalf("write ancestor marker: %v", err)
	}
	scopeRoot := filepath.Join(root, "city", "rig")
	if err := os.MkdirAll(filepath.Join(scopeRoot, ".beads"), 0o755); err != nil {
		t.Fatalf("mkdir scope: %v", err)
	}

	err := checkMigrationFreezeForNativeOpen(scopeRoot)
	if !errors.Is(err, ErrNativeOpenFrozen) {
		t.Fatalf("checkMigrationFreezeForNativeOpen with an ancestor marker = %v, want ErrNativeOpenFrozen", err)
	}
}

func TestCheckMigrationFreezeForNativeOpen_EnvOverrideWins(t *testing.T) {
	clearAmbientMigrationFreezeEnv(t)
	scopeRoot := t.TempDir()
	writeMigrationFreezeMarker(t, scopeRoot, "in the ancestor walk")

	// An env override pointed at a file that does NOT exist opts OUT of the
	// freeze entirely, even though the ancestor walk would have found one —
	// this mirrors internal/migration.EnvFreezeFile's documented "point it
	// at a path that does not exist" opt-out.
	t.Setenv(migrationFreezeEnvFile, filepath.Join(t.TempDir(), "absent"))
	if err := checkMigrationFreezeForNativeOpen(scopeRoot); err != nil {
		t.Fatalf("env override to an absent path should opt out: got %v", err)
	}

	// An env override pointed at a file that DOES exist freezes, even
	// without any marker in scopeRoot's own ancestry.
	overridePath := filepath.Join(t.TempDir(), "override-marker")
	if err := os.WriteFile(overridePath, []byte(""), 0o644); err != nil {
		t.Fatalf("write override marker: %v", err)
	}
	t.Setenv(migrationFreezeEnvFile, overridePath)
	unrelatedScope := t.TempDir()
	err := checkMigrationFreezeForNativeOpen(unrelatedScope)
	if !errors.Is(err, ErrNativeOpenFrozen) {
		t.Fatalf("env override to an existing marker = %v, want ErrNativeOpenFrozen", err)
	}
}

// ---------------------------------------------------------------------
// The public native-open entry points — proving the guard actually sits
// at the choke point, before the real opener ever runs.
// ---------------------------------------------------------------------

func TestOpenNativeDoltStoreAtRefusesWhenFrozen(t *testing.T) {
	clearAmbientMigrationFreezeEnv(t)
	scopeRoot := t.TempDir()
	writeMigrationFreezeMarker(t, scopeRoot, "dolt v2 migration")

	oldOpen := nativeDoltOpenBestAvailable
	t.Cleanup(func() { nativeDoltOpenBestAvailable = oldOpen })
	nativeDoltOpenBestAvailable = func(context.Context, string) (beadslib.Storage, error) {
		t.Fatal("the real opener must never run when the scope is frozen")
		return nil, nil
	}

	_, err := OpenNativeDoltStoreAt(context.Background(), scopeRoot, nil)
	if !errors.Is(err, ErrNativeOpenFrozen) {
		t.Fatalf("OpenNativeDoltStoreAt while frozen = %v, want ErrNativeOpenFrozen", err)
	}
}

func TestOpenNativeDoltStoreAtUnaffectedWhenUnfrozen(t *testing.T) {
	clearAmbientMigrationFreezeEnv(t)
	scopeRoot := t.TempDir()

	oldOpen := nativeDoltOpenBestAvailable
	t.Cleanup(func() { nativeDoltOpenBestAvailable = oldOpen })
	opened := false
	nativeDoltOpenBestAvailable = func(context.Context, string) (beadslib.Storage, error) {
		opened = true
		return &nativeDoltStorageSpy{
			getConfig: func(context.Context, string) (string, error) { return "gc", nil },
		}, nil
	}

	store, err := OpenNativeDoltStoreAt(context.Background(), scopeRoot, nil)
	if err != nil {
		t.Fatalf("OpenNativeDoltStoreAt with no freeze: %v", err)
	}
	if store == nil {
		t.Fatal("OpenNativeDoltStoreAt returned a nil store with no error")
	}
	if !opened {
		t.Fatal("the real opener never ran for an unfrozen scope")
	}
}

// TestOpenNativeStorageReconnectRefusesWhenFrozen is the test the whole bead
// exists for: OpenNativeStorage is used directly as a NativeReopenFunc
// (cmd/gc/main.go's reopen hook) after a managed-Dolt hard-kill/rebind, which
// bypasses the store factory's OpenStoreAtForCity and its preflight check
// entirely (ga-vwupk audit). If a freeze went active while a long-lived
// native store was already open, the very next reconnect must still refuse
// — proving the guard lives at the shared open choke point
// (openNativeStorageWithCredentialCommand), not only on the initial-open
// path.
func TestOpenNativeStorageReconnectRefusesWhenFrozen(t *testing.T) {
	clearAmbientMigrationFreezeEnv(t)
	scopeRoot := t.TempDir()
	writeMigrationFreezeMarker(t, scopeRoot, "dolt v2 migration")

	oldOpen := nativeDoltOpenBestAvailable
	t.Cleanup(func() { nativeDoltOpenBestAvailable = oldOpen })
	nativeDoltOpenBestAvailable = func(context.Context, string) (beadslib.Storage, error) {
		t.Fatal("the real opener must never run on a reconnect into a frozen scope")
		return nil, nil
	}

	_, err := OpenNativeStorage(context.Background(), scopeRoot, nil)
	if !errors.Is(err, ErrNativeOpenFrozen) {
		t.Fatalf("OpenNativeStorage (reconnect) while frozen = %v, want ErrNativeOpenFrozen", err)
	}
}

func TestOpenNativeStorageReconnectUnaffectedWhenUnfrozen(t *testing.T) {
	clearAmbientMigrationFreezeEnv(t)
	scopeRoot := t.TempDir()

	oldOpen := nativeDoltOpenBestAvailable
	t.Cleanup(func() { nativeDoltOpenBestAvailable = oldOpen })
	opened := false
	nativeDoltOpenBestAvailable = func(context.Context, string) (beadslib.Storage, error) {
		opened = true
		return &nativeDoltStorageSpy{}, nil
	}

	storage, err := OpenNativeStorage(context.Background(), scopeRoot, nil)
	if err != nil {
		t.Fatalf("OpenNativeStorage reconnect with no freeze: %v", err)
	}
	if storage == nil {
		t.Fatal("OpenNativeStorage returned a nil handle with no error")
	}
	if !opened {
		t.Fatal("the real opener never ran for an unfrozen reconnect")
	}
}

// TestOpenNativeDoltStoreAtProxiedRefusesWhenFrozen covers the third
// required insertion point (proxied opens): OpenNativeDoltStoreAtProxied and
// its reopen hook OpenNativeStorageAtProxied both go through
// openNativeStorageProxied, guarded the same way as the direct lane.
func TestOpenNativeDoltStoreAtProxiedRefusesWhenFrozen(t *testing.T) {
	clearAmbientMigrationFreezeEnv(t)
	scopeRoot := filepath.Join(t.TempDir(), "scope")
	writeMigrationFreezeMarker(t, scopeRoot, "dolt v2 migration")

	oldOpen := nativeDoltOpenBestAvailable
	t.Cleanup(func() { nativeDoltOpenBestAvailable = oldOpen })
	nativeDoltOpenBestAvailable = func(context.Context, string) (beadslib.Storage, error) {
		t.Fatal("the real opener must never run for a frozen proxied scope")
		return nil, nil
	}

	if _, err := OpenNativeDoltStoreAtProxied(context.Background(), scopeRoot, nil); !errors.Is(err, ErrNativeOpenFrozen) {
		t.Fatalf("OpenNativeDoltStoreAtProxied while frozen = %v, want ErrNativeOpenFrozen", err)
	}
	if _, err := OpenNativeStorageAtProxied(context.Background(), scopeRoot, nil); !errors.Is(err, ErrNativeOpenFrozen) {
		t.Fatalf("OpenNativeStorageAtProxied (reopen hook) while frozen = %v, want ErrNativeOpenFrozen", err)
	}
}

// TestOpenNativeDoltStoreAtWithoutAmbientEnvRefusesWhenFrozen covers the
// fourth required insertion point: the beads-workspace storage binding
// (internal/storebinding/beadsworkspace/engine.go) opens exclusively through
// OpenNativeDoltStoreAtWithoutAmbientEnv and the WithoutAmbientEnv
// credential-command variants, all of which share
// openNativeStorageWithoutAmbientEnvWithCredentialCommand — guarded the same
// way. This test exercises that shared function directly rather than adding
// a beadsworkspace-specific fixture, since the choke point is what's under
// test, not that package's own wiring.
func TestOpenNativeDoltStoreAtWithoutAmbientEnvRefusesWhenFrozen(t *testing.T) {
	clearAmbientMigrationFreezeEnv(t)
	scopeRoot := t.TempDir()
	writeMigrationFreezeMarker(t, scopeRoot, "dolt v2 migration")

	oldOpen := nativeDoltOpenBestAvailable
	t.Cleanup(func() { nativeDoltOpenBestAvailable = oldOpen })
	nativeDoltOpenBestAvailable = func(context.Context, string) (beadslib.Storage, error) {
		t.Fatal("the real opener must never run for a frozen no-ambient-env scope")
		return nil, nil
	}

	if _, err := OpenNativeDoltStoreAtWithoutAmbientEnv(context.Background(), scopeRoot); !errors.Is(err, ErrNativeOpenFrozen) {
		t.Fatalf("OpenNativeDoltStoreAtWithoutAmbientEnv while frozen = %v, want ErrNativeOpenFrozen", err)
	}
}

// ---------------------------------------------------------------------
// BD_ALLOW_REMOTE_MIGRATE withholding on the direct lane
// ---------------------------------------------------------------------

func TestOpenNativeDoltStoreAtWithholdsBDAllowRemoteMigrate(t *testing.T) {
	clearAmbientMigrationFreezeEnv(t)
	t.Setenv("BD_ALLOW_REMOTE_MIGRATE", "1")

	oldOpen := nativeDoltOpenBestAvailable
	t.Cleanup(func() { nativeDoltOpenBestAvailable = oldOpen })
	var duringOpen string
	sawDuringOpen := false
	nativeDoltOpenBestAvailable = func(context.Context, string) (beadslib.Storage, error) {
		duringOpen = os.Getenv("BD_ALLOW_REMOTE_MIGRATE")
		sawDuringOpen = true
		return &nativeDoltStorageSpy{
			getConfig: func(context.Context, string) (string, error) { return "gc", nil },
		}, nil
	}

	scopeRoot := t.TempDir()
	if _, err := OpenNativeDoltStoreAt(context.Background(), scopeRoot, nil); err != nil {
		t.Fatalf("OpenNativeDoltStoreAt: %v", err)
	}
	if !sawDuringOpen {
		t.Fatal("the opener never ran")
	}
	if duringOpen != "" {
		t.Errorf("BD_ALLOW_REMOTE_MIGRATE during the native open = %q, want unset", duringOpen)
	}
	// Restored afterward — this is a withholding for the open window, not a
	// permanent strip of the operator's own ambient environment.
	if got := os.Getenv("BD_ALLOW_REMOTE_MIGRATE"); got != "1" {
		t.Errorf("BD_ALLOW_REMOTE_MIGRATE after the open = %q, want restored to 1", got)
	}
}

func TestOpenNativeStorageReconnectWithholdsBDAllowRemoteMigrate(t *testing.T) {
	clearAmbientMigrationFreezeEnv(t)
	// The reconnect lane goes through the exact same
	// openNativeStorageWithCredentialCommand as the initial open (that
	// sharing is the whole point of the fix), so this pins the same
	// property there.
	t.Setenv("BD_ALLOW_REMOTE_MIGRATE", "1")

	oldOpen := nativeDoltOpenBestAvailable
	t.Cleanup(func() { nativeDoltOpenBestAvailable = oldOpen })
	var duringOpen string
	nativeDoltOpenBestAvailable = func(context.Context, string) (beadslib.Storage, error) {
		duringOpen = os.Getenv("BD_ALLOW_REMOTE_MIGRATE")
		return &nativeDoltStorageSpy{}, nil
	}

	scopeRoot := t.TempDir()
	if _, err := OpenNativeStorage(context.Background(), scopeRoot, nil); err != nil {
		t.Fatalf("OpenNativeStorage: %v", err)
	}
	if duringOpen != "" {
		t.Errorf("BD_ALLOW_REMOTE_MIGRATE during the reconnect open = %q, want unset", duringOpen)
	}
	if got := os.Getenv("BD_ALLOW_REMOTE_MIGRATE"); got != "1" {
		t.Errorf("BD_ALLOW_REMOTE_MIGRATE after the reconnect = %q, want restored to 1", got)
	}
}

// ---------------------------------------------------------------------
// Marker parsing, and the shared fixture with Voxist/beads
// ---------------------------------------------------------------------

// TestMigrationFreezeMarkerFixtureShape pins the exact marker payload shape
// this file must keep byte-compatible with github.com/Voxist/beads
// internal/migration/freeze.go's parse function: operator, an RFC3339
// timestamp, and a reason, tab-separated on the marker's first line, with
// every field optional. This test body is written to be copy-pasteable
// (same fixture line, same field assertions — only the function names and
// the package's own Info/parse symbols differ) into the fork's own
// freeze_test.go, so a change to either side's marker format shows up as a
// fixture mismatch instead of a silent divergence (ga-vwupk, operator
// decision 2026-09-29: this drift risk is accepted, not eliminated).
func TestMigrationFreezeMarkerFixtureShape(t *testing.T) {
	clearAmbientMigrationFreezeEnv(t)
	const fixtureLine = "migrator\t2026-09-29T18:00:00Z\tdolt v2 migration\n"

	info := parseMigrationFreezeInfo(fixtureLine)
	if info.Operator != "migrator" {
		t.Errorf("Operator = %q, want migrator", info.Operator)
	}
	wantTime, _ := time.Parse(time.RFC3339, "2026-09-29T18:00:00Z")
	if !info.Timestamp.Equal(wantTime) {
		t.Errorf("Timestamp = %v, want %v", info.Timestamp, wantTime)
	}
	if info.Reason != "dolt v2 migration" {
		t.Errorf("Reason = %q, want %q", info.Reason, "dolt v2 migration")
	}

	// An empty marker (e.g. `touch MIGRATION-FREEZE`) is a valid freeze with
	// every field at its zero value — no fabricated "now" timestamp.
	empty := parseMigrationFreezeInfo("")
	if empty.Operator != "" || empty.Reason != "" || !empty.Timestamp.IsZero() {
		t.Errorf("empty marker parsed as %+v, want all zero values", empty)
	}

	// Only the first line is payload; a trailing line must not leak into
	// Reason.
	multiline := parseMigrationFreezeInfo("migrator\t2026-09-29T18:00:00Z\treason\nextra garbage line\n")
	if multiline.Reason != "reason" {
		t.Errorf("Reason with a trailing line = %q, want %q (no leakage)", multiline.Reason, "reason")
	}
}

func TestMigrationFreezeFileNameAndEnvMatchTheFork(t *testing.T) {
	clearAmbientMigrationFreezeEnv(t)
	// Pins the two literal strings against accidental drift — these must
	// match github.com/Voxist/beads internal/migration.FileName and
	// .EnvFreezeFile exactly, since gc reimplements rather than imports
	// (see this package's native_dolt_migration_freeze.go header).
	if migrationFreezeFileName != "MIGRATION-FREEZE" {
		t.Errorf("migrationFreezeFileName = %q, want MIGRATION-FREEZE", migrationFreezeFileName)
	}
	if migrationFreezeEnvFile != "BD_MIGRATION_FREEZE_FILE" {
		t.Errorf("migrationFreezeEnvFile = %q, want BD_MIGRATION_FREEZE_FILE", migrationFreezeEnvFile)
	}
}

// ---------------------------------------------------------------------
// Fail-closed carve-outs: symlink, sticky-directory, and EACCES
// ---------------------------------------------------------------------

// TestCheckMigrationFreezeForNativeOpen_UntrustedSymlinkIsIgnoredNotFollowed
// pins statMigrationFreezeSymlinkMarker's untrusted-walk behavior: a
// MIGRATION-FREEZE the ANCESTOR WALK merely passed through must not be
// followed even if it is a symlink to a real regular file elsewhere — an
// indirection like that could otherwise redirect the gate to a marker (or
// non-marker) an attacker controls. The scope stays UNFROZEN, and the
// bypass is announced on stderr (not asserted here; see
// statMigrationFreezeSymlinkMarker's doc comment) rather than silent.
func TestCheckMigrationFreezeForNativeOpen_UntrustedSymlinkIsIgnoredNotFollowed(t *testing.T) {
	clearAmbientMigrationFreezeEnv(t)
	root := t.TempDir()
	realTarget := filepath.Join(t.TempDir(), "real-marker")
	if err := os.WriteFile(realTarget, []byte("migrator\t2026-09-29T18:00:00Z\treal\n"), 0o644); err != nil {
		t.Fatalf("write symlink target: %v", err)
	}
	if err := os.Symlink(realTarget, filepath.Join(root, migrationFreezeFileName)); err != nil {
		t.Fatalf("create symlink marker: %v", err)
	}
	scopeRoot := filepath.Join(root, "city", "rig")
	if err := os.MkdirAll(filepath.Join(scopeRoot, ".beads"), 0o755); err != nil {
		t.Fatalf("mkdir scope: %v", err)
	}

	if err := checkMigrationFreezeForNativeOpen(scopeRoot); err != nil {
		t.Fatalf("checkMigrationFreezeForNativeOpen with an untrusted symlinked marker = %v, want nil (not followed)", err)
	}
}

// TestCheckMigrationFreezeForNativeOpen_TrustedSymlinkViaEnvOverrideIsFollowed
// is the mirror case: EnvFreezeFile is the operator's OWN stated intent, not
// something the ancestor walk stumbled onto, so a symlink there IS followed
// and a regular target still freezes.
func TestCheckMigrationFreezeForNativeOpen_TrustedSymlinkViaEnvOverrideIsFollowed(t *testing.T) {
	clearAmbientMigrationFreezeEnv(t)
	realTarget := filepath.Join(t.TempDir(), "real-marker")
	if err := os.WriteFile(realTarget, []byte(""), 0o644); err != nil {
		t.Fatalf("write symlink target: %v", err)
	}
	linkPath := filepath.Join(t.TempDir(), "current-marker")
	if err := os.Symlink(realTarget, linkPath); err != nil {
		t.Fatalf("create trusted symlink: %v", err)
	}
	t.Setenv(migrationFreezeEnvFile, linkPath)

	scopeRoot := t.TempDir()
	if err := checkMigrationFreezeForNativeOpen(scopeRoot); !errors.Is(err, ErrNativeOpenFrozen) {
		t.Fatalf("checkMigrationFreezeForNativeOpen with a trusted symlinked marker = %v, want ErrNativeOpenFrozen", err)
	}
}

// TestCheckMigrationFreezeForNativeOpen_StickyWorldWritableDirIsIgnored pins
// inUntrustedDir's carve-out: a marker sitting in a world-writable sticky
// directory (/tmp and its kin) is forgeable by any local account and, thanks
// to the sticky bit, undeletable by whoever a refusal would tell to remove
// it. Such a marker must not freeze the scope.
func TestCheckMigrationFreezeForNativeOpen_StickyWorldWritableDirIsIgnored(t *testing.T) {
	clearAmbientMigrationFreezeEnv(t)
	root := t.TempDir()
	stickyDir := filepath.Join(root, "sticky")
	if err := os.Mkdir(stickyDir, 0o755); err != nil {
		t.Fatalf("mkdir sticky dir: %v", err)
	}
	t.Cleanup(func() { _ = os.Chmod(stickyDir, 0o755) })
	// World-writable with the sticky bit set — the /tmp shape. os.Chmod
	// takes an os.FileMode, whose bit layout for the special bits
	// (ModeSticky/ModeSetuid/ModeSetgid) does NOT match the raw Unix mode_t
	// layout (S_ISVTX is bit 9 on disk, but os.ModeSticky is a high bit in
	// Go's own os.FileMode encoding) — passing the raw octal 0o1777
	// therefore does not set what one might expect from reading it as a
	// Unix permission literal. Composing it from os.ModeSticky, which
	// os.Chmod translates correctly, is neither a syscall-package
	// workaround nor a Darwin-specific issue (round 2 review correction: an
	// earlier version of this file wrongly blamed a Darwin os.Chmod bug for
	// what was this literal-vs-os.FileMode mismatch).
	if err := os.Chmod(stickyDir, 0o777|os.ModeSticky); err != nil {
		t.Fatalf("chmod sticky dir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(stickyDir, migrationFreezeFileName), []byte(""), 0o644); err != nil {
		t.Fatalf("write marker in sticky dir: %v", err)
	}
	scopeRoot := filepath.Join(stickyDir, "city", "rig")
	if err := os.MkdirAll(filepath.Join(scopeRoot, ".beads"), 0o755); err != nil {
		t.Fatalf("mkdir scope: %v", err)
	}

	if err := checkMigrationFreezeForNativeOpen(scopeRoot); err != nil {
		t.Fatalf("checkMigrationFreezeForNativeOpen with a marker in a sticky world-writable dir = %v, want nil (untrusted dir)", err)
	}
}

// TestCheckMigrationFreezeForNativeOpen_EACCESIsUndeterminableAndFailsClosed
// pins classifyMigrationFreezeStatErr's central safety property: a stat
// failure that is NOT a definitive not-exist (EACCES from an unsearchable
// ancestor directory, standing in for permission, I/O, or hung-mount errors)
// must be treated as FROZEN — "cannot tell" is not the same answer as "not
// frozen", and a write gate must never collapse the two.
func TestCheckMigrationFreezeForNativeOpen_EACCESIsUndeterminableAndFailsClosed(t *testing.T) {
	clearAmbientMigrationFreezeEnv(t)
	if os.Geteuid() == 0 {
		t.Skip("running as root bypasses directory permission checks")
	}
	root := t.TempDir()
	locked := filepath.Join(root, "locked")
	if err := os.Mkdir(locked, 0o755); err != nil {
		t.Fatalf("mkdir locked: %v", err)
	}
	scopeRoot := filepath.Join(locked, "city", "rig")
	if err := os.MkdirAll(filepath.Join(scopeRoot, ".beads"), 0o755); err != nil {
		t.Fatalf("mkdir scope before locking: %v", err)
	}
	// Restore search permission before t.TempDir's RemoveAll cleanup runs
	// (Cleanup is LIFO, so this registers after — and runs before — it).
	t.Cleanup(func() { _ = os.Chmod(locked, 0o755) })
	if err := os.Chmod(locked, 0o000); err != nil {
		t.Fatalf("chmod locked: %v", err)
	}

	if err := checkMigrationFreezeForNativeOpen(scopeRoot); !errors.Is(err, ErrNativeOpenFrozen) {
		t.Fatalf("checkMigrationFreezeForNativeOpen with an unsearchable ancestor (EACCES) = %v, want ErrNativeOpenFrozen (fail closed)", err)
	}
}

// ---------------------------------------------------------------------
// MigrationFrozen / CheckMigrationFreeze — the exported probes review
// round 2 asked to be pinned directly, not only through
// checkMigrationFreezeForNativeOpen's own tests.
// ---------------------------------------------------------------------

// TestMigrationFrozen is a 3-case table: marker present (frozen), marker
// absent (unfrozen), and EACCES on an ancestor directory (undeterminable,
// must fail CLOSED -- same as checkMigrationFreezeForNativeOpen's own
// EACCES case, pinned here for the bare-bool probe specifically since
// callers outside this package (cmd/gc's initial-open and reopen closures)
// depend on it directly).
func TestMigrationFrozen(t *testing.T) {
	clearAmbientMigrationFreezeEnv(t)

	t.Run("marker present", func(t *testing.T) {
		scopeRoot := t.TempDir()
		writeMigrationFreezeMarker(t, scopeRoot, "present case")
		if !MigrationFrozen(scopeRoot) {
			t.Fatal("MigrationFrozen with a marker present = false, want true")
		}
	})

	t.Run("marker absent", func(t *testing.T) {
		scopeRoot := t.TempDir()
		if MigrationFrozen(scopeRoot) {
			t.Fatal("MigrationFrozen with no marker = true, want false")
		}
	})

	t.Run("EACCES on an ancestor is undeterminable and fails closed", func(t *testing.T) {
		if os.Geteuid() == 0 {
			t.Skip("running as root bypasses directory permission checks")
		}
		root := t.TempDir()
		locked := filepath.Join(root, "locked")
		if err := os.Mkdir(locked, 0o755); err != nil {
			t.Fatalf("mkdir locked: %v", err)
		}
		scopeRoot := filepath.Join(locked, "city", "rig")
		if err := os.MkdirAll(filepath.Join(scopeRoot, ".beads"), 0o755); err != nil {
			t.Fatalf("mkdir scope before locking: %v", err)
		}
		t.Cleanup(func() { _ = os.Chmod(locked, 0o755) })
		if err := os.Chmod(locked, 0o000); err != nil {
			t.Fatalf("chmod locked: %v", err)
		}
		if !MigrationFrozen(scopeRoot) {
			t.Fatal("MigrationFrozen with an unsearchable ancestor (EACCES) = false, want true (fail closed)")
		}
	})
}

// TestCheckMigrationFreeze pins that the exported error-returning probe
// carries the same errors.Is-comparable sentinel as the unexported
// choke-point check, and is nil when unfrozen.
func TestCheckMigrationFreeze(t *testing.T) {
	clearAmbientMigrationFreezeEnv(t)

	t.Run("frozen", func(t *testing.T) {
		scopeRoot := t.TempDir()
		writeMigrationFreezeMarker(t, scopeRoot, "frozen case")
		err := CheckMigrationFreeze(scopeRoot)
		if !errors.Is(err, ErrNativeOpenFrozen) {
			t.Fatalf("CheckMigrationFreeze with a marker present = %v, want ErrNativeOpenFrozen", err)
		}
		// ga-vwupk round 3 LOW: this is exactly the detail a caller
		// propagating this error (every guarded closure in cmd/gc) relies
		// on to tell an operator WHO froze the scope and WHY, not just
		// THAT it is frozen.
		if !strings.Contains(err.Error(), `operator="migrator"`) {
			t.Fatalf("CheckMigrationFreeze error = %q, want it to contain the marker's operator= detail", err.Error())
		}
		if !strings.Contains(err.Error(), `reason="frozen case"`) {
			t.Fatalf("CheckMigrationFreeze error = %q, want it to contain the marker's reason= detail", err.Error())
		}
	})

	t.Run("unfrozen", func(t *testing.T) {
		scopeRoot := t.TempDir()
		if err := CheckMigrationFreeze(scopeRoot); err != nil {
			t.Fatalf("CheckMigrationFreeze with no marker = %v, want nil", err)
		}
	})
}

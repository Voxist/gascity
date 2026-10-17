package main

import (
	"bytes"
	"runtime/debug"
	"testing"
)

func TestNormalizeVersion(t *testing.T) {
	tests := []struct {
		in   string
		want string
	}{
		{in: "v0.13.0", want: "0.13.0"},
		{in: "0.13.0", want: "0.13.0"},
		{in: "v0.13.0-rc2.0.20260317225312-41a12e4914cb+dirty", want: "0.13.0-rc2"},
		{in: "v0.0.0-20260317225312-41a12e4914cb", want: "dev"},
		{in: "(devel)", want: "dev"},
		{in: "", want: "dev"},
		// SemVer build metadata must be preserved.
		{in: "1.3.5+ra.1", want: "1.3.5+ra.1"},
		{in: "1.3.5", want: "1.3.5"},
		// Pseudo-version with a newer timestamp still collapses.
		{in: "v0.0.0-20260719191849-4c2927134266", want: "dev"},
		// +incompatible is the one Go-specific suffix we strip.
		{in: "1.2.3+incompatible", want: "1.2.3"},
	}
	for _, tt := range tests {
		if got := normalizeVersion(tt.in); got != tt.want {
			t.Fatalf("normalizeVersion(%q) = %q, want %q", tt.in, got, tt.want)
		}
	}
}

func TestResolveBuildMetadataUsesModuleVersion(t *testing.T) {
	info := &debug.BuildInfo{
		Main: debug.Module{
			Version: "v0.13.0",
		},
	}
	version, commit, date := resolveBuildMetadata("dev", "unknown", "unknown", true, info)
	if version != "0.13.0" {
		t.Fatalf("version = %q, want %q", version, "0.13.0")
	}
	if commit != "unknown" {
		t.Fatalf("commit = %q, want unknown", commit)
	}
	if date != "unknown" {
		t.Fatalf("date = %q, want unknown", date)
	}
}

func TestResolveBeadsVersion(t *testing.T) {
	beads := "github.com/steveyegge/beads"
	tests := []struct {
		name string
		ok   bool
		info *debug.BuildInfo
		want string
	}{
		{name: "no build info", ok: false, info: nil, want: "unknown"},
		{name: "dep absent", ok: true, info: &debug.BuildInfo{}, want: "unknown"},
		{
			name: "dep present",
			ok:   true,
			info: &debug.BuildInfo{Deps: []*debug.Module{{Path: beads, Version: "v1.1.0"}}},
			want: "v1.1.0",
		},
		{
			name: "replace wins",
			ok:   true,
			info: &debug.BuildInfo{Deps: []*debug.Module{{
				Path:    beads,
				Version: "v1.1.0",
				Replace: &debug.Module{Path: beads, Version: "v1.1.1-0.20260704062855-e97839a2e1c0"},
			}}},
			want: "v1.1.1-0.20260704062855-e97839a2e1c0",
		},
		{
			name: "local dir replace has no version",
			ok:   true,
			info: &debug.BuildInfo{Deps: []*debug.Module{{
				Path:    beads,
				Version: "v1.1.0",
				Replace: &debug.Module{Path: "../beads"},
			}}},
			want: "../beads",
		},
	}
	for _, tt := range tests {
		if got := resolveBeadsVersion(tt.ok, tt.info); got != tt.want {
			t.Errorf("%s: resolveBeadsVersion = %q, want %q", tt.name, got, tt.want)
		}
	}
}

func TestFormatLongVersion(t *testing.T) {
	// Unstamped builds (plain go build / make build) must say so explicitly:
	// silence here is how three binaries claiming "1.1.1" hid three
	// different beads libraries.
	got := formatLongVersion("1.1.1", "50e120757-dirty", "2026-07-07T17:48:08Z", "v1.1.0", "")
	want := "1.1.1 (commit: 50e120757-dirty, built: 2026-07-07T17:48:08Z, beads: v1.1.0, base: unstamped)"
	if got != want {
		t.Errorf("formatLongVersion unstamped = %q, want %q", got, want)
	}

	got = formatLongVersion("1.1.1", "eb743642c", "2026-07-16T10:00:00Z", "v1.1.0", "Voxist/main@eb743642c+0-0")
	want = "1.1.1 (commit: eb743642c, built: 2026-07-16T10:00:00Z, beads: v1.1.0, base: Voxist/main@eb743642c+0-0)"
	if got != want {
		t.Errorf("formatLongVersion stamped = %q, want %q", got, want)
	}
}

func TestResolveBuildMetadataUsesVCSSettings(t *testing.T) {
	info := &debug.BuildInfo{
		Settings: []debug.BuildSetting{
			{Key: "vcs.revision", Value: "abc123"},
			{Key: "vcs.time", Value: "2026-03-17T00:00:00Z"},
			{Key: "vcs.modified", Value: "true"},
		},
	}
	version, commit, date := resolveBuildMetadata("dev", "unknown", "unknown", true, info)
	if version != "dev" {
		t.Fatalf("version = %q, want dev", version)
	}
	if commit != "abc123-dirty" {
		t.Fatalf("commit = %q, want %q", commit, "abc123-dirty")
	}
	if date != "2026-03-17T00:00:00Z" {
		t.Fatalf("date = %q, want %q", date, "2026-03-17T00:00:00Z")
	}
}

// TestResolveBuildMetadataDirtinessFollowsCommitIdentity pins the rule that
// keeps a foreign repository's working-tree state out of our build stamp: the
// toolchain's vcs.modified flag is trusted only when its companion
// vcs.revision describes the same commit the binary was linked against.
//
// The concrete failure this guards (ga-u7fb): Go's buildvcs looks for a `.git`
// *directory*, so inside a git worktree — where `.git` is a gitdir *file* — it
// keeps walking up and stamps whichever repository encloses the worktree. A
// polecat worktree sits inside the city directory, itself a git repo, so a
// pristine checkout at dc8327db3 was stamped with the city's 251dc9e0f9 and
// the city's dirtiness, and `gc version --long` reported `dc8327db3-dirty`.
// That lie propagates to the supervisor's /health build_id and to binary-drift
// detection, so it must not survive.
func TestResolveBuildMetadataDirtinessFollowsCommitIdentity(t *testing.T) {
	const (
		worktreeShort = "dc8327db3"
		worktreeFull  = "dc8327db335c0c99ad0be57dca851f51eae2f01b"
		cityFull      = "251dc9e0f9caf16320cda9e02460c9c867057d12"
	)
	tests := []struct {
		name        string
		stamped     string
		vcsRevision string
		vcsModified string
		want        string
	}{
		{
			name:        "foreign revision cannot dirty our stamp",
			stamped:     worktreeShort,
			vcsRevision: cityFull,
			vcsModified: "true",
			want:        worktreeShort,
		},
		{
			name:        "abbreviated stamp matches full revision",
			stamped:     worktreeShort,
			vcsRevision: worktreeFull,
			vcsModified: "true",
			want:        worktreeShort + "-dirty",
		},
		{
			name:        "full stamp matches full revision",
			stamped:     worktreeFull,
			vcsRevision: worktreeFull,
			vcsModified: "true",
			want:        worktreeFull + "-dirty",
		},
		{
			name:        "matching revision on a clean tree stays clean",
			stamped:     worktreeShort,
			vcsRevision: worktreeFull,
			vcsModified: "false",
			want:        worktreeShort,
		},
		{
			name:        "already-stamped dirtiness is not doubled",
			stamped:     worktreeShort + "-dirty",
			vcsRevision: worktreeFull,
			vcsModified: "true",
			want:        worktreeShort + "-dirty",
		},
		{
			name:        "unstamped build still adopts the toolchain revision",
			stamped:     "unknown",
			vcsRevision: worktreeFull,
			vcsModified: "true",
			want:        worktreeFull + "-dirty",
		},
		{
			name:        "abbreviation too short to identify a commit is not trusted",
			stamped:     "dc83",
			vcsRevision: worktreeFull,
			vcsModified: "true",
			want:        "dc83",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			info := &debug.BuildInfo{
				Settings: []debug.BuildSetting{
					{Key: "vcs.revision", Value: tt.vcsRevision},
					{Key: "vcs.modified", Value: tt.vcsModified},
				},
			}
			_, commit, _ := resolveBuildMetadata("dev", tt.stamped, "unknown", true, info)
			if commit != tt.want {
				t.Fatalf("commit = %q, want %q", commit, tt.want)
			}
		})
	}
}

// TestFormatShortVersion pins the default (no-flag) output contract
// (vp-9ry5w AC1/AC3): an unstamped "dev" build carries its commit in the
// same 9-char form artifact names use, never claims a semver it does not
// have, and reports whether the build carried no provenance at all so the
// caller can point the reader at --long/--json.
func TestFormatShortVersion(t *testing.T) {
	tests := []struct {
		name            string
		version, commit string
		want            string
		wantUnprov      bool
	}{
		{"dev full sha", "dev", "06e394c56b5ddeadbeef06e394c56b5ddeadbeef", "dev (06e394c56)", false},
		{"dev 9-char sha", "dev", "06e394c56", "dev (06e394c56)", false},
		{"dev dirty", "dev", "06e394c56b5ddeadbeef06e394c56b5ddeadbeef-dirty", "dev (06e394c56-dirty)", false},
		{"dev short hash kept whole", "dev", "abc1234", "dev (abc1234)", false},
		{"dev short hash dirty kept whole", "dev", "abc1234-dirty", "dev (abc1234-dirty)", false},
		{"dev unknown commit hints", "dev", "unknown", "dev", true},
		{"dev empty commit hints", "dev", "", "dev", true},
		{"stamped version unchanged", "1.2.3", "unknown", "1.2.3", false},
		{"stamped version ignores commit", "1.2.3", "06e394c56b5d", "1.2.3", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, unprov := formatShortVersion(tt.version, tt.commit)
			if got != tt.want {
				t.Errorf("formatShortVersion(%q, %q) = %q, want %q", tt.version, tt.commit, got, tt.want)
			}
			if unprov != tt.wantUnprov {
				t.Errorf("formatShortVersion(%q, %q) unprovenanced = %v, want %v", tt.version, tt.commit, unprov, tt.wantUnprov)
			}
		})
	}
}

func TestAbbreviateCommit(t *testing.T) {
	tests := []struct {
		in, want string
	}{
		{"06e394c56b5ddeadbeef06e394c56b5ddeadbeef", "06e394c56"},
		{"06e394c56", "06e394c56"},
		{"abc1234", "abc1234"},
		{"06e394c56b5ddeadbeef06e394c56b5ddeadbeef-dirty", "06e394c56-dirty"},
		{"abc1234-dirty", "abc1234-dirty"},
	}
	for _, tt := range tests {
		if got := abbreviateCommit(tt.in); got != tt.want {
			t.Errorf("abbreviateCommit(%q) = %q, want %q", tt.in, got, tt.want)
		}
	}
}

// TestVersionCmdShortFormUnstamped exercises the command wiring end to
// end: the default output carries the commit and the stderr hint stays
// quiet when the build knows what it is (vp-9ry5w AC1).
func TestVersionCmdShortFormUnstamped(t *testing.T) {
	savedVersion, savedCommit := version, commit
	version, commit = "dev", "06e394c56b5ddeadbeef06e394c56b5ddeadbeef"
	defer func() { version, commit = savedVersion, savedCommit }()

	var stdout, stderr bytes.Buffer
	cmd := newVersionCmd(&stdout, &stderr)
	cmd.SetArgs([]string{})
	cmd.SetOut(&stdout)
	cmd.SetErr(&stderr)
	if err := cmd.Execute(); err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if got, want := stdout.String(), "dev (06e394c56)\n"; got != want {
		t.Errorf("stdout = %q, want %q", got, want)
	}
	if stderr.Len() != 0 {
		t.Errorf("stderr = %q, want empty", stderr.String())
	}
}

// The unprovenanced corner (no ldflags, no VCS info): the output stays
// honest "dev" but must TELL the reader where provenance lives when it
// exists (vp-9ry5w AC1 "or tells them how to").
func TestVersionCmdShortFormUnprovenanced(t *testing.T) {
	savedVersion, savedCommit := version, commit
	version, commit = "dev", "unknown"
	defer func() { version, commit = savedVersion, savedCommit }()

	var stdout, stderr bytes.Buffer
	cmd := newVersionCmd(&stdout, &stderr)
	cmd.SetArgs([]string{})
	cmd.SetOut(&stdout)
	cmd.SetErr(&stderr)
	if err := cmd.Execute(); err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if got, want := stdout.String(), "dev\n"; got != want {
		t.Errorf("stdout = %q, want %q", got, want)
	}
	if !bytes.Contains(stderr.Bytes(), []byte("--long")) {
		t.Errorf("stderr = %q, want a pointer at --long", stderr.String())
	}
}

package main

import (
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

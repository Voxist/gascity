package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/fsys"
)

// Tests for vc-quqf (defense-in-depth from incident vc-9wa): a
// [[patches.agent]] entry whose (dir, name) target resolves to no agent in
// the merged config degrades to a composition warning and a skipped patch —
// the config still loads with every other patch applied — instead of
// aborting the city-wide load. Non-agent patch typos and malformed agent
// patches (empty name) keep the strict error contract.

func TestLoadWithIncludes_UnresolvedAgentPatchWarnsAndSkips(t *testing.T) {
	dir := t.TempDir()
	cityTOML := `
[workspace]
name = "test"

[providers.claude]
base = "builtin:claude"

[[agent]]
name = "claude"
provider = "claude"

[[patches.agent]]
name = "claude"
suspended = true

[[patches.agent]]
dir = "ghost-rig"
name = "ghost-agent"
suspended = true
`
	if err := os.WriteFile(filepath.Join(dir, "city.toml"), []byte(cityTOML), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg, prov, err := LoadWithIncludes(fsys.OSFS{}, filepath.Join(dir, "city.toml"))
	if err != nil {
		t.Fatalf("LoadWithIncludes failed; unresolvable patch target must degrade, not abort (vc-9wa): %v", err)
	}

	// The resolvable patch still applied.
	var found bool
	for i := range cfg.Agents {
		a := &cfg.Agents[i]
		if a.Dir == "" && a.Name == "claude" {
			found = true
			if !a.Suspended {
				t.Errorf("agent %q: Suspended=false, want true (remaining patches must still apply)", a.QualifiedName())
			}
		}
	}
	if !found {
		t.Fatal("agent \"claude\" not present in composed config")
	}

	// Exactly one unresolved-patch warning, naming index, dir, and name.
	var hits []string
	for _, w := range prov.Warnings {
		if IsUnresolvedAgentPatchWarning(w) {
			hits = append(hits, w)
		}
	}
	if len(hits) != 1 {
		t.Fatalf("unresolved-patch warnings = %d (%q), want exactly 1; all warnings: %q", len(hits), hits, prov.Warnings)
	}
	for _, want := range []string{"patches.agent[1]", `"ghost-rig/ghost-agent"`, `dir="ghost-rig"`, `name="ghost-agent"`} {
		if !strings.Contains(hits[0], want) {
			t.Errorf("warning %q missing %q", hits[0], want)
		}
	}
}

func TestLoadWithIncludes_NonAgentPatchTypoStillErrors(t *testing.T) {
	dir := t.TempDir()
	cityTOML := `
[workspace]
name = "test"

[[patches.rigs]]
name = "ghost-rig"
prefix = "gh"
`
	if err := os.WriteFile(filepath.Join(dir, "city.toml"), []byte(cityTOML), 0o644); err != nil {
		t.Fatal(err)
	}
	_, _, err := LoadWithIncludes(fsys.OSFS{}, filepath.Join(dir, "city.toml"))
	if err == nil {
		t.Fatal("expected error for rig patch typo; only [[patches.agent]] targets degrade to warnings")
	}
	if !strings.Contains(err.Error(), "not found in merged config") {
		t.Fatalf("unexpected error shape: %v", err)
	}
}

func TestLoadWithIncludes_AgentPatchMissingNameStillErrors(t *testing.T) {
	dir := t.TempDir()
	cityTOML := `
[workspace]
name = "test"

[[patches.agent]]
suspended = true
`
	if err := os.WriteFile(filepath.Join(dir, "city.toml"), []byte(cityTOML), 0o644); err != nil {
		t.Fatal(err)
	}
	_, _, err := LoadWithIncludes(fsys.OSFS{}, filepath.Join(dir, "city.toml"))
	if err == nil {
		t.Fatal("expected error for agent patch with empty name (malformed, not unresolvable)")
	}
	if !strings.Contains(err.Error(), "name is required") {
		t.Fatalf("unexpected error shape: %v", err)
	}
}

func TestUnresolvedAgentPatchWarningClassifier(t *testing.T) {
	// 109 is the patches.agent index from the 2026-06-30 vc-9wa incident.
	w := UnresolvedAgentPatchWarning(109, "", "platform-engineer")
	if !IsUnresolvedAgentPatchWarning(w) {
		t.Errorf("IsUnresolvedAgentPatchWarning(%q) = false, want true", w)
	}
	for _, want := range []string{"patches.agent[109]", `"platform-engineer"`, "patch skipped"} {
		if !strings.Contains(w, want) {
			t.Errorf("warning %q missing %q", w, want)
		}
	}
	for _, not := range []string{
		"patches.agent[3]: agent patch: name is required",
		`rig "x" not found in merged config`,
		"unrelated warning",
	} {
		if IsUnresolvedAgentPatchWarning(not) {
			t.Errorf("IsUnresolvedAgentPatchWarning(%q) = true, want false", not)
		}
	}
}

// TestLoadWithIncludes_WildcardPatchTargetMissingWarnsAndSkips extends the
// vc-9wa / vc-quqf invariant to upstream's rig="*" wildcard branch. The
// 2026-08-31 resync applied the fail-soft only to the (dir, name) branch and
// routed a nothing-matching wildcard into ApplyPatches' hard error, so one
// typo'd wildcard patch aborted the whole city config load — verified by
// running this exact fixture at the merge head (hard error) and at the fork
// parent 15913af6a^1 (warn-skipped). A wildcard that DOES match must still
// apply without a warning, so both halves are asserted here.
func TestLoadWithIncludes_WildcardPatchTargetMissingWarnsAndSkips(t *testing.T) {
	dir := t.TempDir()
	cityTOML := `
[workspace]
name = "test"

[providers.claude]
base = "builtin:claude"

[[agent]]
name = "claude"
provider = "claude"

[[patches.agent]]
rig = "*"
name = "claude"
suspended = true

[[patches.agent]]
rig = "*"
name = "ghost-agent"
suspended = true
`
	if err := os.WriteFile(filepath.Join(dir, "city.toml"), []byte(cityTOML), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg, prov, err := LoadWithIncludes(fsys.OSFS{}, filepath.Join(dir, "city.toml"))
	if err != nil {
		t.Fatalf("LoadWithIncludes failed; a nothing-matching wildcard patch must degrade, not abort (vc-9wa): %v", err)
	}

	// The matching wildcard still applied.
	var found bool
	for i := range cfg.Agents {
		a := &cfg.Agents[i]
		if a.Name == "claude" {
			found = true
			if !a.Suspended {
				t.Errorf("agent %q: Suspended=false, want true (a MATCHING wildcard patch must still apply)", a.QualifiedName())
			}
		}
	}
	if !found {
		t.Fatal("agent \"claude\" not present in composed config")
	}

	// Exactly one unresolved-patch warning, for the ghost only, and it is the
	// lint-recognized shape so the typo still blocks pre-commit.
	var hits []string
	for _, w := range prov.Warnings {
		if IsUnresolvedAgentPatchWarning(w) {
			hits = append(hits, w)
		}
	}
	if len(hits) != 1 {
		t.Fatalf("unresolved-patch warnings = %d (%q), want exactly 1; all warnings: %q", len(hits), hits, prov.Warnings)
	}
	for _, want := range []string{"patches.agent[1]", `"*/ghost-agent"`} {
		if !strings.Contains(hits[0], want) {
			t.Errorf("warning %q missing %q", hits[0], want)
		}
	}
}

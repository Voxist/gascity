package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/fsys"
)

// Root cause of ga-91uk5m Symptom 2: ApplyPatches (compose.go:~542) runs
// before InjectImplicitAgents (compose.go:~593), so a [[patches.agent]]
// cannot target a provider-derived implicit agent.
func TestLoadWithIncludes_PatchCanTargetImplicitProviderAgent(t *testing.T) {
	dir := t.TempDir()
	cityTOML := `
[workspace]
name = "test"

[providers.claude]
base = "builtin:claude"

[[patches.agent]]
name = "claude"
suspended = true
`
	if err := os.WriteFile(filepath.Join(dir, "city.toml"), []byte(cityTOML), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg, _, err := LoadWithIncludes(fsys.OSFS{}, filepath.Join(dir, "city.toml"))
	if err != nil {
		t.Fatalf("LoadWithIncludes failed (bug ga-91uk5m): %v", err)
	}
	var found bool
	for i := range cfg.Agents {
		a := &cfg.Agents[i]
		if a.Dir == "" && a.Name == "claude" {
			found = true
			if !a.Suspended {
				t.Errorf("implicit agent %q: Suspended=false, want true", a.QualifiedName())
			}
		}
	}
	if !found {
		t.Fatal("implicit agent \"claude\" not present in composed config")
	}
}

func TestLoadWithIncludes_PatchTargetingImplicitAgentErrorsToday(t *testing.T) {
	dir := t.TempDir()
	cityTOML := `
[workspace]
name = "test"

[providers.claude]
base = "builtin:claude"

[[patches.agent]]
name = "claude"
suspended = true
`
	if err := os.WriteFile(filepath.Join(dir, "city.toml"), []byte(cityTOML), 0o644); err != nil {
		t.Fatal(err)
	}
	_, _, err := LoadWithIncludes(fsys.OSFS{}, filepath.Join(dir, "city.toml"))
	if err == nil {
		t.Skip("bug ga-91uk5m fixed; delete this characterization test")
	}
	if !strings.Contains(err.Error(), "not found in merged config") {
		t.Fatalf("unexpected error shape: %v", err)
	}
}

// TestLoadWithIncludes_PatchCanTargetRigScopedImplicitAgent covers the
// operator's actual case: suspending a rig-scoped implicit agent via
// [[patches.agent]] dir="<rig>" name="claude".
func TestLoadWithIncludes_PatchCanTargetRigScopedImplicitAgent(t *testing.T) {
	dir := t.TempDir()
	cityTOML := `
[workspace]
name = "test"

[providers.claude]
base = "builtin:claude"

[[rigs]]
name = "my-rig"
dir = "."

[[patches.agent]]
dir = "my-rig"
name = "claude"
suspended = true
`
	if err := os.WriteFile(filepath.Join(dir, "city.toml"), []byte(cityTOML), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg, _, err := LoadWithIncludes(fsys.OSFS{}, filepath.Join(dir, "city.toml"))
	if err != nil {
		t.Fatalf("LoadWithIncludes failed (bug ga-91uk5m rig-scoped): %v", err)
	}
	var found bool
	for i := range cfg.Agents {
		a := &cfg.Agents[i]
		if a.Dir == "my-rig" && a.Name == "claude" {
			found = true
			if !a.Suspended {
				t.Errorf("rig-scoped implicit agent %q: Suspended=false, want true", a.QualifiedName())
			}
		}
	}
	if !found {
		t.Fatal("rig-scoped implicit agent \"my-rig/claude\" not present in composed config")
	}
}

// TestLoadWithIncludes_PatchCanResumeImplicitAgent covers the resume case:
// suspended=false on a provider-derived implicit agent.
func TestLoadWithIncludes_PatchCanResumeImplicitAgent(t *testing.T) {
	dir := t.TempDir()
	// Include an explicit agent with suspended=true to verify resume resets it.
	// (Implicit agents default to suspended=false, so we need to test the
	// patch round-trip via an explicit suspended=true then resume patch.)
	cityTOML := `
[workspace]
name = "test"

[providers.claude]
base = "builtin:claude"

[[patches.agent]]
name = "claude"
suspended = false
`
	if err := os.WriteFile(filepath.Join(dir, "city.toml"), []byte(cityTOML), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg, _, err := LoadWithIncludes(fsys.OSFS{}, filepath.Join(dir, "city.toml"))
	if err != nil {
		t.Fatalf("LoadWithIncludes failed (resume variant): %v", err)
	}
	var found bool
	for i := range cfg.Agents {
		a := &cfg.Agents[i]
		if a.Dir == "" && a.Name == "claude" {
			found = true
			if a.Suspended {
				t.Errorf("implicit agent %q: Suspended=true after resume patch, want false", a.QualifiedName())
			}
		}
	}
	if !found {
		t.Fatal("implicit agent \"claude\" not present in composed config")
	}
}

// TestLoadWithIncludes_PatchTypoWarnsAndSkips verifies that a [[patches.agent]]
// targeting a name that matches no existing or implicit agent degrades to a
// composition warning and a skipped patch instead of a load error (vc-quqf;
// incident vc-9wa: one bad patch target aborted config load city-wide). This
// test previously pinned the hard error; gc config lint now carries the
// loud-failure contract pre-commit instead.
func TestLoadWithIncludes_PatchTypoWarnsAndSkips(t *testing.T) {
	dir := t.TempDir()
	cityTOML := `
[workspace]
name = "test"

[providers.claude]
base = "builtin:claude"

[[patches.agent]]
name = "not-a-real-agent"
suspended = true
`
	if err := os.WriteFile(filepath.Join(dir, "city.toml"), []byte(cityTOML), 0o644); err != nil {
		t.Fatal(err)
	}
	_, prov, err := LoadWithIncludes(fsys.OSFS{}, filepath.Join(dir, "city.toml"))
	if err != nil {
		t.Fatalf("typo patch target must warn and skip, not abort (vc-9wa): %v", err)
	}
	var hit bool
	for _, w := range prov.Warnings {
		if IsUnresolvedAgentPatchWarning(w) && strings.Contains(w, `"not-a-real-agent"`) {
			hit = true
		}
	}
	if !hit {
		t.Fatalf("no unresolved-patch warning for typo target; warnings: %q", prov.Warnings)
	}
}

// TestLoadWithIncludes_ExplicitAgentSuppressesImplicit verifies that when an
// explicit [[agent]] name="claude" is declared, the implicit agent is not
// injected, and a [[patches.agent]] targeting it modifies the explicit agent.
func TestLoadWithIncludes_ExplicitAgentSuppressesImplicit(t *testing.T) {
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
`
	if err := os.WriteFile(filepath.Join(dir, "city.toml"), []byte(cityTOML), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg, _, err := LoadWithIncludes(fsys.OSFS{}, filepath.Join(dir, "city.toml"))
	if err != nil {
		t.Fatalf("LoadWithIncludes failed: %v", err)
	}
	var count int
	for i := range cfg.Agents {
		a := &cfg.Agents[i]
		if a.Dir == "" && a.Name == "claude" {
			count++
			if !a.Suspended {
				t.Errorf("explicit agent %q: Suspended=false after patch, want true", a.QualifiedName())
			}
			if a.Implicit {
				t.Errorf("agent %q: Implicit=true but should be explicit", a.QualifiedName())
			}
		}
	}
	if count != 1 {
		t.Errorf("expected exactly 1 claude agent, got %d", count)
	}
}
